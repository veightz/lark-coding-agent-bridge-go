package bridge

import (
	"context"
	"log"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/ask"
)

// larkAskDispatcher sends ask cards via CardKit (create + reference message).
type larkAskDispatcher struct {
	b *Bridge
}

func (d *larkAskDispatcher) Send(p *ask.Pending) (messageID, cardEntityID string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cardJSON := ask.BuildCard(p, nil)
	cardID, err := d.b.Lark.CreateCard(ctx, cardJSON)
	if err != nil {
		return "", "", err
	}
	msgID, err := d.b.Lark.SendCardByReference(ctx, p.Route.ChatID, cardID, p.Route.ReplyTo, p.Route.InThread)
	if err != nil {
		return "", "", err
	}
	return msgID, cardID, nil
}

func (d *larkAskDispatcher) OnSettle(p *ask.Pending, result ask.Result) {
	if p.CardEntityID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Sequence: use a high enough number; CardKit requires increasing seq.
	// We don't track per-card seq for asks — use unix seconds as a monotonic-ish value.
	seq := int(time.Now().Unix() % 1_000_000_000)
	if seq < 1 {
		seq = 1
	}
	cardJSON := ask.BuildCard(p, &result)
	if err := d.b.Lark.UpdateCard(ctx, p.CardEntityID, cardJSON, seq); err != nil {
		log.Printf("[ask] settle update card %s: %v", p.CardEntityID, err)
	}
}

// handleAskUser runs the Feishu card flow for an in-process EventAskUser
// (OpenCode question / pi extension_ui). Blocks until answered or timeout.
func (b *Bridge) handleAskUser(scope string, chatID, replyTo string, inThread bool, evt agent.Event) {
	if b.Ask == nil {
		log.Printf("[ask] broker not ready, dropping ask %s", evt.AskID)
		if evt.Reply != nil {
			_ = evt.Reply(nil, true)
		}
		return
	}
	questions := make([]ask.Question, 0, len(evt.Questions))
	for _, q := range evt.Questions {
		opts := make([]ask.Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, ask.Option{Key: o.Key, Label: o.Label})
		}
		questions = append(questions, ask.Question{
			Prompt:      q.Prompt,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}
	if len(questions) == 0 {
		if evt.Reply != nil {
			_ = evt.Reply(nil, true)
		}
		return
	}

	source := evt.Source
	if source == "" {
		source = "agent"
	}
	result, err := b.Ask.Register(ask.CreateInput{
		Route: ask.Route{
			ChatID:   chatID,
			ReplyTo:  replyTo,
			InThread: inThread,
			Scope:    scope,
		},
		Questions: questions,
		Source:    source,
		Freeform:  evt.Freeform,
		Timeout:   30 * time.Minute,
	})
	if err != nil {
		log.Printf("[ask] register failed: %v", err)
		if evt.Reply != nil {
			_ = evt.Reply(nil, true)
		}
		return
	}
	if result.Kind != ask.KindAnswered {
		log.Printf("[ask] %s settled as %s (%s)", evt.AskID, result.Kind, result.Reason)
		if evt.Reply != nil {
			if err := evt.Reply(nil, true); err != nil {
				log.Printf("[ask] reply (timeout/invalid) failed: %v", err)
			}
		}
		return
	}
	// Cancel button on freeform cards.
	if len(result.Answers) > 0 && len(result.Answers[0]) == 1 && result.Answers[0][0] == "__cancel__" {
		if evt.Reply != nil {
			if err := evt.Reply(nil, true); err != nil {
				log.Printf("[ask] reply (cancel) failed: %v", err)
			}
		}
		return
	}
	answers := ask.FormatAnswersWithComment(questions, result)
	if evt.Reply != nil {
		if err := evt.Reply(answers, false); err != nil {
			log.Printf("[ask] reply to agent failed: %v", err)
		}
	}
}

// tryAnswerAskWithText settles a freeform ask (pi input/editor) with chat text.
// Returns true when the message was consumed as an answer (do not run agent).
func (b *Bridge) tryAnswerAskWithText(chatID, operatorID, text string) bool {
	if b.Ask == nil {
		return false
	}
	pending := b.Ask.FindFreeformByChat(chatID)
	if pending == nil {
		return false
	}
	outcome := b.Ask.SubmitComment(pending.ID, operatorID, text)
	if outcome != ask.OutcomeAccepted {
		return false
	}
	log.Printf("[ask] freeform answered via chat text ask=%s", pending.ID)
	return true
}

// HandleAskCardAction processes ask_* button callbacks.
// Returns toastKind, toastContent, and optional card JSON for synchronous replace.
func (b *Bridge) HandleAskCardAction(operatorID string, value map[string]any) (toastKind, toast string, card map[string]any) {
	if b.Ask == nil {
		return "info", "提问服务未就绪", nil
	}
	cmd, _ := value["cmd"].(string)
	askID, _ := value["ask_id"].(string)
	nonce, _ := value["nonce"].(string)
	if askID == "" || nonce == "" {
		return "info", "此提问已失效", nil
	}

	var outcome ask.ClickOutcome
	switch cmd {
	case ask.ActionSelect:
		key, _ := value["key"].(string)
		outcome = b.Ask.Select(askID, nonce, key, operatorID)
	case ask.ActionToggle:
		key, _ := value["key"].(string)
		qi := anyToInt(value["question_index"])
		outcome = b.Ask.Toggle(askID, nonce, qi, key, operatorID)
	case ask.ActionSubmit:
		outcome = b.Ask.Submit(askID, nonce, operatorID, nil)
	default:
		return "", "", nil
	}

	if kind, content := ask.ToastFor(outcome); kind != "" {
		return kind, content, nil
	}

	snap := b.Ask.Snapshot(askID)
	if snap == nil {
		return "", "", nil
	}
	switch outcome {
	case ask.OutcomeAccepted:
		// Synchronous card replace so the UI doesn't stay "pending".
		var result ask.Result
		if snap.Result != nil {
			result = *snap.Result
		} else {
			result = ask.Result{Kind: ask.KindAnswered, By: operatorID}
		}
		return "", "", ask.BuildCard(snap, &result)
	case ask.OutcomeToggled:
		return "", "", ask.BuildCard(snap, nil)
	default:
		return "", "", nil
	}
}

func anyToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		var x int
		for _, c := range n {
			if c < '0' || c > '9' {
				return 0
			}
			x = x*10 + int(c-'0')
		}
		return x
	default:
		return 0
	}
}

// SetScopeRoute records chat routing for hook-originated asks.
func (b *Bridge) SetScopeRoute(scope, chatID, replyTo string, inThread bool) {
	b.routesMu.Lock()
	defer b.routesMu.Unlock()
	if b.scopeRoutes == nil {
		b.scopeRoutes = map[string]ask.Route{}
	}
	b.scopeRoutes[scope] = ask.Route{ChatID: chatID, ReplyTo: replyTo, InThread: inThread, Scope: scope}
}

// ClearScopeRoute drops routing when a run ends.
func (b *Bridge) ClearScopeRoute(scope string) {
	b.routesMu.Lock()
	defer b.routesMu.Unlock()
	delete(b.scopeRoutes, scope)
}

// ResolveScopeRoute is used by the ask HTTP server.
func (b *Bridge) ResolveScopeRoute(scope string) (ask.Route, bool) {
	b.routesMu.Lock()
	defer b.routesMu.Unlock()
	r, ok := b.scopeRoutes[scope]
	return r, ok
}
