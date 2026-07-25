package bridge

import (
	"context"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/card"
	"lark-coding-agent-bridge-go/internal/state"
)

const helpText = `**可用命令**

- /new, /reset — 清空当前会话，重新开始
- /cd <path> — 切换工作目录（会话重置）
- /ws list — 列出已保存的工作区
- /ws save <name> — 把当前目录保存为命名工作区
- /ws use <name> — 切换到命名工作区
- /ws remove <name> — 删除命名工作区
- /sessions — 列出命令行里已有的 agent 会话
- /bind <序号或id前缀> [--force] — 把当前聊天绑定到该会话
- /unbind — 解除当前聊天的会话绑定
- /stop — 停止当前运行
- /status — 查看 profile、agent、工作目录和会话状态
- /help — 显示本帮助

直接发消息即可与本地 agent 对话；群里需要 @我。图片和文件会下载到本地供 agent 使用。`

// handleCommand dispatches slash commands. Commands run outside the
// pending queue so /stop and /new can interrupt an in-flight run.
func (b *Bridge) handleCommand(msg *Message, content string) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return
	}
	cmd := fields[0]
	args := fields[1:]
	scope := msg.Scope()

	reply := func(text string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := b.Lark.SendText(ctx, msg.ChatID, text, msg.MessageID); err != nil {
			// reply target may be gone; fall back to a plain send
			_, _ = b.Lark.SendText(ctx, msg.ChatID, text, "")
		}
	}

	switch cmd {
	case "/new", "/reset":
		b.stopRun(scope)
		b.resetSession(scope)
		reply("✅ 已开启新会话")

	case "/stop":
		if b.stopRun(scope) {
			reply("⏹ 已停止当前运行")
		} else {
			reply("当前没有运行中的任务")
		}

	case "/cd":
		if len(args) == 0 {
			reply("用法：/cd <path>")
			return
		}
		dir, errMsg := validateWorkspace(args[0])
		if errMsg != "" {
			reply("⚠️ " + errMsg)
			return
		}
		b.stopRun(scope)
		b.Workspaces.SetCurrent(dir)
		_ = b.Workspaces.Flush()
		b.resetSession(scope)
		reply("✅ 已切换工作目录：\n`" + dir + "`\n（会话已重置）")

	case "/ws":
		b.handleWsCommand(msg, args, reply)

	case "/sessions":
		b.handleSessions(msg, reply)

	case "/bind":
		b.handleBind(msg, args, reply)

	case "/unbind":
		b.handleUnbind(msg, reply)

	case "/status":
		reply(b.statusText(scope))

	case "/help":
		reply(helpText)

	default:
		reply("未知命令 " + cmd + "，发 /help 查看可用命令")
	}
}

func (b *Bridge) handleWsCommand(msg *Message, args []string, reply func(string)) {
	scope := msg.Scope()
	if len(args) == 0 {
		reply("用法：/ws list | save <name> | use <name> | remove <name>")
		return
	}
	switch args[0] {
	case "list":
		named := b.Workspaces.List()
		if len(named) == 0 {
			reply("还没有保存的工作区。用 /ws save <name> 保存当前目录。")
			return
		}
		names := make([]string, 0, len(named))
		for n := range named {
			names = append(names, n)
		}
		sort.Strings(names)
		var sb strings.Builder
		sb.WriteString("**已保存的工作区**\n")
		current := b.Workspaces.Get()
		for _, n := range names {
			mark := ""
			if named[n] == current {
				mark = "  ← 当前"
			}
			sb.WriteString("- `" + n + "` → " + named[n] + mark + "\n")
		}
		reply(strings.TrimRight(sb.String(), "\n"))

	case "save":
		if len(args) < 2 {
			reply("用法：/ws save <name>")
			return
		}
		name := args[1]
		current := b.Workspaces.Get()
		if current == "" {
			reply("当前没有工作目录可保存")
			return
		}
		b.Workspaces.Save(name, current)
		_ = b.Workspaces.Flush()
		reply("✅ 已保存工作区 `" + name + "` → " + current)

	case "use":
		if len(args) < 2 {
			reply("用法：/ws use <name>")
			return
		}
		dir, ok := b.Workspaces.Lookup(args[1])
		if !ok {
			reply("工作区 `" + args[1] + "` 不存在，/ws list 查看")
			return
		}
		if _, errMsg := validateWorkspace(dir); errMsg != "" {
			reply("⚠️ " + errMsg)
			return
		}
		b.stopRun(scope)
		b.Workspaces.SetCurrent(dir)
		_ = b.Workspaces.Flush()
		b.resetSession(scope)
		reply("✅ 已切换到工作区 `" + args[1] + "`：\n" + dir + "\n（会话已重置）")

	case "remove":
		if len(args) < 2 {
			reply("用法：/ws remove <name>")
			return
		}
		if !b.Workspaces.Remove(args[1]) {
			reply("工作区 `" + args[1] + "` 不存在")
			return
		}
		_ = b.Workspaces.Flush()
		reply("✅ 已删除工作区 `" + args[1] + "`")

	default:
		reply("用法：/ws list | save <name> | use <name> | remove <name>")
	}
}

