package ask

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout   = 30 * time.Minute
	settledRetention = 60 * time.Second
	minTimeout       = 5 * time.Second
)

// Dispatcher sends / updates ask cards. The bridge wires a Lark implementation.
type Dispatcher interface {
	// Send posts a new ask card; returns message_id (and optional card entity id).
	Send(p *Pending) (messageID, cardEntityID string, err error)
	// OnSettle patches the card into a terminal state (best-effort).
	OnSettle(p *Pending, result Result)
}

// Broker holds pending asks and arbitrates clicks / timeouts.
type Broker struct {
	mu         sync.Mutex
	pending    map[string]*internalAsk
	dispatcher Dispatcher
}

type internalAsk struct {
	Pending
	resolve       chan Result
	timeoutHandle *time.Timer
	selections    []map[string]struct{} // per-question sets
}

// NewBroker constructs an empty broker. Call SetDispatcher before Register.
func NewBroker() *Broker {
	return &Broker{pending: map[string]*internalAsk{}}
}

// SetDispatcher wires the Feishu card sender (once at bootstrap).
func (b *Broker) SetDispatcher(d Dispatcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dispatcher = d
}

// Register creates a pending ask, dispatches the card, and blocks until
// answered / timed out / invalidated. Safe to call from multiple goroutines.
func (b *Broker) Register(in CreateInput) (Result, error) {
	if err := validateInput(in); err != nil {
		return Result{}, err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout < minTimeout {
		timeout = minTimeout
	}

	b.mu.Lock()
	if b.dispatcher == nil {
		b.mu.Unlock()
		return Result{}, fmt.Errorf("ask broker: dispatcher not wired")
	}
	dispatcher := b.dispatcher

	id := newID()
	nonce := newNonce()
	now := time.Now()
	sels := make([]map[string]struct{}, len(in.Questions))
	for i := range sels {
		sels[i] = map[string]struct{}{}
	}
	ia := &internalAsk{
		Pending: Pending{
			ID:         id,
			Nonce:      nonce,
			Route:      in.Route,
			Questions:  in.Questions,
			Selections: emptySelections(len(in.Questions)),
			Source:     in.Source,
			RawPayload: in.RawPayload,
			Freeform:   in.Freeform,
			CreatedAt:  now,
			DeadlineAt: now.Add(timeout),
		},
		resolve:    make(chan Result, 1),
		selections: sels,
	}
	ia.timeoutHandle = time.AfterFunc(timeout, func() {
		b.settle(id, Result{Kind: KindTimedOut}, false)
	})
	b.pending[id] = ia
	b.mu.Unlock()

	msgID, cardID, err := dispatcher.Send(&ia.Pending)
	if err != nil {
		b.settle(id, Result{
			Kind:   KindInvalidated,
			Reason: "card dispatch failed: " + err.Error(),
		}, false)
		return <-ia.resolve, nil
	}
	b.mu.Lock()
	if cur, ok := b.pending[id]; ok && !cur.Settled {
		cur.CardMessageID = msgID
		cur.CardEntityID = cardID
	}
	b.mu.Unlock()

	return <-ia.resolve, nil
}

// Snapshot returns a copy of the pending ask, or nil.
func (b *Broker) Snapshot(id string) *Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	ia := b.pending[id]
	if ia == nil {
		return nil
	}
	return clonePending(&ia.Pending)
}

// Toggle flips an option for multi-question / multi-select cards.
func (b *Broker) Toggle(askID, nonce string, questionIndex int, key, by string) ClickOutcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcSettledLocked()
	ia := b.pending[askID]
	if ia == nil || ia.Nonce != nonce {
		return OutcomeStale
	}
	if ia.Settled {
		return OutcomeAlreadySettled
	}
	if questionIndex < 0 || questionIndex >= len(ia.Questions) {
		return OutcomeStale
	}
	q := ia.Questions[questionIndex]
	if !optionExists(q, key) {
		return OutcomeStale
	}
	sel := ia.selections[questionIndex]
	if q.MultiSelect {
		if _, ok := sel[key]; ok {
			delete(sel, key)
		} else {
			sel[key] = struct{}{}
		}
	} else {
		clear(sel)
		sel[key] = struct{}{}
	}
	ia.Selections = materializeSelections(ia.selections)
	_ = by
	return OutcomeToggled
}

