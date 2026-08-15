package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// GrokAdapter drives the local `grok` CLI over ACP (`grok agent stdio`).
// Headless stream-json cannot answer ask_user_question; reverse RPC
// `_x.ai/ask_user_question` is required (ADR-0020).
//
// One ACP process is kept per chat scope (keyed with cwd). Session continuity
// uses session/load when a prior sessionId is known.
type GrokAdapter struct {
	binary      string
	botIdentity *BotIdentity
	runtime     config.AgentRuntime
	Env         map[string]string

	mu       sync.Mutex
	sessions map[string]*grokSession
}

// NewGrokAdapter builds an adapter around the given grok binary.
func NewGrokAdapter(binary string) *GrokAdapter {
	if binary == "" {
		binary = "grok"
	}
	return &GrokAdapter{binary: binary, sessions: map[string]*grokSession{}}
}

func (a *GrokAdapter) ID() string          { return "grok" }
func (a *GrokAdapter) DisplayName() string { return "Grok" }

func (a *GrokAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

// ResetSession kills the scope's ACP process; the next Run spawns a fresh one.
func (a *GrokAdapter) ResetSession(scope string) {
	a.mu.Lock()
	gs := a.sessions[scope]
	delete(a.sessions, scope)
	a.mu.Unlock()
	if gs != nil {
		gs.kill()
	}
}

func (a *GrokAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for GrokAdapter.Run")
	}
	gs, err := a.sessionFor(opts)
	if err != nil {
		return nil, err
	}
	return gs.startRun(opts, a.botIdentity)
}

func (a *GrokAdapter) sessionFor(opts RunOptions) (*grokSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := opts.Scope
	if key == "" {
		key = opts.RunID
	}
	if gs := a.sessions[key]; gs != nil {
		if gs.cwd == opts.Cwd && gs.alive() && gs.access == opts.Access {
			return gs, nil
		}
		delete(a.sessions, key)
		gs.kill()
	}

	gs, err := a.spawn(opts)
	if err != nil {
		return nil, err
	}
	a.sessions[key] = gs
	return gs, nil
}

func (a *GrokAdapter) spawn(opts RunOptions) (*grokSession, error) {
	gs := &grokSession{
		cwd:    opts.Cwd,
		access: opts.Access,
		model:  opts.Model,
	}
	args := grokAgentArgs(opts.Access, opts.Model)
	env := mergeEnvMaps(a.Env, opts.Env)

	client, err := startACPLike(a.binary, args, opts.Cwd, env, a.runtime,
		gs.handleUpdate, gs.handleReverse)
	if err != nil {
		return nil, err
	}
	gs.client = client
	return gs, nil
}

// grokAgentArgs builds `grok agent … stdio` argv (flags before the mode name).
func grokAgentArgs(access config.AccessLevel, model string) []string {
	args := []string{"agent"}
	if access == config.AccessFull {
		args = append(args, "--always-approve")
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, "stdio")
	return args
}

// ─── grokSession: one ACP process ──────────────────────────────────

type grokSession struct {
	client *acpClient
	cwd    string
	access config.AccessLevel
	model  string

	mu        sync.Mutex
	sessionID string
	runCh     chan Event
	// pendingAsk replies keyed by toolCallId (for debugging / future cancel).
}

func (gs *grokSession) alive() bool {
	return gs.client != nil && gs.client.alive()
}

func (gs *grokSession) kill() {
	if gs.client != nil {
		gs.client.kill()
	}
}

func (gs *grokSession) handleUpdate(events []Event) {
	gs.mu.Lock()
	ch := gs.runCh
	gs.mu.Unlock()
	if ch == nil {
		return
	}
	for _, evt := range events {
		// Capture session id from system-like tool metadata if present.
		if evt.Type == EventSystem && evt.SessionID != "" {
			gs.mu.Lock()
			gs.sessionID = evt.SessionID
			gs.mu.Unlock()
		}
		safeSend(ch, evt)
	}
}

