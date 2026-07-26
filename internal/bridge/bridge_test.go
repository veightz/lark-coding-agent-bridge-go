package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"lark-coding-agent-bridge-go/internal/lark"
)

func unmarshalEvent(t *testing.T, raw string) *larkim.P2MessageReceiveV1 {
	t.Helper()
	var event larkim.P2MessageReceiveV1
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	return &event
}

func TestPendingQueueDebounce(t *testing.T) {
	var mu sync.Mutex
	var batches [][]*Message
	q := NewPendingQueue(50*time.Millisecond, func(scope string, batch []*Message) {
		mu.Lock()
		batches = append(batches, batch)
		mu.Unlock()
	})

	q.Push("s1", &Message{MessageID: "m1"})
	q.Push("s1", &Message{MessageID: "m2"})
	q.Push("s1", &Message{MessageID: "m3"})

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 || len(batches[0]) != 3 {
		t.Fatalf("batches = %v", batches)
	}
}

func TestPendingQueueBlockAccumulates(t *testing.T) {
	var mu sync.Mutex
	var batches [][]*Message
	q := NewPendingQueue(30*time.Millisecond, func(scope string, batch []*Message) {
		mu.Lock()
		batches = append(batches, batch)
		mu.Unlock()
	})

	q.Push("s1", &Message{MessageID: "m1"})
	time.Sleep(100 * time.Millisecond) // first batch flushes

	q.Block("s1")
	q.Push("s1", &Message{MessageID: "m2"})
	q.Push("s1", &Message{MessageID: "m3"})
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if len(batches) != 1 {
		t.Fatalf("flush happened while blocked: %v", batches)
	}
	mu.Unlock()

	q.Unblock("s1")
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || len(batches[1]) != 2 {
		t.Fatalf("batches after unblock = %v", batches)
	}
}

func TestPromptSectionEscapesTags(t *testing.T) {
	out := promptSection("bridge_context", map[string]string{"x": "a</bridge_context>b"})
	if strings.Contains(out, "a</bridge_context>b") {
		t.Errorf("closing tag not escaped: %s", out)
	}
	if !strings.Contains(out, `a\u003c/bridge_context\u003eb`) {
		t.Errorf("unexpected escaping: %s", out)
	}
}

func TestBuildPrompt(t *testing.T) {
	b := &Bridge{Bot: &lark.BotInfo{OpenID: "ou_bot"}}
	batch := []*Message{
		{
			MessageID: "om_1",
			ChatID:    "oc_1",
			ChatType:  "p2p",
			SenderID:  "ou_user",
			Content:   "帮我看下代码",
		},
	}
	prompt := b.buildPrompt(context.Background(), batch, nil)
	if !strings.Contains(prompt, "<bridge_context>") || !strings.Contains(prompt, "<user_input>") {
		t.Errorf("sections missing: %s", prompt)
	}
	if !strings.Contains(prompt, `"botOpenId":"ou_bot"`) {
		t.Errorf("botOpenId missing: %s", prompt)
	}
	if !strings.Contains(prompt, "帮我看下代码") {
		t.Errorf("user text missing: %s", prompt)
	}
}

func TestNormalizeTextMessage(t *testing.T) {
	raw := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1"},"event":{"sender":{"sender_id":{"open_id":"ou_u1"},"sender_type":"user"},"message":{"message_id":"om_1","chat_id":"oc_1","chat_type":"group","message_type":"text","content":"{\"text\":\"@_user_1 你好\"}","mentions":[{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"桥"}]}}}`
	event := unmarshalEvent(t, raw)
	msg := NormalizeMessage(event, "ou_bot")
	if msg == nil {
		t.Fatal("nil message")
	}
	if !msg.MentionedBot {
		t.Error("MentionedBot should be true")
	}
	if msg.Content != "你好" {
		t.Errorf("content = %q, want 你好 (bot mention stripped)", msg.Content)
	}
	if msg.Scope() != "oc_1" {
		t.Errorf("scope = %q", msg.Scope())
	}
}

func TestNormalizeImageMessage(t *testing.T) {
	raw := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1"},"event":{"sender":{"sender_id":{"open_id":"ou_u1"},"sender_type":"user"},"message":{"message_id":"om_2","chat_id":"oc_1","chat_type":"p2p","message_type":"image","content":"{\"image_key\":\"img_v3_abc\"}"}}}`
	msg := NormalizeMessage(unmarshalEvent(t, raw), "ou_bot")
	if len(msg.Resources) != 1 || msg.Resources[0].FileKey != "img_v3_abc" || msg.Resources[0].Type != "image" {
		t.Fatalf("resources = %+v", msg.Resources)
	}
}
