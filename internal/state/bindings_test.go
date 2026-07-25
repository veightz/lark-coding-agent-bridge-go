package state

import (
	"path/filepath"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestBindingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	b, err := LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}

	key := SessionKey(config.AgentPi, "sess-1")
	if _, ok := b.Get(key); ok {
		t.Fatal("unexpected binding")
	}
	b.Set(key, Binding{Scope: "oc_1", ChatID: "oc_1"})
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}

	// reload: dedup data survives restart
	b2, err := LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	bind, ok := b2.Get(key)
	if !ok || bind.Scope != "oc_1" {
		t.Fatalf("binding = %+v ok=%v", bind, ok)
	}

	// delete by scope
	removed := b2.DeleteByScope("oc_1")
	if len(removed) != 1 || removed[0] != key {
		t.Fatalf("removed = %v", removed)
	}
	if _, ok := b2.Get(key); ok {
		t.Fatal("binding should be gone")
	}
}
