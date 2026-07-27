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
	b.Set(key, Binding{Scope: "oc_1", ChatID: "oc_1", ChatType: "group"})
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}

	// reload: dedup data survives restart
	b2, err := LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	bind, ok := b2.Get(key)
	if !ok || bind.Scope != "oc_1" || bind.ChatType != "group" {
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

func TestBindingSetIfAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bindings.json")
	b, err := LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	key := SessionKey(config.AgentOpenCode, "sess-oc")

	if !b.SetIfAbsent(key, Binding{Scope: "g1", ChatID: "g1", ChatType: "group"}) {
		t.Fatal("first SetIfAbsent should succeed")
	}
	// same scope: refresh ok
	if !b.SetIfAbsent(key, Binding{Scope: "g1", ChatID: "g1", ChatType: "group"}) {
		t.Fatal("same-scope SetIfAbsent should succeed")
	}
	// different scope: refuse to steal
	if b.SetIfAbsent(key, Binding{Scope: "g2", ChatID: "g2", ChatType: "group"}) {
		t.Fatal("SetIfAbsent must not overwrite another scope")
	}
	got, ok := b.Get(key)
	if !ok || got.Scope != "g1" {
		t.Fatalf("binding = %+v", got)
	}

	sk, byScope, ok := b.GetByScope("g1")
	if !ok || sk != key || byScope.ChatID != "g1" {
		t.Fatalf("GetByScope = %q %+v ok=%v", sk, byScope, ok)
	}
}

func TestSessionFindByAgentID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := LoadSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("oc_group", Session{SessionID: "sid-abc", Cwd: "/tmp/p"})
	s.Set("oc_other", Session{ThreadID: "th-xyz"})

	scope, sess, ok := s.FindByAgentID("sid-abc")
	if !ok || scope != "oc_group" || sess.Cwd != "/tmp/p" {
		t.Fatalf("FindByAgentID session = %q %+v ok=%v", scope, sess, ok)
	}
	scope, _, ok = s.FindByAgentID("th-xyz")
	if !ok || scope != "oc_other" {
		t.Fatalf("FindByAgentID thread = %q ok=%v", scope, ok)
	}
	if _, _, ok := s.FindByAgentID("missing"); ok {
		t.Fatal("missing id should not match")
	}
}
