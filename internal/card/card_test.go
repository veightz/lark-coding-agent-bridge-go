package card

import (
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/agent"
)

func TestReduceFlow(t *testing.T) {
	s := InitialState()
	if s.Terminal != TerminalRunning || s.Footer != FooterThinking {
		t.Fatalf("initial state = %+v", s)
	}

	s = s.Reduce(agent.Event{Type: agent.EventThinking, Delta: "想一下"})
	if !s.Reasoning.Active || s.Reasoning.Content != "想一下" {
		t.Errorf("reasoning = %+v", s.Reasoning)
	}

	// streaming text merges into one block
	s = s.Reduce(agent.Event{Type: agent.EventText, Delta: "你"})
	s = s.Reduce(agent.Event{Type: agent.EventText, Delta: "好"})
	if len(s.Blocks) != 1 || s.Blocks[0].Content != "你好" || !s.Blocks[0].Streaming {
		t.Fatalf("blocks = %+v", s.Blocks)
	}

	// tool_use closes the streaming text block
	s = s.Reduce(agent.Event{Type: agent.EventToolUse, ID: "t1", Name: "Bash", Input: map[string]any{"command": "ls"}})
	if len(s.Blocks) != 2 || s.Blocks[0].Streaming {
		t.Fatalf("after tool_use blocks = %+v", s.Blocks)
	}
	if s.Blocks[1].Tool.Status != ToolRunning || s.Footer != FooterToolRunning {
		t.Errorf("tool = %+v footer=%v", s.Blocks[1].Tool, s.Footer)
	}

	// new text after a tool starts a fresh block
	s = s.Reduce(agent.Event{Type: agent.EventToolResult, ID: "t1", Output: "ok"})
	s = s.Reduce(agent.Event{Type: agent.EventText, Delta: "结果"})
	if len(s.Blocks) != 3 || s.Blocks[2].Content != "结果" {
		t.Fatalf("blocks = %+v", s.Blocks)
	}
	if s.Blocks[1].Tool.Status != ToolDone || s.Blocks[1].Tool.Output != "ok" {
		t.Errorf("tool after result = %+v", s.Blocks[1].Tool)
	}

	s = s.Reduce(agent.Event{Type: agent.EventDone, TerminationReason: agent.TermNormal})
	if s.Terminal != TerminalDone || s.Footer != FooterNone || s.Blocks[2].Streaming {
		t.Errorf("final state = terminal=%v streaming=%v", s.Terminal, s.Blocks[2].Streaming)
	}
}

func TestReduceErrorTerminal(t *testing.T) {
	s := InitialState()
	s = s.Reduce(agent.Event{Type: agent.EventError, Message: "boom", TerminationReason: agent.TermFailed})
	if s.Terminal != TerminalError || s.ErrorMsg != "boom" {
		t.Errorf("state = %+v", s)
	}

	s2 := InitialState().Reduce(agent.Event{Type: agent.EventDone, TerminationReason: agent.TermInterrupted})
	if s2.Terminal != TerminalInterrupted {
		t.Errorf("interrupted terminal = %v", s2.Terminal)
	}
}

