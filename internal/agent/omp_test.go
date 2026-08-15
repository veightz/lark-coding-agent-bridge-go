package agent

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

// 断言 omp 与 pi 在启动参数、会话目录、工具 allowlist 上的差异（ADR-0021）。

func TestNewAdapterOmpIdentity(t *testing.T) {
	a := NewAdapter(config.AgentOmp)
	if a.ID() != "omp" {
		t.Fatalf("ID=%q", a.ID())
	}
	if a.DisplayName() != "Oh My Pi" {
		t.Fatalf("DisplayName=%q", a.DisplayName())
	}
	omp, ok := a.(*PiAdapter)
	if !ok {
		t.Fatalf("type=%T, want *PiAdapter (shared RPC implementation)", a)
	}
	if omp.binary != "omp" {
		t.Fatalf("binary=%q", omp.binary)
	}
}

func TestOmpSpawnArgsUseResumeNotSessionID(t *testing.T) {
	a := NewOmpAdapter("omp")
	args := a.piArgs(RunOptions{
		SessionID: "sess-abc",
		Model:     "deepseek/deepseek-v4-flash",
		Access:    config.AccessFull,
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mode rpc") {
		t.Fatalf("missing --mode rpc: %v", args)
	}
	if !strings.Contains(joined, "--resume sess-abc") {
		t.Fatalf("want --resume for session: %v", args)
	}
	if strings.Contains(joined, "--session-id") {
		t.Fatalf("omp must not pass --session-id: %v", args)
	}
	// pi 仍用 --session-id，确保参数化没有互相污染
	piArgs := NewPiAdapter("pi").piArgs(RunOptions{SessionID: "sess-abc"})
	piJoined := strings.Join(piArgs, " ")
	if !strings.Contains(piJoined, "--session-id sess-abc") {
		t.Fatalf("pi should keep --session-id: %v", piArgs)
	}
	if strings.Contains(piJoined, "--resume") {
		t.Fatalf("pi must not use --resume: %v", piArgs)
	}
}

func TestOmpReadOnlyToolAllowlist(t *testing.T) {
	if got := ompAccessArgs(config.AccessFull); len(got) != 0 {
		t.Fatalf("full args=%v", got)
	}
	args := ompAccessArgs(config.AccessReadOnly)
	if len(args) != 2 || args[0] != "--tools" || args[1] != "read,grep,glob" {
		t.Fatalf("read-only args=%v", args)
	}
	// 明确禁止照搬 pi 的 find/ls（按工具名拆分，避免误匹配 "--tools" 里的 "ls" 子串）
	for _, name := range strings.Split(args[1], ",") {
		if name == "find" || name == "ls" {
			t.Fatalf("omp allowlist must not include pi-only tool %q: %v", name, args)
		}
	}
	// pi 回归
	piGot := strings.Join(piAccessArgs(config.AccessReadOnly), " ")
	if piGot != "--tools read,grep,find,ls" {
		t.Fatalf("pi read-only args=%q", piGot)
	}
}

func TestOmpSessionsDirDefaultsAndEnv(t *testing.T) {
	// 清掉 env，断言默认落在 ~/.omp/agent/sessions 而不是 ~/.pi
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	// t.Setenv to empty still sets the key; os.Getenv returns "". piFamilyConfigDir
	// treats empty as unset via envOverride — good.

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	a := NewOmpAdapter("omp")
	want := filepath.Join(home, ".omp", "agent", "sessions")
	if got := a.sessionsDir(); got != want {
		t.Fatalf("default sessionsDir=%q want %q", got, want)
	}
	if got := NewPiAdapter("pi").sessionsDir(); got != filepath.Join(home, ".pi", "agent", "sessions") {
		t.Fatalf("pi sessionsDir polluted: %q", got)
	}

	override := t.TempDir()
	a.Env = map[string]string{"PI_CODING_AGENT_DIR": override}
	if got := a.sessionsDir(); got != filepath.Join(override, "sessions") {
		t.Fatalf("env override sessionsDir=%q", got)
	}
}

func TestOmpFakeBinaryRunResumeAndEvents(t *testing.T) {
	// 假二进制：记录 argv，并回放本机 omp v17 的真实结束序列
	// message_update → turn_end（忽略）→ agent_end（Done）。
	capture := filepath.Join(t.TempDir(), "argv.txt")
	fake := writeFakeAgent(t, `
echo "$@" > "$OMP_TEST_ARGV"
i=0
while IFS= read -r line; do
  i=$((i+1))
  case "$i" in
    1) echo '{"id":"req-1","type":"response","command":"get_state","success":true,"data":{"sessionId":"omp-s1","model":{"provider":"deepseek","id":"flash","contextWindow":1000}}}' ;;
    2) echo '{"id":"req-2","type":"response","command":"prompt","success":true}'
       echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}'
       sleep 0.05
       echo '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input":1,"output":1,"cost":{"total":0}}}}'
       echo '{"type":"agent_end","isTerminal":true}'
       ;;
  esac
done
`)
	a := NewOmpAdapter(fake)
	a.Env = map[string]string{"OMP_TEST_ARGV": capture}
	defer a.ResetSession("scope")

	run, err := a.Run(RunOptions{
		RunID:     "r1",
		Scope:     "scope",
		Prompt:    "hi",
		Cwd:       t.TempDir(),
		SessionID: "resume-me",
		Access:    config.AccessReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)
	argvBytes, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(argvBytes)
	if !strings.Contains(argv, "--mode rpc") {
		t.Fatalf("argv missing --mode rpc: %s", argv)
	}
	if !strings.Contains(argv, "--resume resume-me") {
		t.Fatalf("argv missing --resume: %s", argv)
	}
	if strings.Contains(argv, "--session-id") {
		t.Fatalf("argv must not contain --session-id: %s", argv)
	}
	if !strings.Contains(argv, "--tools read,grep,glob") {
		t.Fatalf("argv missing omp read-only tools: %s", argv)
	}

	var sawSystem, sawText, sawDone bool
	for _, e := range events {
		switch e.Type {
		case EventSystem:
			sawSystem = e.SessionID == "omp-s1" && e.Model == "deepseek/flash"
		case EventText:
			sawText = e.Delta == "ok"
		case EventDone:
			sawDone = e.TerminationReason == TermNormal
		}
	}
	if !sawSystem || !sawText || !sawDone {
		t.Fatalf("events incomplete system=%v text=%v done=%v all=%+v", sawSystem, sawText, sawDone, events)
	}
}

// TestOmpMultiTurnKeepsStreamingAfterMidTurnEnd 覆盖多工具循环：
// tool → turn_end（不得结束 Run）→ 后续 text → agent_end（唯一 Done）。
func TestOmpMultiTurnKeepsStreamingAfterMidTurnEnd(t *testing.T) {
	fake := writeFakeAgent(t, `
i=0
while IFS= read -r line; do
  i=$((i+1))
  case "$i" in
    1) echo '{"id":"req-1","type":"response","command":"get_state","success":true,"data":{"sessionId":"omp-mt","model":{"provider":"deepseek","id":"flash"}}}' ;;
    2) echo '{"id":"req-2","type":"response","command":"prompt","success":true}'
       # 第一轮：工具调用后 mid turn_end（真实 omp 多步循环会出现）
       echo '{"type":"tool_execution_start","toolCallId":"t1","toolName":"read","args":{"path":"a.go"}}'
       echo '{"type":"tool_execution_end","toolCallId":"t1","isError":false,"result":{"content":[{"type":"text","text":"file"}]}}'
       echo '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1"}]}}'
       sleep 0.05
       # 第二轮：继续输出最终文本，再 agent_end
       echo '{"type":"turn_start"}'
       echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"part-a-"}}'
       echo '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"part-b"}}'
       sleep 0.05
       echo '{"type":"agent_end","isTerminal":true}'
       ;;
  esac
done
`)
	a := NewOmpAdapter(fake)
	defer a.ResetSession("mt")
	run, err := a.Run(RunOptions{RunID: "mt", Scope: "mt", Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)

	var text string
	var tools, dones int
	var lastDone Event
	for _, e := range events {
		switch e.Type {
		case EventText:
			text += e.Delta
		case EventToolUse:
			tools++
		case EventDone:
			dones++
			lastDone = e
		case EventError:
			t.Fatalf("unexpected error: %+v", e)
		}
	}
	if tools != 1 {
		t.Fatalf("tool events=%d events=%+v", tools, events)
	}
	if text != "part-a-part-b" {
		t.Fatalf("text=%q (mid turn_end must not close stream before final deltas)", text)
	}
	if dones != 1 || lastDone.TerminationReason != TermNormal {
		t.Fatalf("done count=%d last=%+v events=%+v", dones, lastDone, events)
	}
}

func TestTranslateOmpTerminalEvents(t *testing.T) {
	// agent_end / agent_settled → Done；turn_end 必须被忽略（多步循环 mid-turn）。
	for _, typ := range []string{"agent_end", "agent_settled"} {
		events := translatePiEvent(map[string]any{"type": typ})
		if len(events) != 1 || events[0].Type != EventDone || events[0].TerminationReason != TermNormal {
			t.Fatalf("%s → %+v", typ, events)
		}
	}
	if events := translatePiEvent(map[string]any{"type": "turn_end"}); len(events) != 0 {
		t.Fatalf("turn_end must not emit Done: %+v", events)
	}
}

func TestOmpListModelsIgnoresStartupNoise(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *get_state*)
      echo '{"type":"ready","protocolVersion":1}'
      echo '{"type":"extension_ui_request","id":"ui","method":"setWidget","widgetKey":"x"}'
      echo '{"id":"state","type":"response","command":"get_state","success":true,"data":{"model":{"provider":"deepseek","id":"flash"}}}'
      ;;
    *get_available_models*)
      echo '{"id":"models","type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"deepseek","id":"flash","name":"Flash","contextWindow":1000}]}}'
      ;;
  esac
done
`)
	a := NewOmpAdapter(fake)
	models, err := a.ListModels(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "deepseek/flash" || !models[0].Default {
		t.Fatalf("models=%+v", models)
	}
}

func TestOmpReadUsageFromOmpTree(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
{"type":"session","version":3,"id":"omp-1","cwd":"/tmp"}
{"type":"message","message":{"role":"assistant","usage":{"input":5,"output":2,"cacheRead":1,"cacheWrite":0,"cost":{"total":0.3}}}}
`
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewOmpAdapter("omp")
	a.Env = map[string]string{"PI_CODING_AGENT_SESSION_DIR": root}
	snap, err := a.ReadUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Provider != "Oh My Pi（本机会话）" {
		t.Fatalf("provider=%q", snap.Provider)
	}
	if snap.Activity == nil || snap.Activity.Sessions != 1 || snap.Activity.InputTokens != 5 {
		t.Fatalf("activity=%+v", snap.Activity)
	}
	if math.Abs(snap.Activity.CostUSD-0.3) > 1e-9 {
		t.Fatalf("cost=%f", snap.Activity.CostUSD)
	}
}

func TestScanOmpSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", home)
	// omp 与 pi 共用 env 名，但 discovery 按 kind 默认路径；这里用 env 指向临时树。
	writeLines(t, filepath.Join(home, "sessions", "--tmp--", "2026-01-01_omp.jsonl"),
		`{"type":"session","version":3,"id":"omp-sess-1","cwd":"/tmp/work"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"omp 会话"}]}}`,
	)
	// 注意：scanOmpSessions 在 env 设置时用 ompSessionsDir → 同一 PI_CODING_AGENT_DIR。
	sessions, err := scanOmpSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	s := sessions[0]
	if s.ID != "omp-sess-1" || s.Cwd != "/tmp/work" || s.Preview != "omp 会话" {
		t.Errorf("session=%+v", s)
	}
	if s.Agent != config.AgentOmp {
		t.Errorf("agent=%v", s.Agent)
	}
	// ListSessions 分发
	viaList, err := ListSessions(config.AgentOmp, 10)
	if err != nil || len(viaList) != 1 {
		t.Fatalf("ListSessions omp: %v len=%d", err, len(viaList))
	}
}

func TestOmpAskSourceTag(t *testing.T) {
	// 单元路径：session.source=omp 时 UI 请求 Source 为 omp（与全进程假二进制路径互补）。
	ps := &piSession{source: "omp", label: "Oh My Pi"}
	events := ps.translatePiUIRequest(map[string]any{
		"type":    "extension_ui_request",
		"id":      "ask-1",
		"method":  "select",
		"title":   "选一个",
		"options": []any{"A", "B"},
	})
	if len(events) != 1 || events[0].Type != EventAskUser {
		t.Fatalf("events=%+v", events)
	}
	if events[0].Source != "omp" {
		t.Fatalf("Source=%q want omp", events[0].Source)
	}
	if events[0].Reply == nil {
		t.Fatal("Reply nil")
	}
}
