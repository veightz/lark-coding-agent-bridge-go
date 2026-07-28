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
	"sort"
	"strings"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
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

	run := srv.registerRun(sessionID, opts.Cwd, opts.Access)

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
	// 立刻把 sessionID 回传给 bridge，才能写入 sessions.json / bindings.json
	//（opencode SSE 事件本身不携带“新建会话”系统帧，与 pi 一样由适配器补发）。
	safeSend(run.events, Event{Type: EventSystem, SessionID: sessionID})
	// SSE 是快路径，轮询是兜底：丢帧/半死流也能把结果轮询回来。
	go srv.pollRun(run)
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
	// SSE 看门狗状态（按目录）
	lastFrame  map[string]time.Time
	loopCancel map[string]context.CancelFunc
	// 已投递文本（sessionID → messageID → text），轮询回补的基准
	delivered map[string]map[string]string
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
		lastFrame:  map[string]time.Time{},
		loopCancel: map[string]context.CancelFunc{},
		delivered:  map[string]map[string]string{},
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
func (s *ocServer) registerRun(sessionID, directory string, access config.AccessLevel) *ocRun {
	s.mu.Lock()
	s.runSeq++
	id := s.runSeq
	run := &ocRun{
		srv: s, sessionID: sessionID, directory: directory, id: id,
		access: access, events: make(chan Event, 256),
	}
	if s.runs[sessionID] == nil {
		s.runs[sessionID] = map[uint64]*ocRun{}
	}
	s.runs[sessionID][id] = run
	if !s.eventLoops[directory] {
		s.eventLoops[directory] = true
		go s.eventLoop(directory)
		go s.frameWatchdog(directory)
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
	// full access (bridge default): auto-reply permission.asked with
	// "always" so OpenCode never blocks on tool gates. workspace /
	// read-only keep the Feishu permission card (ADR-0011).
	events = s.autoAllowPermissions(sessionID, events)
	if len(events) == 0 && !terminal {
		return
	}
	// Record delivered main text so the poller can backfill anything the
	// SSE stream lost mid-turn.
	if env.Type == "message.part.delta" {
		if messageID, _ := env.Properties["messageID"].(string); messageID != "" {
			if delta, _ := env.Properties["delta"].(string); delta != "" {
				for _, evt := range events {
					if evt.Type == EventText {
						s.mu.Lock()
						if s.delivered[sessionID] == nil {
							s.delivered[sessionID] = map[string]string{}
						}
						s.delivered[sessionID][messageID] += delta
						s.mu.Unlock()
					}
				}
			}
		}
	}
	s.mu.Lock()
	runs := s.runs[sessionID]
	// Bind AskUser reply closures to each run's working directory so
	// POST /question|permission/{id}/reply hits the correct OpenCode worktree.
	for i := range events {
		if events[i].Type != EventAskUser || events[i].AskID == "" {
			continue
		}
		qid := events[i].AskID
		isPerm := events[i].Source == "opencode-permission" || strings.HasPrefix(qid, "per")
		events[i].Reply = func(answers [][]string, cancelled bool) error {
			// Prefer the directory of any live run for this session.
			s.mu.Lock()
			dir := ""
			for _, run := range s.runs[sessionID] {
				if run != nil && run.directory != "" {
					dir = run.directory
					break
				}
			}
			s.mu.Unlock()
			if isPerm {
				reply := ocPermissionReply(answers, cancelled)
				path := "/permission/" + qid + "/reply"
				if dir != "" {
					path += "?directory=" + urlQueryEscape(dir)
				}
				return s.postJSON(path, map[string]any{"reply": reply}, nil)
			}
			// On cancel/timeout still reply with empty labels so OpenCode unblocks.
			if cancelled && len(answers) == 0 {
				answers = [][]string{{""}}
			}
			path := "/question/" + qid + "/reply"
			if dir != "" {
				path += "?directory=" + urlQueryEscape(dir)
			}
			return s.postJSON(path, map[string]any{"answers": answers}, nil)
		}
	}
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

	case "question.asked":
		// OpenCode native question tool — bridge shows a Feishu card and
		// replies via POST /question/{id}/reply (ADR-0008). Reply is bound
		// in dispatch() with the run's directory.
		qid, questions := parseOCQuestions(props)
		if qid == "" || len(questions) == 0 {
			return nil, false
		}
		return []Event{{
			Type:      EventAskUser,
			AskID:     qid,
			Questions: questions,
			Source:    "opencode",
		}}, false

	case "permission.asked", "permission.v2.asked":
		// Tool-level permission gate (bash/edit/…). Feishu card with
		// once/always/reject → POST /permission/{id}/reply (ADR-0011).
		pid, questions := parseOCPermission(props)
		if pid == "" || len(questions) == 0 {
			return nil, false
		}
		return []Event{{
			Type:      EventAskUser,
			AskID:     pid,
			Questions: questions,
			Source:    "opencode-permission",
		}}, false

	case "session.idle":
		// Send EventDone to mark turn completion, but defer channel close
		// to pollRun so it can extract token/cost from the REST API first.
		return []Event{{Type: EventDone, TerminationReason: TermNormal}}, false

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
// reconnect, plus a frame watchdog: opencode emits server.heartbeat frames
// regularly, so a stream that stays "open" but silent for too long is
// half-dead — cancel it and reconnect instead of trusting it.
func (s *ocServer) eventLoop(directory string) {
	backoff := time.Second
	for {
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.loopCancel[directory] = cancel
		s.mu.Unlock()

		err := s.consumeEvents(ctx, directory)
		cancel()
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

// sseStaleAfter bounds the silence tolerated on a directory stream.
// Heartbeats arrive far more often than this.
const sseStaleAfter = 75 * time.Second

// frameWatchdog cancels the directory's SSE connection when no frame
// (including heartbeats) has arrived for sseStaleAfter.
func (s *ocServer) frameWatchdog(directory string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		stopping := s.stopping
		last := s.lastFrame[directory]
		cancel := s.loopCancel[directory]
		s.mu.Unlock()
		if stopping {
			return
		}
		if !last.IsZero() && time.Since(last) > sseStaleAfter && cancel != nil {
			log.Printf("[opencode] SSE 超过 %s 无帧（疑似半死），强制重连 (dir=%s)", sseStaleAfter, directory)
			cancel()
		}
	}
}

func (s *ocServer) markFrame(directory string) {
	s.mu.Lock()
	s.lastFrame[directory] = time.Now()
	s.mu.Unlock()
}

func (s *ocServer) consumeEvents(ctx context.Context, directory string) error {
	url := s.base + "/event"
	if directory != "" {
		url += "?directory=" + urlQueryEscape(directory)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// No client timeout: the stream is meant to stay open; staleness is
	// handled by frameWatchdog cancelling ctx.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /event: HTTP %d", resp.StatusCode)
	}
	s.markFrame(directory)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var dataLines []string
	for scanner.Scan() {
		s.markFrame(directory)
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
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("SSE 流被对端关闭")
}

// ─── 轮询回补：SSE 丢帧时的第二通道 ──────────────────────────────

// pollInterval is how often an active run reconciles against the
// session's message list. SSE is the fast path; this is the safety net.
const pollInterval = 3 * time.Second

// ocTokens mirrors the token usage in message info.
type ocTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// ocMessageEntry mirrors the relevant slice of GET /session/{id}/message.
type ocMessageEntry struct {
	Info struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Time struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		} `json:"time"`
		Tokens *ocTokens `json:"tokens"`
		Cost   *float64  `json:"cost"`
		Error  *struct {
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	} `json:"info"`
	Parts []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (m *ocMessageEntry) text() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// pollRun reconciles the run against the server's message list until the
// turn provably ends (assistant message completed / errored) or the run
// is closed by the SSE path. Any text the stream lost is re-emitted as a
// single catch-up delta before the terminal event.
func (s *ocServer) pollRun(run *ocRun) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !s.runRegistered(run) {
			return
		}
		messages, err := s.fetchMessages(run.sessionID, run.directory)
		if err != nil || len(messages) == 0 {
			continue
		}
		last := lastAssistant(messages)
		if last == nil {
			continue
		}
		s.mu.Lock()
		delivered := s.delivered[run.sessionID][last.Info.ID]
		s.mu.Unlock()
		events, terminal := reconcilePoll(delivered, last)
		if !terminal {
			continue
		}
		backfilled := 0
		for _, evt := range events {
			if evt.Type == EventText {
				backfilled += len(evt.Delta)
			}
		}
		log.Printf("[opencode] 轮询确认 turn 结束 (session=%s, 回补 %d 字符)", run.sessionID, backfilled)
		s.mu.Lock()
		runs := s.runs[run.sessionID]
		if runs != nil {
			if _, ok := runs[run.id]; ok {
				for _, evt := range events {
					safeSend(run.events, evt)
				}
				close(run.events)
				delete(runs, run.id)
				if len(runs) == 0 {
					delete(s.runs, run.sessionID)
				}
			}
		}
		s.mu.Unlock()
		return
	}
}

func (s *ocServer) runRegistered(run *ocRun) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.runs[run.sessionID][run.id]
	return ok
}

func (s *ocServer) fetchMessages(sessionID, directory string) ([]ocMessageEntry, error) {
	url := s.base + "/session/" + sessionID + "/message?directory=" + urlQueryEscape(directory)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET message: HTTP %d", resp.StatusCode)
	}
	var out []ocMessageEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func lastAssistant(messages []ocMessageEntry) *ocMessageEntry {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Info.Role == "assistant" {
			return &messages[i]
		}
	}
	return nil
}

