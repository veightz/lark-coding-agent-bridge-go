package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanClaudeSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	writeLines(t, filepath.Join(home, "projects", "-Users-test", "aaa111.jsonl"),
		`{"type":"queue-operation","operation":"enqueue","sessionId":"aaa111"}`,
		`{"type":"user","cwd":"/Users/test","message":{"role":"user","content":"帮我修个 bug"}}`,
	)
	writeLines(t, filepath.Join(home, "projects", "-Users-test", "bbb222.jsonl"),
		`{"type":"user","cwd":"/Users/test","message":{"role":"user","content":[{"type":"text","text":"第二个会话"}]}}`,
	)
	// 让 bbb222 更新
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(home, "projects", "-Users-test", "aaa111.jsonl"), old, old)

	sessions, err := scanClaudeSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	if sessions[0].ID != "bbb222" {
		t.Errorf("newest first: %+v", sessions[0])
	}
	if sessions[0].Preview != "第二个会话" {
		t.Errorf("preview = %q", sessions[0].Preview)
	}
	if sessions[1].Cwd != "/Users/test" || sessions[1].Preview != "帮我修个 bug" {
		t.Errorf("aaa111 = %+v", sessions[1])
	}
}

func TestScanPiSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", home)
	writeLines(t, filepath.Join(home, "sessions", "--Users-test--", "2026-01-01_abc123.jsonl"),
		`{"type":"session","version":3,"id":"abc123-def","cwd":"/Users/test","timestamp":"2026-01-01T00:00:00Z"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"pi 会话内容"}]}}`,
	)
	sessions, err := scanPiSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d", len(sessions))
	}
	s := sessions[0]
	if s.ID != "abc123-def" || s.Cwd != "/Users/test" || s.Preview != "pi 会话内容" {
		t.Errorf("session = %+v", s)
	}
	if s.Agent != config.AgentPi {
		t.Errorf("agent = %v", s.Agent)
	}
}

func TestScanCodexSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeLines(t, filepath.Join(home, "sessions", "2026", "01", "01", "rollout-x.jsonl"),
		`{"type":"session_meta","payload":{"id":"thread-1","cwd":"/Users/test/proj"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex 任务"}]}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"正在检查测试"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"测试已经通过"}}`,
	)
	sessions, err := scanCodexSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d", len(sessions))
	}
	s := sessions[0]
	if s.ID != "thread-1" || s.Cwd != "/Users/test/proj" || s.Preview != "codex 任务" {
		t.Errorf("session = %+v", s)
	}
	if s.Status != SessionStatusIdle || s.LastOutput != "测试已经通过" {
		t.Errorf("status snapshot = %+v", s)
	}
}

func TestScanCodexSessionTailActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeLines(t, path,
		`{"type":"session_meta","payload":{"id":"thread-active"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"上一轮完成"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"正在运行 go test"}}`,
	)
	status, output := scanCodexSessionTail(path)
	if status != SessionStatusActive || output != "正在运行 go test" {
		t.Fatalf("status=%q output=%q", status, output)
	}
}

func TestMatchSession(t *testing.T) {
	list := []ExternalSession{
		{ID: "abcdef12-0000"},
		{ID: "abc99999-1111"},
		{ID: "zzzz0000-2222"},
	}
	// 序号
	if m, _ := MatchSession(list, "2"); m == nil || m.ID != "abc99999-1111" {
		t.Error("index match failed")
	}
	// 全 id
	if m, _ := MatchSession(list, "zzzz0000-2222"); m == nil || m.ID != "zzzz0000-2222" {
		t.Error("full id match failed")
	}
	// 唯一前缀
	if m, _ := MatchSession(list, "zz"); m == nil || m.ID != "zzzz0000-2222" {
		t.Error("unique prefix failed")
	}
	// 歧义前缀
	m, candidates := MatchSession(list, "abc")
	if m != nil || len(candidates) != 2 {
		t.Errorf("ambiguous prefix: m=%v candidates=%d", m, len(candidates))
	}
	// 无匹配
	if m, _ := MatchSession(list, "nope"); m != nil {
		t.Error("no-match should be nil")
	}
}
