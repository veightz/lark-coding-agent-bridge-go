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

	// stop/refresh buttons must be present while running
	var sawButton bool
	for _, el := range elements {
		if el["tag"] == "button" {
			sawButton = true
		}
		if el["tag"] == "column_set" {
			columns, _ := el["columns"].([]map[string]any)
			for _, col := range columns {
				if colEls, _ := col["elements"].([]map[string]any); colEls != nil {
					for _, ce := range colEls {
						if ce["tag"] == "button" {
							sawButton = true
						}
					}
				}
			}
		}
	}
	if !sawButton {
		t.Error("no stop/refresh button while running")
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
			t.Error("action buttons should not render when done")
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