// reconcilePoll decides what a poll cycle should emit. Pure function.
func reconcilePoll(delivered string, last *ocMessageEntry) (events []Event, terminal bool) {
	if last.Info.Time.Completed == 0 && last.Info.Error == nil {
		return nil, false // turn still running
	}
	full := last.text()
	if missing := missingText(delivered, full); missing != "" {
		events = append(events, Event{Type: EventText, Delta: missing})
	}
	// Extract token/cost from the message info.
	if last.Info.Tokens != nil || (last.Info.Cost != nil && *last.Info.Cost > 0) {
		usage := Event{Type: EventUsage}
		if t := last.Info.Tokens; t != nil {
			// OpenCode reports input as non-cached only; cache.read /
			// reasoning sit alongside it (not nested under input).
			usage.InputTokens = t.Input
			usage.OutputTokens = t.Output
			usage.CachedInputTokens = t.Cache.Read
			usage.ReasoningOutputTokens = t.Reasoning
		}
		if c := last.Info.Cost; c != nil {
			usage.CostUSD = *c
		}
		events = append(events, usage)
	}
	if last.Info.Error != nil {
		name := last.Info.Error.Name
		if name == "MessageAbortedError" {
			return append(events, Event{Type: EventDone, TerminationReason: TermInterrupted}), true
		}
		msg := stringField(last.Info.Error.Data, "message")
		if msg == "" {
			msg = name
		}
		return append(events, Event{Type: EventError, Message: "opencode: " + msg, TerminationReason: TermFailed}), true
	}
	return append(events, Event{Type: EventDone, TerminationReason: TermNormal}), true
}