func (gs *grokSession) handleReverse(method string, id json.RawMessage, params json.RawMessage) bool {
	if method != "_x.ai/ask_user_question" && method != "x.ai/ask_user_question" {
		return false
	}
	toolCallID, questions := parseGrokAskParams(params)
	if len(questions) == 0 {
		gs.client.respond(id, map[string]any{"outcome": "cancelled"})
		return true
	}
	askID := toolCallID
	if askID == "" {
		askID = fmt.Sprintf("grok-ask-%d", time.Now().UnixNano())
	}

	// Copy questions for the Reply closure.
	qs := append([]AskQuestion(nil), questions...)
	evt := Event{
		Type:      EventAskUser,
		AskID:     askID,
		Questions: qs,
		Freeform:  true,
		Source:    "grok",
		Reply: func(answers [][]string, cancelled bool) error {
			result := formatGrokAskAnswer(qs, answers, cancelled)
			gs.client.respond(id, result)
			return nil
		},
	}

	gs.mu.Lock()
	ch := gs.runCh
	gs.mu.Unlock()
	if ch == nil {
		gs.client.respond(id, map[string]any{"outcome": "cancelled"})
		return true
	}
	// Blocking send with timeout so a full channel cannot stall the ACP reader forever.
	select {
	case ch <- evt:
	case <-time.After(5 * time.Second):
		log.Printf("[grok] ask event dropped (slow consumer), cancelling ask %s", askID)
		gs.client.respond(id, map[string]any{"outcome": "cancelled"})
	}
	return true
}

// sessionMeta builds session/new|load _meta for access + system rules.
func (gs *grokSession) sessionMeta(bot *BotIdentity, model string) map[string]any {
	meta := map[string]any{}
	if gs.access == config.AccessFull {
		meta["yoloMode"] = true
	}
	if rules := BuildSystemPrompt(bot); rules != "" {
		meta["rules"] = rules
	}
	if model != "" {
		// Some agents honor model on session; CLI -m already set at spawn.
		meta["model"] = model
	}
	return meta
}

func (gs *grokSession) ensureSession(opts RunOptions, bot *BotIdentity) (string, error) {
	gs.mu.Lock()
	sid := gs.sessionID
	gs.mu.Unlock()

	wantID := opts.SessionID
	meta := gs.sessionMeta(bot, opts.Model)

	// Prefer loading the bridge-stored session, then the process-local one.
	if wantID != "" && wantID != sid {
		if loaded, err := gs.client.sessionLoad(wantID, opts.Cwd, meta); err == nil && loaded != "" {
			gs.mu.Lock()
			gs.sessionID = loaded
			gs.mu.Unlock()
			return loaded, nil
		}
		log.Printf("[grok] session/load %s failed; creating new session", wantID)
	}
	if sid != "" {
		// Already have a live session in this process.
		return sid, nil
	}
	newID, err := gs.client.sessionNew(opts.Cwd, meta)
	if err != nil {
		return "", fmt.Errorf("Grok session/new 失败: %w", err)
	}
	gs.mu.Lock()
	gs.sessionID = newID
	gs.mu.Unlock()
	return newID, nil
}

type grokRun struct {
	gs      *grokSession
	events  chan Event
	settled chan struct{}
	stopped sync.Once
}

func (gs *grokSession) startRun(opts RunOptions, bot *BotIdentity) (Run, error) {
	sessionID, err := gs.ensureSession(opts, bot)
	if err != nil {
		return nil, err
	}

	ch := make(chan Event, 256)
	gs.mu.Lock()
	if gs.runCh != nil {
		gs.mu.Unlock()
		return nil, fmt.Errorf("Grok scope 已有进行中的 run")
	}
	gs.runCh = ch
	gs.mu.Unlock()

	// Emit system early so the run card shows session id immediately.
	safeSend(ch, Event{Type: EventSystem, SessionID: sessionID, Model: opts.Model})

	run := &grokRun{gs: gs, events: ch, settled: make(chan struct{})}
	prompt := opts.Prompt
	go func() {
		defer run.finish()
		res, err := gs.client.sessionPrompt(sessionID, prompt)
		if err != nil {
			// Process death already emits EventError via onUpdate; avoid double-close noise.
			if gs.client.alive() {
				safeSend(ch, Event{Type: EventError, Message: err.Error(), TerminationReason: TermFailed})
			}
			return
		}
		for _, evt := range usageEventsFromPromptResult(res, sessionID) {
			if evt.Type == EventSystem && evt.SessionID != "" {
				gs.mu.Lock()
				gs.sessionID = evt.SessionID
				gs.mu.Unlock()
			}
			safeSend(ch, evt)
		}
	}()
	return run, nil
}

func (r *grokRun) Events() <-chan Event { return r.events }

func (r *grokRun) Stop() {
	r.stopped.Do(func() {
		r.gs.mu.Lock()
		sid := r.gs.sessionID
		r.gs.mu.Unlock()
		if sid != "" && r.gs.client != nil {
			_ = r.gs.client.sessionCancel(sid)
		}
	})
}

func (r *grokRun) WaitExit(timeoutMs int) bool {
	select {
	case <-r.settled:
		return true
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return false
	}
}

func (r *grokRun) finish() {
	r.gs.mu.Lock()
	if r.gs.runCh == r.events {
		r.gs.runCh = nil
	}
	r.gs.mu.Unlock()
	close(r.events)
	close(r.settled)
}

