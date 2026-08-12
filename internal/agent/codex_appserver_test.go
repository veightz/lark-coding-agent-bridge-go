package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

func newCodexProtocolTestRun() (*codexAppRun, *bufferWriteCloser) {
	stdin := &bufferWriteCloser{}
	return &codexAppRun{
		stdin:   stdin,
		events:  make(chan Event, 10),
		exited:  make(chan struct{}),
		pending: map[string]chan codexRPCResponse{},
	}, stdin
}

func decodeRPCWrite(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.NewDecoder(r).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodexUserInputEventReply(t *testing.T) {
	r, stdin := newCodexProtocolTestRun()
	evt := r.codexUserInputEvent(float64(41), map[string]any{
		"itemId": "ask-1",
		"questions": []any{
			map[string]any{
				"id":       "language",
				"header":   "语言",
				"question": "选择实现语言",
				"isOther":  true,
				"options": []any{
					map[string]any{"label": "Go", "description": "标准库优先"},
					map[string]any{"label": "Rust", "description": "严格类型"},
				},
			},
		},
	})
	if evt.Type != EventAskUser || evt.Source != "codex" || evt.AskID != "ask-1" || !evt.Freeform {
		t.Fatalf("event = %+v", evt)
	}
	if len(evt.Questions) != 1 || len(evt.Questions[0].Options) != 2 {
		t.Fatalf("questions = %+v", evt.Questions)
	}
	if evt.Questions[0].Options[0].Key != "Go" || evt.Questions[0].Options[0].Label != "Go" {
		t.Fatalf("option = %+v", evt.Questions[0].Options[0])
	}
	if err := evt.Reply([][]string{{"Go"}}, false); err != nil {
		t.Fatal(err)
	}
	got := decodeRPCWrite(t, stdin)
	if rpcID(got["id"]) != "41" {
		t.Fatalf("id = %#v", got["id"])
	}
	result := mapField(got, "result")
	answers := mapField(result, "answers")
	answer := mapField(answers, "language")
	values := anyStrings(answer["answers"])
	if len(values) != 1 || values[0] != "Go" {
		t.Fatalf("result = %#v", result)
	}
}

func TestCodexSecretUserInputDisablesFreeform(t *testing.T) {
	r, _ := newCodexProtocolTestRun()
	evt := r.codexUserInputEvent(42, map[string]any{
		"itemId": "ask-secret",
		"questions": []any{map[string]any{
			"id":       "token",
			"question": "请输入令牌",
			"isOther":  true,
			"isSecret": true,
			"options":  nil,
		}},
	})
	if evt.Freeform {
		t.Fatal("secret question must not allow chat freeform")
	}
	if len(evt.Questions) != 1 || len(evt.Questions[0].Options) != 1 {
		t.Fatalf("questions = %+v", evt.Questions)
	}
	if !containsStr(evt.Questions[0].Prompt, "不能通过飞书文字作答") {
		t.Fatalf("prompt = %q", evt.Questions[0].Prompt)
	}
}

func TestCodexCommandApprovalReplyMapping(t *testing.T) {
	tests := []struct {
		name      string
		answers   [][]string
		cancelled bool
		want      string
	}{
		{name: "once", answers: [][]string{{"once"}}, want: "accept"},
		{name: "always", answers: [][]string{{"always"}}, want: "acceptForSession"},
		{name: "reject", answers: [][]string{{"reject"}}, want: "decline"},
		{name: "timeout", cancelled: true, want: "decline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, stdin := newCodexProtocolTestRun()
			evt := r.codexCommandApprovalEvent("req-2", map[string]any{
				"itemId":             "cmd-1",
				"command":            "go test ./...",
				"cwd":                "/repo",
				"reason":             "运行测试",
				"availableDecisions": []any{"accept", "acceptForSession", "decline"},
			})
			if evt.Source != "codex-permission" || evt.AskID != "cmd-1" {
				t.Fatalf("event = %+v", evt)
			}
			if err := evt.Reply(tc.answers, tc.cancelled); err != nil {
				t.Fatal(err)
			}
			got := decodeRPCWrite(t, stdin)
			if decision := stringField(mapField(got, "result"), "decision"); decision != tc.want {
				t.Fatalf("decision = %q, want %q", decision, tc.want)
			}
		})
	}
}

func TestCodexCommandApprovalFallsBackWhenSessionDecisionUnavailable(t *testing.T) {
	r, stdin := newCodexProtocolTestRun()
	evt := r.codexCommandApprovalEvent(2, map[string]any{
		"itemId":             "cmd-2",
		"availableDecisions": []any{"accept", "decline"},
	})
	if err := evt.Reply([][]string{{"always"}}, false); err != nil {
		t.Fatal(err)
	}
	got := decodeRPCWrite(t, stdin)
	if decision := stringField(mapField(got, "result"), "decision"); decision != "accept" {
		t.Fatalf("decision = %q", decision)
	}
}

