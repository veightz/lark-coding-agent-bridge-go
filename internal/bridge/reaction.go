package bridge

import (
	"context"
	"log"
	"time"
)

// addWorkingReaction adds a "Typing" emoji reaction to a message as an
// instant ack that the bot received the message and is processing it.
// Returns the reaction ID on success, empty string on failure.
// Failures are logged but never thrown — losing a decoration must not
// break the actual reply flow.
func (b *Bridge) addWorkingReaction(messageID string) string {
	if messageID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := b.Lark.AddMessageReaction(ctx, messageID, "Typing")
	if err != nil {
		log.Printf("[reaction] add failed messageId=%s err=%v", messageID, err)
		return ""
	}
	log.Printf("[reaction] added messageId=%s reactionId=%s", messageID, id)
	return id
}

// removeWorkingReaction removes a previously-added Typing reaction.
// Errors are logged but tolerated — a leftover reaction is harmless.
func (b *Bridge) removeWorkingReaction(messageID, reactionID string) {
	if messageID == "" || reactionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Lark.DeleteMessageReaction(ctx, messageID, reactionID); err != nil {
		log.Printf("[reaction] remove failed messageId=%s reactionId=%s err=%v", messageID, reactionID, err)
		return
	}
	log.Printf("[reaction] removed messageId=%s reactionId=%s", messageID, reactionID)
}

// setScopeReaction records a working reaction for a scope so it can be
// cleared when the card starts streaming.
func (b *Bridge) setScopeReaction(scope, messageID, reactionID string) {
	b.reactionsMu.Lock()
	defer b.reactionsMu.Unlock()
	if b.reactions == nil {
		b.reactions = map[string]workingReaction{}
	}
	b.reactions[scope] = workingReaction{messageID: messageID, reactionID: reactionID}
}

// clearScopeReaction removes the working reaction for a scope (if any)
// and deletes the tracking entry.
func (b *Bridge) clearScopeReaction(scope string) {
	b.reactionsMu.Lock()
	r, ok := b.reactions[scope]
	if ok {
		delete(b.reactions, scope)
	}
	b.reactionsMu.Unlock()
	if ok {
		b.removeWorkingReaction(r.messageID, r.reactionID)
	}
}
