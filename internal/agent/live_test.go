package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// Live tests drive the real CLIs; they need the binaries installed and
// logged in. Run with: LARK_LIVE_TEST=1 go test ./internal/agent -run Live -v
func liveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("LARK_LIVE_TEST") == "" {
		t.Skip("set LARK_LIVE_TEST=1 to run live agent tests")
	}
}

func drainWithTimeout(t *testing.T, run Run, timeout time.Duration) []Event {
	t.Helper()
	var out []Event
	timer := time.AfterFunc(timeout, func() { run.Stop() })
	defer timer.Stop()
	for evt := range run.Events() {
		out = append(out, evt)
	}
	return out
}

func summarize(events []Event) (text string, done, failed bool) {
	for _, e := range events {
		if e.Type == EventText || e.Type == EventFinalText {
			text += e.Delta + e.Content
		}
		if e.Type == EventDone {
			done = true
		}
		if e.Type == EventError {
			failed = true
		}
	}
	return
}

func TestLivePi(t *testing.T) {
	liveEnabled(t)
	a := NewPiAdapter("pi")
	run, err := a.Run(RunOptions{
		RunID: "live-pi", Scope: "live-pi-scope",
		Prompt: "回复两个字: 你好。不要调用任何工具。",
		Cwd:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drainWithTimeout(t, run, 120*time.Second)
	text, done, failed := summarize(events)
	t.Logf("events=%d done=%v failed=%v text=%q", len(events), done, failed, text)
	if failed {
		for _, e := range events {
			if e.Type == EventError {
				t.Fatalf("run failed: %s", e.Message)
			}
		}
	}
	if !done {
		t.Fatal("no done event")
	}
	if text == "" {
		t.Fatal("no text output")
	}
}

func TestLiveOpenCode(t *testing.T) {
	liveEnabled(t)
	a := NewOpenCodeAdapter("opencode")
	run, err := a.Run(RunOptions{
		RunID: "live-oc", Scope: "live-oc-scope",
		Prompt: "Reply with exactly: hello-bridge. Do not use any tools.",
		Cwd:    t.TempDir(),
		Model:  "opencode/ling-3.0-flash-free",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drainWithTimeout(t, run, 120*time.Second)
	text, done, failed := summarize(events)
	t.Logf("events=%d done=%v failed=%v text=%q", len(events), done, failed, text)
	if failed {
		for _, e := range events {
			if e.Type == EventError {
				t.Fatalf("run failed: %s", e.Message)
			}
		}
	}
	if !done {
		t.Fatal("no done event")
	}
	if text == "" {
		t.Fatal("no text output")
	}
}

func TestLiveOpenCodeCapabilities(t *testing.T) {
	liveEnabled(t)
	a := NewOpenCodeAdapter("opencode")
	a.SetDefaultAccess(config.AccessReadOnly)
	defer func() {
		a.mu.Lock()
		srv := a.server
		a.mu.Unlock()
		if srv != nil {
			srv.shutdown()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	models, err := a.ListModels(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := a.ReadUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := a.ListSessions(20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("models=%d sessions=%d activity=%+v", len(models), len(sessions), usage.Activity)
	if len(models) == 0 || usage.Activity == nil {
		t.Fatal("OpenCode capability response is empty")
	}
}

func TestLiveCodex(t *testing.T) {
	liveEnabled(t)
	a := &CodexAdapter{binary: "codex"}
	run, err := a.Run(RunOptions{
		RunID:  "live-codex",
		Prompt: "Reply with exactly: hello-bridge. Do not use any tools.",
		Cwd:    t.TempDir(),
		Access: config.AccessReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := drainWithTimeout(t, run, 120*time.Second)
	text, done, failed := summarize(events)
	t.Logf("events=%d done=%v failed=%v text=%q", len(events), done, failed, text)
	if failed {
		for _, e := range events {
			if e.Type == EventError {
				t.Fatalf("run failed: %s", e.Message)
			}
		}
	}
	if !done {
		t.Fatal("no done event")
	}
	if text == "" {
		t.Fatal("no text output")
	}
}

func TestLiveCodexUsage(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	usage, err := (&CodexAdapter{binary: "codex"}).ReadUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan=%q limits=%+v resetCredits=%v tokens=%+v",
		usage.Plan, usage.Limits, usage.ResetCredits, usage.TokenSummary)
	if len(usage.Limits) == 0 {
		t.Fatal("no rate limits returned")
	}
}
