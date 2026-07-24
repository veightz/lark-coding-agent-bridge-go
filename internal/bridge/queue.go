package bridge

import (
	"sync"
	"time"
)

// PendingQueue batches rapid-fire messages per scope with a debounce
// window, and blocks a scope while its run is in flight so messages sent
// mid-run accumulate into the next batch. Ported from pending-queue.ts.
type PendingQueue struct {
	mu       sync.Mutex
	debounce time.Duration
	onFlush  func(scope string, batch []*Message)
	queues   map[string]*scopeQueue
}

type scopeQueue struct {
	buffer  []*Message
	timer   *time.Timer
	blocked bool
}

func NewPendingQueue(debounce time.Duration, onFlush func(scope string, batch []*Message)) *PendingQueue {
	return &PendingQueue{
		debounce: debounce,
		onFlush:  onFlush,
		queues:   map[string]*scopeQueue{},
	}
}

// Push appends a message and (re)arms the quiet-window timer.
func (q *PendingQueue) Push(scope string, msg *Message) {
	q.mu.Lock()
	sq := q.queues[scope]
	if sq == nil {
		sq = &scopeQueue{}
		q.queues[scope] = sq
	}
	sq.buffer = append(sq.buffer, msg)
	q.armLocked(scope, sq)
	q.mu.Unlock()
}

// Block stops flushing for a scope (a run is starting).
func (q *PendingQueue) Block(scope string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[scope]; sq != nil {
		sq.blocked = true
		if sq.timer != nil {
			sq.timer.Stop()
			sq.timer = nil
		}
	}
}

// Unblock re-arms the timer if messages accumulated during the run.
func (q *PendingQueue) Unblock(scope string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	sq := q.queues[scope]
	if sq == nil {
		return
	}
	sq.blocked = false
	if len(sq.buffer) > 0 {
		q.armLocked(scope, sq)
	}
}

func (q *PendingQueue) armLocked(scope string, sq *scopeQueue) {
	if sq.blocked {
		return
	}
	if sq.timer != nil {
		sq.timer.Stop()
	}
	sq.timer = time.AfterFunc(q.debounce, func() { q.fire(scope) })
}

func (q *PendingQueue) fire(scope string) {
	q.mu.Lock()
	sq := q.queues[scope]
	if sq == nil || sq.blocked || len(sq.buffer) == 0 {
		q.mu.Unlock()
		return
	}
	batch := sq.buffer
	sq.buffer = nil
	sq.timer = nil
	q.mu.Unlock()

	q.onFlush(scope, batch)
}
