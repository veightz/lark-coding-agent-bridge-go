package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestNewAdapterCursorIdentity(t *testing.T) {
	a := NewAdapter(config.AgentCursor)
	if a.ID() != "cursor" {
		t.Fatalf("ID=%q", a.ID())
	}
	if a.DisplayName() != "Cursor" {
		t.Fatalf("DisplayName=%q", a.DisplayName())
	}
	if _, ok := a.(*CursorAdapter); !ok {
		t.Fatalf("type=%T, want *CursorAdapter", a)
	}
}

func TestCursorPrintAndACPArgs(t *testing.T) {
	printArgs := cursorPrintArgs(RunOptions{
		Prompt:    "hi",
		SessionID: "sess-9",
		Model:     "composer-2",
	})
	joined := strings.Join(printArgs, " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--force", "--resume sess-9", "--model composer-2", "hi"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("print args missing %q: %v", want, printArgs)
		}
	}
	acp := cursorACPArgs()
	if len(acp) != 1 || acp[0] != "acp" {
		t.Fatalf("acp args=%v", acp)
	}
}

func TestTranslateCursorLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []Event
	}{
		{
			name: "system init",
			line: `{"type":"system","subtype":"init","cwd":"/work","session_id":"c-s1","model":"Composer","permissionMode":"default"}`,
			want: []Event{{Type: EventSystem, SessionID: "c-s1", Cwd: "/work", Model: "Composer"}},
		},
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello-cursor"}]},"session_id":"c-s1"}`,
			want: []Event{{Type: EventText, Delta: "hello-cursor"}},
		},
		{
			name: "assistant delta",
			line: `{"type":"assistant","subtype":"delta","text":"chunk","session_id":"c-s1"}`,
			want: []Event{{Type: EventText, Delta: "chunk"}},
		},
		{
			name: "tool_call started",
			line: `{"type":"tool_call","subtype":"started","call_id":"t1","tool_call":{"readToolCall":{"args":{"path":"a.go"}}},"session_id":"c-s1"}`,
			want: []Event{{Type: EventToolUse, ID: "t1", Name: "read", Input: map[string]any{"path": "a.go"}}},
		},
		{
			name: "tool_call completed",
			line: `{"type":"tool_call","subtype":"completed","call_id":"t1","tool_call":{"readToolCall":{"args":{"path":"a.go"},"result":{"success":{"content":"ok"}}}},"session_id":"c-s1"}`,
			want: []Event{{Type: EventToolResult, ID: "t1", Output: `{"success":{"content":"ok"}}`}},
		},
		{
			name: "result success",
			line: `{"type":"result","subtype":"success","is_error":false,"result":"hello-cursor","session_id":"c-s1","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":1}}`,
			want: []Event{
				{Type: EventUsage, InputTokens: 3, OutputTokens: 2, CachedInputTokens: 1},
				{Type: EventDone, SessionID: "c-s1", TerminationReason: TermNormal},
			},
		},
		{
			name: "result error",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","session_id":"c-s1"}`,
			want: []Event{{Type: EventError, SessionID: "c-s1", Message: `"boom"`, TerminationReason: TermFailed}},
		},
		{
			name: "garbage",
			line: `not json`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateCursorLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				assertEventEqual(t, i, got[i], want)
			}
		})
	}
}

func TestTranslateACPUpdateCursorPayloads(t *testing.T) {
	text := translateACPUpdate(json.RawMessage(`{
		"sessionId":"c-s1",
		"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello-acp"}}
	}`))
	if len(text) != 1 || text[0].Type != EventText || text[0].Delta != "hello-acp" {
		t.Fatalf("text=%+v", text)
	}
	tool := translateACPUpdate(json.RawMessage(`{
		"sessionId":"c-s1",
		"update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"read","rawInput":{"path":"a.go"}}
	}`))
	if len(tool) != 1 || tool[0].Type != EventToolUse || tool[0].ID != "t1" || tool[0].Name != "read" {
		t.Fatalf("tool=%+v", tool)
	}
	done := translateACPUpdate(json.RawMessage(`{
		"sessionId":"c-s1",
		"update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{"ok":true}}
	}`))
	if len(done) != 1 || done[0].Type != EventToolResult || done[0].ID != "t1" || done[0].IsError {
		t.Fatalf("result=%+v", done)
	}
}

