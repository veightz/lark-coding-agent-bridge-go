// ACP is a minimal Agent Client Protocol (v1) client over stdio JSON-RPC.
// It covers the headless-bridge subset: initialize, session/new,
// session/load, session/prompt, session/cancel, session/update notifications,
// auto-answering session/request_permission, and pluggable reverse RPCs
// (e.g. Grok `_x.ai/ask_user_question`).
//
// Spec: https://agentclientprotocol.com/protocol/v1/schema
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"lark-coding-agent-bridge-go/internal/config"
)

// acpUpdateHandler receives translated agent events; terminal events
// (done/error) end the current prompt turn.
type acpUpdateHandler func(events []Event)

// acpReverseHandler handles agent→client reverse RPC requests.
// Return true when the handler owns the response (sync or async via respond).
// Return false to fall through to built-in handling (permission auto-allow,
// or method-not-supported).
type acpReverseHandler func(method string, id json.RawMessage, params json.RawMessage) bool

type acpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader

	writeMu sync.Mutex
	mu      sync.Mutex
	dead    bool
	reqSeq  int
	pending map[int]chan acpRPCResult

	onUpdate  acpUpdateHandler
	onReverse acpReverseHandler

	// negotiated at initialize
	agentCapabilities map[string]any
	agentName         string
	agentVersion      string
	authMethods       []string
}

type acpRPCResult struct {
	Result json.RawMessage
	Error  *acpRPCError
}

type acpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *acpRPCError) Error() string { return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message) }

type acpRPCMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

// startACPLike launches an ACP agent subprocess and performs initialize.
// runtime is optional (empty = direct exec); onReverse is optional.
func startACPLike(name string, args []string, cwd string, env map[string]string, runtime config.AgentRuntime, onUpdate acpUpdateHandler, onReverse acpReverseHandler) (*acpClient, error) {
	cmd := agentCommand(runtime, name, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = mergeEnv(env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn %s: %w", name, err)
	}

	c := &acpClient{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		pending:   map[int]chan acpRPCResult{},
		onUpdate:  onUpdate,
		onReverse: onReverse,
	}
	go c.readLoop(stdout)
	go io.Copy(io.Discard, stderr)
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.dead = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if c.onUpdate != nil {
			c.onUpdate([]Event{{Type: EventError, Message: name + " ACP 进程意外退出", TerminationReason: TermFailed}})
		}
	}()

	if err := c.initialize(); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return c, nil
}

func (c *acpClient) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.dead
}

func (c *acpClient) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *acpClient) initialize() error {
	params := map[string]any{
		"protocolVersion": 1,
		"clientInfo":      map[string]any{"name": "lark-coding-agent-bridge-go", "version": "0.2.0"},
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}
	res, err := c.call("initialize", params)
	if err != nil {
		return fmt.Errorf("ACP initialize 失败: %w", err)
	}
	var out struct {
		AgentInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities map[string]any `json:"agentCapabilities"`
		AuthMethods       []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
	}
	if len(res) > 0 {
		_ = json.Unmarshal(res, &out)
	}
	c.agentName = out.AgentInfo.Name
	c.agentVersion = out.AgentInfo.Version
	c.agentCapabilities = out.AgentCapabilities
	c.authMethods = nil
	for _, m := range out.AuthMethods {
		if m.ID != "" {
			c.authMethods = append(c.authMethods, m.ID)
		}
	}
	return nil
}

// authenticate performs ACP authenticate (e.g. Cursor cursor_login).
func (c *acpClient) authenticate(methodID string) error {
	_, err := c.call("authenticate", map[string]any{"methodId": methodID})
	if err != nil {
		return fmt.Errorf("ACP authenticate %s 失败: %w", methodID, err)
	}
	return nil
}

func (c *acpClient) preferredAuthMethod(want string) string {
	if want != "" {
		for _, id := range c.authMethods {
			if id == want {
				return id
			}
		}
	}
	if len(c.authMethods) > 0 {
		return c.authMethods[0]
	}
	return ""
}

// call sends a JSON-RPC request and waits for the correlated response.
func (c *acpClient) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, fmt.Errorf("ACP 进程已退出")
	}
	c.reqSeq++
	id := c.reqSeq
	ch := make(chan acpRPCResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err := c.write(append(body, '\n')); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	res, ok := <-ch
	if !ok {
		return nil, fmt.Errorf("ACP 进程意外退出")
	}
	if res.Error != nil {
		return nil, res.Error
	}
	return res.Result, nil
}

