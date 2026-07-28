package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestTranslatePiExtensionUISelect(t *testing.T) {
	ps := &piSession{}
	events := ps.translatePiUIRequest(map[string]any{
		"type":    "extension_ui_request",
		"id":      "ui-1",
		"method":  "select",
		"title":   "Allow?",
		"options": []any{"Allow", "Block"},
	})
	if len(events) != 1 || events[0].Type != EventAskUser {
		t.Fatalf("events=%v", events)
	}
	if events[0].AskID != "ui-1" || len(events[0].Questions) != 1 || len(events[0].Questions[0].Options) != 2 {
		t.Fatalf("ask=%+v", events[0])
	}
	if events[0].Source != "pi" || events[0].Reply == nil {
		t.Fatalf("source/reply missing")
	}
}

func TestTranslatePiExtensionUIConfirm(t *testing.T) {
	ps := &piSession{}
	events := ps.translatePiUIRequest(map[string]any{
		"type":    "extension_ui_request",
		"id":      "ui-2",
		"method":  "confirm",
		"title":   "Clear?",
		"message": "All lost",
	})
	if len(events) != 1 || events[0].Questions[0].Options[0].Key != "yes" {
		t.Fatalf("events=%+v", events)
	}
}

func TestTranslatePiExtensionUIInputFreeform(t *testing.T) {
	ps := &piSession{}
	events := ps.translatePiUIRequest(map[string]any{
		"type":        "extension_ui_request",
		"id":          "ui-3",
		"method":      "input",
		"title":       "Name?",
		"placeholder": "type here",
	})
	if len(events) != 1 || !events[0].Freeform {
		t.Fatalf("events=%+v", events)
	}
}

func TestTranslateOpenCodeQuestionAsked(t *testing.T) {
	s := &ocServer{}
	events, terminal := s.translate(ocEventEnvelope{
		Type: "question.asked",
		Properties: map[string]any{
			"id":        "que_1",
			"sessionID": "ses_1",
			"questions": []any{
				map[string]any{
					"question": "部署？",
					"options": []any{
						map[string]any{"label": "是"},
						map[string]any{"label": "否"},
					},
				},
			},
		},
	})
	if terminal || len(events) != 1 || events[0].Type != EventAskUser {
		t.Fatalf("events=%v terminal=%v", events, terminal)
	}
	if events[0].AskID != "que_1" || len(events[0].Questions) != 1 {
		t.Fatalf("ask=%+v", events[0])
	}
}

func TestTranslateOpenCodePermissionAsked(t *testing.T) {
	s := &ocServer{}
	events, terminal := s.translate(ocEventEnvelope{
		Type: "permission.asked",
		Properties: map[string]any{
			"id":         "per_1",
			"sessionID":  "ses_1",
			"permission": "bash",
			"patterns":   []any{"rm -rf /tmp/foo"},
			"metadata":   map[string]any{"command": "rm -rf /tmp/foo"},
			"always":     []any{"rm -rf /tmp/foo"},
		},
	})
	if terminal || len(events) != 1 || events[0].Type != EventAskUser {
		t.Fatalf("events=%v terminal=%v", events, terminal)
	}
	evt := events[0]
	if evt.AskID != "per_1" || evt.Source != "opencode-permission" {
		t.Fatalf("ask=%+v", evt)
	}
	if len(evt.Questions) != 1 || len(evt.Questions[0].Options) != 3 {
		t.Fatalf("questions=%+v", evt.Questions)
	}
	keys := map[string]bool{}
	for _, o := range evt.Questions[0].Options {
		keys[o.Key] = true
	}
	for _, want := range []string{"once", "always", "reject"} {
		if !keys[want] {
			t.Errorf("missing option %q", want)
		}
	}
	if !containsSubstr(evt.Questions[0].Prompt, "bash") {
		t.Errorf("prompt should mention permission type: %q", evt.Questions[0].Prompt)
	}
}

func TestTranslateOpenCodePermissionV2Asked(t *testing.T) {
	s := &ocServer{}
	events, terminal := s.translate(ocEventEnvelope{
		Type: "permission.v2.asked",
		Properties: map[string]any{
			"id":        "per_v2",
			"sessionID": "ses_1",
			"action":    "edit",
			"resources": []any{"src/main.go"},
			"save":      []any{"src/*"},
		},
	})
	if terminal || len(events) != 1 {
		t.Fatalf("events=%v terminal=%v", events, terminal)
	}
	if events[0].AskID != "per_v2" || events[0].Source != "opencode-permission" {
		t.Fatalf("ask=%+v", events[0])
	}
	if !containsSubstr(events[0].Questions[0].Prompt, "edit") {
		t.Errorf("prompt=%q", events[0].Questions[0].Prompt)
	}
}

func TestOCAutoAllowPermission(t *testing.T) {
	if !ocAutoAllowPermission("") || !ocAutoAllowPermission(config.AccessFull) {
		t.Fatal("empty/full should auto-allow")
	}
	if ocAutoAllowPermission(config.AccessWorkspace) || ocAutoAllowPermission(config.AccessReadOnly) {
		t.Fatal("workspace/read-only should require Feishu card")
	}
}

func TestAutoAllowPermissionsFiltersEvents(t *testing.T) {
	// postJSON hits a dead server; on failure events are kept (card fallback).
	// Here we only assert filtering when auto is false (workspace).
	s := &ocServer{
		runs: map[string]map[uint64]*ocRun{
			"ses_1": {1: &ocRun{sessionID: "ses_1", directory: "/tmp", access: config.AccessWorkspace}},
		},
		client: &http.Client{},
		base:   "http://127.0.0.1:1", // unreachable; not used when auto=false
	}
	events := []Event{{
		Type: EventAskUser, AskID: "per_1", Source: "opencode-permission",
		Questions: []AskQuestion{{Prompt: "x", Options: ocPermissionOptions}},
	}}
	got := s.autoAllowPermissions("ses_1", events)
	if len(got) != 1 {
		t.Fatalf("workspace should keep permission event: %+v", got)
	}

	// full access with unreachable server → fallback keeps event
	s.runs["ses_1"][1].access = config.AccessFull
	got = s.autoAllowPermissions("ses_1", events)
	if len(got) != 1 {
		t.Fatalf("auto-allow failure should fall back to card: %+v", got)
	}
}

func TestOCPermissionReply(t *testing.T) {
	cases := []struct {
		answers   [][]string
		cancelled bool
		want      string
	}{
		{[][]string{{"once"}}, false, "once"},
		{[][]string{{"always"}}, false, "always"},
		{[][]string{{"reject"}}, false, "reject"},
		{[][]string{{"允许一次"}}, false, "once"},
		{[][]string{{"始终允许"}}, false, "always"},
		{[][]string{{"拒绝"}}, false, "reject"},
		{nil, true, "reject"},
		{[][]string{{}}, false, "reject"},
		{[][]string{{"weird"}}, false, "reject"},
	}
	for _, tc := range cases {
		got := ocPermissionReply(tc.answers, tc.cancelled)
		if got != tc.want {
			t.Errorf("answers=%v cancelled=%v got=%q want=%q", tc.answers, tc.cancelled, got, tc.want)
		}
	}
}

func containsSubstr(s, sub string) bool {
	return strings.Contains(s, sub)
}

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

	// idle → done event, but defers channel close to pollRun
	events, terminal = srv.translate(ocEventEnvelope{
		Type:       "session.idle",
		Properties: map[string]any{"sessionID": "ses_1"},
	})
	if terminal || len(events) != 1 || events[0].Type != EventDone {
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
