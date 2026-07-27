package bridge

import (
	"strings"
	"testing"
)

func TestLooksLikeTask(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		hasResources bool
		want         bool
	}{
		{"空文本", "", false, false},
		{"纯空白", "   ", false, false},
		{"附件即任务", "[图片]", true, true},

		// 闲聊 → 不建群
		{"打招呼", "你好", false, false},
		{"在吗", "在吗", false, false},
		{"谢谢", "谢谢", false, false},
		{"英文打招呼", "hello", false, false},
		{"带标点的闲聊", "好的！", false, false},
		{"嗯", "嗯嗯", false, false},

		// 明确任务 → 建群
		{"帮我开头", "帮我看看现在的服务状态", false, true},
		{"修复", "修复一下登录页的报错", false, true},
		{"写一个", "写一个 Go 的 LRU 缓存", false, true},
		{"分析", "分析一下这个日志文件", false, true},
		{"英文 fix", "fix the broken test in parser", false, true},
		{"英文 implement", "implement retry logic", false, true},
		{"大写英文动作词", "FIX this", false, true},

		// 长消息但无动作词 → 不建群（ADR-0009：去掉纯长度兜底）
		{"长消息无动作词", "我们最近流水线在夜间频繁超时，上下文很长但没有明确指令", false, false},
		{"联调话术", "都可以，我在进行测试，你触发下ask交互，快", false, false},
		{"请触发交互", "你触发一下 ask 交互卡片", false, false},

		// 长消息 + 动作词 → 建群
		{"长消息带动作词", "帮我排查一下部署流水线在夜间频繁超时的问题", false, true},

		// 短问句无动作词 → 不建群
		{"短问句", "今天天气怎么样", false, false},
		{"短句", "你是谁", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeTask(tc.content, tc.hasResources); got != tc.want {
				t.Errorf("LooksLikeTask(%q, %v) = %v, want %v",
					tc.content, tc.hasResources, got, tc.want)
			}
		})
	}
}

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
