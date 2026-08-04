// Package card tracks one agent run's presentation state and renders it
// as a Feishu CardKit 2.0 streaming card. Ported from the original
// run-state.ts / run-renderer.ts / tool-render.ts.
package card

import (
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/pricing"
)

// ToolStatus is a tool call's lifecycle state.
type ToolStatus string

const (
	ToolRunning ToolStatus = "running"
	ToolDone    ToolStatus = "done"
	ToolError   ToolStatus = "error"
)

// ToolEntry is one tool call shown on the card.
type ToolEntry struct {
	ID         string
	Name       string
	Input      any
	Status     ToolStatus
	Output     string
	DurationMs int64 // elapsed wall-clock ms while running (set by bridge)
}

// BlockKind distinguishes text blocks from tool blocks.
type BlockKind string

const (
	BlockText BlockKind = "text"
	BlockTool BlockKind = "tool"
)

// Block is one ordered chunk of the run transcript.
type Block struct {
	Kind      BlockKind
	Content   string // text
	Streaming bool   // text still being appended
	Tool      *ToolEntry
}

// FooterStatus is the live status line under the card.
type FooterStatus string

const (
	FooterNone        FooterStatus = ""
	FooterThinking    FooterStatus = "thinking"
	FooterToolRunning FooterStatus = "tool_running"
	FooterStreaming   FooterStatus = "streaming"
)

// Terminal is the run's end state.
type Terminal string

const (
	TerminalRunning     Terminal = "running"
	TerminalDone        Terminal = "done"
	TerminalContinued   Terminal = "continued"
	TerminalInterrupted Terminal = "interrupted"
	TerminalError       Terminal = "error"
	TerminalIdleTimeout Terminal = "idle_timeout"
)

// RunStats holds metrics collected during a run.
type RunStats struct {
	DurationMs            int64   // wall clock duration in ms
	InputTokens           int     // prompt tokens (see TotalTokens for agent differences)
	OutputTokens          int     // completion tokens
	CachedInputTokens     int     // cached prompt tokens
	ReasoningOutputTokens int     // reasoning / thinking tokens (opencode, codex, …)
	CostUSD               float64 // agent-reported cost in USD
	CostCNY               float64 // calculated cost in CNY from pricing table
	Model                 string  // model name from EventSystem
	Cwd                   string  // working directory
	UsageAvailable        bool    // true if any usage data was reported
	// ContextWindow is the model's context window in tokens reported by the
	// agent runtime; 0 means unknown (renderer falls back to pricing table).
	ContextWindow int
	// Agent conversation handles (for footer: tell runs/sessions apart).
	SessionID string // claude / pi / opencode session id
	ThreadID  string // codex thread id
}

// SessionRef returns a short agent-session label for display, preferring
// SessionID then ThreadID. Empty when neither is known yet.
func (s *RunStats) SessionRef() string {
	if s == nil {
		return ""
	}
	id := s.SessionID
	if id == "" {
		id = s.ThreadID
	}
	if id == "" {
		return ""
	}
	return shortID(id, 12)
}

func shortID(id string, max int) string {
	if max <= 0 || len(id) <= max {
		return id
	}
	return id[:max]
}

// TotalTokens returns the complete token count for display.
// Claude-style usage nests cache under input (cached ≤ input); OpenCode-style
// reports non-cached input + cache.read separately (cached may exceed input).
func (s *RunStats) TotalTokens() int {
	total := s.InputTokens + s.OutputTokens + s.ReasoningOutputTokens
	if s.CachedInputTokens > s.InputTokens {
		total += s.CachedInputTokens
	}
	return total
}

// ContextTokens returns the total number of context tokens (input + cache).
// For Claude-style (cache nested in input) this equals InputTokens.
// For OpenCode-style (additive cache) this is InputTokens + CachedInputTokens.
func (s *RunStats) ContextTokens() int {
	if s.CachedInputTokens > s.InputTokens {
		return s.InputTokens + s.CachedInputTokens
	}
	return s.InputTokens
}

// RunState is the full presentation state of one agent run.
type RunState struct {
	Blocks    []Block
	FinalText string
	Reasoning struct {
		Content string
		Active  bool
	}
	Footer   FooterStatus
	Terminal Terminal
	ErrorMsg string
	Stats    RunStats
}

// InitialState returns the state for a fresh run.
func InitialState() *RunState {
	return &RunState{Footer: FooterThinking, Terminal: TerminalRunning, Stats: RunStats{}}
}

func closeStreamingText(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	copy(out, blocks)
	for i := range out {
		if out[i].Kind == BlockText && out[i].Streaming {
			out[i].Streaming = false
		}
	}
	return out
}

