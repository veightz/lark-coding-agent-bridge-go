package agent

import (
	"encoding/json"
	"fmt"

	"lark-coding-agent-bridge-go/internal/config"
)

// CodexAdapter drives the local `codex` CLI (OpenAI Codex).
type CodexAdapter struct {
	binary      string
	botIdentity *BotIdentity
	runtime     config.AgentRuntime
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

func (a *CodexAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for CodexAdapter.Run")
	}
	prompt := PrefixSystemPrompt(opts.Prompt, a.botIdentity)
	return startCodexAppServer(a.binary, prompt, opts, mergeEnv(mergeEnvMaps(a.Env, opts.Env)), a.runtime)
}

// codexTranslator converts codex exec --json JSONL into AgentEvents.
// Ported from the original jsonl.ts CodexJsonlTranslator.
type codexTranslator struct {
	threadID            string
	model               string
	terminal            bool
	pendingAgentMessage string
	startedItems        map[string]string
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
	case "item.updated":
		// Codex currently uses updates for snapshots such as todo lists.
		// The started/completed pair remains authoritative for card state.
		return nil
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
		return t.prependPending(t.terminalError(raw, "codex error"))
	}
	return nil
}

func (t *codexTranslator) translateItemStarted(raw codexRawEvent) []Event {
	item, _ := raw["item"].(map[string]any)
	if item == nil {
		return nil
	}
	evt, ok := codexToolUse(item)
	if !ok {
		return nil
	}
	if t.startedItems == nil {
		t.startedItems = map[string]string{}
	}
	t.startedItems[evt.ID] = evt.Name
	return []Event{evt}
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
	case "reasoning", "plan":
		if text := stringField(item, "text"); text != "" {
			return t.prependPending([]Event{{Type: EventThinking, Delta: text}})
		}
		return nil
	case "command_execution":
		return t.prependPending(t.completeTool(item, codexCommandResult(item)))
	case "file_change":
		return t.prependPending(t.completeTool(item, codexFileChangeResult(item)))
	case "mcp_tool_call":
		return t.prependPending(t.completeTool(item, codexMCPResult(item)))
	case "web_search":
		return t.prependPending(t.completeTool(item, codexWebSearchResult(item)))
	case "collab_tool_call":
		return t.prependPending(t.completeTool(item, codexCollabResult(item)))
	case "todo_list":
		return t.prependPending(t.completeTool(item, Event{
			Output: codexTodoOutput(item["items"]),
		}))
	case "error":
		return t.prependPending(t.completeTool(item, Event{
			Output:  stringField(item, "message"),
			IsError: true,
		}))
	}
	return nil
}

func (t *codexTranslator) completeTool(item map[string]any, result Event) []Event {
	use, ok := codexToolUse(item)
	if !ok {
		return nil
	}
	var out []Event
	if _, started := t.startedItems[use.ID]; !started {
		out = append(out, use)
	}
	delete(t.startedItems, use.ID)
	result.Type = EventToolResult
	result.ID = use.ID
	out = append(out, result)
	return out
}

func codexToolUse(item map[string]any) (Event, bool) {
	id := stringField(item, "id")
	if id == "" {
		return Event{}, false
	}
	switch stringField(item, "type") {
	case "command_execution":
		return Event{
			Type:  EventToolUse,
			ID:    id,
			Name:  "command_execution",
			Input: map[string]any{"command": stringField(item, "command")},
		}, true
	case "file_change":
		return Event{
			Type: EventToolUse,
			ID:   id,
			Name: "file_change",
			Input: map[string]any{
				"changes": item["changes"],
			},
		}, true
	case "mcp_tool_call":
		name := "mcp"
		server, tool := stringField(item, "server"), stringField(item, "tool")
		if server != "" || tool != "" {
			name += ":" + server + "/" + tool
		}
		return Event{Type: EventToolUse, ID: id, Name: name, Input: item["arguments"]}, true
	case "web_search":
		return Event{
			Type: EventToolUse,
			ID:   id,
			Name: "web_search",
			Input: map[string]any{
				"query":  stringField(item, "query"),
				"action": item["action"],
			},
		}, true
	case "collab_tool_call":
		return Event{
			Type: EventToolUse,
			ID:   id,
			Name: "collab:" + stringField(item, "tool"),
			Input: map[string]any{
				"sender_thread_id":    item["sender_thread_id"],
				"receiver_thread_ids": item["receiver_thread_ids"],
				"prompt":              item["prompt"],
			},
		}, true
	case "todo_list":
		return Event{Type: EventToolUse, ID: id, Name: "todo_list", Input: item["items"]}, true
	case "error":
		return Event{Type: EventToolUse, ID: id, Name: "codex_error"}, true
	}
	return Event{}, false
}

func codexCommandResult(item map[string]any) Event {
	exitCode := intField(item, "exit_code")
	status := stringField(item, "status")
	return Event{
		Output:  stringField(item, "aggregated_output", "output", "stdout"),
		IsError: status == "failed" || status == "declined" || exitCode > 0,
	}
}

func codexFileChangeResult(item map[string]any) Event {
	return Event{
		Output:  codexChangesOutput(item["changes"]),
		IsError: stringField(item, "status") == "failed",
	}
}

func codexMCPResult(item map[string]any) Event {
	if errObj, ok := item["error"].(map[string]any); ok {
		return Event{Output: stringField(errObj, "message"), IsError: true}
	}
	status := stringField(item, "status")
	return Event{
		Output:  codexJSON(item["result"]),
		IsError: status == "failed",
	}
}

func codexWebSearchResult(item map[string]any) Event {
	output := codexJSON(item["action"])
	if output == "" {
		output = stringField(item, "query")
	}
	return Event{Output: output}
}

func codexCollabResult(item map[string]any) Event {
	return Event{
		Output:  codexJSON(item["agents_states"]),
		IsError: stringField(item, "status") == "failed",
	}
}

func codexChangesOutput(value any) string {
	changes, _ := value.([]any)
	if len(changes) == 0 {
		return ""
	}
	var out string
	for _, raw := range changes {
		change, _ := raw.(map[string]any)
		if change == nil {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += stringField(change, "kind") + " " + stringField(change, "path")
	}
	return out
}

func codexTodoOutput(value any) string {
	items, _ := value.([]any)
	var out string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		mark := "☐"
		if done, _ := item["completed"].(bool); done {
			mark = "☑"
		}
		if out != "" {
			out += "\n"
		}
		out += mark + " " + stringField(item, "text")
	}
	return out
}

func codexJSON(value any) string {
	if value == nil {
		return ""
	}
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(b)
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