// Select settles a single-question single-select ask in one click.
func (b *Broker) Select(askID, nonce, key, by string) ClickOutcome {
	b.mu.Lock()
	ia := b.pending[askID]
	if ia == nil || ia.Nonce != nonce {
		b.mu.Unlock()
		return OutcomeStale
	}
	if ia.Settled {
		b.mu.Unlock()
		return OutcomeAlreadySettled
	}
	if len(ia.Questions) != 1 || ia.Questions[0].MultiSelect {
		b.mu.Unlock()
		return OutcomeStale
	}
	if !optionExists(ia.Questions[0], key) {
		b.mu.Unlock()
		return OutcomeStale
	}
	b.mu.Unlock()
	ok := b.settle(askID, Result{
		Kind:    KindAnswered,
		Answers: [][]string{{key}},
		By:      by,
	}, true)
	if !ok {
		return OutcomeAlreadySettled
	}
	return OutcomeAccepted
}

// Submit settles with the current toggled selections (or explicit overrides).
func (b *Broker) Submit(askID, nonce, by string, overrides [][]string) ClickOutcome {
	b.mu.Lock()
	ia := b.pending[askID]
	if ia == nil || ia.Nonce != nonce {
		b.mu.Unlock()
		return OutcomeStale
	}
	if ia.Settled {
		b.mu.Unlock()
		return OutcomeAlreadySettled
	}
	answers := overrides
	if answers == nil {
		answers = materializeSelections(ia.selections)
	}
	// Require at least one selection across all questions when no free-form path.
	any := false
	for _, a := range answers {
		if len(a) > 0 {
			any = true
			break
		}
	}
	if !any {
		b.mu.Unlock()
		return OutcomeNeedSelection
	}
	// Validate keys.
	for i, keys := range answers {
		if i >= len(ia.Questions) {
			b.mu.Unlock()
			return OutcomeStale
		}
		for _, k := range keys {
			if !optionExists(ia.Questions[i], k) {
				b.mu.Unlock()
				return OutcomeStale
			}
		}
	}
	b.mu.Unlock()
	if !b.settle(askID, Result{Kind: KindAnswered, Answers: answers, By: by}, true) {
		return OutcomeAlreadySettled
	}
	return OutcomeAccepted
}

// SubmitComment settles a freeform ask with the user's typed text.
func (b *Broker) SubmitComment(askID, by, comment string) ClickOutcome {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return OutcomeNeedSelection
	}
	b.mu.Lock()
	ia := b.pending[askID]
	if ia == nil {
		b.mu.Unlock()
		return OutcomeStale
	}
	if ia.Settled {
		b.mu.Unlock()
		return OutcomeAlreadySettled
	}
	if !ia.Freeform {
		b.mu.Unlock()
		return OutcomeStale
	}
	// Empty answers + comment: adapters treat Comment as the value.
	answers := emptySelections(len(ia.Questions))
	b.mu.Unlock()
	if !b.settle(askID, Result{
		Kind:    KindAnswered,
		Answers: answers,
		By:      by,
		Comment: comment,
	}, true) {
		return OutcomeAlreadySettled
	}
	return OutcomeAccepted
}

// FindFreeformByChat returns the newest unsettled freeform ask for a chat.
func (b *Broker) FindFreeformByChat(chatID string) *Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	var best *Pending
	for _, ia := range b.pending {
		if ia.Settled || !ia.Freeform || ia.Route.ChatID != chatID {
			continue
		}
		if best == nil || ia.CreatedAt.After(best.CreatedAt) {
			best = clonePending(&ia.Pending)
		}
	}
	return best
}

