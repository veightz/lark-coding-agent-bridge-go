package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const openCodeUsageQuery = `SELECT
  COUNT(*) AS sessions,
  (SELECT COUNT(*) FROM message) AS messages,
  COALESCE(SUM(tokens_input), 0) AS input_tokens,
  COALESCE(SUM(tokens_output), 0) AS output_tokens,
  COALESCE(SUM(tokens_cache_read), 0) AS cache_read_tokens,
  COALESCE(SUM(tokens_cache_write), 0) AS cache_write_tokens,
  COALESCE(SUM(tokens_reasoning), 0) AS reasoning_tokens,
  COALESCE(SUM(cost), 0) AS cost_usd
FROM session
WHERE parent_id IS NULL`

type ocUsageRow struct {
	Sessions         int64   `json:"sessions"`
	Messages         int64   `json:"messages"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// ReadUsage returns all-time local OpenCode activity. OpenCode may use several
// providers, so this intentionally reports measured usage rather than a fake
// shared account quota.
func (a *OpenCodeAdapter) ReadUsage(ctx context.Context) (UsageSnapshot, error) {
	binary := a.binary
	if binary == "" {
		binary = "opencode"
	}
	cmd := agentCommandContext(ctx, a.runtime, binary, "db", openCodeUsageQuery, "--format", "json")
	cmd.Env = mergeEnv(a.Env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				detail = strings.TrimSpace(string(exitErr.Stderr))
			}
		}
		if detail != "" {
			return UsageSnapshot{}, fmt.Errorf("查询 OpenCode 用量: %s", truncateRunes(detail, 500))
		}
		return UsageSnapshot{}, fmt.Errorf("查询 OpenCode 用量: %w", err)
	}
	var rows []ocUsageRow
	if err := json.Unmarshal(stdout, &rows); err != nil {
		return UsageSnapshot{}, fmt.Errorf("解析 OpenCode 用量: %w", err)
	}
	if len(rows) != 1 {
		return UsageSnapshot{}, fmt.Errorf("解析 OpenCode 用量: 期望一行，实际 %d 行", len(rows))
	}
	row := rows[0]
	return UsageSnapshot{
		Provider: "OpenCode",
		Activity: &UsageActivity{
			Sessions:              row.Sessions,
			Messages:              row.Messages,
			InputTokens:           row.InputTokens,
			OutputTokens:          row.OutputTokens,
			CachedInputTokens:     row.CacheReadTokens,
			CacheWriteTokens:      row.CacheWriteTokens,
			ReasoningOutputTokens: row.ReasoningTokens,
			CostUSD:               row.CostUSD,
		},
	}, nil
}
