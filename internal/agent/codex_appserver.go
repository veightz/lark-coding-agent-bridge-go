package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

// codexAppRun is one turn over `codex app-server --stdio`. Unlike
// `codex exec --json`, app-server is bidirectional: Codex can issue
// request_user_input and approval JSON-RPC requests while the turn runs.
type codexAppRun struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event
	exited chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan codexRPCResponse
	stopped bool
	ended   bool

	threadID   string
	turnID     string
	model      string
	access     configAccess
	grace      time.Duration
	shared     bool
	diskActive bool
}

// Kept local so the protocol implementation does not import config in every
// helper signature.
type configAccess string

const (
	codexAccessFull      configAccess = "full"
	codexAccessWorkspace configAccess = "workspace"
	codexAccessReadOnly  configAccess = "read-only"
)

type codexRPCResponse struct {
	Result map[string]any
	Err    error
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// synchronizedBuffer is shared by the stderr drain goroutine and startup/exit
// error reporting. exec.Cmd.Wait may return just before the pipe copier does,
// so bytes.Buffer alone would race on failure paths.
type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (e codexRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("Codex app-server RPC %d: %s", e.Code, e.Message)
}

func startCodexAppServer(binary, prompt string, opts RunOptions, env []string, runtime config.AgentRuntime) (Run, error) {
	if binary == "" {
		binary = "codex"
	}
	shared := runtime.AppServerMode == "daemon"
	if runtime.AppServerMode != "" && runtime.AppServerMode != "stdio" && !shared {
		return nil, fmt.Errorf("unsupported Codex appServerMode %q (want stdio or daemon)", runtime.AppServerMode)
	}
	if shared {
		if err := ensureCodexAppServerDaemon(binary, opts, env, runtime); err != nil {
			return nil, err
		}
	}
	args := []string{"app-server", "--stdio"}
	if shared {
		args = []string{"app-server", "proxy"}
	}
	args = append(args, opts.ExtraArgs...)
	cmd := agentCommand(runtime, binary, args...)
	cmd.Dir = opts.Cwd
	cmd.Env = env

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
		return nil, fmt.Errorf("failed to spawn %s app-server: %w", binary, err)
	}

	grace := time.Duration(opts.StopGraceMs) * time.Millisecond
	if grace <= 0 {
		grace = 5 * time.Second
	}
	r := &codexAppRun{
		cmd:        cmd,
		stdin:      stdin,
		events:     make(chan Event, 256),
		exited:     make(chan struct{}),
		pending:    map[string]chan codexRPCResponse{},
		model:      opts.Model,
		access:     configAccess(opts.Access),
		grace:      grace,
		shared:     shared,
		diskActive: codexSessionStatus(opts.ThreadID) == SessionStatusActive,
	}
	if r.access == "" {
		r.access = codexAccessFull
	}

	var stderrBuf synchronizedBuffer
	go func() { _, _ = io.Copy(&stderrBuf, stderr) }()
	go r.readLoop(stdout)
	go r.waitLoop(&stderrBuf)

	if err := r.initialize(prompt, opts); err != nil {
		r.failStartup(err)
		r.WaitExit(1000)
		detail := strings.TrimSpace(stderrBuf.String())
		if len(detail) > 500 {
			detail = detail[:500]
		}
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return r, nil
}

func ensureCodexAppServerDaemon(binary string, opts RunOptions, env []string, runtime config.AgentRuntime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{"app-server", "daemon", "start"}
	args = append(args, opts.ExtraArgs...)
	cmd := agentCommandContext(ctx, runtime, binary, args...)
	cmd.Dir = opts.Cwd
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if len(detail) > 500 {
		detail = detail[:500]
	}
	if detail != "" {
		return fmt.Errorf("start Codex app-server daemon: %w: %s", err, detail)
	}
	return fmt.Errorf("start Codex app-server daemon: %w", err)
}

func (r *codexAppRun) initialize(prompt string, opts RunOptions) error {
	if _, err := r.request("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "lark-coding-agent-bridge-go",
			"title":   "Lark Coding Agent Bridge",
			"version": "1",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, 15*time.Second); err != nil {
		return fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if err := r.notify("initialized", map[string]any{}); err != nil {
		return err
	}

	threadParams := map[string]any{
		"cwd":               opts.Cwd,
		"approvalPolicy":    codexApprovalPolicy(r.access),
		"approvalsReviewer": "user",
		"sandbox":           codexSandboxMode(opts.Access),
	}
	if opts.Model != "" {
		threadParams["model"] = opts.Model
	}
	var (
		threadResp map[string]any
		err        error
	)
	if opts.ThreadID == "" {
		threadResp, err = r.request("thread/start", threadParams, 30*time.Second)
	} else {
		threadParams["threadId"] = opts.ThreadID
		threadResp, err = r.request("thread/resume", threadParams, 30*time.Second)
	}
	if err != nil {
		return fmt.Errorf("open Codex thread: %w", err)
	}
	thread, _ := threadResp["thread"].(map[string]any)
	r.threadID = stringField(thread, "id")
	if r.threadID == "" {
		r.threadID = opts.ThreadID
	}
	if r.threadID == "" {
		return fmt.Errorf("Codex app-server returned no thread id")
	}
	if model := stringField(threadResp, "model"); model != "" {
		r.model = model
	}
	r.emit(Event{Type: EventSystem, ThreadID: r.threadID, Model: r.model})

	input := []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
	for _, path := range opts.Images {
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	runtimeStatus := codexThreadStatus(thread)
	if runtimeStatus == "active" {
		if !r.shared {
			return fmt.Errorf("Codex 会话仍在其他客户端运行；请等待当前回合结束，或把 profile 的 agent.appServerMode 设为 daemon 后重试")
		}
		turnID := codexActiveTurnID(thread)
		if turnID == "" {
			readResp, readErr := r.request("thread/read", map[string]any{
				"threadId":     r.threadID,
				"includeTurns": true,
			}, 15*time.Second)
			if readErr == nil {
				turnID = codexActiveTurnID(mapField(readResp, "thread"))
			}
		}
		if turnID == "" {
			return fmt.Errorf("Codex shared daemon 报告会话 active，但未返回可 steer 的 turn id")
		}
		steerResp, steerErr := r.request("turn/steer", map[string]any{
			"threadId":       r.threadID,
			"expectedTurnId": turnID,
			"input":          input,
		}, 30*time.Second)
		if steerErr != nil {
			return fmt.Errorf("steer Codex turn: %w", steerErr)
		}
		r.turnID = stringField(steerResp, "turnId")
		if r.turnID == "" {
			r.turnID = turnID
		}
		return nil
	}
	// JSONL 显示 active、但当前 app-server runtime 看不到活动 turn，说明
	// 原会话仍属于另一个独立 CLI/桌面进程。此时启动新 turn 会并发写同一 thread。
	if r.diskActive {
		return fmt.Errorf("Codex 会话仍在独立客户端运行，无法安全接管；请等待当前回合结束，或让原客户端接入同一个 shared daemon")
	}
	turnParams := map[string]any{
		"threadId": r.threadID,
		"input":    input,
	}
	collaborationMode, err := r.resolveCollaborationMode(opts.CollaborationMode)
	if err != nil {
		return err
	}
	if collaborationMode != nil {
		turnParams["collaborationMode"] = collaborationMode
	}
	turnResp, err := r.request("turn/start", turnParams, 30*time.Second)
	if err != nil {
		return fmt.Errorf("start Codex turn: %w", err)
	}
	turn, _ := turnResp["turn"].(map[string]any)
	r.turnID = stringField(turn, "id")
	return nil
}

// resolveCollaborationMode uses the presets advertised by the running Codex
// CLI. Plan's model/effort are protocol-owned and may change between releases,
// so the bridge only persists the stable mode id in sessions.json.
func (r *codexAppRun) resolveCollaborationMode(mode CollaborationMode) (map[string]any, error) {
	switch mode {
	case "", CollaborationModeDefault:
		// Absence is the app-server's native Default mode.
		return nil, nil
	case CollaborationModePlan:
		// Continue below.
	default:
		return nil, fmt.Errorf("Codex 协作模式 %q 无效", mode)
	}

	resp, err := r.request("collaborationMode/list", map[string]any{}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("读取 Codex Plan 预设失败（请升级 Codex CLI）: %w", err)
	}
	data, _ := resp["data"].([]any)
	for _, raw := range data {
		preset, _ := raw.(map[string]any)
		if CollaborationMode(stringField(preset, "mode")) != mode {
			continue
		}
		model := stringField(preset, "model")
		if model == "" {
			model = r.model
		}
		if model == "" {
			return nil, fmt.Errorf("Codex Plan 预设缺少模型，且 thread 未返回当前模型")
		}
		settings := map[string]any{
			"model":                  model,
			"reasoning_effort":       nil,
			"developer_instructions": nil,
		}
		if effort := stringField(preset, "reasoning_effort"); effort != "" {
			settings["reasoning_effort"] = effort
		}
		return map[string]any{
			"mode":     string(mode),
			"settings": settings,
		}, nil
	}
	return nil, fmt.Errorf("当前 Codex CLI 不支持 Plan 协作模式，请升级后重试")
}

func codexThreadStatus(thread map[string]any) string {
	status := mapField(thread, "status")
	return strings.ToLower(stringField(status, "type"))
}

func codexActiveTurnID(thread map[string]any) string {
	turns := anySlice(thread["turns"])
	for i := len(turns) - 1; i >= 0; i-- {
		turn, _ := turns[i].(map[string]any)
		status := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(stringField(turn, "status")))
		if status == "inprogress" {
			return stringField(turn, "id")
		}
	}
	return ""
}

func codexApprovalPolicy(access configAccess) string {
	if access == codexAccessFull {
		return "never"
	}
	return "on-request"
}

func (r *codexAppRun) request(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	r.mu.Lock()
	r.nextID++
	seq := r.nextID
	id := fmt.Sprintf("%d", seq)
	ch := make(chan codexRPCResponse, 1)
	r.pending[id] = ch
	dead := r.ended
	r.mu.Unlock()
	if dead {
		r.dropPending(id)
		return nil, fmt.Errorf("Codex app-server 已退出")
	}
	if err := r.write(map[string]any{"id": seq, "method": method, "params": params}); err != nil {
		r.dropPending(id)
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("Codex app-server 已退出")
		}
		return resp.Result, resp.Err
	case <-time.After(timeout):
		r.dropPending(id)
		return nil, fmt.Errorf("Codex app-server %s 响应超时", method)
	}
}

func (r *codexAppRun) notify(method string, params map[string]any) error {
	return r.write(map[string]any{"method": method, "params": params})
}

func (r *codexAppRun) write(message map[string]any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.stdin.Write(data); err != nil {
		return fmt.Errorf("写入 Codex app-server 失败: %w", err)
	}
	return nil
}

func (r *codexAppRun) dropPending(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

func (r *codexAppRun) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		var raw map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		method := stringField(raw, "method")
		_, hasID := raw["id"]
		if method != "" && hasID {
			r.handleServerRequest(raw)
			continue
		}
		if method != "" {
			r.handleNotification(method, mapField(raw, "params"))
			continue
		}
		if hasID {
			r.handleResponse(raw)
		}
	}
}

