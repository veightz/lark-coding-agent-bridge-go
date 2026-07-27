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
// never be bound to two chats at once (ADR-0005 / ADR-0007).
type Binding struct {
	Scope    string    `json:"scope"`
	ChatID   string    `json:"chatId"`
	ChatType string    `json:"chatType,omitempty"` // p2p | group（用于 /open 判断能否复用群）
	BoundAt  time.Time `json:"boundAt"`
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

// GetByScope returns the first binding pointing at scope (if any).
func (b *BindingStore) GetByScope(scope string) (sessionKey string, bind Binding, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, v := range b.BySession {
		if v.Scope == scope {
			return k, v, true
		}
	}
	return "", Binding{}, false
}

func (b *BindingStore) Set(sessionKey string, bind Binding) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bind.BoundAt = time.Now()
	b.BySession[sessionKey] = bind
	b.dirty = true
}

// SetIfAbsent writes the binding only when the sessionKey is free or
// already points at the same scope. Returns true if the store was updated
// (or already matched). Used for auto-recording group↔session links
// without stealing an explicit binding to another chat.
func (b *BindingStore) SetIfAbsent(sessionKey string, bind Binding) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if existing, ok := b.BySession[sessionKey]; ok {
		if existing.Scope == bind.Scope && existing.ChatID == bind.ChatID {
			// Refresh chatType / boundAt if the same link is reaffirmed.
			if bind.ChatType != "" && existing.ChatType != bind.ChatType {
				existing.ChatType = bind.ChatType
				existing.BoundAt = time.Now()
				b.BySession[sessionKey] = existing
				b.dirty = true
			}
			return true
		}
		return false
	}
	bind.BoundAt = time.Now()
	b.BySession[sessionKey] = bind
	b.dirty = true
	return true
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
