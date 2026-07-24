// PiAdapter drives the pi coding agent (pi-coding-agent) over its native
// RPC mode: one persistent `pi --mode rpc` process per chat scope, JSONL
// commands on stdin, streaming events on stdout. This is pi's native
// machine interface — no scraping, full streaming, graceful abort.
//
// Protocol: docs/rpc.md in the pi-coding-agent package.
package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// PiAdapter keeps one RPC process per scope (keyed with cwd, so /cd
// respawns). Session continuity survives bridge restarts via pi's own
// --session-id project sessions.
type PiAdapter struct {
	binary      string
	botIdentity *BotIdentity
	// Env injected into every child (lark-cli context etc.).
	Env map[string]string

	mu       sync.Mutex
	sessions map[string]*piSession
}

// NewPiAdapter builds an adapter around the given pi binary.
func NewPiAdapter(binary string) *PiAdapter {
	if binary == "" {
		binary = "pi"
	}
	return &PiAdapter{binary: binary, sessions: map[string]*piSession{}}
}

func (a *PiAdapter) ID() string          { return "pi" }
func (a *PiAdapter) DisplayName() string { return "Pi" }

func (a *PiAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

// ResetSession kills the scope's persistent process; the next Run spawns
// a fresh one (used for /new and /cd).
func (a *PiAdapter) ResetSession(scope string) {
	a.mu.Lock()
	ps := a.sessions[scope]
	delete(a.sessions, scope)
	a.mu.Unlock()
	if ps != nil {
		ps.kill()
	}
}

func (a *PiAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for PiAdapter.Run")
	}
	ps, err := a.sessionFor(opts)
	if err != nil {
		return nil, err
	}
	return ps.startRun(opts)
}

// sessionFor returns the live RPC process for the scope, spawning or
// respawning (cwd change, dead process) as needed.
func (a *PiAdapter) sessionFor(opts RunOptions) (*piSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := opts.Scope
	if key == "" {
		key = opts.RunID
	}
	if ps := a.sessions[key]; ps != nil {
		if ps.cwd == opts.Cwd && ps.alive() {
			return ps, nil
		}
		delete(a.sessions, key)
		ps.kill()
	}

	ps, err := a.spawn(opts)
	if err != nil {
		return nil, err
	}
	a.sessions[key] = ps
	return ps, nil
}

func (a *PiAdapter) spawn(opts RunOptions) (*piSession, error) {
	args := []string{"--mode", "rpc", "--append-system-prompt", BuildSystemPrompt(a.botIdentity)}
	if opts.SessionID != "" {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	cmd := exec.Command(a.binary, args...)
	cmd.Dir = opts.Cwd
	cmd.Env = mergeEnv(a.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Drain stderr so a noisy child can't block on a full pipe.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn pi: %w", err)
	}

	ps := &piSession{
		cmd:      cmd,
		stdin:    stdin,
		cwd:      opts.Cwd,
		pending:  map[string]chan piResponse{},
		runChans: map[uint64]chan Event{},
	}
	go ps.readLoop(stdout)
	go io.Copy(io.Discard, stderr)
	go func() {
		_ = cmd.Wait()
		ps.mu.Lock()
		ps.dead = true
		for id, ch := range ps.pending {
			close(ch)
			delete(ps.pending, id)
		}
		for id, ch := range ps.runChans {
			safeSend(ch, Event{Type: EventError, Message: "pi 进程意外退出", TerminationReason: TermFailed})
			close(ch)
			delete(ps.runChans, id)
		}
		ps.mu.Unlock()
	}()
	return ps, nil
}

// ─── piSession: one RPC process ────────────────────────────────────

type piResponse struct {
	Success bool
	Command string
	Error   string
}

type piSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	cwd   string

	writeMu  sync.Mutex // serializes stdin writes
	mu       sync.Mutex // guards the fields below
	dead     bool
	reqSeq   uint64
	runSeq   uint64
	pending  map[string]chan piResponse
	runChans map[uint64]chan Event // one per in-flight Run

	sessionID string
}

func (ps *piSession) alive() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return !ps.dead
}

func (ps *piSession) kill() {
	if ps.cmd.Process != nil {
		_ = ps.cmd.Process.Kill()
	}
}

// send writes one JSONL command; when wantResponse, waits for the
// correlated response (command acceptance, not run completion).
func (ps *piSession) send(cmd map[string]any, wantResponse bool) (piResponse, error) {
	ps.mu.Lock()
	ps.reqSeq++
	id := fmt.Sprintf("req-%d", ps.reqSeq)
	cmd["id"] = id
	var ch chan piResponse
	if wantResponse {
		ch = make(chan piResponse, 1)
		ps.pending[id] = ch
	}
	dead := ps.dead
	ps.mu.Unlock()

	if dead {
		ps.dropPending(id)
		return piResponse{}, fmt.Errorf("pi 进程已退出")
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		ps.dropPending(id)
		return piResponse{}, err
	}
	data = append(data, '\n')

	ps.writeMu.Lock()
	_, err = ps.stdin.Write(data)
	ps.writeMu.Unlock()
	if err != nil {
		ps.dropPending(id)
		return piResponse{}, fmt.Errorf("写入 pi 进程失败: %w", err)
	}

	if !wantResponse {
		return piResponse{Success: true}, nil
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return piResponse{}, fmt.Errorf("pi 进程意外退出")
		}
		return resp, nil
	case <-time.After(15 * time.Second):
		ps.dropPending(id)
		return piResponse{}, fmt.Errorf("pi 命令 %v 响应超时", cmd["type"])
	}
}