// missingText returns the suffix of full the stream never delivered.
func missingText(delivered, full string) string {
	if delivered == "" {
		return full
	}
	if strings.HasPrefix(full, delivered) {
		return full[len(delivered):]
	}
	// Stream and server disagree on content shape; trust the server and
	// re-emit nothing (the finalize path already renders what we have).
	return ""
}

// ListSessions enumerates opencode sessions known to the local server
// (they persist on disk, so this covers CLI-originated sessions too).
func (a *OpenCodeAdapter) ListSessions(limit int) ([]ExternalSession, error) {
	srv, err := a.ensureServer()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Time      struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	resp, err := srv.client.Get(srv.base + "/session")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Time.Updated > raw[j].Time.Updated })
	var out []ExternalSession
	for _, r := range raw {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, ExternalSession{
			ID:        r.ID,
			Cwd:       r.Directory,
			Preview:   r.Title,
			UpdatedAt: time.UnixMilli(r.Time.Updated),
			Agent:     config.AgentOpenCode,
		})
	}
	return out, nil
}

// parseOCQuestions normalizes OpenCode question.asked properties into
// bridge AskQuestion values.
func parseOCQuestions(props map[string]any) (id string, questions []AskQuestion) {
	id, _ = props["id"].(string)
	if id == "" {
		id, _ = props["questionID"].(string)
	}
	rawList, _ := props["questions"].([]any)
	for _, item := range rawList {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		prompt, _ := m["question"].(string)
		if prompt == "" {
			prompt, _ = m["header"].(string)
		}
		multi, _ := m["multiple"].(bool)
		if !multi {
			multi, _ = m["multiSelect"].(bool)
		}
		var opts []AskOption
		if arr, ok := m["options"].([]any); ok {
			for _, o := range arr {
				om, ok := o.(map[string]any)
				if !ok {
					continue
				}
				label, _ := om["label"].(string)
				if label == "" {
					continue
				}
				opts = append(opts, AskOption{Key: label, Label: label})
			}
		}
		if prompt == "" || len(opts) == 0 {
			continue
		}
		questions = append(questions, AskQuestion{Prompt: prompt, Options: opts, MultiSelect: multi})
	}
	return id, questions
}

// ocPermissionOptions are fixed reply keys for POST /permission/{id}/reply.
var ocPermissionOptions = []AskOption{
	{Key: "once", Label: "允许一次"},
	{Key: "always", Label: "始终允许"},
	{Key: "reject", Label: "拒绝"},
}

