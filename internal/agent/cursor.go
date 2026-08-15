package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// CursorAdapter drives the local Cursor CLI over ACP (`cursor-agent acp`
// or a verified Cursor `agent acp`). Print stream-json is translated by
// helpers for tests; runtime is ACP so permission and cursor/ask_question
// can finish a turn (ADR-0022).
//
// One ACP process is kept per chat scope. Session continuity uses
// session/load when a prior sessionId is known.
type CursorAdapter struct {
	binary      string
	botIdentity *BotIdentity
	runtime     config.AgentRuntime
	Env         map[string]string

	mu       sync.Mutex
	sessions map[string]*cursorSession
}

// NewCursorAdapter builds an adapter around the given Cursor CLI binary.
// Empty binary is resolved at spawn time (cursor-agent, then a verified
// Cursor agent — never Grok's ~/.local/bin/agent).
func NewCursorAdapter(binary string) *CursorAdapter {
	return &CursorAdapter{binary: binary, sessions: map[string]*cursorSession{}}
}

func (a *CursorAdapter) ID() string          { return "cursor" }
func (a *CursorAdapter) DisplayName() string { return "Cursor" }

func (a *CursorAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

// ResetSession kills the scope's ACP process; the next Run spawns a fresh one.
func (a *CursorAdapter) ResetSession(scope string) {
	a.mu.Lock()
	cs := a.sessions[scope]
	delete(a.sessions, scope)
	a.mu.Unlock()
	if cs != nil {
		cs.kill()
	}
}

func (a *CursorAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for CursorAdapter.Run")
	}
	cs, err := a.sessionFor(opts)
	if err != nil {
		return nil, err
	}
	return cs.startRun(opts, a.botIdentity)
}

func (a *CursorAdapter) resolvedBinary() (string, error) {
	if a.binary != "" {
		return a.binary, nil
	}
	if p := ResolveCursorBinary(); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("未找到 Cursor CLI（cursor-agent，或已校验的 Cursor agent；不会把 Grok 的 agent 当成 Cursor）")
}

func (a *CursorAdapter) sessionFor(opts RunOptions) (*cursorSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := opts.Scope
	if key == "" {
		key = opts.RunID
	}
	if cs := a.sessions[key]; cs != nil {
		if cs.cwd == opts.Cwd && cs.alive() && cs.access == opts.Access {
			return cs, nil
		}
		delete(a.sessions, key)
		cs.kill()
	}

	cs, err := a.spawn(opts)
	if err != nil {
		return nil, err
	}
	a.sessions[key] = cs
	return cs, nil
}

func (a *CursorAdapter) spawn(opts RunOptions) (*cursorSession, error) {
	binary, err := a.resolvedBinary()
	if err != nil {
		return nil, err
	}
	cs := &cursorSession{
		cwd:    opts.Cwd,
		access: opts.Access,
		model:  opts.Model,
	}
	args := cursorACPArgs()
	env := mergeEnvMaps(a.Env, opts.Env)

	client, err := startACPLike(binary, args, opts.Cwd, env, a.runtime,
		cs.handleUpdate, cs.handleReverse)
	if err != nil {
		return nil, err
	}
	if method := client.preferredAuthMethod("cursor_login"); method != "" {
		if aerr := client.authenticate(method); aerr != nil {
			log.Printf("[cursor] authenticate %s: %v（继续尝试 session）", method, aerr)
		}
	}
	cs.client = client
	return cs, nil
}

// cursorACPArgs builds `<binary> acp` argv. Flags such as --api-key stay
// on the process env (CURSOR_API_KEY) rather than the command line.
func cursorACPArgs() []string {
	return []string{"acp"}
}

// cursorPrintArgs builds the official print-mode argv
// (`-p --output-format stream-json --force`). Runtime is ACP; this helper
// is the documented unattended print path and is covered by table tests.
func cursorPrintArgs(opts RunOptions) []string {
	args := []string{"-p", "--output-format", "stream-json", "--force"}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, opts.ExtraArgs...)
	if opts.Prompt != "" {
		args = append(args, opts.Prompt)
	}
	return args
}

// ─── binary resolution (ADR-0022) ──────────────────────────────────

// ResolveCursorBinary returns a Cursor CLI path or "".
// Order: cursor-agent on PATH / conventional dirs, then `agent` only if
// LookLikeCursorCLI accepts it. Grok's ~/.local/bin/agent is rejected.
func ResolveCursorBinary() string {
	for _, name := range []string{"cursor-agent", "agent"} {
		for _, path := range cursorBinaryCandidates(name) {
			if LookLikeCursorCLI(path) {
				return path
			}
		}
	}
	return ""
}

