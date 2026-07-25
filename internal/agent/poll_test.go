package agent

import (
	"testing"
)

func assistantMsg(completed int64, errName, errMsg, text string) *ocMessageEntry {
	m := &ocMessageEntry{}
	m.Info.Role = "assistant"
	m.Info.ID = "msg_1"
	m.Info.Time.Completed = completed
	if errName != "" {
		m.Info.Error = &struct {
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		}{Name: errName, Data: map[string]any{"message": errMsg}}
	}
	m.Parts = append(m.Parts, struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Text string `json:"text"`
	}{ID: "prt_1", Type: "text", Text: text})
	return m
}

func TestReconcilePoll(t *testing.T) {
	cases := []struct {
		name         string
		delivered    string
		msg          *ocMessageEntry
		wantTerminal bool
		wantDelta    string
		wantType     EventType
		wantReason   TerminationReason
	}{
		{
			name:         "still running",
			delivered:    "hel",
			msg:          assistantMsg(0, "", "", "hello"),
			wantTerminal: false,
		},
		{
			name:         "completed, SSE delivered everything",
			delivered:    "hello",
			msg:          assistantMsg(123, "", "", "hello"),
			wantTerminal: true,
			wantDelta:    "",
			wantType:     EventDone,
			wantReason:   TermNormal,
		},
		{
			name:         "completed, SSE lost tail",
			delivered:    "hel",
			msg:          assistantMsg(123, "", "", "hello world"),
			wantTerminal: true,
			wantDelta:    "lo world",
			wantType:     EventDone,
			wantReason:   TermNormal,
		},
		{
			name:         "completed, SSE lost everything",
			delivered:    "",
			msg:          assistantMsg(123, "", "", "full reply"),
			wantTerminal: true,
			wantDelta:    "full reply",
			wantType:     EventDone,
			wantReason:   TermNormal,
		},
		{
			name:         "aborted",
			delivered:    "hel",
			msg:          assistantMsg(0, "MessageAbortedError", "Aborted", "hel"),
			wantTerminal: true,
			wantDelta:    "",
			wantType:     EventDone,
			wantReason:   TermInterrupted,
		},
		{
			name:         "real error",
			delivered:    "",
			msg:          assistantMsg(0, "ProviderError", "rate limited", ""),
			wantTerminal: true,
			wantDelta:    "",
			wantType:     EventError,
			wantReason:   TermFailed,
		},
		{
			name:         "content mismatch, no crash",
			delivered:    "xyz",
			msg:          assistantMsg(123, "", "", "abc"),
			wantTerminal: true,
			wantDelta:    "",
			wantType:     EventDone,
			wantReason:   TermNormal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, terminal := reconcilePoll(tc.delivered, tc.msg)
			if terminal != tc.wantTerminal {
				t.Fatalf("terminal = %v, want %v", terminal, tc.wantTerminal)
			}
			if !terminal {
				if len(events) != 0 {
					t.Fatalf("non-terminal should emit nothing: %+v", events)
				}
				return
			}
			var delta string
			last := events[len(events)-1]
			for _, e := range events {
				if e.Type == EventText {
					delta += e.Delta
				}
			}
			if delta != tc.wantDelta {
				t.Errorf("delta = %q, want %q", delta, tc.wantDelta)
			}
			if last.Type != tc.wantType {
				t.Errorf("terminal type = %v, want %v", last.Type, tc.wantType)
			}
			if last.TerminationReason != tc.wantReason {
				t.Errorf("reason = %v, want %v", last.TerminationReason, tc.wantReason)
			}
		})
	}
}

func TestLastAssistant(t *testing.T) {
	var msgs []ocMessageEntry
	user := ocMessageEntry{}
	user.Info.Role = "user"
	asst := ocMessageEntry{}
	asst.Info.Role = "assistant"
	asst.Info.ID = "a1"
	msgs = append(msgs, user, asst, user)
	got := lastAssistant(msgs)
	if got == nil || got.Info.ID != "a1" {
		t.Fatalf("lastAssistant = %+v", got)
	}
	if lastAssistant(nil) != nil {
		t.Error("nil input should give nil")
	}
}