func (ps *piSession) dropPending(id string) {
	ps.mu.Lock()
	delete(ps.pending, id)
	ps.mu.Unlock()
}

// readLoop demultiplexes stdout: responses go to pending waiters, agent
// events are translated and fanned out to every active run channel.
func (ps *piSession) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		typ, _ := raw["type"].(string)

		if typ == "response" {
			if data, ok := raw["data"].(map[string]any); ok {
				if sid, ok := data["sessionId"].(string); ok && sid != "" {
					ps.mu.Lock()
					ps.sessionID = sid
					ps.mu.Unlock()
				}
			}
			id, _ := raw["id"].(string)
			resp := piResponse{Command: stringField(raw, "command")}
			resp.Success, _ = raw["success"].(bool)
			if !resp.Success {
				resp.Error = errorMessage(raw, "pi command failed")
			}
			ps.mu.Lock()
			if ch := ps.pending[id]; ch != nil {
				ch <- resp
				delete(ps.pending, id)
			}
			ps.mu.Unlock()
			continue
		}

		events := translatePiEvent(raw)
		if len(events) == 0 {
			continue
		}
		terminal := false
		for _, evt := range events {
			if evt.Type == EventDone || evt.Type == EventError {
				terminal = true
			}
		}
		ps.mu.Lock()
		for id, ch := range ps.runChans {
			for _, evt := range events {
				safeSend(ch, evt)
			}
			if terminal {
				close(ch)
				delete(ps.runChans, id)
			}
		}
		ps.mu.Unlock()
	}
}

func safeSend(ch chan Event, evt Event) {
	defer func() { _ = recover() }()
	select {
	case ch <- evt:
	default: // slow consumer: drop rather than block the reader
	}
}

// ─── piRun: one Run over the persistent process ────────────────────

type piRun struct {
	ps      *piSession
	events  chan Event
	stopped sync.Once
}

func (ps *piSession) startRun(opts RunOptions) (Run, error) {
	// Ask for the session id once per process so the bridge can persist it.
	if ps.sessionID == "" {
		_, _ = ps.send(map[string]any{"type": "get_state"}, true)
	}

	ps.mu.Lock()
	ps.runSeq++
	id := ps.runSeq
	ch := make(chan Event, 256)
	ps.runChans[id] = ch
	sessionID := ps.sessionID
	ps.mu.Unlock()

	cmd := map[string]any{
		"type":    "prompt",
		"message": opts.Prompt,
	}
	if len(opts.Images) > 0 {
		var images []map[string]any
		for _, path := range opts.Images {
			img, err := encodeImage(path)
			if err != nil {
				log.Printf("[pi] image %s: %v", path, err)
				continue
			}
			images = append(images, img)
		}
		if len(images) > 0 {
			cmd["images"] = images
		}
	}
	resp, err := ps.send(cmd, true)
	if err != nil || !resp.Success {
		ps.mu.Lock()
		delete(ps.runChans, id)
		close(ch)
		ps.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("prompt 被拒绝: %s", resp.Error)
		}
		return nil, err
	}

	if sessionID != "" {
		safeSend(ch, Event{Type: EventSystem, SessionID: sessionID})
	}
	return &piRun{ps: ps, events: ch}, nil
}

func (r *piRun) Events() <-chan Event { return r.events }

// Stop sends a graceful abort; the process stays alive for the next run.
func (r *piRun) Stop() {
	r.stopped.Do(func() {
		_, _ = r.ps.send(map[string]any{"type": "abort"}, true)
	})
}

func (r *piRun) WaitExit(timeoutMs int) bool { return true }

// ─── pi event translation ──────────────────────────────────────────

// translatePiEvent converts one RPC stdout object into AgentEvents.
func translatePiEvent(raw map[string]any) []Event {
	typ, _ := raw["type"].(string)
	switch typ {
	case "message_update":
		ame, _ := raw["assistantMessageEvent"].(map[string]any)
		if ame == nil {
			return nil
		}
		switch ame["type"] {
		case "text_delta":
			if d, _ := ame["delta"].(string); d != "" {
				return []Event{{Type: EventText, Delta: d}}
			}
		case "thinking_delta":
			if d, _ := ame["delta"].(string); d != "" {
				return []Event{{Type: EventThinking, Delta: d}}
			}
		case "error":
			msg := errorMessage(ame, "pi message error")
			return []Event{{Type: EventError, Message: msg, TerminationReason: TermFailed}}
		}
	case "tool_execution_start":
		return []Event{{
			Type:  EventToolUse,
			ID:    stringField(raw, "toolCallId"),
			Name:  stringField(raw, "toolName"),
			Input: raw["args"],
		}}
	case "tool_execution_end":
		isErr, _ := raw["isError"].(bool)
		output := ""
		if result, ok := raw["result"].(map[string]any); ok {
			output = extractTextContent(result["content"])
		}
		return []Event{{Type: EventToolResult, ID: stringField(raw, "toolCallId"), Output: output, IsError: isErr}}
	case "agent_settled":
		return []Event{{Type: EventDone, TerminationReason: TermNormal}}
	}
	return nil
}

// extractTextContent flattens pi's [{type:"text",text:...}] content arrays.
func extractTextContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// encodeImage reads a local image for the RPC prompt images field.
func encodeImage(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mime := "image/png"
	switch lower := strings.ToLower(path); {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		mime = "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		mime = "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		mime = "image/webp"
	}
	return map[string]any{
		"type":     "image",
		"data":     base64.StdEncoding.EncodeToString(data),
		"mimeType": mime,
	}, nil
}
