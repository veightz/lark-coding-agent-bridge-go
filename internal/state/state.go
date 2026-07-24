// Package state holds per-profile mutable runtime state persisted as JSON:
// agent sessions (per chat scope) and workspace bindings.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// Session records the agent conversation handle for one chat scope.
type Session struct {
	SessionID string    `json:"sessionId,omitempty"` // claude session id
	ThreadID  string    `json:"threadId,omitempty"`  // codex thread id
	Cwd       string    `json:"cwd,omitempty"`
	Model     string    `json:"model,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SessionStore persists sessions keyed by scope (chat / thread / comment).
type SessionStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]Session
	dirty    bool
}

func LoadSessions(path string) (*SessionStore, error) {
	s := &SessionStore{path: path, sessions: map[string]Session{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.sessions); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return s, nil
}

func (s *SessionStore) Get(scope string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[scope]
	return sess, ok
}

func (s *SessionStore) Set(scope string, sess Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.UpdatedAt = time.Now()
	s.sessions[scope] = sess
	s.dirty = true
}

func (s *SessionStore) Delete(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, scope)
	s.dirty = true
}

// Flush writes pending changes to disk.
func (s *SessionStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := config.WriteJSONAtomic(s.path, s.sessions); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// WorkspaceStore keeps the current working directory and named bindings.
type WorkspaceStore struct {
	mu      sync.Mutex
	path    string
	Current string            `json:"current,omitempty"`
	Named   map[string]string `json:"named,omitempty"`
	dirty   bool
}

func LoadWorkspaces(path string) (*WorkspaceStore, error) {
	w := &WorkspaceStore{path: path, Named: map[string]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, w); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if w.Named == nil {
		w.Named = map[string]string{}
	}
	return w, nil
}

func (w *WorkspaceStore) Get() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Current
}

func (w *WorkspaceStore) SetCurrent(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Current = dir
	w.dirty = true
}

func (w *WorkspaceStore) Save(name, dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Named[name] = dir
	w.dirty = true
}

func (w *WorkspaceStore) Remove(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.Named[name]; !ok {
		return false
	}
	delete(w.Named, name)
	w.dirty = true
	return true
}

func (w *WorkspaceStore) Lookup(name string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dir, ok := w.Named[name]
	return dir, ok
}

// List returns a sorted snapshot of named workspaces.
func (w *WorkspaceStore) List() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]string, len(w.Named))
	for k, v := range w.Named {
		out[k] = v
	}
	return out
}

func (w *WorkspaceStore) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	if err := config.WriteJSONAtomic(w.path, w); err != nil {
		return err
	}
	w.dirty = false
	return nil
}
