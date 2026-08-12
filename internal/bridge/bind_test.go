package bridge

import (
	"testing"

	"lark-coding-agent-bridge-go/internal/agent"
)

func TestExternalSessionStatusLabel(t *testing.T) {
	tests := []struct {
		status agent.ExternalSessionStatus
		want   string
	}{
		{status: agent.SessionStatusActive, want: "🟡 运行中"},
		{status: agent.SessionStatusIdle, want: "🟢 已完成"},
		{status: agent.SessionStatusUnknown, want: ""},
	}
	for _, tc := range tests {
		if got := externalSessionStatusLabel(tc.status); got != tc.want {
			t.Fatalf("status %q label = %q, want %q", tc.status, got, tc.want)
		}
	}
}