func cursorBinaryCandidates(name string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if path, err := exec.LookPath(name); err == nil {
		add(path)
	}
	if p := resolveAgentBinary(name); p != name {
		add(p)
	}
	return out
}

// LookLikeCursorCLI reports whether path is a real Cursor CLI, not Grok's
// `agent`. Empty / missing paths are false.
func LookLikeCursorCLI(path string) bool {
	if path == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	slash := filepath.ToSlash(resolved)
	if strings.Contains(slash, "/.grok/") {
		return false
	}
	base := strings.ToLower(filepath.Base(resolved))
	if strings.HasPrefix(base, "cursor-agent") {
		return true
	}
	if strings.HasPrefix(base, "grok") {
		return false
	}
	out := probeCLIIdentity(path)
	low := strings.ToLower(out)
	if strings.Contains(low, "grok") && !strings.Contains(low, "cursor") {
		return false
	}
	return strings.Contains(low, "cursor")
}

func probeCLIIdentity(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var buf bytes.Buffer
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		cmd := exec.CommandContext(ctx, path, args...)
		b, _ := cmd.CombinedOutput()
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.String()
}

// ─── cursorSession: one ACP process ────────────────────────────────

type cursorSession struct {
	client *acpClient
	cwd    string
	access config.AccessLevel
	model  string

	mu        sync.Mutex
	sessionID string
	runCh     chan Event
}

func (cs *cursorSession) alive() bool {
	return cs.client != nil && cs.client.alive()
}

func (cs *cursorSession) kill() {
	if cs.client != nil {
		cs.client.kill()
	}
}

func (cs *cursorSession) handleUpdate(events []Event) {
	cs.mu.Lock()
	ch := cs.runCh
	cs.mu.Unlock()
	if ch == nil {
		return
	}
	for _, evt := range events {
		if evt.Type == EventSystem && evt.SessionID != "" {
			cs.mu.Lock()
			cs.sessionID = evt.SessionID
			cs.mu.Unlock()
		}
		safeSend(ch, evt)
	}
}

func (cs *cursorSession) handleReverse(method string, id json.RawMessage, params json.RawMessage) bool {
	switch method {
	case "cursor/ask_question":
		return cs.handleAskQuestion(id, params)
	case "cursor/create_plan":
		// Non-goal: do not hang the default-agent turn (ADR-0022).
		cs.client.respond(id, cursorCancelledOutcome())
		return true
	}
	if strings.HasPrefix(method, "cursor/") {
		cs.client.respond(id, cursorCancelledOutcome())
		return true
	}
	return false
}

func (cs *cursorSession) handleAskQuestion(id json.RawMessage, params json.RawMessage) bool {
	toolCallID, parsed := parseCursorAskParams(params)
	if len(parsed) == 0 {
		cs.client.respond(id, cursorCancelledOutcome())
		return true
	}
	askID := toolCallID
	if askID == "" {
		askID = fmt.Sprintf("cursor-ask-%d", time.Now().UnixNano())
	}
	qs := make([]AskQuestion, len(parsed))
	for i, q := range parsed {
		qs[i] = q.AskQuestion
	}
	evt := Event{
		Type:      EventAskUser,
		AskID:     askID,
		Questions: qs,
		Freeform:  true,
		Source:    "cursor",
		Reply: func(answers [][]string, cancelled bool) error {
			cs.client.respond(id, formatCursorAskAnswer(parsed, answers, cancelled))
			return nil
		},
	}

	cs.mu.Lock()
	ch := cs.runCh
	cs.mu.Unlock()
	if ch == nil {
		cs.client.respond(id, cursorCancelledOutcome())
		return true
	}
	select {
	case ch <- evt:
	case <-time.After(5 * time.Second):
		log.Printf("[cursor] ask event dropped (slow consumer), cancelling ask %s", askID)
		cs.client.respond(id, cursorCancelledOutcome())
	}
	return true
}

func (cs *cursorSession) sessionMeta(bot *BotIdentity, model string) map[string]any {
	meta := map[string]any{}
	if rules := BuildSystemPrompt(bot); rules != "" {
		meta["rules"] = rules
	}
	if model != "" {
		meta["model"] = model
	}
	return meta
}

func (cs *cursorSession) ensureSession(opts RunOptions, bot *BotIdentity) (string, error) {
	cs.mu.Lock()
	sid := cs.sessionID
	cs.mu.Unlock()

	wantID := opts.SessionID
	meta := cs.sessionMeta(bot, opts.Model)

	if wantID != "" && wantID != sid {
		if loaded, err := cs.client.sessionLoad(wantID, opts.Cwd, meta); err == nil && loaded != "" {
			cs.mu.Lock()
			cs.sessionID = loaded
			cs.mu.Unlock()
			return loaded, nil
		}
		log.Printf("[cursor] session/load %s failed; creating new session", wantID)
	}
	if sid != "" {
		return sid, nil
	}
	newID, err := cs.client.sessionNew(opts.Cwd, meta)
	if err != nil {
		return "", fmt.Errorf("Cursor session/new 失败: %w", err)
	}
	cs.mu.Lock()
	cs.sessionID = newID
	cs.mu.Unlock()
	return newID, nil
}

