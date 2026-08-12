// ExternalSession is a CLI agent session discovered on disk (or via the
// agent's server), bindable to a chat scope with /bind.
package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// ExternalSession describes one discoverable agent session.
type ExternalSession struct {
	ID         string
	Cwd        string
	Preview    string
	LastOutput string
	Status     ExternalSessionStatus
	UpdatedAt  time.Time
	Agent      config.AgentKind
}

// ExternalSessionStatus is a best-effort view of the latest persisted turn.
// Only adapters whose local history exposes lifecycle events populate it.
type ExternalSessionStatus string

const (
	SessionStatusUnknown ExternalSessionStatus = ""
	SessionStatusActive  ExternalSessionStatus = "active"
	SessionStatusIdle    ExternalSessionStatus = "idle"
)

// ShortID returns the id prefix used for display and /bind matching.
func (s ExternalSession) ShortID() string {
	if len(s.ID) > 8 {
		return s.ID[:8]
	}
	return s.ID
}

// SessionLister is implemented by adapters that can enumerate existing
// CLI sessions for /sessions + /bind.
type SessionLister interface {
	ListSessions(limit int) ([]ExternalSession, error)
}

// ListSessions dispatches to the per-kind discovery (file scanning for
// claude/pi/codex; adapters with a live server override via SessionLister).
func ListSessions(kind config.AgentKind, limit int) ([]ExternalSession, error) {
	switch kind {
	case config.AgentClaude:
		return scanClaudeSessions(limit)
	case config.AgentPi:
		return scanPiSessions(limit)
	case config.AgentOmp:
		return scanOmpSessions(limit)
	case config.AgentCodex:
		return scanCodexSessions(limit)
	}
	return nil, nil
}

// MatchSession resolves a token (1-based index, full id, or unique id
// prefix) against a session list; ambiguous prefixes return candidates.
func MatchSession(sessions []ExternalSession, token string) (*ExternalSession, []*ExternalSession) {
	if n := parseIndex(token); n >= 1 && n <= len(sessions) {
		return &sessions[n-1], nil
	}
	var prefix []*ExternalSession
	for i := range sessions {
		s := &sessions[i]
		if s.ID == token {
			return s, nil
		}
		if strings.HasPrefix(s.ID, token) {
			prefix = append(prefix, s)
		}
	}
	if len(prefix) == 1 {
		return prefix[0], nil
	}
	return nil, prefix
}

