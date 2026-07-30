package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// writeFakeAgent creates an executable shell script emitting stream-json.
func writeFakeAgent(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func drain(run Run) []Event {
	var out []Event
	for evt := range run.Events() {
		out = append(out, evt)
	}
	return out
}

func TestClaudeAdapterRun(t *testing.T) {
	fake := writeFakeAgent(t, `
cat >/dev/null  # consume the prompt from stdin
echo '{"type":"system","subtype":"init","session_id":"sess-1","cwd":"/tmp","model":"fake"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}'
echo '{"type":"result","session_id":"sess-1"}'
`)
	a := &ClaudeAdapter{binary: fake}
	run, err := a.Run(RunOptions{RunID: "r1", Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)

	var sawSystem, sawText, sawDone bool
	for _, e := range events {
		switch e.Type {
		case EventSystem:
			sawSystem = e.SessionID == "sess-1"
		case EventText:
			sawText = e.Delta == "hello"
		case EventDone:
			sawDone = e.TerminationReason == TermNormal
		}
	}
	if !sawSystem || !sawText || !sawDone {
		t.Errorf("missing events: system=%v text=%v done=%v; all=%+v", sawSystem, sawText, sawDone, events)
	}
}

func TestCodexAdapterRunMergesRunEnv(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      if [ "$CODEX_RUN_MARKER" != "overlay" ]; then
        echo '{"id":1,"error":{"code":-1,"message":"missing run env"}}'
      else
        echo '{"id":1,"result":{"userAgent":"fake"}}'
      fi
      ;;
    *'"method":"thread/start"'*)
      echo '{"id":2,"result":{"thread":{"id":"thread-1"},"model":"fake-model"}}'
      ;;
    *'"method":"turn/start"'*)
      echo '{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}'
      echo '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"msg-1","delta":"hello"}}'
      echo '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1,"item":{"id":"msg-1","type":"agentMessage","text":"hello","phase":"final_answer","memoryCitation":null}}}'
      echo '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[],"error":null}}}'
      ;;
  esac
done
`)
	a := &CodexAdapter{
		binary: fake,
		Env:    map[string]string{"CODEX_RUN_MARKER": "base"},
	}
	run, err := a.Run(RunOptions{
		RunID:  "codex-env",
		Prompt: "hi",
		Cwd:    t.TempDir(),
		Env:    map[string]string{"CODEX_RUN_MARKER": "overlay"},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)
	_, done, failed := summarize(events)
	sawText := false
	for _, evt := range events {
		sawText = sawText || (evt.Type == EventText && evt.Delta == "hello")
	}
	if failed || !done || !sawText {
		t.Fatalf("events = %+v", events)
	}
}

func TestCodexAdapterBidirectionalAskAndPermission(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"id":1,"result":{}}'
      ;;
    *'"method":"thread/start"'*)
      echo '{"id":2,"result":{"thread":{"id":"thread-ask"},"model":"fake"}}'
      ;;
    *'"method":"turn/start"'*)
      echo '{"id":3,"result":{"turn":{"id":"turn-ask","status":"inProgress"}}}'
      echo '{"method":"item/tool/requestUserInput","id":90,"params":{"threadId":"thread-ask","turnId":"turn-ask","itemId":"ask-1","questions":[{"id":"lang","header":"语言","question":"选择语言","isOther":true,"isSecret":false,"options":[{"label":"Go","description":"Go language"},{"label":"Rust","description":"Rust language"}]}],"autoResolutionMs":null}}'
      ;;
    *'"id":90'*'"Go"'*)
      echo '{"method":"serverRequest/resolved","params":{"threadId":"thread-ask","requestId":90}}'
      echo '{"method":"item/commandExecution/requestApproval","id":91,"params":{"threadId":"thread-ask","turnId":"turn-ask","itemId":"cmd-1","startedAtMs":1,"command":"go test ./...","cwd":"/repo","reason":"运行测试","availableDecisions":["accept","acceptForSession","decline"]}}'
      ;;
    *'"id":91'*'"acceptForSession"'*)
      echo '{"method":"serverRequest/resolved","params":{"threadId":"thread-ask","requestId":91}}'
      echo '{"method":"item/agentMessage/delta","params":{"threadId":"thread-ask","turnId":"turn-ask","itemId":"msg-1","delta":"done"}}'
      echo '{"method":"item/completed","params":{"threadId":"thread-ask","turnId":"turn-ask","completedAtMs":1,"item":{"id":"msg-1","type":"agentMessage","text":"done","phase":"final_answer","memoryCitation":null}}}'
      echo '{"method":"turn/completed","params":{"threadId":"thread-ask","turn":{"id":"turn-ask","status":"completed","items":[],"error":null}}}'
      ;;
    *'"id":90'*)
      echo '{"method":"error","params":{"message":"bad user input reply","willRetry":false}}'
      ;;
    *'"id":91'*)
      echo '{"method":"error","params":{"message":"bad approval reply","willRetry":false}}'
      ;;
  esac
done
`)
	a := &CodexAdapter{binary: fake}
	run, err := a.Run(RunOptions{
		RunID:  "codex-ask",
		Prompt: "ask me",
		Cwd:    t.TempDir(),
		Access: config.AccessWorkspace,
	})
	if err != nil {
		t.Fatal(err)
	}

	var asks []Event
	var done, failed bool
	for evt := range run.Events() {
		switch evt.Type {
		case EventAskUser:
			asks = append(asks, evt)
			switch evt.Source {
			case "codex":
				if err := evt.Reply([][]string{{"Go"}}, false); err != nil {
					t.Fatal(err)
				}
			case "codex-permission":
				if err := evt.Reply([][]string{{"always"}}, false); err != nil {
					t.Fatal(err)
				}
			}
		case EventDone:
			done = true
		case EventError:
			failed = true
		}
	}
	if failed || !done || len(asks) != 2 {
		t.Fatalf("asks=%+v done=%v failed=%v", asks, done, failed)
	}
	if asks[0].Source != "codex" || asks[1].Source != "codex-permission" {
		t.Fatalf("ask sources = %q, %q", asks[0].Source, asks[1].Source)
	}
}

