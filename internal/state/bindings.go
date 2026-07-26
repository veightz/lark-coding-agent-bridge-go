package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// Binding links one agent session (sessionKey = "<agent>:<sessionId>")
// to one chat scope. It is the dedup source of truth: a session must
// never be bound to two chats at once (ADR-0005).
type Binding struct {
	Scope   string    `json:"scope"`
	ChatID  string    `json:"chatId"`
	BoundAt time.Time `json:"boundAt"`
}

// BindingStore persists bindings per profile.
type BindingStore struct {
	mu        sync.Mutex
	path      string
	BySession map[string]Binding `json:"bySession"`
	dirty     bool
}

func LoadBindings(path string) (*BindingStore, error) {
	b := &BindingStore{path: path, BySession: map[string]Binding{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if b.BySession == nil {
		b.BySession = map[string]Binding{}
	}
	return b, nil
}

// SessionKey builds the binding key for an agent session.
func SessionKey(kind config.AgentKind, sessionID string) string {
	return string(kind) + ":" + sessionID
}

// HasScope reports whether any binding points at the given scope.
func (b *BindingStore) HasScope(scope string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, bind := range b.BySession {
		if bind.Scope == scope {
			return true
		}
	}
	return false
}

func (b *BindingStore) Get(sessionKey string) (Binding, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bind, ok := b.BySession[sessionKey]
	return bind, ok
}

func (b *BindingStore) Set(sessionKey string, bind Binding) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bind.BoundAt = time.Now()
	b.BySession[sessionKey] = bind
	b.dirty = true
}

// DeleteByScope removes every binding pointing at scope; returns the
// session keys it removed.
func (b *BindingStore) DeleteByScope(scope string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var removed []string
	for k, bind := range b.BySession {
		if bind.Scope == scope {
			delete(b.BySession, k)
			removed = append(removed, k)
			b.dirty = true
		}
	}
	return removed
}

func (b *BindingStore) Delete(sessionKey string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.BySession, sessionKey)
	b.dirty = true
}

func (b *BindingStore) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirty {
		return nil
	}
	if err := config.WriteJSONAtomic(b.path, b); err != nil {
		return err
	}
	b.dirty = false
	return nil
}