func parseIndex(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ─── shared scanning helpers ───────────────────────────────────────

type sessionFile struct {
	path  string
	mtime time.Time
}

// collectJSONL walks root for *.jsonl files, newest first.
func collectJSONL(root string, maxDepth int) ([]sessionFile, error) {
	var files []sessionFile
	root = filepath.Clean(root)
	baseDepth := len(strings.Split(root, string(filepath.Separator)))
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && len(strings.Split(path, string(filepath.Separator)))-baseDepth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") {
			files = append(files, sessionFile{path: path, mtime: info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	return files, nil
}

// scanJSONLLines reads at most maxLines lines of a jsonl file.
func scanJSONLLines(path string, maxLines int, fn func(line map[string]any) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for scanner.Scan() && maxLines > 0 {
		maxLines--
		var m map[string]any
		if json.Unmarshal(scanner.Bytes(), &m) == nil {
			if !fn(m) {
				return
			}
		}
	}
}

func clamp(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// ─── claude: ~/.claude/projects/*/*.jsonl ──────────────────────────

func claudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func scanClaudeSessions(limit int) ([]ExternalSession, error) {
	files, err := collectJSONL(filepath.Join(claudeHome(), "projects"), 2)
	if err != nil {
		return nil, err
	}
	var out []ExternalSession
	for _, f := range files {
		if limit > 0 && len(out) >= limit {
			break
		}
		sess := ExternalSession{Agent: config.AgentClaude, UpdatedAt: f.mtime}
		sess.ID = strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
		scanJSONLLines(f.path, 80, func(m map[string]any) bool {
			if sess.Cwd == "" {
				if cwd, _ := m["cwd"].(string); cwd != "" {
					sess.Cwd = cwd
				}
			}
			if sess.Preview == "" && m["type"] == "user" {
				if msg, ok := m["message"].(map[string]any); ok {
					sess.Preview = clamp(extractClaudeText(msg["content"]), 60)
				}
			}
			return sess.Cwd == "" || sess.Preview == ""
		})
		out = append(out, sess)
	}
	return out, nil
}

func extractClaudeText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		for _, part := range c {
			if m, ok := part.(map[string]any); ok && m["type"] == "text" {
				if t, _ := m["text"].(string); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// ─── pi: ~/.pi/agent/sessions/*/*.jsonl ────────────────────────────

func scanPiSessions(limit int) ([]ExternalSession, error) {
	return scanPiFamilySessions(piSessionsDir(nil), config.AgentPi, limit)
}

// ─── omp (Oh My Pi): ~/.omp/agent/sessions/*/*.jsonl ───────────────

func scanOmpSessions(limit int) ([]ExternalSession, error) {
	return scanPiFamilySessions(ompSessionsDir(nil), config.AgentOmp, limit)
}

// scanPiFamilySessions reads session v3 JSONL under root (shared by pi / omp).
func scanPiFamilySessions(root string, agent config.AgentKind, limit int) ([]ExternalSession, error) {
	files, err := collectJSONL(root, 2)
	if err != nil {
		return nil, err
	}
	var out []ExternalSession
	for _, f := range files {
		if limit > 0 && len(out) >= limit {
			break
		}
		sess := ExternalSession{Agent: agent, UpdatedAt: f.mtime}
		scanJSONLLines(f.path, 80, func(m map[string]any) bool {
			if m["type"] == "session" {
				sess.ID, _ = m["id"].(string)
				sess.Cwd, _ = m["cwd"].(string)
				return true
			}
			if sess.Preview == "" && m["type"] == "message" {
				if msg, ok := m["message"].(map[string]any); ok && msg["role"] == "user" {
					sess.Preview = clamp(extractTextContent(msg["content"]), 60)
				}
			}
			return sess.Preview == ""
		})
		if sess.ID != "" {
			out = append(out, sess)
		}
	}
	return out, nil
}

// ─── codex: ~/.codex/sessions/**/*.jsonl ───────────────────────────

func codexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func scanCodexSessions(limit int) ([]ExternalSession, error) {
	files, err := collectJSONL(filepath.Join(codexHome(), "sessions"), 5)
	if err != nil {
		return nil, err
	}
	var out []ExternalSession
	for _, f := range files {
		if limit > 0 && len(out) >= limit {
			break
		}
		sess := ExternalSession{Agent: config.AgentCodex, UpdatedAt: f.mtime}
		scanJSONLLines(f.path, 80, func(m map[string]any) bool {
			if m["type"] == "session_meta" {
				if payload, ok := m["payload"].(map[string]any); ok {
					sess.ID = stringField(payload, "id", "session_id")
					sess.Cwd, _ = payload["cwd"].(string)
				}
				return true
			}
			if sess.Preview == "" && m["type"] == "response_item" {
				if payload, ok := m["payload"].(map[string]any); ok {
					if payload["type"] == "message" && payload["role"] == "user" {
						sess.Preview = clamp(extractCodexText(payload["content"]), 60)
					}
				}
			}
			return sess.ID == "" || sess.Preview == ""
		})
		if sess.ID != "" {
			status, lastOutput := scanCodexSessionTail(f.path)
			sess.Status = status
			sess.LastOutput = lastOutput
			out = append(out, sess)
		}
	}
	return out, nil
}

const codexTailBytes = 512 * 1024

// scanCodexSessionTail reads only the newest part of a rollout. Tool output
// entries can be very large, so the first (possibly truncated) line is simply
// ignored. Walking backwards finds both the latest lifecycle marker and the
// latest user-visible agent message without loading the full history.
func scanCodexSessionTail(path string) (ExternalSessionStatus, string) {
	f, err := os.Open(path)
	if err != nil {
		return SessionStatusUnknown, ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SessionStatusUnknown, ""
	}
	start := info.Size() - codexTailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return SessionStatusUnknown, ""
	}
	data, err := io.ReadAll(io.LimitReader(f, codexTailBytes))
	if err != nil {
		return SessionStatusUnknown, ""
	}
	lines := bytes.Split(data, []byte{'\n'})
	status := SessionStatusUnknown
	lastOutput := ""
	for i := len(lines) - 1; i >= 0 && (status == SessionStatusUnknown || lastOutput == ""); i-- {
		var row map[string]any
		if json.Unmarshal(lines[i], &row) != nil {
			continue
		}
		payload := mapField(row, "payload")
		switch stringField(row, "type") {
		case "event_msg":
			switch stringField(payload, "type") {
			case "task_complete":
				if status == SessionStatusUnknown {
					status = SessionStatusIdle
				}
				if lastOutput == "" {
					lastOutput = clamp(stringField(payload, "last_agent_message"), 120)
				}
			case "task_started":
				if status == SessionStatusUnknown {
					status = SessionStatusActive
				}
			case "agent_message":
				if lastOutput == "" {
					lastOutput = clamp(stringField(payload, "message"), 120)
				}
			}
		case "response_item":
			if lastOutput == "" && stringField(payload, "type") == "message" && stringField(payload, "role") == "assistant" {
				lastOutput = clamp(extractCodexText(payload["content"]), 120)
			}
		}
	}
	return status, lastOutput
}

// codexSessionStatus returns the persisted status for one thread. Active
// threads are necessarily among the newest rollouts, but this lookup still
// scans all metadata files so a custom CODEX_HOME remains correct.
func codexSessionStatus(threadID string) ExternalSessionStatus {
	if threadID == "" {
		return SessionStatusUnknown
	}
	files, _ := collectJSONL(filepath.Join(codexHome(), "sessions"), 5)
	for _, f := range files {
		matched := false
		scanJSONLLines(f.path, 8, func(m map[string]any) bool {
			if m["type"] != "session_meta" {
				return true
			}
			payload, _ := m["payload"].(map[string]any)
			matched = stringField(payload, "id", "session_id") == threadID
			return false
		})
		if matched {
			status, _ := scanCodexSessionTail(f.path)
			return status
		}
	}
	return SessionStatusUnknown
}

func extractCodexText(content any) string {
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if t, _ := m["text"].(string); t != "" {
				return t
			}
		}
	}
	return ""
}

// ─── opencode: 经 serve GET /session（在 opencode.go 上实现） ────────
