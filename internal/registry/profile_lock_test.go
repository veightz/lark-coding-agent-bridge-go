package registry

import (
	"errors"
	"os"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestAcquireProfileLockRejectsSameProfile(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	first, err := AcquireProfileLock(paths, "codex-dev")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	_, err = AcquireProfileLock(paths, "codex-dev")
	var locked *ProfileLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("second acquire error = %v, want ProfileLockedError", err)
	}
	if locked.Profile != "codex-dev" || locked.PID != os.Getpid() {
		t.Fatalf("locked = %+v, want profile codex-dev pid %d", locked, os.Getpid())
	}
}

func TestAcquireProfileLockAllowsDifferentProfilesAndReuse(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	first, err := AcquireProfileLock(paths, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireProfileLock(paths, "two")
	if err != nil {
		first.Release()
		t.Fatal(err)
	}
	second.Release()
	first.Release()
	first.Release() // idempotent

	reused, err := AcquireProfileLock(paths, "one")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	reused.Release()
}
