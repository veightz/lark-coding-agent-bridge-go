package card

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/lark"
)

// Stream drives one CardKit streaming card: throttled full-card updates
// while the run is live, plus an explicit finish that closes streaming mode.
// Ported from the original CardStreamController (throttle + update queue).
type Stream struct {
	client   *lark.Client
	chatID   string
	replyTo  string
	inThread bool // reply_in_thread: open/continue Feishu topic under replyTo

	throttle time.Duration

	mu       sync.Mutex
	cardID   string
	msgID    string
	seq      int
	latest   map[string]any
	failed   bool
	lastPush time.Time

	notify chan struct{}
	done   chan struct{}
	loopWg sync.WaitGroup
}

// NewStream prepares a streaming card sender.
// inThread=true creates/continues a Feishu topic when replyTo is set (p2p).
func NewStream(client *lark.Client, chatID, replyTo string, inThread bool) *Stream {
	return &Stream{
		client:   client,
		chatID:   chatID,
		replyTo:  replyTo,
		inThread: inThread,
		throttle: 400 * time.Millisecond,
		notify:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Start creates the card entity, sends it to the chat, and starts the
// background update loop.
func (s *Stream) Start(ctx context.Context, initial map[string]any) error {
	cardID, err := s.client.CreateCard(ctx, initial)
	if err != nil {
		return err
	}
	msgID, err := s.client.SendCardByReference(ctx, s.chatID, cardID, s.replyTo, s.inThread)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cardID = cardID
	s.msgID = msgID
	s.mu.Unlock()

	s.loopWg.Add(1)
	go s.loop()
	return nil
}

// MessageID is the chat message carrying the card (valid after Start).
func (s *Stream) MessageID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgID
}

// Update records the newest card snapshot; the loop pushes it throttled.
func (s *Stream) Update(card map[string]any) {
	s.mu.Lock()
	s.latest = card
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Stream) loop() {
	defer s.loopWg.Done()
	for {
		select {
		case <-s.done:
			return
		case <-s.notify:
		}
		// Enforce the minimum interval between API pushes.
		s.mu.Lock()
		wait := s.throttle - time.Since(s.lastPush)
		s.mu.Unlock()
		if wait > 0 {
			select {
			case <-s.done:
				return
			case <-time.After(wait):
			}
		}
		s.push()
	}
}

// push sends the pending snapshot exactly once. Called only from loop/Finish.
func (s *Stream) push() {
	s.mu.Lock()
	if s.failed || s.latest == nil || s.cardID == "" {
		s.mu.Unlock()
		return
	}
	snapshot := s.latest
	s.latest = nil
	s.seq++
	seq := s.seq
	cardID := s.cardID
	s.lastPush = time.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := s.client.UpdateCard(ctx, cardID, snapshot, seq)
	cancel()
	if err != nil {
		s.mu.Lock()
		s.failed = true
		s.mu.Unlock()
		log.Printf("[card] update failed (card %s seq %d): %v", cardID, seq, err)
	}
}

// Finish drains pending updates, pushes the final state, and closes
// streaming mode. Safe to call once; later Updates are ignored.
func (s *Stream) Finish(summary string) {
	close(s.done)
	s.loopWg.Wait()

	s.mu.Lock()
	cardID := s.cardID
	s.mu.Unlock()
	if cardID == "" {
		return
	}

	s.push() // final snapshot, if any

	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	if summary == "" {
		summary = "已完成"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := s.client.FinishStreamingCard(ctx, cardID, seq, truncateSummary(summary, 50))
	cancel()
	if err != nil {
		// best effort — Feishu auto-closes streaming after 10min anyway
		log.Printf("[card] finishStreamingCard failed (card %s): %v", cardID, err)
	}
}

func truncateSummary(text string, max int) string {
	cleaned := strings.Join(strings.Fields(text), " ")
	if len(cleaned) <= max {
		return cleaned
	}
	return cleaned[:max-1] + "…"
}