// notify sends a JSON-RPC notification (no id, no response).
func (c *acpClient) notify(method string, params any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	return c.write(append(body, '\n'))
}

func (c *acpClient) write(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	dead := c.dead
	c.mu.Unlock()
	if dead {
		return fmt.Errorf("ACP 进程已退出")
	}
	_, err := c.stdin.Write(data)
	return err
}

// respond answers a reverse (agent→client) request.
func (c *acpClient) respond(id json.RawMessage, result any) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
	_ = c.write(append(body, '\n'))
}

func (c *acpClient) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg acpRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		switch {
		case msg.Method == "session/update":
			if c.onUpdate != nil {
				c.onUpdate(translateACPUpdate(msg.Params))
			}

		case len(msg.ID) > 0 && (msg.Result != nil || msg.Error != nil) && msg.Method == "":
			// Response to one of our requests. IDs we issue are ints.
			var id int
			if err := json.Unmarshal(msg.ID, &id); err == nil {
				c.mu.Lock()
				if ch := c.pending[id]; ch != nil {
					ch <- acpRPCResult{Result: msg.Result, Error: msg.Error}
					delete(c.pending, id)
				}
				c.mu.Unlock()
			}

		case len(msg.ID) > 0 && msg.Method != "":
			// Reverse request (agent → client).
			if c.onReverse != nil && c.onReverse(msg.Method, msg.ID, msg.Params) {
				break
			}
			if msg.Method == "session/request_permission" {
				c.respond(msg.ID, autoAllowPermission(msg.Params))
				break
			}
			// Unknown reverse request (fs/*, terminal/*): refuse politely.
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error":   map[string]any{"code": -32601, "message": "method not supported by bridge client"},
			})
			_ = c.write(append(body, '\n'))
		}
	}
}

// sessionNew creates a new ACP session; returns sessionId.
func (c *acpClient) sessionNew(cwd string, meta map[string]any) (string, error) {
	params := map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	res, err := c.call("session/new", params)
	if err != nil {
		return "", err
	}
	return parseACPSessionID(res)
}

// sessionLoad resumes an existing session by id. cwd is required by some agents.
func (c *acpClient) sessionLoad(sessionID, cwd string, meta map[string]any) (string, error) {
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	res, err := c.call("session/load", params)
	if err != nil {
		return "", err
	}
	if sid, err := parseACPSessionID(res); err == nil && sid != "" {
		return sid, nil
	}
	// Some agents return empty result on success and keep the requested id.
	return sessionID, nil
}

// sessionPrompt sends a user prompt and waits until the turn completes.
func (c *acpClient) sessionPrompt(sessionID, text string) (json.RawMessage, error) {
	return c.call("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": text},
		},
	})
}

// sessionCancel asks the agent to abort the in-flight prompt.
func (c *acpClient) sessionCancel(sessionID string) error {
	return c.notify("session/cancel", map[string]any{"sessionId": sessionID})
}

func parseACPSessionID(res json.RawMessage) (string, error) {
	if len(res) == 0 {
		return "", fmt.Errorf("empty session result")
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", fmt.Errorf("session result missing sessionId")
	}
	return out.SessionID, nil
}

