package bridge

import (
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/ask"
)

type askActionTestDispatcher struct {
	sent chan *ask.Pending
}

func (d *askActionTestDispatcher) Send(p *ask.Pending) (string, string, error) {
	d.sent <- p
	return "om_ask", "card_ask", nil
}

func (*askActionTestDispatcher) OnSettle(*ask.Pending, ask.Result) {}

func TestHandleAskCardActionSubmitInput(t *testing.T) {
	broker := ask.NewBroker()
	dispatcher := &askActionTestDispatcher{sent: make(chan *ask.Pending, 1)}
	broker.SetDispatcher(dispatcher)
	b := &Bridge{Ask: broker}

	resultCh := make(chan ask.Result, 1)
	go func() {
		result, err := broker.Register(ask.CreateInput{
			Route: ask.Route{ChatID: "oc_ask", Scope: "scope_ask"},
			Questions: []ask.Question{{
				Prompt:  "请输入文本",
				Options: []ask.Option{{Key: "__cancel__", Label: "取消"}},
			}},
			Freeform: true,
			Timeout:  time.Second,
			Source:   "pi",
		})
		if err != nil {
			t.Errorf("register ask: %v", err)
			return
		}
		resultCh <- result
	}()

	var pending *ask.Pending
	select {
	case pending = <-dispatcher.sent:
	case <-time.After(time.Second):
		t.Fatal("ask card was not dispatched")
	}

	kind, toast, card := b.HandleAskCardAction("ou_user", map[string]any{
		"cmd":            ask.ActionSubmitInput,
		"ask_id":         pending.ID,
		"nonce":          pending.Nonce,
		"question_index": 0,
	}, "  文本答案  ", nil)
	if kind != "" || toast != "" || card == nil {
		t.Fatalf("callback response: kind=%q toast=%q card=%v", kind, toast, card != nil)
	}

	select {
	case result := <-resultCh:
		if result.Kind != ask.KindAnswered || result.Comment != "文本答案" || result.By != "ou_user" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("input answer did not settle the ask")
	}
}
