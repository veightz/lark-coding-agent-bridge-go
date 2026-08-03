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

	"lark-coding-agent-bridge-go/internal/config"
)

// PiAdapter keeps one RPC process per scope (keyed with cwd, so /cd
// respawns). Session continuity survives bridge restarts via pi's own
// --session-id project sessions.
type PiAdapter struct {
	binary      string
	botIdentity *BotIdentity
	runtime     config.AgentRuntime
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
	args = append(args, piAccessArgs(opts.Access)...)
	cmd := agentCommand(a.runtime, a.binary, args...)
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
		runChans: map[uint64]*piRunSlot{},
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
		for id, slot := range ps.runChans {
			safeSend(slot.ch, Event{Type: EventError, Message: "pi 进程意外退出", TerminationReason: TermFailed})
			ps.closeSlotLocked(id, slot)
		}
		ps.mu.Unlock()
	}()
	return ps, nil
}

func piAccessArgs(access config.AccessLevel) []string {
	if access != config.AccessReadOnly {
		return nil
	}
	// Pi 没有进程级 sandbox；只读 profile 至少从源头移除 shell、写文件和
	// 扩展工具。read/grep/find/ls 是 Pi 0.83 的内置只读工具集合。
	return []string{"--tools", "read,grep,find,ls"}
}

// ─── piSession: one RPC process ────────────────────────────────────

type piResponse struct {
	Success bool
	Command string
	Error   string
	Data    map[string]any
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
	runChans map[uint64]*piRunSlot // one per in-flight Run

	sessionID string
	modelID   string
	// contextWindow is the current model's context window in tokens, read
	// from the RPC get_state/set_model response so ctx% can use the
	// agent-reported value instead of only the static pricing table.
	contextWindow int
}

// piRunSlot bundles a run's event channel with a settled signal that
// fires when the channel is closed (terminal event or process death).
type piRunSlot struct {
	ch      chan Event
	settled chan struct{}
}

func (ps *piSession) closeSlotLocked(id uint64, slot *piRunSlot) {
	close(slot.ch)
	close(slot.settled)
	delete(ps.runChans, id)
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
//
// For extension_ui_response the caller's id must be preserved (matches the
// extension_ui_request). Only assign a synthetic req-N id when absent.
func (ps *piSession) send(cmd map[string]any, wantResponse bool) (piResponse, error) {
	ps.mu.Lock()
	id, _ := cmd["id"].(string)
	if id == "" {
		ps.reqSeq++
		id = fmt.Sprintf("req-%d", ps.reqSeq)
		cmd["id"] = id
	}
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
				model := data["model"]
				if model == nil && stringField(raw, "command") == "set_model" {
					model = data
				}
				if id := piModelFullID(model); id != "" {
					ps.mu.Lock()
					ps.modelID = id
					ps.mu.Unlock()
				}
				if m, ok := model.(map[string]any); ok {
					if cw, ok := m["contextWindow"].(float64); ok && cw > 0 {
						ps.mu.Lock()
						ps.contextWindow = int(cw)
						ps.mu.Unlock()
					}
				}
			}
			id, _ := raw["id"].(string)
			resp := piResponse{Command: stringField(raw, "command")}
			resp.Data, _ = raw["data"].(map[string]any)
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

		// Dialog UI requests need a Reply bound to this session (stdin write).
		var events []Event
		if typ == "extension_ui_request" {
			method := stringField(raw, "method")
			switch method {
			case "select", "confirm", "input", "editor":
				events = ps.translatePiUIRequest(raw)
			default:
				events = translatePiEvent(raw)
			}
		} else {
			events = translatePiEvent(raw)
		}
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
		for id, slot := range ps.runChans {
			for _, evt := range events {
				safeSend(slot.ch, evt)
			}
			if terminal {
				ps.closeSlotLocked(id, slot)
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
	settled chan struct{}
	stopped sync.Once
}

func (ps *piSession) startRun(opts RunOptions) (Run, error) {
	// Ask for the session id once per process so the bridge can persist it.
	ps.mu.Lock()
	needsState := ps.sessionID == "" || ps.modelID == ""
	ps.mu.Unlock()
	if needsState {
		if resp, err := ps.send(map[string]any{"type": "get_state"}, true); err != nil || !resp.Success {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("读取 Pi 会话状态失败: %s", resp.Error)
		}
	}
	if opts.Model != "" {
		if err := ps.ensureModel(opts.Model); err != nil {
			return nil, err
		}
	}

	ps.mu.Lock()
	ps.runSeq++
	id := ps.runSeq
	slot := &piRunSlot{ch: make(chan Event, 256), settled: make(chan struct{})}
	ps.runChans[id] = slot
	sessionID := ps.sessionID
	modelID := ps.modelID
	contextWindow := ps.contextWindow
	ch := slot.ch
	// 在 prompt 交给 Pi 前先入队 system 事件，保证事件顺序稳定，也避免
	// agent_settled 抢先关闭 channel 后再补发 system 的 send/close 竞态。
	if sessionID != "" || modelID != "" {
		ch <- Event{Type: EventSystem, SessionID: sessionID, Model: modelID, ContextWindow: contextWindow}
	}
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
		ps.closeSlotLocked(id, slot)
		ps.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("prompt 被拒绝: %s", resp.Error)
		}
		return nil, err
	}

	return &piRun{ps: ps, events: ch, settled: slot.settled}, nil
}