// usageEventsFromPromptResult extracts optional usage from a session/prompt result.
func usageEventsFromPromptResult(res json.RawMessage, sessionID string) []Event {
	if len(res) == 0 {
		return nil
	}
	var out struct {
		StopReason string `json:"stopReason"`
		Meta       *struct {
			SessionID        string `json:"sessionId"`
			ModelID          string `json:"modelId"`
			InputTokens      int    `json:"inputTokens"`
			OutputTokens     int    `json:"outputTokens"`
			CachedReadTokens int    `json:"cachedReadTokens"`
			ReasoningTokens  int    `json:"reasoningTokens"`
			TotalTokens      int    `json:"totalTokens"`
			Usage            *struct {
				InputTokens      int   `json:"inputTokens"`
				OutputTokens     int   `json:"outputTokens"`
				CachedReadTokens int   `json:"cachedReadTokens"`
				ReasoningTokens  int   `json:"reasoningTokens"`
				CostUsdTicks     int64 `json:"costUsdTicks"`
			} `json:"usage"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil
	}
	var events []Event
	sid := sessionID
	model := ""
	if out.Meta != nil {
		if out.Meta.SessionID != "" {
			sid = out.Meta.SessionID
		}
		model = out.Meta.ModelID
	}
	if sid != "" || model != "" {
		events = append(events, Event{Type: EventSystem, SessionID: sid, Model: model})
	}
	if out.Meta != nil {
		inTok, outTok, cacheTok, reasonTok := out.Meta.InputTokens, out.Meta.OutputTokens, out.Meta.CachedReadTokens, out.Meta.ReasoningTokens
		var cost float64
		if u := out.Meta.Usage; u != nil {
			if u.InputTokens > 0 {
				inTok = u.InputTokens
			}
			if u.OutputTokens > 0 {
				outTok = u.OutputTokens
			}
			if u.CachedReadTokens > 0 {
				cacheTok = u.CachedReadTokens
			}
			if u.ReasoningTokens > 0 {
				reasonTok = u.ReasoningTokens
			}
			if u.CostUsdTicks > 0 {
				cost = float64(u.CostUsdTicks) / 1e10
			}
		}
		if inTok > 0 || outTok > 0 || cacheTok > 0 || reasonTok > 0 || cost > 0 {
			events = append(events, Event{
				Type:                  EventUsage,
				InputTokens:           inTok,
				OutputTokens:          outTok,
				CachedInputTokens:     cacheTok,
				ReasoningOutputTokens: reasonTok,
				CostUSD:               cost,
			})
		}
	}
	reason := TermNormal
	if out.StopReason == "cancelled" || out.StopReason == "interrupted" {
		reason = TermInterrupted
	}
	events = append(events, Event{Type: EventDone, SessionID: sid, TerminationReason: reason})
	return events
}

// autoAllowPermission picks an allow option, preferring allow_always.
// Headless policy: the bridge runs agents in full-access mode, so
// permission prompts are auto-approved (documented behavior).
func autoAllowPermission(params json.RawMessage) map[string]any {
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)
	chosen := ""
	for _, opt := range p.Options {
		if opt.Kind == "allow_always" {
			chosen = opt.OptionID
			break
		}
	}
	if chosen == "" {
		for _, opt := range p.Options {
			if opt.Kind == "allow_once" {
				chosen = opt.OptionID
				break
			}
		}
	}
	if chosen == "" && len(p.Options) > 0 {
		chosen = p.Options[0].OptionID
	}
	if chosen == "" {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": chosen}}
}

// translateACPUpdate converts a session/update notification's params
// into bridge events.
func translateACPUpdate(params json.RawMessage) []Event {
	var p struct {
		SessionID string         `json:"sessionId"`
		Update    map[string]any `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	kind, _ := p.Update["sessionUpdate"].(string)
	switch kind {
	case "agent_message_chunk":
		if text := acpContentText(p.Update["content"]); text != "" {
			return []Event{{Type: EventText, Delta: text}}
		}
	case "agent_thought_chunk":
		if text := acpContentText(p.Update["content"]); text != "" {
			return []Event{{Type: EventThinking, Delta: text}}
		}
	case "tool_call":
		name := stringField(p.Update, "title")
		if name == "" {
			name = stringField(p.Update, "kind")
		}
		return []Event{{
			Type:  EventToolUse,
			ID:    stringField(p.Update, "toolCallId"),
			Name:  name,
			Input: p.Update["rawInput"],
		}}
	case "tool_call_update":
		status := stringField(p.Update, "status")
		if status != "completed" && status != "failed" {
			return nil
		}
		output := ""
		if raw := p.Update["rawOutput"]; raw != nil {
			if data, err := json.Marshal(raw); err == nil {
				output = string(data)
			}
		}
		if output == "" {
			output = acpToolContentText(p.Update["content"])
		}
		return []Event{{
			Type:    EventToolResult,
			ID:      stringField(p.Update, "toolCallId"),
			Output:  output,
			IsError: status == "failed",
		}}
	}
	return nil
}

// acpContentText extracts text from an ACP ContentBlock.
func acpContentText(content any) string {
	m, ok := content.(map[string]any)
	if !ok {
		return ""
	}
	if t, _ := m["type"].(string); t == "text" {
		s, _ := m["text"].(string)
		return s
	}
	return ""
}

// acpToolContentText flattens tool_call_update content entries.
func acpToolContentText(content any) string {
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	out := ""
	for _, part := range parts {
		m, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "content" {
			out += acpContentText(m["content"])
		}
	}
	return out
}
