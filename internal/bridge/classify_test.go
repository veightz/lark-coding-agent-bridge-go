package bridge

import "testing"

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

		// 长消息兜底 → 建群
		{"长消息", "我们最近的部署流水线在夜间频繁超时需要处理", false, true},

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
	if got := groupNameFor(""); got == "" {
		t.Error("空内容应回退到默认群名")
	}
	short := "修复登录报错"
	if got := groupNameFor(short); got != short {
		t.Errorf("短标题不应截断: %q", got)
	}
	long := "这是一个非常非常长的任务描述它一定会超过二十个字符的所以应该被截断处理"
	got := groupNameFor(long)
	runes := []rune(got)
	if len(runes) != 21 || runes[20] != '…' {
		t.Errorf("长标题应截断为 20 字 + 省略号: %q (%d runes)", got, len(runes))
	}
	// 连续空白压缩
	if got := groupNameFor("  修复   登录\n报错  "); got != "修复 登录 报错" {
		t.Errorf("空白应压缩: %q", got)
	}
}