func (r *codexAppRun) handleResponse(raw map[string]any) {
	id := rpcID(raw["id"])
	resp := codexRPCResponse{Result: mapField(raw, "result")}
	if e := mapField(raw, "error"); e != nil {
		resp.Err = codexRPCError{Code: intField(e, "code"), Message: stringField(e, "message")}
	}
	r.mu.Lock()
	ch := r.pending[id]
	delete(r.pending, id)
	r.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func rpcID(v any) string {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%.0f", n)
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}

func (r *codexAppRun) handleServerRequest(raw map[string]any) {
	method := stringField(raw, "method")
	id := raw["id"]
	params := mapField(raw, "params")
	switch method {
	case "item/tool/requestUserInput":
		r.emit(r.codexUserInputEvent(id, params))
	case "item/commandExecution/requestApproval":
		r.emit(r.codexCommandApprovalEvent(id, params))
	case "item/fileChange/requestApproval":
		r.emit(r.codexFileApprovalEvent(id, params))
	case "item/permissions/requestApproval":
		r.emit(r.codexPermissionsApprovalEvent(id, params))
	default:
		_ = r.write(map[string]any{
			"id": id,
			"error": map[string]any{
				"code":    -32601,
				"message": "bridge does not support server request " + method,
			},
		})
	}
}

func (r *codexAppRun) codexUserInputEvent(requestID any, params map[string]any) Event {
	rawQuestions, _ := params["questions"].([]any)
	questions := make([]AskQuestion, 0, len(rawQuestions))
	questionIDs := make([]string, 0, len(rawQuestions))
	freeform := false
	for _, raw := range rawQuestions {
		q, _ := raw.(map[string]any)
		id := stringField(q, "id")
		prompt := stringField(q, "question")
		header := stringField(q, "header")
		if header != "" && header != prompt {
			prompt = "**" + header + "**\n" + prompt
		}
		var options []AskOption
		for _, rawOpt := range anySlice(q["options"]) {
			opt, _ := rawOpt.(map[string]any)
			label := stringField(opt, "label")
			if label == "" {
				continue
			}
			description := stringField(opt, "description")
			if description != "" {
				prompt += "\n- **" + label + "**: " + description
			}
			options = append(options, AskOption{Key: label, Label: label})
		}
		isOther, _ := q["isOther"].(bool)
		isSecret, _ := q["isSecret"].(bool)
		if isSecret {
			options = []AskOption{{
				Key:   "拒绝在聊天中提供敏感信息",
				Label: "拒绝在聊天中提供敏感信息",
			}}
			isOther = false
			prompt += "\n\n为避免泄露敏感信息，此问题不能通过飞书文字作答。"
		}
		if isOther && len(rawQuestions) == 1 {
			freeform = true
		}
		if id == "" || prompt == "" {
			continue
		}
		if len(options) == 0 {
			options = []AskOption{{Key: "__other__", Label: "请直接发送文字回答"}}
			freeform = true
		}
		questionIDs = append(questionIDs, id)
		questions = append(questions, AskQuestion{Prompt: prompt, Options: options})
	}
	return Event{
		Type:      EventAskUser,
		AskID:     stringField(params, "itemId"),
		Questions: questions,
		Freeform:  freeform,
		Source:    "codex",
		Reply: func(answers [][]string, cancelled bool) error {
			result := map[string]any{"answers": map[string]any{}}
			answerMap := result["answers"].(map[string]any)
			for i, qid := range questionIDs {
				var values []string
				if !cancelled && i < len(answers) {
					values = answers[i]
				}
				answerMap[qid] = map[string]any{"answers": values}
			}
			return r.reply(requestID, result)
		},
	}
}

var codexPermissionOptions = []AskOption{
	{Key: "once", Label: "允许一次"},
	{Key: "always", Label: "本会话始终允许"},
	{Key: "reject", Label: "拒绝"},
}

func (r *codexAppRun) codexCommandApprovalEvent(requestID any, params map[string]any) Event {
	var b strings.Builder
	b.WriteString("🔐 Codex 请求执行命令")
	appendMarkdownField(&b, "命令", stringField(params, "command"))
	appendMarkdownField(&b, "目录", stringField(params, "cwd"))
	appendMarkdownField(&b, "原因", stringField(params, "reason"))
	return r.codexDecisionEvent(requestID, params, b.String(), "command")
}

func (r *codexAppRun) codexFileApprovalEvent(requestID any, params map[string]any) Event {
	var b strings.Builder
	b.WriteString("🔐 Codex 请求修改文件")
	appendMarkdownField(&b, "授权目录", stringField(params, "grantRoot"))
	appendMarkdownField(&b, "原因", stringField(params, "reason"))
	return r.codexDecisionEvent(requestID, params, b.String(), "file")
}

func (r *codexAppRun) codexDecisionEvent(requestID any, params map[string]any, prompt, kind string) Event {
	return Event{
		Type:   EventAskUser,
		AskID:  codexAskID(params, requestID),
		Source: "codex-permission",
		Questions: []AskQuestion{{
			Prompt:  prompt,
			Options: append([]AskOption(nil), codexPermissionOptions...),
		}},
		Reply: func(answers [][]string, cancelled bool) error {
			decision := codexApprovalDecision(answers, cancelled)
			if kind == "command" {
				if !codexDecisionAvailable(params["availableDecisions"], decision) {
					if decision == "acceptForSession" {
						decision = "accept"
					}
				}
			}
			return r.reply(requestID, map[string]any{"decision": decision})
		},
	}
}

func codexAskID(params map[string]any, requestID any) string {
	if id := stringField(params, "approvalId", "itemId"); id != "" {
		return id
	}
	return "codex-" + rpcID(requestID)
}

func codexApprovalDecision(answers [][]string, cancelled bool) string {
	if cancelled || len(answers) == 0 || len(answers[0]) == 0 {
		return "decline"
	}
	switch strings.ToLower(strings.TrimSpace(answers[0][0])) {
	case "once", "允许一次":
		return "accept"
	case "always", "本会话始终允许", "始终允许":
		return "acceptForSession"
	default:
		return "decline"
	}
}

func codexDecisionAvailable(raw any, decision string) bool {
	values := anySlice(raw)
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if s, ok := v.(string); ok && s == decision {
			return true
		}
	}
	return false
}

