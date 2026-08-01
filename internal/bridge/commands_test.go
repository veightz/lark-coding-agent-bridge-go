package bridge

import (
	"strings"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
)

func int64Ptr(value int64) *int64 { return &value }

func TestFormatUsage(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, location)
	reset := time.Date(2026, 7, 31, 18, 30, 0, 0, location).Unix()
	usage := agent.UsageSnapshot{
		Provider: "Codex CLI",
		Plan:     "plus",
		Limits: []agent.UsageLimit{{
			ID:   "codex",
			Name: "Codex",
			Primary: &agent.UsageWindow{
				UsedPercent:       25,
				WindowDurationMin: 300,
				ResetsAt:          reset,
			},
			Secondary: &agent.UsageWindow{
				UsedPercent:       40,
				WindowDurationMin: 7 * 24 * 60,
			},
		}},
		ResetCredits: int64Ptr(2),
		TokenSummary: agent.UsageTokenSummary{
			LifetimeTokens:  int64Ptr(1234567),
			PeakDailyTokens: int64Ptr(45678),
		},
	}

	got := formatUsage(usage, now)
	for _, want := range []string{
		"**用量统计**",
		"套餐：`plus`",
		"5 小时窗口：已用 25%，剩余 75%；07-31 18:30 重置",
		"7 天窗口：已用 40%，剩余 60%",
		"可用额度重置次数：2",
		"累计 tokens：1,234,567",
		"单日峰值 tokens：45,678",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatOpenCodeActivity(t *testing.T) {
	usage := agent.UsageSnapshot{
		Provider: "OpenCode",
		Activity: &agent.UsageActivity{
			Sessions:              12,
			Messages:              345,
			InputTokens:           1234567,
			OutputTokens:          45678,
			CachedInputTokens:     9000000,
			ReasoningOutputTokens: 321,
			CostUSD:               1.23456,
		},
	}
	got := formatUsage(usage, time.Now())
	for _, want := range []string{
		"来源：OpenCode",
		"会话数：12",
		"消息数：345",
		"输入 tokens：1,234,567",
		"cache read：9,000,000",
		"reasoning tokens：321",
		"原生成本：`$1.2346`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatWindowDuration(t *testing.T) {
	tests := []struct {
		minutes int64
		want    string
	}{
		{0, "用量窗口"},
		{30, "30 分钟窗口"},
		{300, "5 小时窗口"},
		{10080, "7 天窗口"},
	}
	for _, test := range tests {
		if got := formatWindowDuration(test.minutes); got != test.want {
			t.Errorf("formatWindowDuration(%d) = %q, want %q", test.minutes, got, test.want)
		}
	}
}
