// Package agent drives local coding-agent CLIs (Claude Code, Codex CLI)
// and translates their streaming output into a unified event stream.
package agent

import (
	"context"

	"lark-coding-agent-bridge-go/internal/config"
)

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
	// ContextWindow is the model's context window in tokens as reported by
	// the agent runtime (0 = unknown; fall back to the pricing table).
	ContextWindow int

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
	// ("opencode" | "opencode-permission" | "pi" | "omp" | …).
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
	RunID             string
	Scope             string // chat scope; persistent adapters key processes by it
	Prompt            string
	Cwd               string
	SessionID         string // claude --resume / pi --session-id / omp --resume
	ThreadID          string // codex resume
	Model             string
	CollaborationMode CollaborationMode
	Images            []string
	Access            config.AccessLevel
	StopGraceMs       int
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

// UsageProvider is implemented by adapters whose native protocol exposes
// account-level usage. The bridge uses it for the /usage command.
type UsageProvider interface {
	ReadUsage(ctx context.Context) (UsageSnapshot, error)
}

// ModelProvider is implemented by adapters that can discover the models
// currently available to the authenticated local CLI account.
type ModelProvider interface {
	ListModels(ctx context.Context, cwd string) ([]ModelInfo, error)
}

// ModelInfo is the provider-neutral model picker entry shown by the bridge.
type ModelInfo struct {
	ID          string
	DisplayName string
	Description string
	Default     bool
}

// CollaborationMode is an agent-native conversation workflow. It is
// deliberately separate from AccessLevel: Plan changes how Codex collaborates,
// while access controls what tools the process may use.
type CollaborationMode string

const (
	CollaborationModeDefault CollaborationMode = "default"
	CollaborationModePlan    CollaborationMode = "plan"
)

// CollaborationModeProvider is implemented by adapters that expose native
// collaboration modes to the bridge's /mode command.
type CollaborationModeProvider interface {
	CollaborationModes() []CollaborationModeInfo
}

// CollaborationModeInfo is one user-selectable native collaboration mode.
type CollaborationModeInfo struct {
	ID          CollaborationMode
	DisplayName string
	Description string
}

// UsageSnapshot is a provider-neutral account usage view.
type UsageSnapshot struct {
	Provider     string
	Plan         string
	Limits       []UsageLimit
	ResetCredits *int64
	TokenSummary UsageTokenSummary
	Activity     *UsageActivity
}

// UsageActivity describes local, provider-reported activity. It complements
// account quota windows because multi-provider agents such as OpenCode do not
// have one shared subscription limit.
type UsageActivity struct {
	Sessions              int64
	Messages              int64
	InputTokens           int64
	OutputTokens          int64
	CachedInputTokens     int64
	CacheWriteTokens      int64
	ReasoningOutputTokens int64
	CostUSD               float64
}

type UsageLimit struct {
	ID        string
	Name      string
	Primary   *UsageWindow
	Secondary *UsageWindow
	Credits   *UsageCredits
}

type UsageWindow struct {
	UsedPercent       int
	WindowDurationMin int64
	ResetsAt          int64
}

type UsageCredits struct {
	Balance   string
	HasCredit bool
	Unlimited bool
}

type UsageTokenSummary struct {
	LifetimeTokens        *int64
	PeakDailyTokens       *int64
	CurrentStreakDays     *int64
	LongestStreakDays     *int64
	LongestRunningTurnSec *int64
}

// SessionResetter is implemented by adapters with persistent agent
// processes (pi RPC, opencode serve): the bridge calls ResetSession when
// the user runs /new or switches directory, so the next Run starts fresh.
type SessionResetter interface {
	ResetSession(scope string)
}

// AccessConfigurer is implemented by persistent adapters whose server must be
// started with the profile access policy before the first Run.
type AccessConfigurer interface {
	SetDefaultAccess(access config.AccessLevel)
}

// NewAdapter builds the adapter for an agent kind. runtime is optional to
// preserve the direct-construction/test API; the bridge passes the profile's
// per-machine launch configuration.
func NewAdapter(kind config.AgentKind, runtimes ...config.AgentRuntime) Adapter {
	var runtime config.AgentRuntime
	if len(runtimes) > 0 {
		runtime = runtimes[0]
	}
	switch kind {
	case config.AgentCodex:
		return &CodexAdapter{binary: "codex", runtime: runtime}
	case config.AgentPi:
		a := NewPiAdapter("pi")
		a.runtime = runtime
		return a
	case config.AgentOmp:
		// Oh My Pi：复用 Pi RPC 事件协议，差异见 piKindConfig / ADR-0021。
		a := NewOmpAdapter("omp")
		a.runtime = runtime
		return a
	case config.AgentOpenCode:
		a := NewOpenCodeAdapter("opencode")
		a.runtime = runtime
		return a
	case config.AgentGrok:
		a := NewGrokAdapter("grok")
		a.runtime = runtime
		return a
	case config.AgentKimi:
		return &KimiAdapter{binary: "kimi", runtime: runtime}
	case config.AgentCursor:
		a := NewCursorAdapter("")
		a.runtime = runtime
		return a
	default:
		return &ClaudeAdapter{binary: "claude", runtime: runtime}
	}
}
