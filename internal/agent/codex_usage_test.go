package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexReadUsage(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"id":1'*)
      printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}'
      ;;
    *'"id":2'*)
      printf '%s\n' '{"method":"remoteControl/status/changed","params":{"status":"disabled"}}'
      printf '%s\n' '{"id":2,"result":{"rateLimits":{"limitId":"legacy","planType":"plus"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":"Codex","planType":"plus","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1785439000},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":1786000000},"credits":{"balance":"12.50","hasCredits":true,"unlimited":false}}},"rateLimitResetCredits":{"availableCount":2}}}'
      ;;
    *'"id":3'*)
      printf '%s\n' '{"id":3,"result":{"summary":{"lifetimeTokens":1234567,"peakDailyTokens":45678,"currentStreakDays":3,"longestStreakDays":8,"longestRunningTurnSec":90},"dailyUsageBuckets":[]}}'
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	adapter := &CodexAdapter{binary: script}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := adapter.ReadUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "Codex CLI" || got.Plan != "plus" {
		t.Fatalf("identity = %+v", got)
	}
	if len(got.Limits) != 1 {
		t.Fatalf("limits = %+v", got.Limits)
	}
	limit := got.Limits[0]
	if limit.ID != "codex" || limit.Name != "Codex" {
		t.Fatalf("limit = %+v", limit)
	}
	if limit.Primary == nil || limit.Primary.UsedPercent != 25 || limit.Primary.WindowDurationMin != 300 {
		t.Fatalf("primary = %+v", limit.Primary)
	}
	if limit.Secondary == nil || limit.Secondary.UsedPercent != 40 {
		t.Fatalf("secondary = %+v", limit.Secondary)
	}
	if limit.Credits == nil || limit.Credits.Balance != "12.50" {
		t.Fatalf("credits = %+v", limit.Credits)
	}
	if got.ResetCredits == nil || *got.ResetCredits != 2 {
		t.Fatalf("reset credits = %v", got.ResetCredits)
	}
	if got.TokenSummary.LifetimeTokens == nil || *got.TokenSummary.LifetimeTokens != 1234567 {
		t.Fatalf("token summary = %+v", got.TokenSummary)
	}
}

func TestCodexReadUsageReportsRPCError(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"id":2'*) printf '%s\n' '{"id":2,"error":{"code":-32000,"message":"not logged in"}}' ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	adapter := &CodexAdapter{binary: script}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := adapter.ReadUsage(ctx); err == nil {
		t.Fatal("expected RPC error")
	}
}

func TestCodexListModels(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"id":1'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"id":2'*) printf '%s\n' '{"id":2,"result":{"data":[{"id":"id-sol","model":"gpt-sol","displayName":"SOL","description":"Fast","hidden":false,"isDefault":true},{"id":"hidden","model":"hidden","displayName":"Hidden","description":"","hidden":true,"isDefault":false}],"nextCursor":"next"}}' ;;
    *'"id":3'*) printf '%s\n' '{"id":3,"result":{"data":[{"id":"id-pro","model":"gpt-pro","displayName":"Pro","description":"Strong","hidden":false,"isDefault":false}],"nextCursor":null}}' ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	adapter := &CodexAdapter{binary: script}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := adapter.ListModels(ctx, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %+v", got)
	}
	if got[0].ID != "gpt-sol" || got[0].DisplayName != "SOL" || !got[0].Default {
		t.Fatalf("first model = %+v", got[0])
	}
	if got[1].ID != "gpt-pro" || got[1].DisplayName != "Pro" || got[1].Default {
		t.Fatalf("second model = %+v", got[1])
	}
}
