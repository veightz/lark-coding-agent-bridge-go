// Package agent drives local coding-agent CLIs (Claude Code, Codex CLI)
// and translates their streaming output into a unified event stream.
package agent

import "lark-coding-agent-bridge-go/internal/config"

// EventType classifies an AgentEvent.
type EventType string

const (
	EventSystem     EventType = "system"
	EventText       EventType = "text"
	EventFinalText  EventType = "final_text"
	EventThinking   EventType = "thinking"
	EventToolUse    EventType = "tool_use"
	EventToolResult EventType = "tool_result"
	EventAskUser    EventType = "ask_user" // agent needs a human choice (ADR-0008)
	EventUsage      EventType = "usage"
	EventDone       EventType = "done"
	EventError      EventType = "error"
)

// AskOption is one choice on an EventAskUser card.
type AskOption struct {
	Key   string
	Label string
}

// AskQuestion is one structured question from the agent.
type AskQuestion struct {
	Prompt      string
	Options     []AskOption
	MultiSelect bool
}

// AskReplyFunc delivers the user's answers back to the agent runtime
// (e.g. OpenCode POST /question/{id}/reply, pi extension_ui_response).
// cancelled is true on timeout / invalidate / user cancel.
// Called once by the bridge.
type AskReplyFunc func(answers [][]string, cancelled bool) error

// TerminationReason explains why a run ended.
type TerminationReason string

const (
	TermNormal      TerminationReason = "normal"
	TermInterrupted TerminationReason = "interrupted"
	TermTimeout     TerminationReason = "timeout"
	TermFailed      TerminationReason = "failed"
)

// Event is one normalized item from an agent's output stream.
type Event struct {
	Type EventType

	// system
	SessionID string
	ThreadID  string
	Cwd       string
	Model     string

	// text / thinking deltas
	Delta string

	// final_text
	Content string

	// tool_use / tool_result
	ID      string
	Name    string
	Input   any
	Output  string
	IsError bool

	// ask_user (ADR-0008)
	AskID     string
	Questions []AskQuestion
	// Freeform allows the user to answer by typing in chat (pi input/editor).
	Freeform bool
	// Source tags the origin for card chrome
	// ("opencode" | "opencode-permission" | "pi" | …).
	Source string
	// Reply posts the chosen answers back into the agent (in-process only).
	// Nil for hook-originated asks handled outside the event stream.
	// For OpenCode permission cards, answers[0][0] is once|always|reject.
	Reply AskReplyFunc

	// usage
	InputTokens           int
	OutputTokens          int
	CachedInputTokens     int
	ReasoningOutputTokens int
	CostUSD               float64

	// done / error
	Message           string
	TerminationReason TerminationReason
}

// RunOptions parameterizes one agent invocation.
type RunOptions struct {
	RunID       string
	Scope       string // chat scope; persistent adapters key processes by it
	Prompt      string
	Cwd         string
	SessionID   string // claude --resume / pi --session-id
	ThreadID    string // codex resume
	Model       string
	Images      []string
	Access      config.AccessLevel
	StopGraceMs int
	// Env is merged on top of the adapter's base Env for this run
	// (ask routing: LARK_BRIDGE_* for Claude hooks).
	Env map[string]string
	// ExtraArgs are appended to the CLI argv (e.g. claude --settings).
	ExtraArgs []string
}

// Run is a live agent invocation.
type Run interface {
	// Events streams normalized events; closed after a terminal event.
	Events() <-chan Event
	// Stop asks the process to exit (SIGTERM, then SIGKILL after grace).
	Stop()
	// WaitExit waits up to timeoutMs for a natural exit; false on timeout.
	WaitExit(timeoutMs int) bool
}

// BotIdentity is the bridge bot's own IM identity, injected into prompts.
type BotIdentity struct {
	OpenID string
	Name   string
}

// Adapter starts agent processes for a specific CLI kind.
type Adapter interface {
	ID() string
	DisplayName() string
	SetBotIdentity(id BotIdentity)
	Run(opts RunOptions) (Run, error)
}

// SessionResetter is implemented by adapters with persistent agent
// processes (pi RPC, opencode serve): the bridge calls ResetSession when
// the user runs /new or switches directory, so the next Run starts fresh.
type SessionResetter interface {
	ResetSession(scope string)
}

// NewAdapter builds the adapter for an agent kind.
func NewAdapter(kind config.AgentKind) Adapter {
	switch kind {
	case config.AgentCodex:
		return &CodexAdapter{binary: "codex"}
	case config.AgentPi:
		return NewPiAdapter("pi")
	case config.AgentOpenCode:
		return NewOpenCodeAdapter("opencode")
	case config.AgentGrok:
		return &GrokAdapter{binary: "grok"}
	default:
		return &ClaudeAdapter{binary: "claude"}
	}
}