type cursorRun struct {
	cs      *cursorSession
	events  chan Event
	settled chan struct{}
	stopped sync.Once
}

func (cs *cursorSession) startRun(opts RunOptions, bot *BotIdentity) (Run, error) {
	sessionID, err := cs.ensureSession(opts, bot)
	if err != nil {
		return nil, err
	}

	ch := make(chan Event, 256)
	cs.mu.Lock()
	if cs.runCh != nil {
		cs.mu.Unlock()
		return nil, fmt.Errorf("Cursor scope 已有进行中的 run")
	}
	cs.runCh = ch
	cs.mu.Unlock()

	safeSend(ch, Event{Type: EventSystem, SessionID: sessionID, Model: opts.Model})

	run := &cursorRun{cs: cs, events: ch, settled: make(chan struct{})}
	prompt := opts.Prompt
	go func() {
		defer run.finish()
		res, err := cs.client.sessionPrompt(sessionID, prompt)
		if err != nil {
			if cs.client.alive() {
				safeSend(ch, Event{Type: EventError, Message: err.Error(), TerminationReason: TermFailed})
			}
			return
		}
		for _, evt := range usageEventsFromPromptResult(res, sessionID) {
			if evt.Type == EventSystem && evt.SessionID != "" {
				cs.mu.Lock()
				cs.sessionID = evt.SessionID
				cs.mu.Unlock()
			}
			safeSend(ch, evt)
		}
	}()
	return run, nil
}

func (r *cursorRun) Events() <-chan Event { return r.events }

func (r *cursorRun) Stop() {
	r.stopped.Do(func() {
		r.cs.mu.Lock()
		sid := r.cs.sessionID
		r.cs.mu.Unlock()
		if sid != "" && r.cs.client != nil {
			_ = r.cs.client.sessionCancel(sid)
		}
	})
}

func (r *cursorRun) WaitExit(timeoutMs int) bool {
	select {
	case <-r.settled:
		return true
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return false
	}
}

func (r *cursorRun) finish() {
	r.cs.mu.Lock()
	if r.cs.runCh == r.events {
		r.cs.runCh = nil
	}
	r.cs.mu.Unlock()
	close(r.events)
	close(r.settled)
}

// ─── ask parse / format (ADR-0022) ─────────────────────────────────

// cursorAskQ is one cursor/ask_question item plus its stable id.
type cursorAskQ struct {
	ID string
	AskQuestion
}

