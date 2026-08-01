package bridge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/state"
)

func TestHandleModelSelectPersistsAdvertisedChoice(t *testing.T) {
	sessions, err := state.LoadSessions(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{
		Sessions:     sessions,
		runs:         map[string]*activeRun{},
		modelPickers: map[string]modelPicker{},
	}
	b.registerModelPicker("nonce", "oc_chat:om_root", []agent.ModelInfo{{
		ID:          "gpt-5.6-pro",
		DisplayName: "GPT-5.6 Pro",
	}})

	got := b.handleModelSelect("oc_chat", map[string]any{
		"nonce": "nonce",
		"model": "gpt-5.6-pro",
	})
	if got.ToastKind != "success" || !strings.Contains(got.Toast, "GPT-5.6 Pro") || got.Card == nil {
		t.Fatalf("result = %+v", got)
	}
	sess, ok := sessions.Get("oc_chat:om_root")
	if !ok || sess.Model != "gpt-5.6-pro" {
		t.Fatalf("session = %+v, ok=%v", sess, ok)
	}

	reused := b.handleModelSelect("oc_chat", map[string]any{
		"nonce": "nonce",
		"model": "gpt-5.6-pro",
	})
	if reused.ToastKind != "warning" {
		t.Fatalf("reused result = %+v", reused)
	}
}

func TestHandleModelSelectRejectsUnadvertisedOrExpiredChoice(t *testing.T) {
	sessions, err := state.LoadSessions(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{
		Sessions: sessions,
		modelPickers: map[string]modelPicker{
			"valid": {
				Scope:     "oc_chat",
				Models:    map[string]agent.ModelInfo{"sol": {ID: "sol"}},
				ExpiresAt: time.Now().Add(time.Minute),
			},
			"expired": {
				Scope:     "oc_chat",
				Models:    map[string]agent.ModelInfo{"sol": {ID: "sol"}},
				ExpiresAt: time.Now().Add(-time.Minute),
			},
		},
	}

	unadvertised := b.handleModelSelect("oc_chat", map[string]any{
		"nonce": "valid",
		"model": "arbitrary-model",
	})
	if unadvertised.ToastKind != "error" {
		t.Fatalf("unadvertised result = %+v", unadvertised)
	}
	expired := b.handleModelSelect("oc_chat", map[string]any{
		"nonce": "expired",
		"model": "sol",
	})
	if expired.ToastKind != "warning" {
		t.Fatalf("expired result = %+v", expired)
	}
}

func TestBuildModelPickerCardMarksCurrentModel(t *testing.T) {
	card := buildModelPickerCard([]agent.ModelInfo{
		{ID: "sol", DisplayName: "SOL", Default: true},
		{ID: "pro", DisplayName: "Pro"},
	}, "pro", "oc_chat", "nonce", "", "Codex CLI")
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatalf("body = %#v", card["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		t.Fatalf("elements = %#v", body["elements"])
	}
	foundCurrent := false
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		text, _ := element["text"].(map[string]any)
		if text["content"] == "Pro · 当前" && element["type"] == "primary" {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("current model button missing: %#v", elements)
	}
}
