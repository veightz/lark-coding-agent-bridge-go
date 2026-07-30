package bridge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/ask"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/lark"
	"lark-coding-agent-bridge-go/internal/state"
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

type stopTestRun struct {
	stopped chan struct{}
}

func (r *stopTestRun) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (r *stopTestRun) Stop() {
	select {
	case <-r.stopped:
	default:
		close(r.stopped)
	}
}
func (*stopTestRun) WaitExit(int) bool { return true }

type stopTestAskDispatcher struct {
	sent chan *ask.Pending
}

func (d *stopTestAskDispatcher) Send(p *ask.Pending) (string, string, error) {
	d.sent <- p
	return "message", "card", nil
}
func (*stopTestAskDispatcher) OnSettle(*ask.Pending, ask.Result) {}

func TestStopRunInvalidatesPendingAsk(t *testing.T) {
	broker := ask.NewBroker()
	dispatcher := &stopTestAskDispatcher{sent: make(chan *ask.Pending, 1)}
	broker.SetDispatcher(dispatcher)
	run := &stopTestRun{stopped: make(chan struct{})}
	b := &Bridge{
		Ask:  broker,
		runs: map[string]*activeRun{"scope-1": {run: run, scope: "scope-1"}},
	}

	resultCh := make(chan ask.Result, 1)
	go func() {
		result, err := broker.Register(ask.CreateInput{
			Route: ask.Route{ChatID: "chat-1", Scope: "scope-1"},
			Questions: []ask.Question{{
				Prompt:  "继续吗？",
				Options: []ask.Option{{Key: "yes", Label: "继续"}},
			}},
			Source:  "codex",
			Timeout: time.Minute,
		})
		if err != nil {
			t.Errorf("register ask: %v", err)
		}
		resultCh <- result
	}()
	select {
	case <-dispatcher.sent:
	case <-time.After(time.Second):
		t.Fatal("ask was not registered")
	}

	if !b.stopRun("scope-1") {
		t.Fatal("stopRun returned false")
	}
	select {
	case result := <-resultCh:
		if result.Kind != ask.KindInvalidated || result.Reason != "run stopped" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("pending ask was not invalidated")
	}
	select {
	case <-run.stopped:
	default:
		t.Fatal("run was not stopped")
	}
}

func TestConfigureClaudeAskRunOptionsDoesNotLeakIntoCodex(t *testing.T) {
	first := &Message{MessageID: "om_1", ChatID: "oc_1"}
	opts := agent.RunOptions{}
	configureClaudeAskRunOptions(
		&opts,
		config.AgentCodex,
		"http://127.0.0.1:1234",
		config.Paths{Home: t.TempDir()},
		"codex-dev",
		"scope-1",
		first,
	)
	if len(opts.ExtraArgs) != 0 || len(opts.Env) != 0 {
		t.Fatalf("Codex received Claude hook options: %+v", opts)
	}
}

