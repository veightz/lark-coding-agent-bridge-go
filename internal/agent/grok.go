package agent

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// GrokAdapter drives the local `grok` CLI (xAI Grok coding agent).
type GrokAdapter struct {
	binary      string
	botIdentity *BotIdentity
	Env         map[string]string
}

func (a *GrokAdapter) ID() string          { return "grok" }
func (a *GrokAdapter) DisplayName() string { return "Grok" }

func (a *GrokAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

func (a *GrokAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for GrokAdapter.Run")
	}

	prompt := BuildSystemPrompt(a.botIdentity) + "\n\n" + opts.Prompt

	args := []string{
		"-p",
		prompt,
		"--output-format", "streaming-json",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	args = append(args, opts.ExtraArgs...)

	binary := a.binary
	if binary == "" {
		binary = "grok"
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = opts.Cwd

	run, err := startProc(cmd, "", opts.StopGraceMs, mergeEnv(mergeEnvMaps(a.Env, opts.Env)), translateGrokLine, func(msg string) Event {
		return Event{Type: EventError, Message: msg, TerminationReason: TermFailed}
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

type grokEndUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	ReasoningTokens      int `json:"reasoning_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type grokEndModelUsage struct {
	InputTokens          int     `json:"inputTokens"`
	OutputTokens         int     `json:"outputTokens"`
	CacheReadInputTokens int     `json:"cacheReadInputTokens"`
	ModelCalls           int     `json:"modelCalls"`
	CostUSD              float64 `json:"costUSD"`
}

type grokRawEvent struct {
	Type         string                       `json:"type"`
	Data         string                       `json:"data,omitempty"`
	Usage        *grokEndUsage                `json:"usage,omitempty"`
	ModelUsage   map[string]grokEndModelUsage `json:"modelUsage,omitempty"`
	SessionID    string                       `json:"sessionId,omitempty"`
	StopReason   string                       `json:"stopReason,omitempty"`
	TotalCostUSD float64                      `json:"total_cost_usd,omitempty"`
}

func translateGrokLine(line []byte) []Event {
	var raw grokRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "thought":
		if raw.Data != "" {
			return []Event{{Type: EventThinking, Delta: raw.Data}}
		}
	case "text":
		if raw.Data != "" {
			return []Event{{Type: EventText, Delta: raw.Data}}
		}
	case "end":
		var out []Event
		modelName := ""
		for name := range raw.ModelUsage {
			modelName = name
			break
		}
		if raw.SessionID != "" {
			out = append(out, Event{Type: EventSystem, SessionID: raw.SessionID, Model: modelName})
		}
		if raw.Usage != nil {
			out = append(out, Event{
				Type:                  EventUsage,
				InputTokens:           raw.Usage.InputTokens,
				OutputTokens:          raw.Usage.OutputTokens,
				CachedInputTokens:     raw.Usage.CacheReadInputTokens,
				ReasoningOutputTokens: raw.Usage.ReasoningTokens,
				CostUSD:               raw.TotalCostUSD,
			})
		}
		reason := TermNormal
		if raw.StopReason == "Stop" || raw.StopReason == "EndTurn" {
			reason = TermNormal
		}
		out = append(out, Event{Type: EventDone, SessionID: raw.SessionID, TerminationReason: reason})
		return out
	}
	return nil
}