func (r *codexAppRun) codexPermissionsApprovalEvent(requestID any, params map[string]any) Event {
	var b strings.Builder
	b.WriteString("🔐 Codex 请求额外权限")
	appendMarkdownField(&b, "目录", stringField(params, "cwd"))
	appendMarkdownField(&b, "原因", stringField(params, "reason"))
	permissions := mapField(params, "permissions")
	if fs := mapField(permissions, "fileSystem"); fs != nil {
		appendMarkdownList(&b, "读取", anyStrings(fs["read"]))
		appendMarkdownList(&b, "写入", anyStrings(fs["write"]))
	}
	if network := mapField(permissions, "network"); network != nil {
		if enabled, _ := network["enabled"].(bool); enabled {
			b.WriteString("\n**网络**: 允许")
		}
	}
	return Event{
		Type:   EventAskUser,
		AskID:  codexAskID(params, requestID),
		Source: "codex-permission",
		Questions: []AskQuestion{{
			Prompt:  b.String(),
			Options: append([]AskOption(nil), codexPermissionOptions...),
		}},
		Reply: func(answers [][]string, cancelled bool) error {
			decision := codexApprovalDecision(answers, cancelled)
			granted := map[string]any{}
			scope := "turn"
			if decision == "accept" || decision == "acceptForSession" {
				granted = permissions
			}
			if decision == "acceptForSession" {
				scope = "session"
			}
			return r.reply(requestID, map[string]any{
				"permissions": granted,
				"scope":       scope,
			})
		},
	}
}

