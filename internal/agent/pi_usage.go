package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReadUsage reports all-time local Pi activity. Pi can route requests to many
// providers, so a single account quota would be misleading; session JSONL is
// the authoritative source for token, cache and native cost totals.
func (a *PiAdapter) ReadUsage(ctx context.Context) (UsageSnapshot, error) {
	files, err := collectJSONL(piSessionsDir(a.Env), 2)
	if err != nil {
		return UsageSnapshot{}, err
	}
	activity := &UsageActivity{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return UsageSnapshot{}, err
		}
		if err := addPiSessionUsage(ctx, file.path, activity); err != nil {
			return UsageSnapshot{}, err
		}
	}
	return UsageSnapshot{Provider: "Pi（本机会话）", Activity: activity}, nil
}

func addPiSessionUsage(ctx context.Context, path string, activity *UsageActivity) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("读取 Pi 会话 %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	sawHeader := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		switch stringField(entry, "type") {
		case "session":
			if !sawHeader {
				sawHeader = true
				activity.Sessions++
			}
		case "message":
			activity.Messages++
			message, _ := entry["message"].(map[string]any)
			role := stringField(message, "role")
			if role == "assistant" || role == "toolResult" {
				addPiUsage(activity, message["usage"])
			}
		case "compaction", "branch_summary":
			addPiUsage(activity, entry["usage"])
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("扫描 Pi 会话 %s: %w", filepath.Base(path), err)
	}
	return nil
}

func addPiUsage(activity *UsageActivity, value any) {
	usage, ok := parsePiUsage(value)
	if !ok {
		return
	}
	activity.InputTokens += usage.Input
	activity.OutputTokens += usage.Output
	activity.CachedInputTokens += usage.CacheRead
	activity.CacheWriteTokens += usage.CacheWrite
	activity.CostUSD += usage.Cost
}

type piUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
}

func parsePiUsage(value any) (piUsage, bool) {
	data, ok := value.(map[string]any)
	if !ok || data == nil {
		return piUsage{}, false
	}
	usage := piUsage{
		Input:      int64Field(data, "input"),
		Output:     int64Field(data, "output"),
		CacheRead:  int64Field(data, "cacheRead"),
		CacheWrite: int64Field(data, "cacheWrite"),
	}
	if cost, ok := data["cost"].(map[string]any); ok {
		usage.Cost = float64Field(cost, "total")
	}
	return usage, true
}

func piUsageEvent(value any) (Event, bool) {
	usage, ok := parsePiUsage(value)
	if !ok {
		return Event{}, false
	}
	return Event{
		Type:              EventUsage,
		InputTokens:       int(usage.Input),
		OutputTokens:      int(usage.Output),
		CachedInputTokens: int(usage.CacheRead),
		CostUSD:           usage.Cost,
	}, true
}

func int64Field(data map[string]any, key string) int64 {
	switch value := data[key].(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	case json.Number:
		result, _ := value.Int64()
		return result
	default:
		return 0
	}
}

func float64Field(data map[string]any, key string) float64 {
	switch value := data[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		result, _ := value.Float64()
		return result
	default:
		return 0
	}
}

func piSessionsDir(overrides map[string]string) string {
	if dir := envOverride(overrides, "PI_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(piConfigDir(overrides), "sessions")
}

func piConfigDir(overrides map[string]string) string {
	if dir := envOverride(overrides, "PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent")
}

func envOverride(overrides map[string]string, key string) string {
	if value := overrides[key]; value != "" {
		return value
	}
	return os.Getenv(key)
}
