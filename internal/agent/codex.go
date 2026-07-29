package agent

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"lark-coding-agent-bridge-go/internal/config"
)

// CodexAdapter drives the local `codex` CLI (OpenAI Codex).
type CodexAdapter struct {
	binary      string
	botIdentity *BotIdentity
	// Env injected into every child (lark-cli context etc.).
	Env map[string]string
}

func (a *CodexAdapter) ID() string          { return "codex" }
func (a *CodexAdapter) DisplayName() string { return "Codex CLI" }

func (a *CodexAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

func codexSandboxMode(access config.AccessLevel) string {
	switch access {
	case config.AccessWorkspace:
		return "workspace-write"
	case config.AccessReadOnly:
		return "read-only"
	default:
		return "danger-full-access"
	}
}

// buildCodexArgs mirrors the original argv.ts ordering.
func buildCodexArgs(opts RunOptions) ([]string, error) {
	sandbox := codexSandboxMode(opts.Access)

	globalFlags := []string{
		"--sandbox", sandbox,
	}
	if opts.Model != "" {
		globalFlags = append(globalFlags, "--model", opts.Model)
	}
	globalFlags = append(globalFlags,
		"-c", `approval_policy="never"`,
		"-c", `shell_environment_policy.inherit="all"`,
		"--ignore-rules",
		"--skip-git-repo-check",
		"-C", opts.Cwd,
	)

	var imageFlags []string
	for _, img := range opts.Images {
		imageFlags = append(imageFlags, "--image", img)
	}

	if opts.ThreadID != "" {
		args := []string{"exec"}
		args = append(args, globalFlags...)
		args = append(args, "resume", "--json")
		args = append(args, imageFlags...)
		args = append(args, opts.ThreadID, "-")
		return args, nil
	}

	args := []string{"exec", "--json"}
	args = append(args, globalFlags...)
	args = append(args, imageFlags...)
	if len(imageFlags) > 0 {
		args = append(args, "--")
	}
	args = append(args, "-")
	return args, nil
}

func (a *CodexAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for CodexAdapter.Run")
	}
	args, err := buildCodexArgs(opts)
	if err != nil {
		return nil, err
	}

	binary := a.binary
	if binary == "" {
		binary = "codex"
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = opts.Cwd

	translator := &codexTranslator{model: opts.Model}
	prompt := PrefixSystemPrompt(opts.Prompt, a.botIdentity)

	return startProc(cmd, prompt, opts.StopGraceMs, mergeEnv(a.Env), translator.translate, func(msg string) Event {
		return Event{Type: EventError, Message: msg, TerminationReason: TermFailed}
	})
}

// codexTranslator converts codex exec --json JSONL into AgentEvents.
// Ported from the original jsonl.ts CodexJsonlTranslator.
type codexTranslator struct {
	threadID            string
	model               string
	terminal            bool
	lastNonTerminalErr  string
	pendingAgentMessage string
	startedItems        map[string]bool
}

type codexRawEvent map[string]any

func (t *codexTranslator) translate(line []byte) []Event {
	if t.terminal {
		return nil
	}
	var raw codexRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	typ, _ := raw["type"].(string)
	if typ == "" {
		return nil
	}

	switch typ {
	case "thread.started":
		if id, ok := raw["thread_id"].(string); ok && id != "" {
			t.threadID = id
			return []Event{{Type: EventSystem, ThreadID: id, Model: t.model}}
		}
	case "turn.started":
		return nil
	case "item.started":
		return t.prependPending(t.translateItemStarted(raw))
	case "item.completed":
		return t.translateItemCompleted(raw)
	case "agent_message":
		if msg := stringField(raw, "message", "text"); msg != "" {
			return t.queueAgentMessage(msg)
		}
	case "turn.completed":
		return t.translateTurnCompleted(raw)
	case "turn.failed":
		return t.prependPending(t.terminalError(raw, "codex turn failed"))
	case "error":
		if msg := errorMessage(raw, "codex error"); msg != "" {
			t.lastNonTerminalErr = msg
		}
		return nil
	}
	return nil
}

func (t *codexTranslator) translateItemStarted(raw codexRawEvent) []Event {
	item, _ := raw["item"].(map[string]any)
	if item == nil || item["type"] != "command_execution" {
		return nil
	}
	id, _ := item["id"].(string)
	if id == "" {
		return nil
	}
	if t.startedItems == nil {
		t.startedItems = map[string]bool{}
	}
	t.startedItems[id] = true
	command, _ := item["command"].(string)
	return []Event{{
		Type:  EventToolUse,
		ID:    id,
		Name:  "command_execution",
		Input: map[string]any{"command": command},
	}}
}

func (t *codexTranslator) translateItemCompleted(raw codexRawEvent) []Event {
	item, _ := raw["item"].(map[string]any)
	if item == nil {
		return nil
	}
	switch item["type"] {
	case "agent_message":
		if msg := stringField(item, "text", "message"); msg != "" {
			return t.queueAgentMessage(msg)
		}
		return nil
	case "command_execution":
		id, _ := item["id"].(string)
		if id == "" {
			return nil
		}
		delete(t.startedItems, id)
		exitCode := -1
		if ec, ok := item["exit_code"].(float64); ok {
			exitCode = int(ec)
		}
		return t.prependPending([]Event{{
			Type:    EventToolResult,
			ID:      id,
			Output:  stringField(item, "output", "aggregated_output", "stdout"),
			IsError: exitCode > 0,
		}})
	}
	return nil
}

func (t *codexTranslator) translateTurnCompleted(raw codexRawEvent) []Event {
	t.terminal = true
	var out []Event
	if t.pendingAgentMessage != "" {
		out = append(out, Event{Type: EventFinalText, Content: t.pendingAgentMessage})
		t.pendingAgentMessage = ""
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		out = append(out, Event{
			Type:                  EventUsage,
			InputTokens:           intField(usage, "input_tokens", "inputTokens"),
			OutputTokens:          intField(usage, "output_tokens", "outputTokens"),
			CachedInputTokens:     intField(usage, "cached_input_tokens", "cachedInputTokens"),
			ReasoningOutputTokens: intField(usage, "reasoning_output_tokens", "reasoningOutputTokens"),
		})
	}
	out = append(out, Event{Type: EventDone, ThreadID: t.threadID, TerminationReason: TermNormal})
	return out
}

func (t *codexTranslator) queueAgentMessage(message string) []Event {
	var out []Event
	if t.pendingAgentMessage != "" {
		out = append(out, Event{Type: EventText, Delta: t.pendingAgentMessage})
	}
	t.pendingAgentMessage = message
	return out
}

func (t *codexTranslator) prependPending(events []Event) []Event {
	if len(events) == 0 || t.pendingAgentMessage == "" {
		return events
	}
	pending := t.pendingAgentMessage
	t.pendingAgentMessage = ""
	return append([]Event{{Type: EventText, Delta: pending}}, events...)
}

func (t *codexTranslator) terminalError(raw codexRawEvent, fallback string) []Event {
	t.terminal = true
	msg := errorMessage(raw, fallback)
	if len(msg) > 4096 {
		msg = msg[:4096]
	}
	return []Event{{Type: EventError, Message: msg, TerminationReason: TermFailed}}
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return int(v)
		}
	}
	return 0
}

func errorMessage(raw map[string]any, fallback string) string {
	if msg, ok := raw["message"].(string); ok && msg != "" {
		return msg
	}
	switch e := raw["error"].(type) {
	case map[string]any:
		if msg, ok := e["message"].(string); ok && msg != "" {
			return msg
		}
	case string:
		if e != "" {
			return e
		}
	}
	return fallback
}