// Invalidate forces a pending ask to settle as invalidated (run stop / shutdown).
func (b *Broker) Invalidate(askID, reason string) {
	b.settle(askID, Result{Kind: KindInvalidated, Reason: reason}, true)
}

// InvalidateScope invalidates every unsettled ask for a chat scope.
func (b *Broker) InvalidateScope(scope, reason string) {
	b.mu.Lock()
	ids := make([]string, 0)
	for id, ia := range b.pending {
		if !ia.Settled && ia.Route.Scope == scope {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.Invalidate(id, reason)
	}
}

// PendingCount reports unsettled asks (for idle-watchdog pause).
func (b *Broker) PendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, ia := range b.pending {
		if !ia.Settled {
			n++
		}
	}
	return n
}

// PendingCountForScope reports unsettled asks for one scope.
func (b *Broker) PendingCountForScope(scope string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, ia := range b.pending {
		if !ia.Settled && ia.Route.Scope == scope {
			n++
		}
	}
	return n
}

func (b *Broker) settle(id string, result Result, notifyDispatcher bool) bool {
	b.mu.Lock()
	ia := b.pending[id]
	if ia == nil || ia.Settled {
		b.mu.Unlock()
		return false
	}
	ia.Settled = true
	ia.Result = &result
	if ia.timeoutHandle != nil {
		ia.timeoutHandle.Stop()
	}
	// Keep snapshot briefly for race-loser clicks.
	time.AfterFunc(settledRetention, func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	})
	dispatcher := b.dispatcher
	snap := clonePending(&ia.Pending)
	b.mu.Unlock()

	select {
	case ia.resolve <- result:
	default:
	}
	if notifyDispatcher && dispatcher != nil {
		go dispatcher.OnSettle(snap, result)
	}
	return true
}

func (b *Broker) gcSettledLocked() {
	// Retention is timer-based; nothing urgent here.
}

func validateInput(in CreateInput) error {
	if in.Route.ChatID == "" {
		return fmt.Errorf("ask: chatId required")
	}
	if len(in.Questions) == 0 {
		return fmt.Errorf("ask: questions required")
	}
	for i, q := range in.Questions {
		if q.Prompt == "" {
			return fmt.Errorf("ask: question %d empty prompt", i)
		}
		if len(q.Options) < 1 {
			return fmt.Errorf("ask: question %d needs options", i)
		}
		seen := map[string]struct{}{}
		for _, o := range q.Options {
			if o.Key == "" {
				return fmt.Errorf("ask: question %d option missing key", i)
			}
			if _, ok := seen[o.Key]; ok {
				return fmt.Errorf("ask: question %d duplicate key %q", i, o.Key)
			}
			seen[o.Key] = struct{}{}
		}
	}
	return nil
}

func optionExists(q Question, key string) bool {
	for _, o := range q.Options {
		if o.Key == key {
			return true
		}
	}
	return false
}

func emptySelections(n int) [][]string {
	out := make([][]string, n)
	for i := range out {
		out[i] = []string{}
	}
	return out
}

func materializeSelections(sels []map[string]struct{}) [][]string {
	out := make([][]string, len(sels))
	for i, s := range sels {
		keys := make([]string, 0, len(s))
		for k := range s {
			keys = append(keys, k)
		}
		out[i] = keys
	}
	return out
}

func clonePending(p *Pending) *Pending {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Questions = append([]Question(nil), p.Questions...)
	for i := range cp.Questions {
		cp.Questions[i].Options = append([]Option(nil), p.Questions[i].Options...)
	}
	cp.Selections = make([][]string, len(p.Selections))
	for i, s := range p.Selections {
		cp.Selections[i] = append([]string(nil), s...)
	}
	if p.Result != nil {
		r := *p.Result
		r.Answers = make([][]string, len(p.Result.Answers))
		for i, a := range p.Result.Answers {
			r.Answers[i] = append([]string(nil), a...)
		}
		cp.Result = &r
	}
	return &cp
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newNonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
