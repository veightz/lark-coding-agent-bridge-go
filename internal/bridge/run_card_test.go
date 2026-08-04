package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/card"
)

type fakeRunCardStream struct {
	messageID string
	cardID    string
	startErr  error
	started   []map[string]any
	updates   []map[string]any
	finished  []string
}

func (s *fakeRunCardStream) Start(_ context.Context, initial map[string]any) error {
	if s.startErr != nil {
		return s.startErr
	}
	s.started = append(s.started, initial)
	return nil
}

func (s *fakeRunCardStream) MessageID() string { return s.messageID }
func (s *fakeRunCardStream) CardID() string    { return s.cardID }
func (s *fakeRunCardStream) Update(next map[string]any) {
	s.updates = append(s.updates, next)
}
func (s *fakeRunCardStream) Finish(summary string) {
	s.finished = append(s.finished, summary)
}

func TestRotateRunCardMovesLiveRoutingBelowAsk(t *testing.T) {
	oldStream := &fakeRunCardStream{messageID: "om_old", cardID: "card_old"}
	newStream := &fakeRunCardStream{messageID: "om_new", cardID: "card_new"}
	run := &stopTestRun{stopped: make(chan struct{})}
	runState := card.InitialState().Reduce(agent.Event{Type: agent.EventText, Delta: "交互前输出"})
	b := &Bridge{
		runs: map[string]*activeRun{
			"scope": {run: run, scope: "scope", runState: runState, stream: oldStream},
		},
		cardScopes:      map[string]string{"om_old": "scope"},
		activeCardsPath: filepath.Join(t.TempDir(), "active-cards.json"),
		newRunCardStream: func(chatID, replyTo string, inThread bool) runCardStream {
			if chatID != "oc_chat" || replyTo != "om_root" || !inThread {
				t.Fatalf("factory route = %q %q %v", chatID, replyTo, inThread)
			}
			return newStream
		},
	}

	got, err := b.rotateRunCard("scope", "oc_chat", "om_root", true, false, true, oldStream, runState)
	if err != nil {
		t.Fatal(err)
	}
	if got != newStream || len(newStream.started) != 1 {
		t.Fatalf("new stream not started: got=%T starts=%d", got, len(newStream.started))
	}
	if newStream.started[0]["config"].(map[string]any)["streaming_mode"] != true {
		t.Fatal("continuation card must start in streaming mode")
	}
	if len(oldStream.updates) != 1 || len(oldStream.finished) != 1 {
		t.Fatalf("old stream lifecycle: updates=%d finishes=%d", len(oldStream.updates), len(oldStream.finished))
	}
	if oldStream.updates[0]["config"].(map[string]any)["streaming_mode"] != false {
		t.Fatal("old stream checkpoint must stop streaming")
	}
	if oldStream.finished[0] != "已在下方新卡片继续" {
		t.Fatalf("old stream summary = %q", oldStream.finished[0])
	}
	if _, ok := b.cardScopes["om_old"]; ok {
		t.Fatal("old stop-button route still active")
	}
	if b.cardScopes["om_new"] != "scope" {
		t.Fatalf("new stop-button route = %#v", b.cardScopes)
	}
	if b.runs["scope"].stream != newStream || b.runs["scope"].runState != runState {
		t.Fatal("active run did not move to new stream")
	}
	if entry := b.loadActiveCards()["scope"]; entry.CardID != "card_new" || entry.ChatID != "oc_chat" {
		t.Fatalf("active card entry = %+v", entry)
	}
	if runState.Terminal != card.TerminalRunning {
		t.Fatal("rotation must not end the underlying run")
	}
}

func TestRotateRunCardKeepsOldStreamWhenCreationFails(t *testing.T) {
	oldStream := &fakeRunCardStream{messageID: "om_old", cardID: "card_old"}
	failedStream := &fakeRunCardStream{startErr: errors.New("send failed")}
	runState := card.InitialState()
	b := &Bridge{
		runs:       map[string]*activeRun{"scope": {runState: runState, stream: oldStream}},
		cardScopes: map[string]string{"om_old": "scope"},
		newRunCardStream: func(string, string, bool) runCardStream {
			return failedStream
		},
	}

	got, err := b.rotateRunCard("scope", "oc_chat", "om_root", true, false, false, oldStream, runState)
	if err == nil || got != oldStream {
		t.Fatalf("rotation result: got=%T err=%v", got, err)
	}
	if len(oldStream.updates) != 0 || len(oldStream.finished) != 0 {
		t.Fatal("old stream was finalized after replacement creation failed")
	}
	if b.cardScopes["om_old"] != "scope" || b.runs["scope"].stream != oldStream {
		t.Fatal("old routing changed after failed rotation")
	}
}