func TestRenderCard(t *testing.T) {
	s := InitialState()
	s = s.Reduce(agent.Event{Type: agent.EventText, Delta: "hello contact aabbcc@example.com"})
	s = s.Reduce(agent.Event{Type: agent.EventToolUse, ID: "t1", Name: "Bash", Input: map[string]any{"command": "ls"}})

	cardJSON := Render(s, RenderOptions{StopButton: true})
	if cardJSON["schema"] != "2.0" {
		t.Errorf("schema = %v", cardJSON["schema"])
	}
	cfg := cardJSON["config"].(map[string]any)
	if cfg["streaming_mode"] != true {
		t.Error("streaming_mode should be true while running")
	}
	body := cardJSON["body"].(map[string]any)
	elements := body["elements"].([]map[string]any)
	if len(elements) == 0 {
		t.Fatal("no elements")
	}

	// stop/refresh buttons must be present while running as auto-width columns
	var sawButtons bool
	for _, el := range elements {
		if el["tag"] != "column_set" {
			continue
		}
		if el["flex_mode"] != "flow" {
			t.Errorf("button column_set flex_mode = %v, want flow", el["flex_mode"])
		}
		columns, _ := el["columns"].([]map[string]any)
		if len(columns) != 2 {
			t.Errorf("button columns = %d, want 2", len(columns))
		}
		for _, col := range columns {
			if col["width"] != "auto" {
				t.Errorf("button column width = %v, want auto", col["width"])
			}
			if colEls, _ := col["elements"].([]map[string]any); len(colEls) > 0 && colEls[0]["tag"] == "button" {
				sawButtons = true
			}
		}
	}
	if !sawButtons {
		t.Error("no stop/refresh buttons while running")
	}

	// email masked in markdown content
	first := elements[0]
	if content, _ := first["content"].(string); strings.Contains(content, "aabbcc@example.com") {
		t.Errorf("email not masked: %q", content)
	} else if !strings.Contains(content, "aa***@example.com") {
		t.Errorf("unexpected masked content: %q", content)
	}

	// done state: streaming off, no stop button
	s = s.Reduce(agent.Event{Type: agent.EventDone, TerminationReason: agent.TermNormal})
	doneCard := Render(s, RenderOptions{StopButton: true})
	if doneCard["config"].(map[string]any)["streaming_mode"] != false {
		t.Error("streaming_mode should be false when done")
	}
	for _, el := range doneCard["body"].(map[string]any)["elements"].([]map[string]any) {
		if el["tag"] == "button" {
			t.Error("stop button should not render when done")
		}
		if el["tag"] == "column_set" {
			// Only the action button row uses column_set with buttons.
			if columns, ok := el["columns"].([]map[string]any); ok {
				for _, col := range columns {
					if colEls, _ := col["elements"].([]map[string]any); len(colEls) > 0 && colEls[0]["tag"] == "button" {
						t.Error("action buttons should not render when done")
					}
				}
			}
		}
	}
}

func TestToolGrouping(t *testing.T) {
	s := InitialState()
	for i, name := range []string{"Bash", "Read", "Grep", "Glob"} {
		s = s.Reduce(agent.Event{Type: agent.EventToolUse, ID: string(rune('a' + i)), Name: name, Input: map[string]any{"command": "x"}})
	}
	// 4 tools while running: collapsed summary + latest panel
	cardJSON := Render(s, RenderOptions{})
	elements := cardJSON["body"].(map[string]any)["elements"].([]map[string]any)
	var panels int
	for _, el := range elements {
		if el["tag"] == "collapsible_panel" {
			panels++
		}
	}
	if panels != 2 {
		t.Errorf("running with 4 tools: panels = %d, want 2", panels)
	}

	// finalized: single collapsed summary
	s = s.Reduce(agent.Event{Type: agent.EventDone, TerminationReason: agent.TermNormal})
	elements = Render(s, RenderOptions{})["body"].(map[string]any)["elements"].([]map[string]any)
	panels = 0
	for _, el := range elements {
		if el["tag"] == "collapsible_panel" {
			panels++
		}
	}
	if panels != 1 {
		t.Errorf("finalized with 4 tools: panels = %d, want 1", panels)
	}
}

func TestTotalTokensAndFormat(t *testing.T) {
	// Claude-style: cache nested under input
	claude := &RunStats{InputTokens: 1000, OutputTokens: 50, CachedInputTokens: 200}
	if got := claude.TotalTokens(); got != 1050 {
		t.Errorf("claude total = %d, want 1050", got)
	}
	if got := formatTokenUsage(claude); got != "🔤 1050 token (in 1000 · cache 200 · out 50)" {
		t.Errorf("claude format = %q", got)
	}

	// OpenCode-style: cache additive, input is non-cached only
	oc := &RunStats{InputTokens: 165, OutputTokens: 50, CachedInputTokens: 19200, ReasoningOutputTokens: 20}
	if got := oc.TotalTokens(); got != 165+19200+50+20 {
		t.Errorf("opencode total = %d, want %d", got, 165+19200+50+20)
	}
	if got := formatTokenUsage(oc); got != "🔤 19435 token (in 165 · cache 19200 · out 50 · reason 20)" {
		t.Errorf("opencode format = %q", got)
	}

	plain := &RunStats{InputTokens: 100, OutputTokens: 20}
	if got := formatTokenUsage(plain); got != "🔤 120 token" {
		t.Errorf("plain format = %q", got)
	}
}