func (r *codexAppRun) reply(id any, result map[string]any) error {
	r.mu.Lock()
	ended := r.ended
	r.mu.Unlock()
	if ended {
		return fmt.Errorf("Codex turn 已结束")
	}
	return r.write(map[string]any{"id": id, "result": result})
}

func appendMarkdownField(b *strings.Builder, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString("\n**")
	b.WriteString(name)
	b.WriteString("**: `")
	b.WriteString(truncateRunes(value, 600))
	b.WriteString("`")
}

func appendMarkdownList(b *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString("\n**")
	b.WriteString(name)
	b.WriteString("**:")
	for _, value := range values {
		b.WriteString("\n- `")
		b.WriteString(truncateRunes(value, 300))
		b.WriteString("`")
	}
}

func (r *codexAppRun) handleNotification(method string, params map[string]any) {
	switch method {
	case "item/agentMessage/delta":
		if delta := stringField(params, "delta"); delta != "" {
			r.emit(Event{Type: EventText, Delta: delta})
		}
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if delta := stringField(params, "delta"); delta != "" {
			r.emit(Event{Type: EventThinking, Delta: delta})
		}
	case "item/started":
		if evt, ok := codexAppToolUse(mapField(params, "item")); ok {
			r.emit(evt)
		}
	case "item/completed":
		item := mapField(params, "item")
		if evt, ok := codexAppToolResult(item); ok {
			r.emit(evt)
		}
		if stringField(item, "type") == "agentMessage" {
			if text := stringField(item, "text"); text != "" {
				r.emit(Event{Type: EventFinalText, Content: text})
			}
		}
	case "thread/tokenUsage/updated":
		if last := mapField(mapField(params, "tokenUsage"), "last"); last != nil {
			r.emit(Event{
				Type:                  EventUsage,
				InputTokens:           intField(last, "inputTokens"),
				OutputTokens:          intField(last, "outputTokens"),
				CachedInputTokens:     intField(last, "cachedInputTokens"),
				ReasoningOutputTokens: intField(last, "reasoningOutputTokens"),
			})
		}
	case "turn/completed":
		turn := mapField(params, "turn")
		status := stringField(turn, "status")
		if status == "failed" {
			message := "Codex turn failed"
			if e := mapField(turn, "error"); e != nil {
				message = stringField(e, "message")
			}
			r.finish(Event{Type: EventError, Message: message, TerminationReason: TermFailed})
		} else if status == "interrupted" {
			r.finish(Event{Type: EventDone, ThreadID: r.threadID, TerminationReason: TermInterrupted})
		} else {
			r.finish(Event{Type: EventDone, ThreadID: r.threadID, Model: r.model, TerminationReason: TermNormal})
		}
	case "error":
		message := stringField(mapField(params, "error"), "message")
		if message == "" {
			message = "Codex app-server error"
		}
		willRetry, _ := params["willRetry"].(bool)
		if !willRetry {
			r.finish(Event{Type: EventError, Message: message, TerminationReason: TermFailed})
		}
	}
}