func TestConfigureClaudeAskRunOptionsForClaude(t *testing.T) {
	first := &Message{MessageID: "om_1", ChatID: "oc_1"}
	opts := agent.RunOptions{}
	configureClaudeAskRunOptions(
		&opts,
		config.AgentClaude,
		"http://127.0.0.1:1234",
		config.Paths{Home: t.TempDir()},
		"claude-dev",
		"scope-1",
		first,
	)
	if len(opts.ExtraArgs) != 2 || opts.ExtraArgs[0] != "--settings" {
		t.Fatalf("Claude settings args = %#v", opts.ExtraArgs)
	}
	if opts.Env[ask.EnvAskURL] != "http://127.0.0.1:1234" || opts.Env[ask.EnvAskScope] != "scope-1" {
		t.Fatalf("Claude hook env = %#v", opts.Env)
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

func TestReplyInThread(t *testing.T) {
	cases := []struct {
		chatType string
		want     bool
	}{
		{"p2p", true},
		{"group", false},
		{"topic_group", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := ReplyInThread(tc.chatType); got != tc.want {
			t.Errorf("ReplyInThread(%q) = %v, want %v", tc.chatType, got, tc.want)
		}
	}
	p2p := &Message{ChatType: "p2p"}
	if !p2p.ReplyInThread() {
		t.Error("p2p Message.ReplyInThread() should be true")
	}
	group := &Message{ChatType: "group"}
	if group.ReplyInThread() {
		t.Error("group Message.ReplyInThread() should be false")
	}
	var nilMsg *Message
	if nilMsg.ReplyInThread() {
		t.Error("nil Message.ReplyInThread() should be false")
	}
}

// ADR-0010: p2p 主时间线每条消息独立 scope；话题内用 root_id 续同一 scope。
func TestP2PTopicScope(t *testing.T) {
	// 主时间线首条：无 root/thread → scope 含本条 message id（新会话）
	first := &Message{ChatID: "oc_p2p", ChatType: "p2p", MessageID: "om_root_1"}
	if got := first.Scope(); got != "oc_p2p:om_root_1" {
		t.Errorf("first scope = %q", got)
	}
	if first.TopicRootID() != "om_root_1" {
		t.Errorf("TopicRootID = %q", first.TopicRootID())
	}

	// 同话题后续：RootID 指向首条 → 同一 scope（续聊）
	follow := &Message{
		ChatID: "oc_p2p", ChatType: "p2p",
		MessageID: "om_follow_2", RootID: "om_root_1", ThreadID: "omt_topic_x",
	}
	if got := follow.Scope(); got != "oc_p2p:om_root_1" {
		t.Errorf("follow scope = %q, want shared root", got)
	}
	if follow.Scope() != first.Scope() {
		t.Errorf("follow scope %q != first %q", follow.Scope(), first.Scope())
	}

	// 另一条主时间线消息 → 不同 scope（新会话，不串上下文）
	other := &Message{ChatID: "oc_p2p", ChatType: "p2p", MessageID: "om_root_9"}
	if other.Scope() == first.Scope() {
		t.Error("unrelated main-timeline messages must not share scope")
	}

	// 群聊：整群仍 chat 级；话题内才带 thread
	g := &Message{ChatID: "oc_g", ChatType: "group", MessageID: "om_g1"}
	if g.Scope() != "oc_g" {
		t.Errorf("group scope = %q", g.Scope())
	}
	gt := &Message{ChatID: "oc_g", ChatType: "group", MessageID: "om_g2", ThreadID: "omt_g"}
	if gt.Scope() != "oc_g:omt_g" {
		t.Errorf("group topic scope = %q", gt.Scope())
	}
}

func TestRecordGroupBinding(t *testing.T) {
	path := t.TempDir()
	bindings, err := state.LoadBindings(filepath.Join(path, "bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{
		Bindings: bindings,
		Profile:  &config.Profile{AgentKind: config.AgentOpenCode},
	}

	sess := state.Session{SessionID: "sess-1", Cwd: "/tmp/x"}
	// p2p must not auto-bind
	b.recordGroupBinding("p2p_1", "p2p_1", "p2p", sess)
	if bindings.HasScope("p2p_1") {
		t.Fatal("p2p should not auto-record binding")
	}

	b.recordGroupBinding("oc_group", "oc_group", "group", sess)
	bind, ok := bindings.Get(state.SessionKey(config.AgentOpenCode, "sess-1"))
	if !ok || bind.ChatID != "oc_group" || bind.ChatType != "group" {
		t.Fatalf("auto binding = %+v ok=%v", bind, ok)
	}

	// already bound elsewhere: do not steal
	b.recordGroupBinding("oc_other", "oc_other", "group", sess)
	bind, _ = bindings.Get(state.SessionKey(config.AgentOpenCode, "sess-1"))
	if bind.ChatID != "oc_group" {
		t.Fatalf("binding stolen: %+v", bind)
	}
}

func TestReusableGroupSkipsP2P(t *testing.T) {
	b := &Bridge{}
	if _, _, ok := b.reusableGroup(context.Background(), state.Binding{
		ChatID: "oc_x", ChatType: "p2p",
	}); ok {
		t.Fatal("p2p binding must not be reusable as group")
	}
	if _, _, ok := b.reusableGroup(context.Background(), state.Binding{}); ok {
		t.Fatal("empty chat id must not be reusable")
	}
}
