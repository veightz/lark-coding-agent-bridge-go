package ask

import (
	"sync"
	"testing"
	"time"
)

type memDispatch struct {
	mu   sync.Mutex
	sent []*Pending
}

func (m *memDispatch) Send(p *Pending) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := clonePending(p)
	m.sent = append(m.sent, cp)
	return "msg_" + p.ID[:8], "card_" + p.ID[:8], nil
}

func (m *memDispatch) OnSettle(p *Pending, result Result) {}

func TestBrokerSelectSingle(t *testing.T) {
	b := NewBroker()
	d := &memDispatch{}
	b.SetDispatcher(d)

	var result Result
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		result, err = b.Register(CreateInput{
			Route: Route{ChatID: "oc_x", Scope: "s1"},
			Questions: []Question{{
				Prompt: "部署还是回滚？",
				Options: []Option{
					{Key: "deploy", Label: "部署"},
					{Key: "rollback", Label: "回滚"},
				},
			}},
			Timeout: 5 * time.Second,
			Source:  "test",
		})
		if err != nil {
			t.Errorf("register: %v", err)
		}
	}()

	// Wait until card dispatched.
	deadline := time.Now().Add(2 * time.Second)
	var snap *Pending
	for time.Now().Before(deadline) {
		d.mu.Lock()
		if len(d.sent) > 0 {
			snap = d.sent[0]
		}
		d.mu.Unlock()
		if snap != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap == nil {
		t.Fatal("card not dispatched")
	}

	out := b.Select(snap.ID, snap.Nonce, "deploy", "ou_user")
	if out != OutcomeAccepted {
		t.Fatalf("select = %s", out)
	}
	wg.Wait()
	if result.Kind != KindAnswered || len(result.Answers) != 1 || result.Answers[0][0] != "deploy" {
		t.Fatalf("result = %+v", result)
	}
	if result.By != "ou_user" {
		t.Fatalf("by = %s", result.By)
	}
}

