// OpenCodeAdapter drives opencode through its headless server mode
// (`opencode serve`): one local HTTP+SSE server per bridge profile,
// sessions per chat scope, prompt_async for non-blocking prompts,
// abort for interrupts, and /global/health for liveness probes.
//
// Session ids are persisted by the bridge (sessions.json), so
// conversations survive both server and bridge restarts (opencode
// stores sessions on disk).
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// OpenCodeAdapter manages one opencode serve process and routes its
// global SSE event bus to per-run channels.
type OpenCodeAdapter struct {
	binary      string
	botIdentity *BotIdentity
	// Env injected into the server child (lark-cli context etc.).
	Env map[string]string

	mu        sync.Mutex
	server    *ocServer
	sessions  map[string]string // scope → sessionID
	sessionMu sync.Mutex
}

// NewOpenCodeAdapter builds an adapter around the given opencode binary.
func NewOpenCodeAdapter(binary string) *OpenCodeAdapter {
	if binary == "" {
		binary = "opencode"
	}
	return &OpenCodeAdapter{binary: binary, sessions: map[string]string{}}
}

func (a *OpenCodeAdapter) ID() string          { return "opencode" }
func (a *OpenCodeAdapter) DisplayName() string { return "OpenCode" }

func (a *OpenCodeAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

// ResetSession forgets the scope's session mapping (/new, /cd).
// The on-disk opencode session is left intact but unused.
func (a *OpenCodeAdapter) ResetSession(scope string) {
	a.sessionMu.Lock()
	delete(a.sessions, scope)
	a.sessionMu.Unlock()
}

func (a *OpenCodeAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for OpenCodeAdapter.Run")
	}
	srv, err := a.ensureServer()
	if err != nil {
		return nil, err
	}

	sessionID, err := a.sessionFor(srv, opts)
	if err != nil {
		return nil, err
	}

	run := srv.registerRun(sessionID, opts.Cwd)

	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": opts.Prompt}},
	}
	if sys := BuildSystemPrompt(a.botIdentity); sys != "" {
		body["system"] = sys
	}
	if opts.Model != "" {
		if provider, model, ok := splitModel(opts.Model); ok {
			body["model"] = map[string]any{"providerID": provider, "modelID": model}
		}
	}
	// Attach local images as file parts (opencode reads them from disk).
	for _, img := range opts.Images {
		body["parts"] = append(body["parts"].([]map[string]any), map[string]any{
			"type": "file",
			"mime": guessMime(img),
			"url":  "file://" + img,
		})
	}

	if err := srv.postJSON("/session/"+sessionID+"/prompt_async?directory="+urlQueryEscape(opts.Cwd), body, nil); err != nil {
		srv.closeRun(sessionID, run)
		return nil, fmt.Errorf("prompt_async 失败: %w", err)
	}
	return run, nil
}

func splitModel(model string) (provider, id string, ok bool) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func guessMime(path string) string {
	switch lower := strings.ToLower(path); {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/png"
	}
}

