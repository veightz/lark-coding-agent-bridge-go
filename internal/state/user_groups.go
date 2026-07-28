package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// UserGroupMapping records a user's p2p chat → group chat relationship.
// Once a group is created for a user's p2p task, subsequent p2p messages
// from the same user are forwarded to this group.
type UserGroupMapping struct {
	P2pChatID   string    `json:"p2pChatId"`
	GroupChatID string    `json:"groupChatId"`
	CreatedAt   time.Time `json:"createdAt"`
}

// UserGroupStore persists the p2pChatID → groupChatID mapping across restarts.
type UserGroupStore struct {
	mu       sync.Mutex
	path     string
	Mappings []UserGroupMapping `json:"mappings"`
	dirty    bool
	byUser   map[string]string // p2pChatID → groupChatID (in-memory index)
}

func LoadUserGroups(path string) (*UserGroupStore, error) {
	s := &UserGroupStore{path: path, byUser: map[string]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if s.Mappings == nil {
		s.Mappings = nil
	}
	for _, m := range s.Mappings {
		s.byUser[m.P2pChatID] = m.GroupChatID
	}
	return s, nil
}

// Get returns the group chat ID for a user's p2p chat, if one is mapped.
func (s *UserGroupStore) Get(p2pChatID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gid, ok := s.byUser[p2pChatID]
	return gid, ok
}

// Set maps a user's p2p chat to a group chat. If the user already has a
// mapping, it is overwritten.
func (s *UserGroupStore) Set(p2pChatID, groupChatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byUser[p2pChatID]; ok && existing == groupChatID {
		return
	}
	s.byUser[p2pChatID] = groupChatID
	// Rebuild the slice from the map (small dataset: one entry per user).
	s.rebuild()
	s.dirty = true
}

// Delete removes the mapping for a user's p2p chat.
func (s *UserGroupStore) Delete(p2pChatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byUser[p2pChatID]; !ok {
		return
	}
	delete(s.byUser, p2pChatID)
	s.rebuild()
	s.dirty = true
}

// DeleteByGroup removes any mapping pointing to the given group chat ID.
// Used when a group is destroyed or the session is reset.
func (s *UserGroupStore) DeleteByGroup(groupChatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for p2p, gid := range s.byUser {
		if gid == groupChatID {
			delete(s.byUser, p2p)
			changed = true
		}
	}
	if changed {
		s.rebuild()
		s.dirty = true
	}
}

func (s *UserGroupStore) rebuild() {
	s.Mappings = make([]UserGroupMapping, 0, len(s.byUser))
	for p2p, gid := range s.byUser {
		s.Mappings = append(s.Mappings, UserGroupMapping{
			P2pChatID:   p2p,
			GroupChatID: gid,
			CreatedAt:   time.Now(),
		})
	}
}

func (s *UserGroupStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	if err := os.MkdirAll(config.NewPaths().ProfileDir(""), 0o755); err != nil {
		return err
	}
	if err := config.WriteJSONAtomic(s.path, s); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