// parseCursorAskParams extracts toolCallId and questions from ACP reverse params.
func parseCursorAskParams(params json.RawMessage) (toolCallID string, questions []cursorAskQ) {
	var p struct {
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
		Questions  []struct {
			ID            string `json:"id"`
			Prompt        string `json:"prompt"`
			AllowMultiple *bool  `json:"allowMultiple"`
			Options       []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", nil
	}
	toolCallID = p.ToolCallID
	for i, q := range p.Questions {
		prompt := q.Prompt
		if prompt == "" {
			prompt = p.Title
		}
		if prompt == "" {
			continue
		}
		qid := q.ID
		if qid == "" {
			qid = fmt.Sprintf("q%d", i+1)
		}
		multi := false
		if q.AllowMultiple != nil {
			multi = *q.AllowMultiple
		}
		var opts []AskOption
		for _, o := range q.Options {
			key := o.ID
			label := o.Label
			if key == "" {
				key = label
			}
			if label == "" {
				label = key
			}
			if key == "" {
				continue
			}
			opts = append(opts, AskOption{Key: key, Label: label})
		}
		if len(opts) == 0 {
			opts = append(opts, AskOption{Key: "ok", Label: "确定"})
		}
		questions = append(questions, cursorAskQ{
			ID: qid,
			AskQuestion: AskQuestion{
				Prompt:      prompt,
				Options:     opts,
				MultiSelect: multi,
			},
		})
	}
	return toolCallID, questions
}

// cursorExtResult is the official Cursor ACP reverse-RPC result envelope.
// The wire shape is nested: {"outcome":{"outcome":"answered"|"cancelled"|..., ...}}.
// A flat {"outcome":"answered"} is rejected by the real Cursor CLI.
// See https://cursor.com/docs/cli/acp (CursorAskQuestionResponse).
type cursorExtResult struct {
	Outcome cursorExtOutcome `json:"outcome"`
}

type cursorExtOutcome struct {
	Outcome string            `json:"outcome"`
	Answers []cursorAskAnswer `json:"answers,omitempty"`
	Reason  string            `json:"reason,omitempty"`
}

type cursorAskAnswer struct {
	QuestionID        string   `json:"questionId"`
	SelectedOptionIDs []string `json:"selectedOptionIds"`
}

func cursorCancelledOutcome() cursorExtResult {
	return cursorExtResult{Outcome: cursorExtOutcome{Outcome: "cancelled"}}
}

// formatCursorAskAnswer builds the official nested ACP result for cursor/ask_question.
// answers are keys or display labels (bridge maps keys → labels).
func formatCursorAskAnswer(questions []cursorAskQ, answers [][]string, cancelled bool) cursorExtResult {
	if cancelled {
		return cursorCancelledOutcome()
	}
	var out []cursorAskAnswer
	for i, q := range questions {
		var vals []string
		if i < len(answers) {
			vals = answers[i]
		}
		var selected []string
		for _, v := range vals {
			if id := matchCursorOptionID(q.AskQuestion, v); id != "" {
				selected = append(selected, id)
			}
		}
		out = append(out, cursorAskAnswer{
			QuestionID:        q.ID,
			SelectedOptionIDs: selected,
		})
	}
	return cursorExtResult{Outcome: cursorExtOutcome{Outcome: "answered", Answers: out}}
}

func matchCursorOptionID(q AskQuestion, answer string) string {
	for _, o := range q.Options {
		if o.Key == answer || o.Label == answer {
			return o.Key
		}
	}
	return answer
}

// ─── print stream-json translator (not the runtime path) ───────────

type cursorRawEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	Model     string          `json:"model"`
	CallID    string          `json:"call_id"`
	Text      string          `json:"text"`
	IsError   bool            `json:"is_error"`
	Result    json.RawMessage `json:"result"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	ToolCall map[string]json.RawMessage `json:"tool_call"`
	Usage    *struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// translateCursorLine converts one official print stream-json line into events.
func translateCursorLine(line []byte) []Event {
	var raw cursorRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			return []Event{{Type: EventSystem, SessionID: raw.SessionID, Cwd: raw.Cwd, Model: raw.Model}}
		}
	case "assistant":
		var out []Event
		if raw.Text != "" {
			out = append(out, Event{Type: EventText, Delta: raw.Text})
		}
		for _, b := range raw.Message.Content {
			if b.Type == "text" && b.Text != "" {
				out = append(out, Event{Type: EventText, Delta: b.Text})
			}
		}
		return out
	case "thinking":
		if raw.Text != "" {
			return []Event{{Type: EventThinking, Delta: raw.Text}}
		}
	case "tool_call":
		name, input, output, failed := decodeCursorToolCall(raw.ToolCall)
		switch raw.Subtype {
		case "started":
			return []Event{{Type: EventToolUse, ID: raw.CallID, Name: name, Input: input}}
		case "completed":
			return []Event{{Type: EventToolResult, ID: raw.CallID, Output: output, IsError: failed}}
		}
	case "result":
		var out []Event
		if raw.Usage != nil {
			out = append(out, Event{
				Type:              EventUsage,
				InputTokens:       raw.Usage.InputTokens,
				OutputTokens:      raw.Usage.OutputTokens,
				CachedInputTokens: raw.Usage.CacheReadInputTokens,
			})
		}
		if raw.IsError || strings.Contains(raw.Subtype, "error") {
			msg := string(raw.Result)
			if msg == "" {
				msg = raw.Subtype
			}
			out = append(out, Event{Type: EventError, SessionID: raw.SessionID, Message: msg, TerminationReason: TermFailed})
			return out
		}
		out = append(out, Event{Type: EventDone, SessionID: raw.SessionID, TerminationReason: TermNormal})
		return out
	}
	return nil
}

func decodeCursorToolCall(toolCall map[string]json.RawMessage) (name string, input any, output string, failed bool) {
	if len(toolCall) == 0 {
		return "tool", nil, "", false
	}
	for key, raw := range toolCall {
		name = strings.TrimSuffix(key, "ToolCall")
		if name == "" {
			name = key
		}
		var body struct {
			Args   json.RawMessage `json:"args"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			output = string(raw)
			return name, nil, output, false
		}
		if len(body.Args) > 0 {
			_ = json.Unmarshal(body.Args, &input)
		}
		if len(body.Result) > 0 {
			output = string(body.Result)
			var wrap struct {
				Error   json.RawMessage `json:"error"`
				Success json.RawMessage `json:"success"`
			}
			if json.Unmarshal(body.Result, &wrap) == nil && len(wrap.Error) > 0 && string(wrap.Error) != "null" {
				failed = true
			}
		}
		return name, input, output, failed
	}
	return "tool", nil, "", false
}