// ─── ask parse / format (ADR-0020) ─────────────────────────────────

// parseGrokAskParams extracts toolCallId and questions from ACP reverse params.
func parseGrokAskParams(params json.RawMessage) (toolCallID string, questions []AskQuestion) {
	var p struct {
		ToolCallID string `json:"toolCallId"`
		Questions  []struct {
			Question    string `json:"question"`
			MultiSelect *bool  `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", nil
	}
	toolCallID = p.ToolCallID
	for _, q := range p.Questions {
		prompt := q.Question
		if prompt == "" {
			continue
		}
		multi := false
		if q.MultiSelect != nil {
			multi = *q.MultiSelect
		}
		var opts []AskOption
		for _, o := range q.Options {
			label := o.Label
			if label == "" {
				continue
			}
			// Grok options have no separate key; label is the stable id.
			opts = append(opts, AskOption{Key: label, Label: label})
		}
		if len(opts) == 0 {
			// Freeform-only question: still show the card with freeform input.
			opts = append(opts, AskOption{Key: "ok", Label: "确定"})
		}
		questions = append(questions, AskQuestion{
			Prompt:      prompt,
			Options:     opts,
			MultiSelect: multi,
		})
	}
	return toolCallID, questions
}

// formatGrokAskAnswer builds the ACP result for `_x.ai/ask_user_question`.
// answers are display labels (bridge already maps keys → labels).
// Value type is StringOrVec: single-select → string, multi → []string.
func formatGrokAskAnswer(questions []AskQuestion, answers [][]string, cancelled bool) map[string]any {
	if cancelled {
		return map[string]any{"outcome": "cancelled"}
	}
	m := map[string]any{}
	for i, q := range questions {
		var vals []string
		if i < len(answers) {
			vals = answers[i]
		}
		if q.MultiSelect {
			if vals == nil {
				vals = []string{}
			}
			m[q.Prompt] = vals
			continue
		}
		if len(vals) == 0 {
			m[q.Prompt] = ""
			continue
		}
		if len(vals) == 1 {
			m[q.Prompt] = vals[0]
		} else {
			// Unexpected multi keys for single-select: join for the model.
			m[q.Prompt] = vals
		}
	}
	return map[string]any{"outcome": "accepted", "answers": m}
}

// translateGrokLine keeps the legacy stream-json projector for unit tests and
// any residual tooling. Runtime path is ACP (see GrokAdapter.Run).
func translateGrokLine(line []byte) []Event {
	var raw grokRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "thought":
		if raw.Data != "" {
			return []Event{{Type: EventThinking, Delta: raw.Data}}
		}
	case "text":
		if raw.Data != "" {
			return []Event{{Type: EventText, Delta: raw.Data}}
		}
	case "end":
		var out []Event
		modelName := ""
		for name := range raw.ModelUsage {
			modelName = name
			break
		}
		if raw.SessionID != "" {
			out = append(out, Event{Type: EventSystem, SessionID: raw.SessionID, Model: modelName})
		}
		if raw.Usage != nil {
			out = append(out, Event{
				Type:                  EventUsage,
				InputTokens:           raw.Usage.InputTokens,
				OutputTokens:          raw.Usage.OutputTokens,
				CachedInputTokens:     raw.Usage.CacheReadInputTokens,
				ReasoningOutputTokens: raw.Usage.ReasoningTokens,
				CostUSD:               raw.TotalCostUSD,
			})
		}
		reason := TermNormal
		if raw.StopReason == "Stop" || raw.StopReason == "EndTurn" || raw.StopReason == "end_turn" {
			reason = TermNormal
		}
		out = append(out, Event{Type: EventDone, SessionID: raw.SessionID, TerminationReason: reason})
		return out
	}
	return nil
}

type grokEndUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	ReasoningTokens      int `json:"reasoning_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type grokEndModelUsage struct {
	InputTokens          int     `json:"inputTokens"`
	OutputTokens         int     `json:"outputTokens"`
	CacheReadInputTokens int     `json:"cacheReadInputTokens"`
	ModelCalls           int     `json:"modelCalls"`
	CostUSD              float64 `json:"costUSD"`
}

type grokRawEvent struct {
	Type         string                       `json:"type"`
	Data         string                       `json:"data,omitempty"`
	Usage        *grokEndUsage                `json:"usage,omitempty"`
	ModelUsage   map[string]grokEndModelUsage `json:"modelUsage,omitempty"`
	SessionID    string                       `json:"sessionId,omitempty"`
	StopReason   string                       `json:"stopReason,omitempty"`
	TotalCostUSD float64                      `json:"total_cost_usd,omitempty"`
}