func TestBrokerToggleSubmitFull(t *testing.T) {
	b := NewBroker()
	d := &memDispatch{}
	b.SetDispatcher(d)

	done := make(chan Result, 1)
	go func() {
		r, err := b.Register(CreateInput{
			Route: Route{ChatID: "oc_x", Scope: "s"},
			Questions: []Question{{
				Prompt:      "跑哪些？",
				MultiSelect: true,
				Options: []Option{
					{Key: "unit", Label: "单测"},
					{Key: "lint", Label: "lint"},
				},
			}},
			Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Errorf("register: %v", err)
		}
		done <- r
	}()

	var snap *Pending
	for i := 0; i < 200; i++ {
		d.mu.Lock()
		if len(d.sent) > 0 {
			snap = d.sent[0]
		}
		d.mu.Unlock()
		if snap != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap == nil {
		t.Fatal("no card")
	}
	if b.Toggle(snap.ID, snap.Nonce, 0, "unit", "u") != OutcomeToggled {
		t.Fatal("toggle unit")
	}
	if b.Toggle(snap.ID, snap.Nonce, 0, "lint", "u") != OutcomeToggled {
		t.Fatal("toggle lint")
	}
	if b.Submit(snap.ID, snap.Nonce, "u", nil) != OutcomeAccepted {
		t.Fatal("submit")
	}
	result := <-done
	if result.Kind != KindAnswered || len(result.Answers[0]) != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseClaudeHookPayload(t *testing.T) {
	raw := []byte(`{
		"hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion",
		"tool_input":{"questions":[
			{"question":"今晚部署还是回滚？","multiSelect":false,
			 "options":[{"label":"部署"},{"label":"回滚"}]}
		]}
	}`)
	qs, rawQs, err := ParseClaudeHookPayload(raw)
	if err != nil || len(qs) != 1 || len(rawQs) != 1 {
		t.Fatalf("parse: qs=%v raw=%v err=%v", qs, rawQs, err)
	}
	if qs[0].Prompt != "今晚部署还是回滚？" || qs[0].Options[0].Key != "部署" {
		t.Fatalf("qs = %+v", qs[0])
	}
	dir := FormatClaudeAnswer("PreToolUse", rawQs, qs, Result{
		Kind:    KindAnswered,
		Answers: [][]string{{"部署"}},
	})
	if dir == "" || !containsAll(dir, "permissionDecision", "部署") {
		t.Fatalf("directive = %s", dir)
	}
}

func TestParseOpenCodeQuestions(t *testing.T) {
	id, qs := ParseOpenCodeQuestions(map[string]any{
		"id": "que_1",
		"questions": []any{
			map[string]any{
				"question": "选环境",
				"multiple": false,
				"options": []any{
					map[string]any{"label": "prod"},
					map[string]any{"label": "staging"},
				},
			},
		},
	})
	if id != "que_1" || len(qs) != 1 || qs[0].Options[0].Key != "prod" {
		t.Fatalf("id=%s qs=%+v", id, qs)
	}
	ans := FormatOpenCodeAnswers(qs, Result{Kind: KindAnswered, Answers: [][]string{{"prod"}}})
	if len(ans) != 1 || ans[0][0] != "prod" {
		t.Fatalf("ans=%v", ans)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}

func TestBuildCard(t *testing.T) {
	p := &Pending{
		ID:         "abc",
		Nonce:      "n1",
		DeadlineAt: time.Now().Add(time.Hour),
		Source:     "test",
		Questions: []Question{{
			Prompt:  "A or B?",
			Options: []Option{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}},
		}},
	}
	card := BuildCard(p, nil)
	if card["schema"] != "2.0" {
		t.Fatalf("schema = %v", card["schema"])
	}
	settled := BuildCard(p, &Result{Kind: KindAnswered, Answers: [][]string{{"a"}}})
	header := settled["header"].(map[string]any)
	if header["template"] != "green" {
		t.Fatalf("template = %v", header["template"])
	}
}

// TestBrokerSubmitInput 验证卡片输入框提交：freeform ask 可被非空文本 settle，
// 空文本、nonce 不匹配与重复提交均拒绝。
func TestBrokerSubmitInput(t *testing.T) {
	b := NewBroker()
	d := &memDispatch{}
	b.SetDispatcher(d)
	done := make(chan Result, 1)
	go func() {
		r, err := b.Register(CreateInput{
			Route: Route{ChatID: "oc_x", Scope: "s"},
			Questions: []Question{{
				Prompt:  "请输入仓库名",
				Options: []Option{{Key: "__cancel__", Label: "取消"}},
			}},
			Freeform: true,
			Timeout:  5 * time.Second,
			Source:   "pi",
		})
		if err != nil {
			t.Errorf("register: %v", err)
		}
		done <- r
	}()
	var snap *Pending
	deadline := time.Now().Add(2 * time.Second)
	for snap == nil && time.Now().Before(deadline) {
		d.mu.Lock()
		if len(d.sent) > 0 {
			snap = d.sent[0]
		}
		d.mu.Unlock()
		if snap == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if snap == nil {
		t.Fatal("card not dispatched")
	}

	// 空文本 → 需要输入
	if got := b.SubmitInput(snap.ID, snap.Nonce, "ou_u", "   "); got != OutcomeNeedInput {
		t.Fatalf("empty text outcome = %v", got)
	}
	// nonce 错误 → stale
	if got := b.SubmitInput(snap.ID, "bad-nonce", "ou_u", "hello"); got != OutcomeStale {
		t.Fatalf("bad nonce outcome = %v", got)
	}
	// 正确提交
	if got := b.SubmitInput(snap.ID, snap.Nonce, "ou_u", " my-repo "); got != OutcomeAccepted {
		t.Fatalf("submit outcome = %v", got)
	}
	r := <-done
	if r.Kind != KindAnswered || r.Comment != "my-repo" {
		t.Fatalf("result = %+v", r)
	}
	// 重复提交 → already settled
	if got := b.SubmitInput(snap.ID, snap.Nonce, "ou_u", "x"); got != OutcomeAlreadySettled {
		t.Fatalf("second submit outcome = %v", got)
	}
}

// TestBuildCardFreeformInput 验证 freeform ask 输入框直接绑定 callback。
// CardKit 2.0 会在用户点击输入框提交图标后通过 action.input_value 回传文本。
func TestBuildCardFreeformInput(t *testing.T) {
	p := &Pending{
		ID:         "abc",
		Nonce:      "n1",
		DeadlineAt: time.Now().Add(time.Hour),
		Source:     "pi",
		Freeform:   true,
		Questions: []Question{{
			Prompt:  "请输入内容",
			Options: []Option{{Key: "__cancel__", Label: "取消"}},
		}},
	}
	card := BuildCard(p, nil)
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	var input map[string]any
	for _, el := range elements {
		m, _ := el.(map[string]any)
		if m["tag"] == "input" {
			input = m
		}
	}
	if input == nil {
		t.Fatal("freeform card missing input")
	}
	if input["element_id"] != "ask_input_0" {
		t.Fatalf("input element_id = %v", input["element_id"])
	}
	behaviors, _ := input["behaviors"].([]map[string]any)
	if len(behaviors) != 1 || behaviors[0]["type"] != "callback" {
		t.Fatalf("input callback behaviors = %#v", input["behaviors"])
	}
	value, _ := behaviors[0]["value"].(map[string]any)
	if value["cmd"] != ActionSubmitInput || value["ask_id"] != "abc" || value["nonce"] != "n1" || value["question_index"] != 0 {
		t.Fatalf("input callback value = %#v", value)
	}
}
