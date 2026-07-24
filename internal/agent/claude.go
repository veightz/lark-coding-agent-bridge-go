package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"lark-coding-agent-bridge-go/internal/config"
)

// ClaudeAdapter drives the local `claude` CLI (Claude Code).
type ClaudeAdapter struct {
	binary      string
	botIdentity *BotIdentity
	// Env injected into every child (lark-cli context etc.).
	Env map[string]string
}

func (a *ClaudeAdapter) ID() string          { return "claude" }
func (a *ClaudeAdapter) DisplayName() string { return "Claude Code" }

func (a *ClaudeAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

func claudePermissionMode(access config.AccessLevel) string {
	switch access {
	case config.AccessWorkspace:
		return "acceptEdits"
	case config.AccessReadOnly:
		return "plan"
	default:
		return "bypassPermissions"
	}
}

func (a *ClaudeAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for ClaudeAdapter.Run")
	}

	// The prompt goes via stdin and the appended system prompt via a temp
	// file, so no special characters ever reach a shell (see the original
	// adapter for the Windows cmd.exe mangling this avoids).
	sysFile, cleanup, err := writeSystemPromptFile(BuildSystemPrompt(a.botIdentity))
	if err != nil {
		return nil, err
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", claudePermissionMode(opts.Access),
		"--append-system-prompt-file", sysFile,
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	binary := a.binary
	if binary == "" {
		binary = "claude"
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = opts.Cwd

	run, err := startProc(cmd, opts.Prompt, opts.StopGraceMs, mergeEnv(a.Env), translateClaudeLine, func(msg string) Event {
		return Event{Type: EventError, Message: msg, TerminationReason: TermFailed}
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	run.onCleanup(cleanup)
	return run, nil
}

// writeSystemPromptFile persists the system prompt to a throwaway temp file.
func writeSystemPromptFile(content string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "lark-claude-")
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, "append-system-prompt.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

// claudeRawEvent mirrors claude's stream-json wire format.
type claudeRawEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Session string `json:"session_id"`
	Cwd     string `json:"cwd"`
	Model   string `json:"model"`
	Message struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
	Usage *struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// translateClaudeLine converts one stream-json line into zero or more events.
func translateClaudeLine(line []byte) []Event {
	var raw claudeRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			return []Event{{Type: EventSystem, SessionID: raw.Session, Cwd: raw.Cwd, Model: raw.Model}}
		}
	case "assistant":
		var out []Event
		for _, b := range raw.Message.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					out = append(out, Event{Type: EventText, Delta: b.Text})
				}
			case "thinking":
				if b.Thinking != "" {
					out = append(out, Event{Type: EventThinking, Delta: b.Thinking})
				}
			case "tool_use":
				if b.ID != "" && b.Name != "" {
					var input any
					if len(b.Input) > 0 {
						_ = json.Unmarshal(b.Input, &input)
					}
					out = append(out, Event{Type: EventToolUse, ID: b.ID, Name: b.Name, Input: input})
				}
			}
		}
		return out
	case "user":
		var out []Event
		for _, b := range raw.Message.Content {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				var output string
				var s string
				if err := json.Unmarshal(b.Content, &s); err == nil {
					output = s
				} else {
					output = string(b.Content)
				}
				out = append(out, Event{Type: EventToolResult, ID: b.ToolUseID, Output: output, IsError: b.IsError})
			}
		}
		return out
	case "result":
		var out []Event
		if raw.Usage != nil {
			out = append(out, Event{
				Type:              EventUsage,
				InputTokens:       raw.Usage.InputTokens,
				OutputTokens:      raw.Usage.OutputTokens,
				CachedInputTokens: raw.Usage.CacheReadInputTokens,
				CostUSD:           raw.TotalCostUSD,
			})
		}
		out = append(out, Event{Type: EventDone, SessionID: raw.Session, TerminationReason: TermNormal})
		return out
	}
	return nil
}