func TestCodexAdapterResumesThread(t *testing.T) {
	fake := writeFakeAgent(t, `
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      echo '{"id":1,"result":{}}'
      ;;
    *'"method":"thread/resume"'*'"threadId":"thread-old"'*)
      echo '{"id":2,"result":{"thread":{"id":"thread-old"},"model":"fake"}}'
      ;;
    *'"method":"thread/resume"'*)
      echo '{"id":2,"error":{"code":-1,"message":"wrong thread id"}}'
      ;;
    *'"method":"turn/start"'*)
      echo '{"id":3,"result":{"turn":{"id":"turn-resume","status":"inProgress"}}}'
      echo '{"method":"turn/completed","params":{"threadId":"thread-old","turn":{"id":"turn-resume","status":"completed","items":[],"error":null}}}'
      ;;
  esac
done
`)
	a := &CodexAdapter{binary: fake}
	run, err := a.Run(RunOptions{
		RunID:    "codex-resume",
		Prompt:   "continue",
		Cwd:      t.TempDir(),
		ThreadID: "thread-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)
	var systemThread, doneThread string
	for _, evt := range events {
		if evt.Type == EventSystem {
			systemThread = evt.ThreadID
		}
		if evt.Type == EventDone {
			doneThread = evt.ThreadID
		}
	}
	if systemThread != "thread-old" || doneThread != "thread-old" {
		t.Fatalf("events = %+v", events)
	}
}

func TestClaudeAdapterExitError(t *testing.T) {
	fake := writeFakeAgent(t, `
echo 'something broke' >&2
exit 3
`)
	a := &ClaudeAdapter{binary: fake}
	run, err := a.Run(RunOptions{RunID: "r2", Prompt: "hi", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	events := drain(run)
	last := events[len(events)-1]
	if last.Type != EventError || last.TerminationReason != TermFailed {
		t.Fatalf("last event = %+v", last)
	}
	if want := "code 3"; !containsStr(last.Message, want) {
		t.Errorf("error message %q missing %q", last.Message, want)
	}
}

func TestClaudeAdapterStop(t *testing.T) {
	fake := writeFakeAgent(t, `
cat >/dev/null
sleep 60
`)
	a := &ClaudeAdapter{binary: fake}
	run, err := a.Run(RunOptions{RunID: "r3", Prompt: "hi", Cwd: t.TempDir(), StopGraceMs: 200})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		run.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5s")
	}
	// events channel must close after stop
	_ = drain(run)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