func codexAppToolUse(item map[string]any) (Event, bool) {
	id := stringField(item, "id")
	if id == "" {
		return Event{}, false
	}
	switch stringField(item, "type") {
	case "commandExecution":
		return Event{Type: EventToolUse, ID: id, Name: "command_execution", Input: map[string]any{"command": stringField(item, "command")}}, true
	case "fileChange":
		return Event{Type: EventToolUse, ID: id, Name: "file_change", Input: map[string]any{"changes": item["changes"]}}, true
	case "mcpToolCall":
		return Event{Type: EventToolUse, ID: id, Name: "mcp:" + stringField(item, "server") + "/" + stringField(item, "tool"), Input: item["arguments"]}, true
	case "webSearch":
		return Event{Type: EventToolUse, ID: id, Name: "web_search", Input: map[string]any{"query": stringField(item, "query")}}, true
	case "collabAgentToolCall":
		return Event{Type: EventToolUse, ID: id, Name: "collab:" + stringField(item, "tool"), Input: map[string]any{"prompt": stringField(item, "prompt")}}, true
	}
	return Event{}, false
}

func codexAppToolResult(item map[string]any) (Event, bool) {
	use, ok := codexAppToolUse(item)
	if !ok {
		return Event{}, false
	}
	evt := Event{Type: EventToolResult, ID: use.ID}
	status := stringField(item, "status")
	evt.IsError = status == "failed" || status == "declined"
	switch stringField(item, "type") {
	case "commandExecution":
		evt.Output = stringField(item, "aggregatedOutput")
	case "fileChange":
		evt.Output = codexAppChangesOutput(item["changes"])
	case "mcpToolCall":
		if e := mapField(item, "error"); e != nil {
			evt.Output = stringField(e, "message")
			evt.IsError = true
		} else {
			evt.Output = codexJSON(item["result"])
		}
	case "webSearch":
		evt.Output = stringField(item, "query")
	case "collabAgentToolCall":
		evt.Output = codexJSON(item["agentsStates"])
	}
	return evt, true
}

