package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

var (
	_ ModelProvider = (*PiAdapter)(nil)
	_ UsageProvider = (*PiAdapter)(nil)
)

func TestPiListModelsRPC(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *get_state*)
      echo '{"id":"state","type":"response","command":"get_state","success":true,"data":{"sessionId":"ephemeral","model":{"provider":"openai","id":"gpt-z"}}}'
      ;;
    *get_available_models*)
      echo '{"type":"extension_ui_request","id":"ui-1","method":"setStatus","statusKey":"x"}'
      echo '{"id":"models","type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"anthropic","id":"claude-b","name":"Claude B","reasoning":false,"contextWindow":200000,"input":["text","image"]},{"provider":"openai","id":"gpt-z","name":"GPT Z","reasoning":true,"contextWindow":400000,"input":["text"]}]}}'
      ;;
  esac
done
`)
	a := NewPiAdapter(fake)
	models, err := a.ListModels(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models=%+v", models)
	}
	if models[0].ID != "openai/gpt-z" || !models[0].Default {
		t.Fatalf("default model=%+v", models[0])
	}
	if !strings.Contains(models[0].Description, "400,000") || !strings.Contains(models[0].Description, "支持推理") {
		t.Fatalf("description=%q", models[0].Description)
	}
	if models[1].ID != "anthropic/claude-b" || !strings.Contains(models[1].Description, "支持图片") {
		t.Fatalf("second model=%+v", models[1])
	}
}

func TestPiReusedSessionAppliesModelSwitch(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "rpc.jsonl")
	fake := writeFakeAgent(t, `
i=0
while IFS= read -r line; do
  echo "$line" >> "$PI_TEST_CAPTURE"
  i=$((i+1))
  case "$i" in
    1) echo '{"id":"req-1","type":"response","command":"get_state","success":true,"data":{"sessionId":"s1","model":{"provider":"openai","id":"old"}}}' ;;
    2) echo '{"id":"req-2","type":"response","command":"prompt","success":true}'; sleep 0.1; echo '{"type":"agent_settled"}' ;;
    3) echo '{"id":"req-3","type":"response","command":"set_model","success":true,"data":{"provider":"anthropic","id":"new"}}' ;;
    4) echo '{"id":"req-4","type":"response","command":"prompt","success":true}'; sleep 0.1; echo '{"type":"agent_settled"}' ;;
  esac
done
`)
	a := NewPiAdapter(fake)
	a.Env = map[string]string{"PI_TEST_CAPTURE": capture}
	defer a.ResetSession("scope")
	cwd := t.TempDir()

	first, err := a.Run(RunOptions{RunID: "r1", Scope: "scope", Prompt: "one", Cwd: cwd, Model: "openai/old"})
	if err != nil {
		t.Fatal(err)
	}
	_ = drain(first)
	second, err := a.Run(RunOptions{RunID: "r2", Scope: "scope", Prompt: "two", Cwd: cwd, Model: "anthropic/new"})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(second)
	if len(events) == 0 || events[0].Type != EventSystem || events[0].Model != "anthropic/new" {
		t.Fatalf("events=%+v", events)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	commands := string(data)
	setAt := strings.Index(commands, `"type":"set_model"`)
	promptAt := strings.LastIndex(commands, `"type":"prompt"`)
	if setAt < 0 || promptAt < 0 || setAt > promptAt {
		t.Fatalf("model switch must precede second prompt:\n%s", commands)
	}
	if !strings.Contains(commands, `"provider":"anthropic"`) || !strings.Contains(commands, `"modelId":"new"`) {
		t.Fatalf("wrong set_model payload:\n%s", commands)
	}
}

func TestPiReadOnlyToolAllowlist(t *testing.T) {
	if got := piAccessArgs(config.AccessFull); len(got) != 0 {
		t.Fatalf("full args=%v", got)
	}
	if got := piAccessArgs(config.AccessWorkspace); len(got) != 0 {
		t.Fatalf("workspace args=%v", got)
	}
	got := strings.Join(piAccessArgs(config.AccessReadOnly), " ")
	if got != "--tools read,grep,find,ls" {
		t.Fatalf("read-only args=%q", got)
	}
}

func TestPiReadUsageAggregatesSessionJSONL(t *testing.T) {
	root := t.TempDir()
	projectA := filepath.Join(root, "project-a")
	projectB := filepath.Join(root, "project-b")
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSession(filepath.Join(projectA, "a.jsonl"), `
{"type":"session","version":3,"id":"a","cwd":"/a"}
{"type":"message","message":{"role":"user","content":"hi"}}
{"type":"message","message":{"role":"assistant","usage":{"input":10,"output":2,"cacheRead":3,"cacheWrite":4,"cost":{"total":0.5}}}}
{"type":"message","message":{"role":"toolResult","usage":{"input":1,"output":1,"cacheRead":1,"cacheWrite":0,"cost":{"total":0.1}}}}
{"type":"compaction","usage":{"input":5,"output":2,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.2}}}
`)
	writeSession(filepath.Join(projectB, "b.jsonl"), `
{"type":"session","version":3,"id":"b","cwd":"/b"}
{"type":"message","message":{"role":"assistant","usage":{"input":20,"output":4,"cacheRead":2,"cacheWrite":1,"cost":{"total":1.0}}}}
`)

	a := NewPiAdapter("pi")
	a.Env = map[string]string{"PI_CODING_AGENT_SESSION_DIR": root}
	snapshot, err := a.ReadUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	activity := snapshot.Activity
	if snapshot.Provider != "Pi（本机会话）" || activity == nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if activity.Sessions != 2 || activity.Messages != 4 || activity.InputTokens != 36 || activity.OutputTokens != 9 || activity.CachedInputTokens != 6 || activity.CacheWriteTokens != 5 {
		t.Fatalf("activity=%+v", activity)
	}
	if math.Abs(activity.CostUSD-1.8) > 0.000001 {
		t.Fatalf("cost=%f", activity.CostUSD)
	}
}

func TestTranslatePiMessageEndUsage(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"type":"message_end","message":{"role":"assistant","usage":{"input":12,"output":3,"cacheRead":4,"cacheWrite":1,"cost":{"total":0.25}}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	events := translatePiEvent(raw)
	if len(events) != 1 || events[0].Type != EventUsage || events[0].InputTokens != 12 || events[0].OutputTokens != 3 || events[0].CachedInputTokens != 4 || events[0].CostUSD != 0.25 {
		t.Fatalf("events=%+v", events)
	}
}