// sessionFor resolves (or creates) the opencode session for a scope.
func (a *OpenCodeAdapter) sessionFor(srv *ocServer, opts RunOptions) (string, error) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()

	key := opts.Scope
	if key == "" {
		key = opts.RunID
	}
	if id := a.sessions[key]; id != "" {
		return id, nil
	}
	// The bridge persisted a session id from a previous process: reuse it
	// (opencode keeps sessions on disk).
	if opts.SessionID != "" {
		a.sessions[key] = opts.SessionID
		return opts.SessionID, nil
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := srv.postJSON("/session?directory="+urlQueryEscape(opts.Cwd), map[string]any{}, &resp); err != nil {
		return "", fmt.Errorf("创建 opencode 会话失败: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("创建 opencode 会话失败：响应缺少 id")
	}
	a.sessions[key] = resp.ID
	return resp.ID, nil
}

// ensureServer starts (or restarts) the opencode serve process.
func (a *OpenCodeAdapter) ensureServer() (*ocServer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil && a.server.healthy() {
		return a.server, nil
	}
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	srv, err := startOCServer(a.binary, port, a.Env)
	if err != nil {
		return nil, err
	}
	a.server = srv
	return srv, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ─── ocServer: one opencode serve process ──────────────────────────

type ocServer struct {
	cmd    *exec.Cmd
	base   string
	client *http.Client

	mu         sync.Mutex
	runs       map[string]map[uint64]*ocRun // sessionID → runs
	runSeq     uint64
	partKinds  map[string]string // partID → part type ("text"/"reasoning")
	stopping   bool
	eventLoops map[string]bool // directories with an active SSE loop
}

func startOCServer(binary string, port int, env map[string]string) (*ocServer, error) {
	cmd := exec.Command(binary, "serve", "--port", fmt.Sprint(port), "--hostname", "127.0.0.1")
	cmd.Env = mergeEnv(env)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn opencode serve: %w", err)
	}

	srv := &ocServer{
		cmd:        cmd,
		base:       fmt.Sprintf("http://127.0.0.1:%d", port),
		client:     &http.Client{Timeout: 30 * time.Second},
		runs:       map[string]map[uint64]*ocRun{},
		partKinds:  map[string]string{},
		eventLoops: map[string]bool{},
	}

	// Wait for the server to accept connections.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if srv.healthy() {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("opencode serve 启动超时")
		}
		time.Sleep(200 * time.Millisecond)
	}

	go func() {
		_ = cmd.Wait()
		srv.mu.Lock()
		for sessionID, runs := range srv.runs {
			for id, run := range runs {
				safeSend(run.events, Event{Type: EventError, Message: "opencode serve 进程意外退出", TerminationReason: TermFailed})
				close(run.events)
				delete(runs, id)
			}
			delete(srv.runs, sessionID)
		}
		srv.mu.Unlock()
	}()

	return srv, nil
}

func (s *ocServer) healthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/global/health", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *ocServer) shutdown() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *ocServer) postJSON(path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(s.base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// registerRun subscribes a run to a session's events and makes sure the
// directory-scoped SSE stream for the session's workspace is live.
//
// Note: opencode routes events per workspace — sessions created with
// ?directory=X publish to /event?directory=X, NOT to the global /event
// bus (verified against v1.18). One SSE loop per directory.
func (s *ocServer) registerRun(sessionID, directory string) *ocRun {
	s.mu.Lock()
	s.runSeq++
	id := s.runSeq
	run := &ocRun{srv: s, sessionID: sessionID, directory: directory, id: id, events: make(chan Event, 256)}
	if s.runs[sessionID] == nil {
		s.runs[sessionID] = map[uint64]*ocRun{}
	}
	s.runs[sessionID][id] = run
	if !s.eventLoops[directory] {
		s.eventLoops[directory] = true
		go s.eventLoop(directory)
	}
	s.mu.Unlock()
	return run
}

func (s *ocServer) closeRun(sessionID string, run *ocRun) {
	s.mu.Lock()
	if runs := s.runs[sessionID]; runs != nil {
		if _, ok := runs[run.id]; ok {
			delete(runs, run.id)
			close(run.events)
		}
		if len(runs) == 0 {
			delete(s.runs, sessionID)
		}
	}
	s.mu.Unlock()
}

// dispatch fans an SSE event out to the runs of its session.
func (s *ocServer) dispatch(env ocEventEnvelope) {
	sessionID, _ := env.Properties["sessionID"].(string)
	if sessionID == "" {
		return
	}

	events, terminal := s.translate(env)
	if len(events) == 0 && !terminal {
		return
	}
	s.mu.Lock()
	runs := s.runs[sessionID]
	for id, run := range runs {
		for _, evt := range events {
			safeSend(run.events, evt)
		}
		if terminal {
			close(run.events)
			delete(runs, id)
		}
	}
	if terminal && runs != nil && len(runs) == 0 {
		delete(s.runs, sessionID)
	}
	s.mu.Unlock()
}