// ocAutoAllowPermission is true when bridge access should YOLO tool gates
// (matches Claude bypassPermissions / codex danger-full-access). Empty
// access is treated as full — that is the profile default.
func ocAutoAllowPermission(access config.AccessLevel) bool {
	switch access {
	case config.AccessWorkspace, config.AccessReadOnly:
		return false
	default:
		return true
	}
}

// autoAllowPermissions silently replies "always" for permission asks when
// the session's live run is on full access. Remaining events are returned.
func (s *ocServer) autoAllowPermissions(sessionID string, events []Event) []Event {
	if len(events) == 0 {
		return events
	}
	s.mu.Lock()
	dir := ""
	auto := false
	for _, run := range s.runs[sessionID] {
		if run == nil {
			continue
		}
		if run.directory != "" {
			dir = run.directory
		}
		if ocAutoAllowPermission(run.access) {
			auto = true
		}
	}
	s.mu.Unlock()
	if !auto {
		return events
	}

	kept := make([]Event, 0, len(events))
	for _, evt := range events {
		if evt.Type != EventAskUser || evt.Source != "opencode-permission" || evt.AskID == "" {
			kept = append(kept, evt)
			continue
		}
		path := "/permission/" + evt.AskID + "/reply"
		if dir != "" {
			path += "?directory=" + urlQueryEscape(dir)
		}
		if err := s.postJSON(path, map[string]any{"reply": "always"}, nil); err != nil {
			log.Printf("[opencode] auto-allow permission %s 失败: %v（降级弹卡）", evt.AskID, err)
			kept = append(kept, evt) // fall back to Feishu card
			continue
		}
		log.Printf("[opencode] auto-allow permission %s (access=full → always)", evt.AskID)
	}
	return kept
}

// parseOCPermission normalizes permission.asked / permission.v2.asked into
// a single-select EventAskUser (keys: once | always | reject).
func parseOCPermission(props map[string]any) (id string, questions []AskQuestion) {
	id, _ = props["id"].(string)
	if id == "" {
		id, _ = props["requestID"].(string)
	}
	if id == "" {
		return "", nil
	}

	// v1: permission + patterns; v2: action + resources
	kind := stringField(props, "permission")
	if kind == "" {
		kind = stringField(props, "action")
	}
	targets := anyStringSlice(props["patterns"])
	if len(targets) == 0 {
		targets = anyStringSlice(props["resources"])
	}
	always := anyStringSlice(props["always"])
	if len(always) == 0 {
		always = anyStringSlice(props["save"])
	}

	var b strings.Builder
	b.WriteString("🔐 工具权限请求")
	if kind != "" {
		b.WriteString("\n**类型**: `")
		b.WriteString(kind)
		b.WriteString("`")
	}
	if len(targets) > 0 {
		b.WriteString("\n**目标**:")
		for _, t := range targets {
			b.WriteString("\n- `")
			b.WriteString(truncateRunes(t, 200))
			b.WriteString("`")
		}
	}
	if meta, ok := props["metadata"].(map[string]any); ok && len(meta) > 0 {
		for _, k := range []string{"command", "filepath", "path", "description", "title", "cwd"} {
			if v := stringField(meta, k); v != "" {
				b.WriteString("\n**")
				b.WriteString(k)
				b.WriteString("**: `")
				b.WriteString(truncateRunes(v, 300))
				b.WriteString("`")
			}
		}
	}
	if len(always) > 0 {
		b.WriteString("\n\n选择「始终允许」将记住：")
		for _, t := range always {
			b.WriteString("\n- `")
			b.WriteString(truncateRunes(t, 120))
			b.WriteString("`")
		}
	}

	questions = []AskQuestion{{
		Prompt:  b.String(),
		Options: append([]AskOption(nil), ocPermissionOptions...),
	}}
	return id, questions
}

// ocPermissionReply maps card answers (keys or Chinese labels) to the
// OpenCode permission reply enum. Timeout/cancel → reject.
func ocPermissionReply(answers [][]string, cancelled bool) string {
	if cancelled {
		return "reject"
	}
	if len(answers) == 0 || len(answers[0]) == 0 {
		return "reject"
	}
	v := strings.TrimSpace(answers[0][0])
	switch strings.ToLower(v) {
	case "once", "allow_once", "allow once", "允许一次":
		return "once"
	case "always", "allow_always", "allow always", "始终允许":
		return "always"
	case "reject", "deny", "block", "拒绝":
		return "reject"
	default:
		// Unknown freeform: still unblock the agent safely.
		return "reject"
	}
}

func anyStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ─── ocRun ─────────────────────────────────────────────────────────

type ocRun struct {
	srv       *ocServer
	sessionID string
	directory string
	access    config.AccessLevel
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
