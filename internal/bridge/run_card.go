package bridge

import (
	"context"
	"fmt"
	"log"
	"time"

	"lark-coding-agent-bridge-go/internal/card"
)

// rotateRunCard starts a new streaming message after an ask interaction.
// The new message is created first, so a creation failure leaves the current
// stream usable. Once created, stop/refresh routing and crash-cleanup state
// move to the new card; the old card is finalized as a continuation marker.
func (b *Bridge) rotateRunCard(
	scope, chatID, replyTo string,
	inThread, groupChat, persistActive bool,
	current runCardStream,
	runState *card.RunState,
) (runCardStream, error) {
	if current == nil || runState == nil {
		return current, fmt.Errorf("run card rotation requires current stream and state")
	}

	next := b.makeRunCardStream(chatID, replyTo, inThread)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	initial := card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: groupChat})
	if err := next.Start(ctx, initial); err != nil {
		return current, fmt.Errorf("create continuation card: %w", err)
	}

	oldMessageID := current.MessageID()
	newMessageID := next.MessageID()
	if persistActive {
		b.saveActiveCard(scope, chatID, next.CardID())
	}
	b.runsMu.Lock()
	delete(b.cardScopes, oldMessageID)
	if newMessageID != "" {
		b.cardScopes[newMessageID] = scope
	}
	if ar, ok := b.runs[scope]; ok {
		ar.runState = runState
		ar.stream = next
	}
	b.runsMu.Unlock()

	checkpoint := runState.MarkContinued()
	current.Update(card.Render(checkpoint, card.RenderOptions{GroupChat: groupChat}))
	current.Finish("已在下方新卡片继续")
	log.Printf("[ask] rotated run card scope=%s old=%s new=%s", scope, oldMessageID, newMessageID)
	return next, nil
}