// Reduce folds one agent event into the state (immutable update).
func (s *RunState) Reduce(evt agent.Event) *RunState {
	next := *s // shallow copy; slices replaced before mutation
	switch evt.Type {
	case agent.EventText:
		next.Blocks = append([]Block(nil), s.Blocks...)
		if n := len(next.Blocks); n > 0 && next.Blocks[n-1].Kind == BlockText && next.Blocks[n-1].Streaming {
			next.Blocks[n-1].Content += evt.Delta
		} else {
			next.Blocks = append(next.Blocks, Block{Kind: BlockText, Content: evt.Delta, Streaming: true})
		}
		next.Reasoning.Active = false
		next.Footer = FooterStreaming

	case agent.EventFinalText:
		next.FinalText = evt.Content

	case agent.EventThinking:
		next.Reasoning.Content += evt.Delta
		next.Reasoning.Active = true
		next.Footer = FooterThinking

	case agent.EventToolUse:
		next.Blocks = closeStreamingText(s.Blocks)
		next.Blocks = append(next.Blocks, Block{
			Kind: BlockTool,
			Tool: &ToolEntry{ID: evt.ID, Name: evt.Name, Input: evt.Input, Status: ToolRunning},
		})
		next.Reasoning.Active = false
		next.Footer = FooterToolRunning

	case agent.EventToolResult:
		next.Blocks = append([]Block(nil), s.Blocks...)
		for i := range next.Blocks {
			b := &next.Blocks[i]
			if b.Kind == BlockTool && b.Tool.ID == evt.ID {
				tool := *b.Tool
				tool.Output = evt.Output
				if evt.IsError {
					tool.Status = ToolError
				} else {
					tool.Status = ToolDone
				}
				b.Tool = &tool
			}
		}

	case agent.EventSystem:
		if evt.Model != "" {
			next.Stats.Model = evt.Model
		}
		if evt.SessionID != "" {
			next.Stats.SessionID = evt.SessionID
		}
		if evt.ThreadID != "" {
			next.Stats.ThreadID = evt.ThreadID
		}
		if evt.ContextWindow > 0 {
			next.Stats.ContextWindow = evt.ContextWindow
		}

	case agent.EventUsage:
		next.Stats.InputTokens = evt.InputTokens
		next.Stats.OutputTokens = evt.OutputTokens
		next.Stats.CachedInputTokens = evt.CachedInputTokens
		next.Stats.ReasoningOutputTokens = evt.ReasoningOutputTokens
		next.Stats.CostUSD = evt.CostUSD
		next.Stats.UsageAvailable = true
		// Calculate CNY cost from pricing table when model is known
		if next.Stats.Model != "" {
			cny, _ := pricing.Calculate(next.Stats.Model, evt.InputTokens, evt.OutputTokens, evt.CachedInputTokens, time.Now())
			if cny > 0 {
				next.Stats.CostCNY = cny
			}
		}

	case agent.EventError:
		switch evt.TerminationReason {
		case agent.TermInterrupted:
			next.Terminal = TerminalInterrupted
		case agent.TermTimeout:
			next.Terminal = TerminalIdleTimeout
		default:
			next.Terminal = TerminalError
			next.ErrorMsg = evt.Message
		}
		next.Footer = FooterNone

	case agent.EventDone:
		next.Blocks = closeStreamingText(s.Blocks)
		next.Reasoning.Active = false
		if evt.SessionID != "" {
			next.Stats.SessionID = evt.SessionID
		}
		if evt.ThreadID != "" {
			next.Stats.ThreadID = evt.ThreadID
		}
		switch evt.TerminationReason {
		case agent.TermInterrupted:
			next.Terminal = TerminalInterrupted
		case agent.TermTimeout:
			next.Terminal = TerminalIdleTimeout
		default:
			next.Terminal = TerminalDone
		}
		next.Footer = FooterNone
	}
	return &next
}

// MarkInterrupted forces the interrupted terminal state.
func (s *RunState) MarkInterrupted() *RunState {
	next := *s
	next.Blocks = closeStreamingText(s.Blocks)
	next.Reasoning.Active = false
	next.Terminal = TerminalInterrupted
	next.Footer = FooterNone
	return &next
}

// MarkIdleTimeout forces the idle-timeout terminal state.
func (s *RunState) MarkIdleTimeout() *RunState {
	next := *s
	next.Blocks = closeStreamingText(s.Blocks)
	next.Reasoning.Active = false
	next.Terminal = TerminalIdleTimeout
	next.Footer = FooterNone
	return &next
}

// FinalizeIfRunning closes a still-running state as done.
func (s *RunState) FinalizeIfRunning() *RunState {
	if s.Terminal != TerminalRunning {
		return s
	}
	next := *s
	next.Blocks = closeStreamingText(s.Blocks)
	next.Reasoning.Active = false
	next.Terminal = TerminalDone
	next.Footer = FooterNone
	return &next
}

// MarkContinued finalizes a presentation segment without ending the agent
// run. The bridge uses it when an ask card is answered, before continuing
// live output in a newly-created message below that interaction.
func (s *RunState) MarkContinued() *RunState {
	next := *s
	next.Blocks = closeStreamingText(s.Blocks)
	next.Reasoning.Active = false
	next.Terminal = TerminalContinued
	next.Footer = FooterNone
	return &next
}

// LastRunningTool returns the most recent tool block still running.
func (s *RunState) LastRunningTool() *ToolEntry {
	for i := len(s.Blocks) - 1; i >= 0; i-- {
		if s.Blocks[i].Kind == BlockTool && s.Blocks[i].Tool != nil && s.Blocks[i].Tool.Status == ToolRunning {
			return s.Blocks[i].Tool
		}
	}
	return nil
}

// TextContent concatenates the run's visible text (used for fallbacks).
func (s *RunState) TextContent() string {
	var sb strings.Builder
	for _, b := range s.Blocks {
		if b.Kind == BlockText {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Content)
		}
	}
	if s.FinalText != "" {
		return s.FinalText
	}
	return sb.String()
}
