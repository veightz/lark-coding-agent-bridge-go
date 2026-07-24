package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
