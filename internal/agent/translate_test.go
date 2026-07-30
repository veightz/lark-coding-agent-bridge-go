package agent

import (
	"strings"
	"testing"
)

func TestTranslateClaudeLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []Event
	}{
		{
			name: "system init",
			line: `{"type":"system","subtype":"init","session_id":"s1","cwd":"/tmp","model":"claude-x"}`,
			want: []Event{{Type: EventSystem, SessionID: "s1", Cwd: "/tmp", Model: "claude-x"}},
		},
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"你好"}]}}`,
			want: []Event{{Type: EventText, Delta: "你好"}},
		},
		{
			name: "assistant thinking + tool_use",
			line: `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
			want: []Event{
				{Type: EventThinking, Delta: "hmm"},
				{Type: EventToolUse, ID: "t1", Name: "Bash", Input: map[string]any{"command": "ls"}},
			},
		},
		{
			name: "user tool_result string",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","is_error":false}]}}`,
			want: []Event{{Type: EventToolResult, ID: "t1", Output: "ok"}},
		},
		{
			name: "user tool_result structured",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":{"stdout":"x"},"is_error":true}]}}`,
			want: []Event{{Type: EventToolResult, ID: "t1", Output: `{"stdout":"x"}`, IsError: true}},
		},
		{
			name: "result",
			line: `{"type":"result","session_id":"s1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3},"total_cost_usd":0.01}`,
			want: []Event{
				{Type: EventUsage, InputTokens: 10, OutputTokens: 5, CachedInputTokens: 3, CostUSD: 0.01},
				{Type: EventDone, SessionID: "s1", TerminationReason: TermNormal},
			},
		},
		{
			name: "garbage",
			line: `not json`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateClaudeLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				assertEventEqual(t, i, got[i], want)
			}
		})
	}
}

func assertEventEqual(t *testing.T, idx int, got, want Event) {
	t.Helper()
	if got.Type != want.Type {
		t.Errorf("event %d: type = %q, want %q", idx, got.Type, want.Type)
	}
	if got.SessionID != want.SessionID || got.ThreadID != want.ThreadID {
		t.Errorf("event %d: ids = (%q,%q), want (%q,%q)", idx, got.SessionID, got.ThreadID, want.SessionID, want.ThreadID)
	}
	if got.Model != want.Model {
		t.Errorf("event %d: model = %q, want %q", idx, got.Model, want.Model)
	}
	if got.Delta != want.Delta || got.Content != want.Content {
		t.Errorf("event %d: delta/content = (%q,%q), want (%q,%q)", idx, got.Delta, got.Content, want.Delta, want.Content)
	}
	if got.ID != want.ID || got.Name != want.Name || got.Output != want.Output || got.IsError != want.IsError {
		t.Errorf("event %d: tool fields mismatch: %+v vs %+v", idx, got, want)
	}
	if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens ||
		got.CachedInputTokens != want.CachedInputTokens || got.ReasoningOutputTokens != want.ReasoningOutputTokens ||
		got.CostUSD != want.CostUSD {
		t.Errorf("event %d: usage mismatch: %+v vs %+v", idx, got, want)
	}
	if got.TerminationReason != want.TerminationReason {
		t.Errorf("event %d: termination = %q, want %q", idx, got.TerminationReason, want.TerminationReason)
	}
	if want.Input != nil {
		gotMap, ok := got.Input.(map[string]any)
		if !ok {
			t.Errorf("event %d: input not a map: %+v", idx, got.Input)
			return
		}
		for k, v := range want.Input.(map[string]any) {
			if gotMap[k] != v {
				t.Errorf("event %d: input[%s] = %v, want %v", idx, k, gotMap[k], v)
			}
		}
	}
}

func TestTranslateGrokLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []Event
	}{
		{
			name: "thought",
			line: `{"type":"thought","data":"thinking about it"}`,
			want: []Event{{Type: EventThinking, Delta: "thinking about it"}},
		},
		{
			name: "text",
			line: `{"type":"text","data":"Hello, world!"}`,
			want: []Event{{Type: EventText, Delta: "Hello, world!"}},
		},
		{
			name: "end with usage",
			line: `{"type":"end","stopReason":"EndTurn","sessionId":"s1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"reasoning_tokens":7},"total_cost_usd":0.01}`,
			want: []Event{
				{Type: EventSystem, SessionID: "s1"},
				{Type: EventUsage, InputTokens: 10, OutputTokens: 5, CachedInputTokens: 3, ReasoningOutputTokens: 7, CostUSD: 0.01},
				{Type: EventDone, SessionID: "s1", TerminationReason: TermNormal},
			},
		},
		{
			name: "end with model",
			line: `{"type":"end","stopReason":"EndTurn","sessionId":"s1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"reasoning_tokens":7},"modelUsage":{"grok-4.5-build":{"inputTokens":10,"outputTokens":5}},"total_cost_usd":0.01}`,
			want: []Event{
				{Type: EventSystem, SessionID: "s1", Model: "grok-4.5-build"},
				{Type: EventUsage, InputTokens: 10, OutputTokens: 5, CachedInputTokens: 3, ReasoningOutputTokens: 7, CostUSD: 0.01},
				{Type: EventDone, SessionID: "s1", TerminationReason: TermNormal},
			},
		},
		{
			name: "end without usage",
			line: `{"type":"end","stopReason":"Stop","sessionId":"s2"}`,
			want: []Event{
				{Type: EventSystem, SessionID: "s2"},
				{Type: EventDone, SessionID: "s2", TerminationReason: TermNormal},
			},
		},
		{
			name: "empty thought",
			line: `{"type":"thought"}`,
			want: nil,
		},
		{
			name: "garbage",
			line: `not json`,
			want: nil,
		},
		{
			name: "unknown type",
			line: `{"type":"unknown","data":"x"}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateGrokLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				assertEventEqual(t, i, got[i], want)
			}
		})
	}
}

func TestCodexTranslator(t *testing.T) {
	tr := &codexTranslator{}

	lines := []string{
		`{"type":"thread.started","thread_id":"th1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"type":"command_execution","id":"c1","command":"ls -la"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","id":"c1","exit_code":0,"output":"total 0"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"第一段"} }`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"最终回答"} }`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":5,"reasoning_output_tokens":7}}`,
	}

	var events []Event
	for _, l := range lines {
		events = append(events, tr.translate([]byte(l))...)
	}

	// thread.started → system
	if events[0].Type != EventSystem || events[0].ThreadID != "th1" {
		t.Fatalf("first event = %+v", events[0])
	}

	var types []EventType
	var finalText, toolOutput string
	var usageSeen bool
	for _, e := range events {
		types = append(types, e.Type)
		if e.Type == EventFinalText {
			finalText = e.Content
		}
		if e.Type == EventToolResult {
			toolOutput = e.Output
		}
		if e.Type == EventUsage {
			usageSeen = true
			if e.InputTokens != 100 || e.CachedInputTokens != 5 || e.ReasoningOutputTokens != 7 {
				t.Errorf("usage = %+v", e)
			}
		}
	}

	if finalText != "最终回答" {
		t.Errorf("final text = %q, want 最终回答", finalText)
	}
	if toolOutput != "total 0" {
		t.Errorf("tool output = %q", toolOutput)
	}
	if !usageSeen {
		t.Error("no usage event")
	}
	// pending agent message: first message flushed as text delta when the
	// second arrives, second becomes final_text on turn.completed.
	var sawTextDelta bool
	for _, e := range events {
		if e.Type == EventText && e.Delta == "第一段" {
			sawTextDelta = true
		}
	}
	if !sawTextDelta {
		t.Error("pending agent message was not flushed as text delta")
	}
	// terminal event present
	last := events[len(events)-1]
	if last.Type != EventDone || last.TerminationReason != TermNormal || last.ThreadID != "th1" {
		t.Errorf("last event = %+v", last)
	}
}