func (b *Bridge) statusText(scope string) string {
	var sb strings.Builder
	sb.WriteString("**状态**\n")
	sb.WriteString("- profile: `" + b.ProfileName + "`\n")
	sb.WriteString("- agent: " + b.Agent.DisplayName() + " (`" + b.Agent.ID() + "`)\n")
	if b.Bot != nil {
		sb.WriteString("- bot: " + b.Bot.AppName + " (`" + b.Bot.OpenID + "`)\n")
	}
	sb.WriteString("- 工作目录: `" + b.Workspaces.Get() + "`\n")
	if sess, ok := b.Sessions.Get(scope); ok && (sess.SessionID != "" || sess.ThreadID != "") {
		id := sess.SessionID
		if id == "" {
			id = sess.ThreadID
		}
		sb.WriteString("- 会话: `" + id + "`\n")
	} else {
		sb.WriteString("- 会话: （无，下条消息新建）\n")
	}
	b.runsMu.Lock()
	_, running := b.runs[scope]
	b.runsMu.Unlock()
	if running {
		sb.WriteString("- 运行状态: 运行中\n")
	} else {
		sb.WriteString("- 运行状态: 空闲\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// validateWorkspace checks a directory is usable as an agent cwd and
// rejects overly broad locations (ported from policy/workspace.ts).
func validateWorkspace(input string) (string, string) {
	dir := os.ExpandEnv(input)
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			if dir == "~" {
				dir = home
			} else if strings.HasPrefix(dir, "~/") {
				dir = home + dir[1:]
			}
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", "目录不存在：" + dir
	}
	if !info.IsDir() {
		return "", "不是目录：" + dir
	}

	home, _ := os.UserHomeDir()
	blocked := []string{"/", home, "/tmp", "/var", "/private/tmp", os.TempDir()}
	for _, b := range blocked {
		if b != "" && dir == b {
			return "", "目录范围过大，不能用作工作目录：" + dir
		}
	}
	return dir, ""
}

// HandleCardAction processes card button callbacks (⏹ stop / 💬 quick reply).
// Returns a toast message for the clicker, or "" for none.
func (b *Bridge) HandleCardAction(chatID, messageID, operatorID string, value map[string]any) string {
	cmd, _ := value["cmd"].(string)
	switch cmd {
	case "stop":
		if b.stopRunByCardMessage(messageID) {
			return "已停止当前运行"
		}
		return "任务已结束"
	case "quick_reply":
		if operatorID == "" {
			return ""
		}
		go b.startQuickReply(chatID, messageID, operatorID)
		return "💬 请查看机器人的私信"
	default:
		return ""
	}
}

// startQuickReply asks the user to reply in p2p, then routes their text
// back to the group chat. This avoids the @-mention requirement in groups.
func (b *Bridge) startQuickReply(groupChatID, cardMessageID, operatorID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Tell the user to reply in private chat (no @ needed in p2p).
	msgID, err := b.Lark.SendText(ctx, operatorID,
		"💬 你想让机器人做什么？请直接回复这条消息。\n\n回复后我会在群里继续处理。",
		"")
	if err != nil {
		log.Printf("[quick_reply] send p2p prompt to %s failed: %v", operatorID, err)
		return
	}

	// Register so the next p2p message from this user is forwarded to the group.
	b.pendingRepliesMu.Lock()
	if b.pendingReplies == nil {
		b.pendingReplies = map[string]*pendingReply{}
	}
	b.pendingReplies[operatorID] = &pendingReply{
		groupChatID:   groupChatID,
		cardMessageID: cardMessageID,
		promptMsgID:   msgID,
	}
	b.pendingRepliesMu.Unlock()
}

// consumePendingReply checks and removes a pending quick_reply registration.
func (b *Bridge) consumePendingReply(operatorID string) *pendingReply {
	b.pendingRepliesMu.Lock()
	defer b.pendingRepliesMu.Unlock()
	if b.pendingReplies == nil {
		return nil
	}
	pr, ok := b.pendingReplies[operatorID]
	if !ok {
		return nil
	}
	delete(b.pendingReplies, operatorID)
	return pr
}

// forwardToGroup takes the user's p2p reply text and runs it as a new
// agent in the original group chat, avoiding the @-mention requirement.
func (b *Bridge) forwardToGroup(groupChatID, cardMessageID, userOpenID, text string) {
	log.Printf("[quick_reply] forwarding from %s to group %s: %.60s", userOpenID, groupChatID, text)

	// Build a synthetic event to reuse the normal run pipeline.
	scope := groupChatID
	sess, _ := b.Sessions.Get(scope)
	cwd, err := b.resolveCwd()
	if err != nil {
		log.Printf("[quick_reply] resolveCwd: %v", err)
		return
	}

	runOpts := agent.RunOptions{
		RunID:     scope + "-qr-" + time.Now().Format("150405.000"),
		Scope:     scope,
		Prompt:    text,
		Cwd:       cwd,
		Access:    b.Profile.DefaultAccess(),
		SessionID: sess.SessionID,
		ThreadID:  sess.ThreadID,
	}

	run, err := b.Agent.Run(runOpts)
	if err != nil {
		log.Printf("[quick_reply] agent.Run: %v", err)
		return
	}

	b.runsMu.Lock()
	b.runs[scope] = &activeRun{run: run, scope: scope}
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.runs, scope)
		b.runsMu.Unlock()
	}()

	stream := card.NewStream(b.Lark, groupChatID, cardMessageID)
	runState := card.InitialState()
	if err := stream.Start(ctxb(), card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: true})); err != nil {
		run.Stop()
		log.Printf("[quick_reply] stream.Start: %v", err)
		return
	}

	b.runsMu.Lock()
	b.cardScopes[stream.MessageID()] = scope
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.cardScopes, stream.MessageID())
		b.runsMu.Unlock()
	}()

	newSess := state.Session{Cwd: cwd, Model: sess.Model}
	eventsCh := run.Events()
	for evt := range eventsCh {
		runState = runState.Reduce(evt)
		switch evt.Type {
		case agent.EventSystem:
			if evt.SessionID != "" {
				newSess.SessionID = evt.SessionID
			}
			if evt.ThreadID != "" {
				newSess.ThreadID = evt.ThreadID
			}
		case agent.EventDone:
			if evt.SessionID != "" {
				newSess.SessionID = evt.SessionID
			}
			if evt.ThreadID != "" {
				newSess.ThreadID = evt.ThreadID
			}
		}
		stream.Update(card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: true}))
	}

	runState = runState.FinalizeIfRunning()
	stream.Update(card.Render(runState, card.RenderOptions{GroupChat: true}))
	stream.Finish(runState.TextContent())

	if newSess.SessionID != "" || newSess.ThreadID != "" {
		b.Sessions.Set(scope, newSess)
	}
}

// ctxb returns a background context (convenience helper).
func ctxb() context.Context {
	return context.Background()
}