func TestCodexFileApprovalReply(t *testing.T) {
	r, stdin := newCodexProtocolTestRun()
	evt := r.codexFileApprovalEvent("file-request", map[string]any{
		"itemId":    "file-1",
		"grantRoot": "/repo/generated",
		"reason":    "写入生成文件",
	})
	if evt.Source != "codex-permission" || evt.AskID != "file-1" {
		t.Fatalf("event = %+v", evt)
	}
	if len(evt.Questions) != 1 || !containsStr(evt.Questions[0].Prompt, "/repo/generated") {
		t.Fatalf("questions = %+v", evt.Questions)
	}
	if err := evt.Reply([][]string{{"once"}}, false); err != nil {
		t.Fatal(err)
	}
	got := decodeRPCWrite(t, stdin)
	if decision := stringField(mapField(got, "result"), "decision"); decision != "accept" {
		t.Fatalf("decision = %q", decision)
	}
}

func TestCodexPermissionsApprovalGrantAndDeny(t *testing.T) {
	requested := map[string]any{
		"network": map[string]any{"enabled": true},
		"fileSystem": map[string]any{
			"read":  []any{"/shared"},
			"write": []any{"/repo"},
		},
	}
	tests := []struct {
		name        string
		answer      string
		cancelled   bool
		wantScope   string
		wantGranted bool
	}{
		{name: "turn", answer: "once", wantScope: "turn", wantGranted: true},
		{name: "session", answer: "always", wantScope: "session", wantGranted: true},
		{name: "deny", answer: "reject", wantScope: "turn", wantGranted: false},
		{name: "timeout", cancelled: true, wantScope: "turn", wantGranted: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, stdin := newCodexProtocolTestRun()
			evt := r.codexPermissionsApprovalEvent(8, map[string]any{
				"itemId":      "perm-1",
				"cwd":         "/repo",
				"reason":      "访问共享依赖",
				"permissions": requested,
			})
			if err := evt.Reply([][]string{{tc.answer}}, tc.cancelled); err != nil {
				t.Fatal(err)
			}
			got := decodeRPCWrite(t, stdin)
			result := mapField(got, "result")
			if scope := stringField(result, "scope"); scope != tc.wantScope {
				t.Fatalf("scope = %q", scope)
			}
			granted := mapField(result, "permissions")
			if tc.wantGranted && mapField(granted, "network") == nil {
				t.Fatalf("permissions not granted: %#v", granted)
			}
			if !tc.wantGranted && len(granted) != 0 {
				t.Fatalf("permissions should be empty: %#v", granted)
			}
		})
	}
}

func TestCodexApprovalPolicyByAccess(t *testing.T) {
	if got := codexApprovalPolicy(codexAccessFull); got != "never" {
		t.Errorf("full = %q", got)
	}
	if got := codexApprovalPolicy(codexAccessWorkspace); got != "on-request" {
		t.Errorf("workspace = %q", got)
	}
	if got := codexApprovalPolicy(codexAccessReadOnly); got != "on-request" {
		t.Errorf("read-only = %q", got)
	}
}

func TestCodexActiveTurnID(t *testing.T) {
	thread := map[string]any{
		"status": map[string]any{"type": "active"},
		"turns": []any{
			map[string]any{"id": "turn-old", "status": "completed"},
			map[string]any{"id": "turn-live", "status": "inProgress"},
		},
	}
	if got := codexThreadStatus(thread); got != "active" {
		t.Fatalf("status = %q", got)
	}
	if got := codexActiveTurnID(thread); got != "turn-live" {
		t.Fatalf("active turn = %q", got)
	}
}

func TestCodexActiveTurnIDAcceptsSnakeCaseStatus(t *testing.T) {
	thread := map[string]any{"turns": []any{
		map[string]any{"id": "turn-live", "status": "in_progress"},
	}}
	if got := codexActiveTurnID(thread); got != "turn-live" {
		t.Fatalf("active turn = %q", got)
	}
}

func TestCodexAppToolTranslation(t *testing.T) {
	item := map[string]any{
		"type":             "commandExecution",
		"id":               "cmd-1",
		"command":          "go test ./...",
		"status":           "completed",
		"aggregatedOutput": "ok",
	}
	use, ok := codexAppToolUse(item)
	if !ok || use.Type != EventToolUse || use.Name != "command_execution" {
		t.Fatalf("use = %+v, %v", use, ok)
	}
	result, ok := codexAppToolResult(item)
	if !ok || result.Type != EventToolResult || result.Output != "ok" || result.IsError {
		t.Fatalf("result = %+v, %v", result, ok)
	}
}