// ensureModel applies a persisted /model choice to an already-running RPC
// process. Passing --model only at spawn time is insufficient because Pi
// processes are intentionally reused across messages in the same scope.
func (ps *piSession) ensureModel(want string) error {
	ps.mu.Lock()
	current := ps.modelID
	ps.mu.Unlock()
	if current == want {
		return nil
	}
	provider, modelID, ok := parsePiModelRef(want)
	if !ok {
		return fmt.Errorf("Pi 模型 %q 缺少 provider 前缀，请重新发送 /model", want)
	}
	resp, err := ps.send(map[string]any{
		"type":     "set_model",
		"provider": provider,
		"modelId":  modelID,
	}, true)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("切换 Pi 模型失败: %s", resp.Error)
	}
	applied := piModelFullID(resp.Data)
	if applied == "" {
		applied = want
	}
	ps.mu.Lock()
	ps.modelID = applied
	ps.mu.Unlock()
	return nil
}

func (r *piRun) Events() <-chan Event { return r.events }

// Stop sends a graceful abort; the process stays alive for the next run.
// If the abort doesn't settle the run within abortGrace (wedged process),
// escalate to killing the process — the next Run respawns it fresh.
func (r *piRun) Stop() {
	r.stopped.Do(func() {
		// Escalation timer starts BEFORE the abort round-trip: if the
		// process is wedged, the 15s send timeout must not delay the kill.
		go func() {
			timer := time.NewTimer(abortGrace)
			defer timer.Stop()
			select {
			case <-r.settled:
				// 优雅中止成功
			case <-timer.C:
				log.Printf("[pi] abort %s 未生效，升级杀进程", abortGrace)
				r.ps.kill()
			}
		}()
		if _, err := r.ps.send(map[string]any{"type": "abort"}, true); err != nil {
			r.ps.kill()
		}
	})
}

// abortGrace is how long Stop waits for a graceful abort to close the run
// before killing the process.
const abortGrace = 5 * time.Second

func (r *piRun) WaitExit(timeoutMs int) bool { return true }

// ─── pi event translation ──────────────────────────────────────────

// translatePiEvent converts one RPC stdout object into AgentEvents.
// Dialog extension_ui_request events are built in readLoop (need session for Reply).
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
	case "message_end":
		message, _ := raw["message"].(map[string]any)
		if message == nil || stringField(message, "role") != "assistant" {
			return nil
		}
		if usage, ok := piUsageEvent(message["usage"]); ok {
			return []Event{usage}
		}
		return nil
	case "extension_ui_request":
		// Dialog methods handled in readLoop with a bound Reply; fire-and-forget
		// methods become a short system note so the run card still shows them.
		method := stringField(raw, "method")
		switch method {
		case "notify":
			msg := stringField(raw, "message")
			if msg == "" {
				return nil
			}
			kind := stringField(raw, "notifyType")
			if kind != "" && kind != "info" {
				msg = "[" + kind + "] " + msg
			}
			return []Event{{Type: EventText, Delta: msg + "\n"}}
		case "select", "confirm", "input", "editor":
			return nil // handled in readLoop
		default:
			// setStatus / setWidget / setTitle / set_editor_text: ignore
			return nil
		}
	case "agent_settled":
		return []Event{{Type: EventDone, TerminationReason: TermNormal}}
	}
	return nil
}

