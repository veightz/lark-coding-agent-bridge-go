package bridge

import (
	"strings"
	"testing"
)

func TestGroupNameFor(t *testing.T) {
	if got := groupNameFor(""); !strings.HasPrefix(got, "任务 · ") {
		t.Errorf("空内容应回退到默认群名, got %q", got)
	}

	// 短任务：剥「帮我」+ 前缀
	if got := groupNameFor("帮我修复登录报错"); got != "任务 · 修复登录报错" {
		t.Errorf("短标题: got %q", got)
	}

	// 长标题截断（「任务 · 」+ 最多 16 字标题，含省略号）
	long := "帮我处理这是一个非常非常长的任务描述它一定会超过限制所以应该被截断"
	got := groupNameFor(long)
	if !strings.HasPrefix(got, "任务 · ") {
		t.Fatalf("应有统一前缀: %q", got)
	}
	title := strings.TrimPrefix(got, "任务 · ")
	if n := len([]rune(title)); n > groupNameMaxRunes {
		t.Errorf("标题过长: %q (%d runes)", title, n)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("超长应截断加省略号: %q", title)
	}

	// 多行只取首行/提炼关键词段
	if got := groupNameFor("  帮我修复   登录\n报错详情很多  "); got != "任务 · 修复 登录" {
		t.Errorf("多行/空白: %q", got)
	}

	// 背景铺垫 + 后置动作词 → 从动作词起并剥「帮我」
	if got := groupNameFor("线上有点问题，帮我排查超时"); got != "任务 · 排查超时" {
		t.Errorf("背景+动作词: got %q", got)
	}

	// 无动作词时仍给可用群名（截断原文），不原样甩整段
	chatty := "都可以，我在进行测试，你触发下ask交互，快"
	got = groupNameFor(chatty)
	if got == chatty || !strings.HasPrefix(got, "任务 · ") {
		t.Errorf("闲聊式文案应规范化: %q", got)
	}
}
