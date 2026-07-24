package agent

import (
	"encoding/json"
	"testing"
)

func TestTranslatePiEvent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []Event
	}{
		{
			name: "text delta",
			raw:  `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"你好"}}`,
			want: []Event{{Type: EventText, Delta: "你好"}},
		},
		{
			name: "thinking delta",
			raw:  `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"hmm"}}`,
			want: []Event{{Type: EventThinking, Delta: "hmm"}},
		},
		{
			name: "tool start",
			raw:  `{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"ls"}}`,
			want: []Event{{Type: EventToolUse, ID: "c1", Name: "bash", Input: map[string]any{"command": "ls"}}},
		},
		{
			name: "tool end",
			raw:  `{"type":"tool_execution_end","toolCallId":"c1","isError":false,"result":{"content":[{"type":"text","text":"ok\n"}]}}`,
			want: []Event{{Type: EventToolResult, ID: "c1", Output: "ok\n"}},
		},
		{
			name: "settled",
			raw:  `{"type":"agent_settled"}`,
			want: []Event{{Type: EventDone, TerminationReason: TermNormal}},
		},
		{
			name: "noise ignored",
			raw:  `{"type":"turn_start"}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatal(err)
			}
			got := translatePiEvent(raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events: %+v", len(got), got)
			}
			for i, want := range tc.want {
				assertEventEqual(t, i, got[i], want)
			}
		})
	}
}

func TestTranslateACPUpdate(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []Event
	}{
		{
			name: "message chunk",
			raw:  `{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}`,
			want: []Event{{Type: EventText, Delta: "hi"}},
		},
		{
			name: "thought chunk",
			raw:  `{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"think"}}}`,
			want: []Event{{Type: EventThinking, Delta: "think"}},
		},
		{
			name: "tool call",
			raw:  `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{"command":"ls"}}}`,
			want: []Event{{Type: EventToolUse, ID: "call_1", Name: "bash"}},
		},
		{
			name: "tool completed",
			raw:  `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"out"}}]}}`,
			want: []Event{{Type: EventToolResult, ID: "call_1", Output: "out"}},
		},
		{
			name: "tool in_progress ignored",
			raw:  `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"in_progress"}}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateACPUpdate(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events: %+v", len(got), got)
			}
			for i, want := range tc.want {
				assertEventEqual(t, i, got[i], want)
			}
		})
	}
}

func TestAutoAllowPermission(t *testing.T) {
	params := json.RawMessage(`{"options":[{"optionId":"o1","kind":"reject_once"},{"optionId":"o2","kind":"allow_once"},{"optionId":"o3","kind":"allow_always"}]}`)
	out := autoAllowPermission(params)
	outcome := out["outcome"].(map[string]any)
	if outcome["optionId"] != "o3" {
		t.Errorf("should prefer allow_always: %+v", outcome)
	}
}

func TestOpenCodeTranslate(t *testing.T) {
	srv := &ocServer{runs: map[string]map[uint64]*ocRun{}, partKinds: map[string]string{}}

	// tool pending → tool_use
	events, terminal := srv.translate(ocEventEnvelope{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": "ses_1",
			"part": map[string]any{
				"id": "prt_1", "type": "tool", "tool": "bash", "callID": "call_1",
				"state": map[string]any{"status": "running", "input": map[string]any{"command": "ls"}},
			},
		},
	})
	if terminal || len(events) != 1 || events[0].Type != EventToolUse || events[0].Name != "bash" {
		t.Fatalf("tool running: %+v terminal=%v", events, terminal)
	}

	// tool completed → tool_result with output
	events, _ = srv.translate(ocEventEnvelope{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": "ses_1",
			"part": map[string]any{
				"id": "prt_1", "type": "tool", "tool": "bash", "callID": "call_1",
				"state": map[string]any{"status": "completed", "output": "done\n"},
			},
		},
	})
	if len(events) != 1 || events[0].Type != EventToolResult || events[0].Output != "done\n" || events[0].IsError {
		t.Fatalf("tool completed: %+v", events)
	}

	// text part registered → delta becomes text
	srv.translate(ocEventEnvelope{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": "ses_1",
			"part":      map[string]any{"id": "prt_2", "type": "text"},
		},
	})
	events, _ = srv.translate(ocEventEnvelope{
		Type:       "message.part.delta",
		Properties: map[string]any{"sessionID": "ses_1", "partID": "prt_2", "delta": "hello"},
	})
	if len(events) != 1 || events[0].Type != EventText || events[0].Delta != "hello" {
		t.Fatalf("text delta: %+v", events)
	}

	// reasoning part → thinking delta
	srv.translate(ocEventEnvelope{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": "ses_1",
			"part":      map[string]any{"id": "prt_3", "type": "reasoning"},
		},
	})
	events, _ = srv.translate(ocEventEnvelope{
		Type:       "message.part.delta",
		Properties: map[string]any{"sessionID": "ses_1", "partID": "prt_3", "delta": "hmm"},
	})
	if len(events) != 1 || events[0].Type != EventThinking {
		t.Fatalf("reasoning delta: %+v", events)
	}

	// idle → terminal done
	events, terminal = srv.translate(ocEventEnvelope{
		Type:       "session.idle",
		Properties: map[string]any{"sessionID": "ses_1"},
	})
	if !terminal || len(events) != 1 || events[0].Type != EventDone {
		t.Fatalf("idle: %+v terminal=%v", events, terminal)
	}

	// abort error → interrupted terminal
	events, terminal = srv.translate(ocEventEnvelope{
		Type: "session.error",
		Properties: map[string]any{
			"sessionID": "ses_1",
			"error":     map[string]any{"name": "MessageAbortedError", "data": map[string]any{"message": "Aborted"}},
		},
	})
	if !terminal || events[0].TerminationReason != TermInterrupted {
		t.Fatalf("abort: %+v terminal=%v", events, terminal)
	}
}
