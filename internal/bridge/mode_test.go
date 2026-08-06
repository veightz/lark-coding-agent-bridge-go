package bridge

import (
	"path/filepath"
	"testing"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/state"
)

type modeTestAdapter struct{}

func (modeTestAdapter) ID() string                              { return "mode-test" }
func (modeTestAdapter) DisplayName() string                     { return "Mode Test" }
func (modeTestAdapter) SetBotIdentity(agent.BotIdentity)        {}
func (modeTestAdapter) Run(agent.RunOptions) (agent.Run, error) { return nil, nil }
func (modeTestAdapter) CollaborationModes() []agent.CollaborationModeInfo {
	return []agent.CollaborationModeInfo{
		{ID: agent.CollaborationModePlan, DisplayName: "Plan"},
		{ID: agent.CollaborationModeDefault, DisplayName: "Default"},
	}
}

func TestSetCollaborationModePersistsPerScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sessions, err := state.LoadSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := modeTestAdapter{}
	b := &Bridge{
		Agent:    provider,
		Sessions: sessions,
		runs:     map[string]*activeRun{},
	}

	if got := b.currentCollaborationMode("chat:topic"); got != agent.CollaborationModeDefault {
		t.Fatalf("initial mode = %q", got)
	}
	if err := b.setCollaborationMode("chat:topic", provider, agent.CollaborationModePlan); err != nil {
		t.Fatal(err)
	}

	reloaded, err := state.LoadSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := reloaded.Get("chat:topic")
	if !ok || sess.CollaborationMode != "plan" {
		t.Fatalf("session = %+v, ok=%v", sess, ok)
	}
	if _, ok := reloaded.Get("chat:other"); ok {
		t.Fatal("mode leaked into another scope")
	}

	if err := b.setCollaborationMode("chat:topic", provider, agent.CollaborationMode("unknown")); err == nil {
		t.Fatal("unknown mode should be rejected")
	}
	if got := b.currentCollaborationMode("chat:topic"); got != agent.CollaborationModePlan {
		t.Fatalf("mode after rejected update = %q", got)
	}
}
