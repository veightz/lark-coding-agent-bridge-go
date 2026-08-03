package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"lark-coding-agent-bridge-go/internal/config"
)

// ReadUsage queries Codex's account APIs without opening a thread or spending
// a model turn.
func (a *CodexAdapter) ReadUsage(ctx context.Context) (UsageSnapshot, error) {
	binary := a.binary
	if binary == "" {
		binary = "codex"
	}
	return readCodexUsage(ctx, binary, mergeEnv(a.Env), a.runtime)
}

type codexUsageRPC struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  bytes.Buffer
	nextID  int
}

type codexUsageRPCResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func startCodexUsageRPC(ctx context.Context, binary string, env []string, runtime config.AgentRuntime) (*codexUsageRPC, error) {
	cmd := agentCommandContext(ctx, runtime, binary, "app-server", "--stdio")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	rpc := &codexUsageRPC{cmd: cmd, stdin: stdin}
	rpc.scanner = bufio.NewScanner(stdout)
	rpc.scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	cmd.Stderr = &rpc.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return rpc, nil
}

func (r *codexUsageRPC) close() {
	_ = r.stdin.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	_ = r.cmd.Wait()
}

func (r *codexUsageRPC) notify(method string, params any) error {
	return r.write(map[string]any{"method": method, "params": params})
}

func (r *codexUsageRPC) request(method string, params any, out any) error {
	r.nextID++
	id := r.nextID
	if err := r.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for r.scanner.Scan() {
		var response codexUsageRPCResponse
		if err := json.Unmarshal(r.scanner.Bytes(), &response); err != nil || len(response.ID) == 0 {
			// Notifications may arrive between request responses.
			continue
		}
		var responseID int
		if err := json.Unmarshal(response.ID, &responseID); err != nil || responseID != id {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("Codex app-server %s: %s", method, response.Error.Message)
		}
		if out == nil || len(response.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.Result, out); err != nil {
			return fmt.Errorf("解析 Codex app-server %s 响应: %w", method, err)
		}
		return nil
	}
	if err := r.scanner.Err(); err != nil {
		return fmt.Errorf("读取 Codex app-server 响应: %w", err)
	}
	detail := strings.TrimSpace(r.stderr.String())
	if len(detail) > 500 {
		detail = detail[:500]
	}
	if detail != "" {
		return fmt.Errorf("Codex app-server 提前退出: %s", detail)
	}
	return fmt.Errorf("Codex app-server 在响应 %s 前退出", method)
}

func (r *codexUsageRPC) write(message map[string]any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := r.stdin.Write(data); err != nil {
		return fmt.Errorf("写入 Codex app-server 失败: %w", err)
	}
	return nil
}

type codexRateLimitsResponse struct {
	RateLimits           codexRateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID  map[string]codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
	RateLimitResetCredit *struct {
		AvailableCount int64 `json:"availableCount"`
	} `json:"rateLimitResetCredits"`
}

type codexRateLimitSnapshot struct {
	LimitID   string                `json:"limitId"`
	LimitName string                `json:"limitName"`
	PlanType  string                `json:"planType"`
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
	Credits   *codexCreditsSnapshot `json:"credits"`
}

type codexRateLimitWindow struct {
	UsedPercent       int   `json:"usedPercent"`
	WindowDurationMin int64 `json:"windowDurationMins"`
	ResetsAt          int64 `json:"resetsAt"`
}

type codexCreditsSnapshot struct {
	Balance    *string `json:"balance"`
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
}

type codexTokenUsageResponse struct {
	Summary struct {
		LifetimeTokens        *int64 `json:"lifetimeTokens"`
		PeakDailyTokens       *int64 `json:"peakDailyTokens"`
		CurrentStreakDays     *int64 `json:"currentStreakDays"`
		LongestStreakDays     *int64 `json:"longestStreakDays"`
		LongestRunningTurnSec *int64 `json:"longestRunningTurnSec"`
	} `json:"summary"`
}

func readCodexUsage(ctx context.Context, binary string, env []string, runtime config.AgentRuntime) (UsageSnapshot, error) {
	rpc, err := startCodexUsageRPC(ctx, binary, env, runtime)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("启动 Codex app-server: %w", err)
	}
	defer rpc.close()

	var initialized map[string]any
	if err := rpc.request("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "lark-coding-agent-bridge-go",
			"title":   "Lark Coding Agent Bridge",
			"version": "1",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return UsageSnapshot{}, err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return UsageSnapshot{}, err
	}

	var limits codexRateLimitsResponse
	if err := rpc.request("account/rateLimits/read", nil, &limits); err != nil {
		return UsageSnapshot{}, err
	}

	var tokens codexTokenUsageResponse
	// Token history is supplemental and may be unavailable for some account
	// types or older Codex versions; rate-limit data remains useful on its own.
	_ = rpc.request("account/usage/read", nil, &tokens)

	return makeCodexUsageSnapshot(limits, tokens), nil
}

func makeCodexUsageSnapshot(limits codexRateLimitsResponse, tokens codexTokenUsageResponse) UsageSnapshot {
	out := UsageSnapshot{
		Provider: "Codex CLI",
		TokenSummary: UsageTokenSummary{
			LifetimeTokens:        tokens.Summary.LifetimeTokens,
			PeakDailyTokens:       tokens.Summary.PeakDailyTokens,
			CurrentStreakDays:     tokens.Summary.CurrentStreakDays,
			LongestStreakDays:     tokens.Summary.LongestStreakDays,
			LongestRunningTurnSec: tokens.Summary.LongestRunningTurnSec,
		},
	}
	if limits.RateLimitResetCredit != nil {
		count := limits.RateLimitResetCredit.AvailableCount
		out.ResetCredits = &count
	}

	if len(limits.RateLimitsByLimitID) == 0 {
		out.Limits = append(out.Limits, makeUsageLimit(limits.RateLimits))
	} else {
		ids := make([]string, 0, len(limits.RateLimitsByLimitID))
		for id := range limits.RateLimitsByLimitID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			snapshot := limits.RateLimitsByLimitID[id]
			if snapshot.LimitID == "" {
				snapshot.LimitID = id
			}
			out.Limits = append(out.Limits, makeUsageLimit(snapshot))
		}
	}
	for _, limit := range out.Limits {
		if limit.ID == "" && limit.Name == "" && limit.Primary == nil && limit.Secondary == nil && limit.Credits == nil {
			continue
		}
		if snapshot, ok := limits.RateLimitsByLimitID[limit.ID]; ok && snapshot.PlanType != "" {
			out.Plan = snapshot.PlanType
			break
		}
	}
	if out.Plan == "" {
		out.Plan = limits.RateLimits.PlanType
	}
	return out
}

func makeUsageLimit(in codexRateLimitSnapshot) UsageLimit {
	out := UsageLimit{
		ID:        in.LimitID,
		Name:      in.LimitName,
		Primary:   makeUsageWindow(in.Primary),
		Secondary: makeUsageWindow(in.Secondary),
	}
	if in.Credits != nil {
		out.Credits = &UsageCredits{
			HasCredit: in.Credits.HasCredits,
			Unlimited: in.Credits.Unlimited,
		}
		if in.Credits.Balance != nil {
			out.Credits.Balance = *in.Credits.Balance
		}
	}
	return out
}

func makeUsageWindow(in *codexRateLimitWindow) *UsageWindow {
	if in == nil {
		return nil
	}
	return &UsageWindow{
		UsedPercent:       in.UsedPercent,
		WindowDurationMin: in.WindowDurationMin,
		ResetsAt:          in.ResetsAt,
	}
}