func TestParseAndFormatCursorAsk(t *testing.T) {
	raw := json.RawMessage(`{
		"toolCallId":"call-1",
		"title":"Need input",
		"questions":[{
			"id":"q1",
			"prompt":"Which mode should I use?",
			"options":[{"id":"agent","label":"Agent"},{"id":"plan","label":"Plan"}],
			"allowMultiple":false
		}]
	}`)
	id, qs := parseCursorAskParams(raw)
	if id != "call-1" || len(qs) != 1 || qs[0].ID != "q1" {
		t.Fatalf("id=%s qs=%+v", id, qs)
	}
	if qs[0].Prompt != "Which mode should I use?" || qs[0].MultiSelect || qs[0].Options[0].Key != "agent" {
		t.Fatalf("q=%+v", qs[0])
	}

	got := formatCursorAskAnswer(qs, [][]string{{"Agent"}}, false)
	inner := cursorAskWireOutcome(t, got, "answered")
	ans, _ := inner["answers"].([]any)
	if len(ans) != 1 {
		t.Fatalf("answers=%v", inner["answers"])
	}
	row, _ := ans[0].(map[string]any)
	if row["questionId"] != "q1" {
		t.Fatalf("answers=%v", ans)
	}
	ids, _ := row["selectedOptionIds"].([]any)
	if len(ids) != 1 || ids[0] != "agent" {
		t.Fatalf("selected=%v", ids)
	}

	cancelled := formatCursorAskAnswer(qs, nil, true)
	cursorAskWireOutcome(t, cancelled, "cancelled")
	// create_plan / 其它阻塞 cursor/* 与 ask 取消共用同一信封。
	cursorAskWireOutcome(t, cursorCancelledOutcome(), "cancelled")

	multiRaw := json.RawMessage(`{
		"toolCallId":"c2",
		"questions":[{
			"id":"feat",
			"prompt":"Pick features",
			"allowMultiple":true,
			"options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]
		}]
	}`)
	_, multi := parseCursorAskParams(multiRaw)
	if len(multi) != 1 || !multi[0].MultiSelect {
		t.Fatalf("multi=%+v", multi)
	}
	gotM := formatCursorAskAnswer(multi, [][]string{{"a", "B"}}, false)
	innerM := cursorAskWireOutcome(t, gotM, "answered")
	ansM, _ := innerM["answers"].([]any)
	rowM, _ := ansM[0].(map[string]any)
	idsM, _ := rowM["selectedOptionIds"].([]any)
	if len(idsM) != 2 || idsM[0] != "a" || idsM[1] != "b" {
		t.Fatalf("multi selected=%v", idsM)
	}
}

// cursorAskWireOutcome marshals the shipped result and requires the official
// nested envelope {"outcome":{"outcome":"<tag>",...}}. A flat
// {"outcome":"answered"} fails here the same way real Cursor would.
func cursorAskWireOutcome(t *testing.T, result cursorExtResult, wantTag string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	inner, ok := wire["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("official Cursor ACP requires nested outcome envelope, got %s", raw)
	}
	tag, _ := inner["outcome"].(string)
	if tag != wantTag {
		t.Fatalf("inner outcome=%q want %q in %s", tag, wantTag, raw)
	}
	return inner
}

func TestLookLikeCursorCLIRejectsGrokAgent(t *testing.T) {
	// 本机真实 Grok `agent`（若存在）绝不能被认成 Cursor。
	if path, err := exec.LookPath("agent"); err == nil {
		if LookLikeCursorCLI(path) {
			t.Fatalf("Grok/unknown agent at %s must not be classified as Cursor", path)
		}
	}
	dir := t.TempDir()
	grok := filepath.Join(dir, "agent")
	if err := os.WriteFile(grok, []byte("#!/bin/sh\necho 'Grok Build TUI'\necho 'grok 1.0.3 (deadbeef)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if LookLikeCursorCLI(grok) {
		t.Fatal("Grok-like --version agent classified as Cursor")
	}

	// symlink 落到 ~/.grok/ 也必须拒绝，即使 basename 叫 agent。
	grokHome := filepath.Join(dir, ".grok", "bin")
	if err := os.MkdirAll(grokHome, 0o755); err != nil {
		t.Fatal(err)
	}
	realGrok := filepath.Join(grokHome, "grok-1.0.3-macos-aarch64")
	if err := os.WriteFile(realGrok, []byte("#!/bin/sh\necho grok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked-agent")
	if err := os.Symlink(realGrok, link); err != nil {
		t.Fatal(err)
	}
	if LookLikeCursorCLI(link) {
		t.Fatal("symlink into .grok/ classified as Cursor")
	}
}

func TestResolveCursorBinaryFindsCursorAgentNotGrok(t *testing.T) {
	dir := t.TempDir()
	cursor := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(cursor, []byte("#!/bin/sh\necho cursor-agent 1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	grok := filepath.Join(dir, "agent")
	if err := os.WriteFile(grok, []byte("#!/bin/sh\necho 'Grok Build TUI'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := ResolveCursorBinary()
	if got != cursor {
		t.Fatalf("ResolveCursorBinary=%q want %q", got, cursor)
	}

	// 只有 Grok agent 时必须返回空。
	if err := os.Remove(cursor); err != nil {
		t.Fatal(err)
	}
	if got := ResolveCursorBinary(); got != "" {
		t.Fatalf("expected empty when only Grok agent on PATH, got %q", got)
	}
}

func TestLookLikeCursorCLIAcceptsCursorNamedAgent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	body := "#!/bin/sh\necho 'Cursor CLI 1.2.3'\necho 'Usage: agent [options] [acp]'\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if !LookLikeCursorCLI(bin) {
		t.Fatal("Cursor-branded agent should be accepted")
	}
}

func TestCursorAdapterRunFakeACP(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"cursor","version":"fake"}}}'
      ;;
    *'"method":"session/new"'*)
      echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"cursor-sess-1"}}'
      ;;
    *'"method":"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"cursor-sess-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello-cursor"}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"cursor-sess-1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"read","kind":"read","rawInput":{"path":"a.go"}}}}'
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"cursor-sess-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":{"ok":true}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'
      ;;
  esac
