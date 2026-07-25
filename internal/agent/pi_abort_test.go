package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPiAbortEscalation drives a fake pi that answers get_state and prompt
// but wedges afterwards (never answers abort, never emits events). Stop()
// must escalate to killing the process within abortGrace instead of
// hanging the run forever.
func TestPiAbortEscalation(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-pi")
	script := `#!/bin/sh
i=0
while IFS= read -r line; do
  i=$((i+1))
  case "$i" in
    1) echo '{"id":"req-1","type":"response","command":"get_state","success":true,"data":{"sessionId":"s1"}}' ;;
    2) echo '{"id":"req-2","type":"response","command":"prompt","success":true}' ;;
  esac
  # 后续命令（abort）一律不应答，模拟卡死
done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := NewPiAdapter(fake)
	run, err := a.Run(RunOptions{RunID: "r1", Scope: "s1", Prompt: "hi", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	run.Stop()

	var events []Event
	for evt := range run.Events() {
		events = append(events, evt)
	}
	elapsed := time.Since(start)

	if elapsed > abortGrace+7*time.Second {
		t.Fatalf("run did not close promptly after escalation: %s", elapsed)
	}
	last := events[len(events)-1]
	if last.Type != EventError || last.TerminationReason != TermFailed {
		t.Fatalf("last event = %+v, want process-death error", last)
	}

	// 进程已被杀：adapter 会在下一次 Run 时重拉
	if !a.sessionsEmptyOrDead("s1") {
		t.Error("killed pi session should be marked dead")
	}
}

// sessionsEmptyOrDead reports whether the scope's session is gone or dead.
func (a *PiAdapter) sessionsEmptyOrDead(scope string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := a.sessions[scope]
	return ps == nil || !ps.alive()
}
