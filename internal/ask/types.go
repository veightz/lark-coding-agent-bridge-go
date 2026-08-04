// Package ask implements the Feishu interactive-card takeover for agent
// questions (Claude AskUserQuestion / OpenCode question), inspired by
// deepcoldy/botmux's ask-broker design.
package ask

import "time"

// Option is one selectable choice on an ask card.
// Key is the stable id returned to the agent; Label is shown on the button.
type Option struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Question is one structured prompt with options.
type Question struct {
	Prompt      string   `json:"prompt"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect"`
}

// Route tells the broker where to send the Feishu card.
type Route struct {
	ChatID   string // required
	ReplyTo  string // optional message_id to reply under (om_…)
	InThread bool   // reply_in_thread: topic form (p2p)
	Scope    string // chat scope / run id for audit
}

// CreateInput is everything needed to register a pending ask.
type CreateInput struct {
	Route      Route
	Questions  []Question
	Timeout    time.Duration // default 30m when ≤0
	Source     string        // "claude-hook" | "opencode" | "pi" | "api"
	RawPayload any           // optional; Claude hook keeps original questions
	// Freeform allows answering by typing in the chat (pi input/editor).
	Freeform bool
}

// Result is the terminal outcome of an ask.
type Result struct {
	Kind ResultKind
	// Answers[i] = selected keys for Questions[i]. Empty when Kind != Answered
	// or when the user replied with free-form Comment only.
	Answers [][]string
	By      string // operator open_id
	Comment string // free-form text reply (optional)
	Reason  string // invalidated reason
}

// ResultKind classifies a settled ask.
type ResultKind string

const (
	KindAnswered    ResultKind = "answered"
	KindTimedOut    ResultKind = "timed_out"
	KindInvalidated ResultKind = "invalidated"
)

// ClickOutcome is returned to the card callback handler.
type ClickOutcome string

const (
	OutcomeAccepted       ClickOutcome = "accepted"
	OutcomeToggled        ClickOutcome = "toggled"
	OutcomeUnauthorized   ClickOutcome = "unauthorized"
	OutcomeStale          ClickOutcome = "stale"
	OutcomeAlreadySettled ClickOutcome = "already_settled"
	OutcomeNeedSelection  ClickOutcome = "need_selection"
	OutcomeNeedInput      ClickOutcome = "need_input"
)

// Pending is a snapshot of an in-flight (or recently settled) ask.
type Pending struct {
	ID            string
	Nonce         string
	Route         Route
	Questions     []Question
	Selections    [][]string // current toggled keys per question
	Source        string
	RawPayload    any
	Freeform      bool
	CreatedAt     time.Time
	DeadlineAt    time.Time
	CardMessageID string
	CardEntityID  string // CardKit card_id when using entity updates
	Settled       bool
	Result        *Result
}

// Action names embedded in button callback values.
const (
	ActionSelect = "ask_select" // single-question single-select: click settles
	ActionToggle = "ask_toggle" // accumulate selection
	ActionSubmit = "ask_submit" // settle with current selections
	// ActionSubmitInput settles a freeform ask with the text typed into the
	// card's input element (pi input/editor).
	ActionSubmitInput = "ask_submit_input"
)
