// ACP is a minimal Agent Client Protocol (v1) client over stdio JSON-RPC.
// It covers the headless-bridge subset: initialize, session/new,
// session/prompt, session/cancel, session/update notifications, and
// auto-answering session/request_permission.
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
)

// acpUpdateHandler receives translated agent events; terminal events
// (done/error) end the current prompt turn.
type acpUpdateHandler func(events []Event)

type acpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader

	writeMu sync.Mutex
	mu      sync.Mutex
	dead    bool
	reqSeq  int
	pending map[int]chan acpRPCResult

	onUpdate acpUpdateHandler

	// negotiated at initialize
	agentCapabilities map[string]any
	agentName         string
	agentVersion      string
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
func startACPLike(name string, args []string, cwd string, env map[string]string, onUpdate acpUpdateHandler) (*acpClient, error) {
	cmd := exec.Command(name, args...)
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
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		pending:  map[int]chan acpRPCResult{},
		onUpdate: onUpdate,
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
		c.onUpdate([]Event{{Type: EventError, Message: name + " ACP 进程意外退出", TerminationReason: TermFailed}})
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
	}
	if len(res) > 0 {
		_ = json.Unmarshal(res, &out)
	}
	c.agentName = out.AgentInfo.Name
	c.agentVersion = out.AgentInfo.Version
	c.agentCapabilities = out.AgentCapabilities
	return nil
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
			c.onUpdate(translateACPUpdate(msg.Params))

		case msg.Method == "session/request_permission" && len(msg.ID) > 0:
			c.respond(msg.ID, autoAllowPermission(msg.Params))

		case len(msg.ID) > 0 && (msg.Result != nil || msg.Error != nil):
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