// translatePiUIRequest maps pi extension UI dialogs onto EventAskUser.
// Reply writes extension_ui_response back to the RPC process.
func (ps *piSession) translatePiUIRequest(raw map[string]any) []Event {
	method := stringField(raw, "method")
	id := stringField(raw, "id")
	if id == "" {
		return nil
	}
	var questions []AskQuestion
	freeform := false
	switch method {
	case "select":
		title := stringField(raw, "title")
		optsRaw, _ := raw["options"].([]any)
		var opts []AskOption
		for _, o := range optsRaw {
			label, _ := o.(string)
			if label == "" {
				continue
			}
			opts = append(opts, AskOption{Key: label, Label: label})
		}
		if len(opts) == 0 {
			return nil
		}
		questions = []AskQuestion{{Prompt: title, Options: opts}}
	case "confirm":
		title := stringField(raw, "title")
		msg := stringField(raw, "message")
		prompt := title
		if msg != "" {
			if prompt != "" {
				prompt += "\n" + msg
			} else {
				prompt = msg
			}
		}
		if prompt == "" {
			prompt = "请确认"
		}
		questions = []AskQuestion{{
			Prompt: prompt,
			Options: []AskOption{
				{Key: "yes", Label: "确认"},
				{Key: "no", Label: "取消"},
			},
		}}
	case "input", "editor":
		title := stringField(raw, "title")
		if title == "" {
			title = "请输入"
		}
		extra := ""
		if method == "input" {
			if ph := stringField(raw, "placeholder"); ph != "" {
				extra = "\n（占位：" + ph + "）"
			}
		} else if pre := stringField(raw, "prefill"); pre != "" {
			extra = "\n\n预填内容：\n```\n" + truncateRunes(pre, 500) + "\n```"
		}
		questions = []AskQuestion{{
			Prompt: title + extra + "\n\n请在聊天中直接回复文字作答，或点「取消」。",
			Options: []AskOption{
				{Key: "__cancel__", Label: "取消"},
			},
		}}
		freeform = true
	default:
		return nil
	}

	return []Event{{
		Type:      EventAskUser,
		AskID:     id,
		Questions: questions,
		Freeform:  freeform,
		Source:    "pi",
		Reply: func(answers [][]string, cancelled bool) error {
			return ps.replyExtensionUI(id, method, answers, cancelled)
		},
	}}
}

// replyExtensionUI writes the matching extension_ui_response to pi stdin.
func (ps *piSession) replyExtensionUI(id, method string, answers [][]string, cancelled bool) error {
	cmd := map[string]any{
		"type": "extension_ui_response",
		"id":   id,
	}
	if cancelled {
		cmd["cancelled"] = true
	} else {
		switch method {
		case "confirm":
			// yes → true; no / empty → false (pi treats false like cancel for confirm)
			confirmed := false
			if len(answers) > 0 && len(answers[0]) > 0 {
				k := answers[0][0]
				confirmed = k == "yes" || k == "确认" || k == "true"
			}
			cmd["confirmed"] = confirmed
		case "select", "input", "editor":
			val := ""
			if len(answers) > 0 && len(answers[0]) > 0 {
				val = answers[0][0]
			}
			if val == "__cancel__" {
				cmd["cancelled"] = true
			} else {
				cmd["value"] = val
			}
		default:
			cmd["cancelled"] = true
		}
	}
	// Fire-and-forget write (no response correlation for extension_ui_response).
	_, err := ps.send(cmd, false)
	return err
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
