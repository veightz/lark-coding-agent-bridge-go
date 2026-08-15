package agent

import (
	"encoding/json"
	"testing"
)

func TestParseGrokAskParams(t *testing.T) {
	raw := json.RawMessage(`{
		"sessionId":"s1",
		"toolCallId":"call-1",
		"questions":[{
			"question":"Pick a color?",
			"options":[{"label":"red","description":"R"},{"label":"blue","description":"B"}],
			"multiSelect":false
		}],
		"mode":"default"
	}`)
	id, qs := parseGrokAskParams(raw)
	if id != "call-1" {
		t.Fatalf("toolCallId=%s", id)
	}
	if len(qs) != 1 || qs[0].Prompt != "Pick a color?" {
		t.Fatalf("qs=%+v", qs)
	}
	if qs[0].MultiSelect || len(qs[0].Options) != 2 {
		t.Fatalf("opts=%+v multi=%v", qs[0].Options, qs[0].MultiSelect)
	}
	if qs[0].Options[0].Key != "red" || qs[0].Options[0].Label != "red" {
		t.Fatalf("option key/label: %+v", qs[0].Options[0])
	}
}

func TestParseGrokAskParamsMulti(t *testing.T) {
	raw := json.RawMessage(`{
		"toolCallId":"c2",
		"questions":[{
			"question":"Pick features",
			"multiSelect":true,
			"options":[{"label":"a"},{"label":"b"}]
		}]
	}`)
	_, qs := parseGrokAskParams(raw)
	if len(qs) != 1 || !qs[0].MultiSelect {
		t.Fatalf("qs=%+v", qs)
	}
}

func TestFormatGrokAskAnswer(t *testing.T) {
	qs := []AskQuestion{
		{Prompt: "Pick a color?", Options: []AskOption{{Key: "red", Label: "red"}, {Key: "blue", Label: "blue"}}},
	}
	got := formatGrokAskAnswer(qs, [][]string{{"red"}}, false)
	if got["outcome"] != "accepted" {
		t.Fatalf("outcome=%v", got["outcome"])
	}
	ans := got["answers"].(map[string]any)
	if ans["Pick a color?"] != "red" {
		t.Fatalf("answers=%v", ans)
	}

	cancelled := formatGrokAskAnswer(qs, nil, true)
	if cancelled["outcome"] != "cancelled" {
		t.Fatalf("cancelled=%v", cancelled)
	}

	multi := []AskQuestion{{
		Prompt: "Pick features", MultiSelect: true,
		Options: []AskOption{{Key: "a", Label: "a"}, {Key: "b", Label: "b"}},
	}}
	gotM := formatGrokAskAnswer(multi, [][]string{{"a", "b"}}, false)
	ansM := gotM["answers"].(map[string]any)
	vals, ok := ansM["Pick features"].([]string)
	if !ok || len(vals) != 2 || vals[0] != "a" {
		t.Fatalf("multi answers=%T %v", ansM["Pick features"], ansM["Pick features"])
	}
}

func TestGrokAgentArgs(t *testing.T) {
	full := grokAgentArgs("full", "grok-4.5")
	// full → always-approve
	joined := ""
	for _, a := range full {
		joined += a + " "
	}
	if !containsSub(joined, "agent") || !containsSub(joined, "stdio") || !containsSub(joined, "--always-approve") {
		t.Fatalf("full args=%v", full)
	}
	ro := grokAgentArgs("read-only", "")
	for _, a := range ro {
		if a == "--always-approve" {
			t.Fatalf("read-only should not always-approve: %v", ro)
		}
	}
}

func TestUsageEventsFromPromptResult(t *testing.T) {
	raw := json.RawMessage(`{
		"stopReason":"end_turn",
		"_meta":{
			"sessionId":"s9",
			"modelId":"grok-4.5",
			"inputTokens":10,
			"outputTokens":5,
			"cachedReadTokens":3,
			"reasoningTokens":2,
			"usage":{"inputTokens":100,"outputTokens":20,"cachedReadTokens":50,"reasoningTokens":7,"costUsdTicks":1000000000}
		}
	}`)
	evts := usageEventsFromPromptResult(raw, "fallback")
	if len(evts) < 2 {
		t.Fatalf("events=%+v", evts)
	}
	var sawSystem, sawUsage, sawDone bool
	for _, e := range evts {
		switch e.Type {
		case EventSystem:
			sawSystem = true
			if e.SessionID != "s9" || e.Model != "grok-4.5" {
				t.Fatalf("system=%+v", e)
			}
		case EventUsage:
			sawUsage = true
			if e.InputTokens != 100 || e.OutputTokens != 20 || e.CostUSD != 0.1 {
				t.Fatalf("usage=%+v", e)
			}
		case EventDone:
			sawDone = true
			if e.SessionID != "s9" {
				t.Fatalf("done=%+v", e)
			}
		}
	}
	if !sawSystem || !sawUsage || !sawDone {
		t.Fatalf("missing events: sys=%v use=%v done=%v %+v", sawSystem, sawUsage, sawDone, evts)
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