done
`)
	a := NewCursorAdapter(fake)
	defer a.ResetSession("scope")
	run, err := a.Run(RunOptions{
		RunID:  "r1",
		Scope:  "scope",
		Prompt: "hi",
		Cwd:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)

	var sawSystem, sawText, sawTool, sawResult, sawDone bool
	for _, e := range events {
		switch e.Type {
		case EventSystem:
			sawSystem = e.SessionID == "cursor-sess-1"
		case EventText:
			sawText = sawText || e.Delta == "hello-cursor"
		case EventToolUse:
			sawTool = e.ID == "t1" && e.Name == "read"
		case EventToolResult:
			sawResult = e.ID == "t1" && !e.IsError
		case EventDone:
			sawDone = e.TerminationReason == TermNormal
		case EventError:
			t.Fatalf("unexpected error: %+v", e)
		}
	}
	if !sawSystem || !sawText || !sawTool || !sawResult || !sawDone {
		t.Fatalf("missing events system=%v text=%v tool=%v result=%v done=%v all=%+v",
			sawSystem, sawText, sawTool, sawResult, sawDone, events)
	}
}

func TestCursorAdapterResumesViaSessionLoad(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"jsonrpc":"2.0","id":1,"result":{}}'
      ;;
    *'"method":"session/load"'*'"sessionId":"old-sess"'*)
      echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"old-sess"}}'
      ;;
    *'"method":"session/load"'*)
      echo '{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"wrong session"}}'
      ;;
    *'"method":"session/new"'*)
      echo '{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"should load not new"}}'
      ;;
    *'"method":"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"old-sess","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"resumed"}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'
      ;;
  esac
done
`)
	a := NewCursorAdapter(fake)
	defer a.ResetSession("resume")
	run, err := a.Run(RunOptions{
		RunID:     "r-resume",
		Scope:     "resume",
		Prompt:    "continue",
		Cwd:       t.TempDir(),
		SessionID: "old-sess",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)
	var systemID, doneID, text string
	for _, e := range events {
		if e.Type == EventSystem {
			systemID = e.SessionID
		}
		if e.Type == EventText {
			text += e.Delta
		}
		if e.Type == EventDone {
			doneID = e.SessionID
		}
		if e.Type == EventError {
			t.Fatalf("error: %+v", e)
		}
	}
	if systemID != "old-sess" || doneID != "old-sess" || text != "resumed" {
		t.Fatalf("system=%q done=%q text=%q events=%+v", systemID, doneID, text, events)
	}
}

func TestCursorAdapterAskAndCreatePlan(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"jsonrpc":"2.0","id":1,"result":{}}'
      ;;
    *'"method":"session/new"'*)
      echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"ask-sess"}}'
      ;;
    *'"method":"session/prompt"'*)
      echo '{"jsonrpc":"2.0","id":90,"method":"cursor/create_plan","params":{"title":"Plan"}}'
      echo '{"jsonrpc":"2.0","id":91,"method":"cursor/ask_question","params":{"toolCallId":"ask-1","questions":[{"id":"q1","prompt":"Which mode?","options":[{"id":"agent","label":"Agent"},{"id":"plan","label":"Plan"}]}]}}'
      ;;
    *'"id":90'*'"outcome":{"outcome":"cancelled"'*)
      ;;
    *'"id":90'*)
      echo '{"jsonrpc":"2.0","id":3,"error":{"code":-1,"message":"create_plan needs nested outcome.cancelled"}}'
      ;;
    *'"id":91'*'"outcome":{"outcome":"answered"'*'"questionId":"q1"'*'"selectedOptionIds":["agent"]'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ask-sess","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"picked"}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'
      ;;
    *'"id":91'*)
      echo '{"jsonrpc":"2.0","id":3,"error":{"code":-1,"message":"ask reply needs nested outcome.answered"}}'
      ;;
  esac
done
`)
	a := NewCursorAdapter(fake)
	defer a.ResetSession("ask")
	run, err := a.Run(RunOptions{
		RunID:  "r-ask",
		Scope:  "ask",
		Prompt: "ask me",
		Cwd:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var asks []Event
	var text string
	var done, failed bool
	for evt := range run.Events() {
		switch evt.Type {
		case EventAskUser:
			asks = append(asks, evt)
			if evt.Source != "cursor" {
				t.Fatalf("source=%q", evt.Source)
			}
			if err := evt.Reply([][]string{{"Agent"}}, false); err != nil {
				t.Fatal(err)
			}
		case EventText:
			text += evt.Delta
		case EventDone:
			done = true
		case EventError:
			failed = true
			t.Fatalf("error: %+v", evt)
		}
	}
	if failed || !done || len(asks) != 1 || text != "picked" {
		t.Fatalf("asks=%d done=%v text=%q", len(asks), done, text)
	}
}