type ocEventEnvelope struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// translate converts one bus event into bridge events; terminal is true
// when the session's turn ended (idle or error).
func (s *ocServer) translate(env ocEventEnvelope) (events []Event, terminal bool) {
	props := env.Properties
	switch env.Type {
	case "message.part.delta":
		partID, _ := props["partID"].(string)
		delta, _ := props["delta"].(string)
		if delta == "" {
			return nil, false
		}
		s.mu.Lock()
		kind := s.partKinds[partID]
		s.mu.Unlock()
		if kind == "reasoning" {
			return []Event{{Type: EventThinking, Delta: delta}}, false
		}
		return []Event{{Type: EventText, Delta: delta}}, false

	case "message.part.updated":
		part, _ := props["part"].(map[string]any)
		if part == nil {
			return nil, false
		}
		partType, _ := part["type"].(string)
		partID, _ := part["id"].(string)
		if partID != "" && partType != "" {
			s.mu.Lock()
			s.partKinds[partID] = partType
			s.mu.Unlock()
		}
		if partType != "tool" {
			return nil, false
		}
		state, _ := part["state"].(map[string]any)
		status := stringField(state, "status")
		callID := stringField(part, "callID")
		toolName := stringField(part, "tool")
		switch status {
		case "pending", "running":
			return []Event{{Type: EventToolUse, ID: callID, Name: toolName, Input: state["input"]}}, false
		case "completed", "error":
			out := stringField(state, "output")
			if out == "" {
				out = stringField(state, "title")
			}
			return []Event{{Type: EventToolResult, ID: callID, Output: out, IsError: status == "error"}}, false
		}
		return nil, false

	case "session.idle":
		return []Event{{Type: EventDone, TerminationReason: TermNormal}}, true

	case "session.error":
		errObj, _ := props["error"].(map[string]any)
		name := stringField(errObj, "name")
		if name == "MessageAbortedError" {
			// abort already emits session.idle right after; report the
			// interruption only if idle doesn't close the run first.
			return []Event{{Type: EventDone, TerminationReason: TermInterrupted}}, true
		}
		msg := ""
		if data, ok := errObj["data"].(map[string]any); ok {
			msg = stringField(data, "message")
		}
		if msg == "" {
			msg = name
		}
		return []Event{{Type: EventError, Message: "opencode: " + msg, TerminationReason: TermFailed}}, true
	}
	return nil, false
}

// eventLoop maintains the directory-scoped SSE subscription with backoff
// reconnect.
func (s *ocServer) eventLoop(directory string) {
	backoff := time.Second
	for {
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}
		err := s.consumeEvents(directory)
		if err != nil {
			log.Printf("[opencode] SSE 断开 (dir=%s): %v（%s 后重连）", directory, err, backoff)
		}
		select {
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (s *ocServer) consumeEvents(directory string) error {
	url := s.base + "/event"
	if directory != "" {
		url += "?directory=" + urlQueryEscape(directory)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// No client timeout: the stream is meant to stay open.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /event: HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" { // end of SSE frame
			if len(dataLines) > 0 {
				payload := strings.Join(dataLines, "\n")
				dataLines = nil
				var env ocEventEnvelope
				if err := json.Unmarshal([]byte(payload), &env); err == nil {
					s.dispatch(env)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return scanner.Err()
}

// ─── ocRun ─────────────────────────────────────────────────────────

type ocRun struct {
	srv       *ocServer
	sessionID string
	directory string
	id        uint64
	events    chan Event
	stopped   sync.Once
}

func (r *ocRun) Events() <-chan Event { return r.events }

// Stop aborts the session's current turn via the server API.
func (r *ocRun) Stop() {
	r.stopped.Do(func() {
		var out any
		if err := r.srv.postJSON("/session/"+r.sessionID+"/abort?directory="+urlQueryEscape(r.directory), map[string]any{}, &out); err != nil {
			log.Printf("[opencode] abort 失败: %v", err)
		}
	})
}

func (r *ocRun) WaitExit(timeoutMs int) bool { return true }

func urlQueryEscape(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		if b == '/' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '~' {
			sb.WriteByte(b)
		} else {
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}