func codexAppChangesOutput(value any) string {
	var out []string
	for _, raw := range anySlice(value) {
		change, _ := raw.(map[string]any)
		if change == nil {
			continue
		}
		out = append(out, stringField(change, "kind")+" "+stringField(change, "path"))
	}
	return strings.Join(out, "\n")
}

func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func anySlice(v any) []any {
	out, _ := v.([]any)
	return out
}

func anyStrings(v any) []string {
	var out []string
	for _, raw := range anySlice(v) {
		if s, ok := raw.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *codexAppRun) emit(evt Event) {
	defer func() { _ = recover() }()
	r.events <- evt
}

func (r *codexAppRun) finish(evt Event) {
	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return
	}
	r.ended = true
	r.mu.Unlock()
	r.emit(evt)
	_ = r.stdin.Close()
}

func (r *codexAppRun) failStartup(err error) {
	r.mu.Lock()
	r.stopped = true
	r.ended = true
	r.mu.Unlock()
	_ = r.stdin.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}

func (r *codexAppRun) waitLoop(stderr *synchronizedBuffer) {
	err := r.cmd.Wait()
	r.mu.Lock()
	expected := r.ended || r.stopped
	r.ended = true
	for id, ch := range r.pending {
		close(ch)
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if err != nil && !expected {
		var ee *exec.ExitError
		code := -1
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 500 {
			detail = detail[:500]
		}
		message := fmt.Sprintf("codex app-server exited with code %d", code)
		if detail != "" {
			message += ": " + detail
		}
		r.emit(Event{Type: EventError, Message: message, TerminationReason: TermFailed})
	}
	close(r.events)
	close(r.exited)
}

func (r *codexAppRun) Events() <-chan Event { return r.events }

func (r *codexAppRun) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	threadID, turnID := r.threadID, r.turnID
	r.mu.Unlock()

	if threadID != "" && turnID != "" {
		_, _ = r.request("turn/interrupt", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
		}, 2*time.Second)
	}
	select {
	case <-r.exited:
		return
	case <-time.After(r.grace):
	}
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-r.exited:
	case <-time.After(r.grace):
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		<-r.exited
	}
}

func (r *codexAppRun) WaitExit(timeoutMs int) bool {
	select {
	case <-r.exited:
		return true
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return false
	}
}
