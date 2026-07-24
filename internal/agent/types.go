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
	EventUsage      EventType = "usage"
	EventDone       EventType = "done"
	EventError      EventType = "error"
)

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
	default:
		return &ClaudeAdapter{binary: "claude"}
	}
}