func TestCodexTranslatorExtendedItems(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		check func(*testing.T, []Event)
	}{
		{
			name: "reasoning",
			lines: []string{
				`{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"检查依赖关系"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 1 || got[0].Type != EventThinking || got[0].Delta != "检查依赖关系" {
					t.Fatalf("events = %+v", got)
				}
			},
		},
		{
			name: "command failed by status",
			lines: []string{
				`{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"false","aggregated_output":"nope","exit_code":null,"status":"failed"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Type != EventToolUse || got[1].Type != EventToolResult {
					t.Fatalf("events = %+v", got)
				}
				if !got[1].IsError || got[1].Output != "nope" {
					t.Fatalf("result = %+v", got[1])
				}
			},
		},
		{
			name: "file change without started",
			lines: []string{
				`{"type":"item.completed","item":{"id":"p1","type":"file_change","changes":[{"path":"a.go","kind":"update"},{"path":"b.go","kind":"add"}],"status":"completed"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "file_change" {
					t.Fatalf("events = %+v", got)
				}
				if got[1].Output != "update a.go\nadd b.go" || got[1].IsError {
					t.Fatalf("result = %+v", got[1])
				}
			},
		},
		{
			name: "mcp tool",
			lines: []string{
				`{"type":"item.started","item":{"id":"m1","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"q":"codex"},"status":"in_progress"}}`,
				`{"type":"item.completed","item":{"id":"m1","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"q":"codex"},"result":{"content":[{"type":"text","text":"ok"}],"structured_content":null},"error":null,"status":"completed"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "mcp:docs/search" {
					t.Fatalf("events = %+v", got)
				}
				if got[1].Type != EventToolResult || got[1].IsError || !strings.Contains(got[1].Output, `"text":"ok"`) {
					t.Fatalf("result = %+v", got[1])
				}
			},
		},
		{
			name: "web search",
			lines: []string{
				`{"type":"item.started","item":{"id":"w1","type":"web_search","query":"Codex CLI","action":{"type":"search","query":"Codex CLI"}}}`,
				`{"type":"item.completed","item":{"id":"w1","type":"web_search","query":"Codex CLI","action":{"type":"search","query":"Codex CLI"}}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "web_search" || !strings.Contains(got[1].Output, `"type":"search"`) {
					t.Fatalf("events = %+v", got)
				}
			},
		},
		{
			name: "collab tool",
			lines: []string{
				`{"type":"item.started","item":{"id":"a1","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"th1","receiver_thread_ids":["th2"],"prompt":"test","agents_states":{},"status":"in_progress"}}`,
				`{"type":"item.completed","item":{"id":"a1","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"th1","receiver_thread_ids":["th2"],"prompt":"test","agents_states":{"th2":{"status":"completed"}},"status":"completed"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "collab:spawn_agent" || !strings.Contains(got[1].Output, `"completed"`) {
					t.Fatalf("events = %+v", got)
				}
			},
		},
		{
			name: "todo list",
			lines: []string{
				`{"type":"item.started","item":{"id":"t1","type":"todo_list","items":[{"text":"实现","completed":false}]}}`,
				`{"type":"item.updated","item":{"id":"t1","type":"todo_list","items":[{"text":"实现","completed":true}]}}`,
				`{"type":"item.completed","item":{"id":"t1","type":"todo_list","items":[{"text":"实现","completed":true}]}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "todo_list" || got[1].Output != "☑ 实现" {
					t.Fatalf("events = %+v", got)
				}
			},
		},
		{
			name: "nonfatal item error",
			lines: []string{
				`{"type":"item.completed","item":{"id":"e1","type":"error","message":"MCP unavailable"}}`,
			},
			check: func(t *testing.T, got []Event) {
				if len(got) != 2 || got[0].Name != "codex_error" || !got[1].IsError {
					t.Fatalf("events = %+v", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &codexTranslator{}
			var got []Event
			for _, line := range tc.lines {
				got = append(got, tr.translate([]byte(line))...)
			}
			tc.check(t, got)
		})
	}
}

func TestCodexTranslatorTopLevelErrorIsTerminal(t *testing.T) {
	tr := &codexTranslator{}
	got := tr.translate([]byte(`{"type":"error","message":"authentication failed"}`))
	if len(got) != 1 || got[0].Type != EventError || got[0].Message != "authentication failed" {
		t.Fatalf("events = %+v", got)
	}
	if more := tr.translate([]byte(`{"type":"turn.completed","usage":{}}`)); more != nil {
		t.Fatalf("translator emitted after terminal error: %+v", more)
	}
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
