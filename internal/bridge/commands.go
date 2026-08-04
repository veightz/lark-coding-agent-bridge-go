package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/ask"
	"lark-coding-agent-bridge-go/internal/card"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/policy"
	"lark-coding-agent-bridge-go/internal/state"
)

const helpText = `**可用命令**

- /new, /reset — 清空当前会话，重新开始
- /new chat [名称] — 新建群聊，自动拉你进群
- /cd <path> — 切换工作目录（会话重置）
- /ws list — 列出已保存的工作区
- /ws save <name> — 把当前目录保存为命名工作区
- /ws use <name> — 切换到命名工作区
- /ws remove <name> — 删除命名工作区
- /sessions — 列出命令行里已有的 agent 会话
- /bind <序号或id前缀> [--force] — 把当前聊天绑定到该会话
- /open <序号或id前缀> — 为该会话复用/新建群并给跳转按钮（私聊常用）
- /unbind — 解除当前聊天的会话绑定
- /stop — 停止当前运行
- /status — 查看 profile、agent、工作目录和会话状态
- /model — 用交互卡片切换当前会话模型
- /usage — 查看账户额度或本地使用统计
- /invite user @某人 — 允许对方私聊 bot（owner/admin）
- /invite admin @某人 — 添加管理员（owner/admin）
- /invite group — 开放当前群给群内所有人（owner/admin）
- /remove user|admin|group — 移出白名单（owner/admin）
- /help — 显示本帮助

默认仅应用 owner 可用；其他人需被 /invite。群里需要 @我（除非开启 autoReply）。`

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
		if _, err := b.Lark.SendText(ctx, msg.ChatID, text, msg.MessageID, msg.ReplyInThread()); err != nil {
			// reply target may be gone; fall back to a plain send
			_, _ = b.Lark.SendText(ctx, msg.ChatID, text, "", false)
		}
	}

	switch cmd {
	case "/new", "/reset":
		// /new chat [name] — 显式建群（对齐原版 TS）。
		if cmd == "/new" && len(args) > 0 && args[0] == "chat" {
			b.handleNewChat(msg, strings.Join(args[1:], " "), reply)
			return
		}
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

	case "/open":
		b.handleOpen(msg, args, reply)

	case "/unbind":
		b.handleUnbind(msg, reply)

	case "/status":
		reply(b.statusText(scope))

	case "/model":
		b.handleModel(msg, reply)

	case "/usage":
		b.handleUsage(reply)

	case "/invite":
		b.handleInvite(msg, args, reply)

	case "/remove":
		b.handleRemove(msg, args, reply)

	case "/help":
		reply(helpText)

	default:
		reply("未知命令 " + cmd + "，发 /help 查看可用命令")
	}
}

func (b *Bridge) handleUsage(reply func(string)) {
	provider, ok := b.Agent.(agent.UsageProvider)
	if !ok {
		reply("当前 agent（" + b.Agent.DisplayName() + "）暂不支持 /usage。")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	usage, err := provider.ReadUsage(ctx)
	if err != nil {
		reply("⚠️ 查询用量失败：" + err.Error())
		return
	}
	reply(formatUsage(usage, time.Now()))
}

func formatUsage(usage agent.UsageSnapshot, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("**用量统计**\n")
	if usage.Provider != "" {
		sb.WriteString("- 来源：" + usage.Provider + "\n")
	}
	if usage.Plan != "" {
		sb.WriteString("- 套餐：`" + usage.Plan + "`\n")
	}
	for _, limit := range usage.Limits {
		name := limit.Name
		if name == "" {
			name = limit.ID
		}
		if name == "" {
			name = "Codex"
		}
		sb.WriteString("- **" + name + "**\n")
		if limit.Primary != nil {
			sb.WriteString("  - " + formatUsageWindow(limit.Primary, now) + "\n")
		}
		if limit.Secondary != nil {
			sb.WriteString("  - " + formatUsageWindow(limit.Secondary, now) + "\n")
		}
		if limit.Credits != nil {
			switch {
			case limit.Credits.Unlimited:
				sb.WriteString("  - credits：无限\n")
			case limit.Credits.Balance != "":
				sb.WriteString("  - credits：`" + limit.Credits.Balance + "`\n")
			}
		}
	}
	if usage.ResetCredits != nil {
		sb.WriteString("- 可用额度重置次数：" + strconv.FormatInt(*usage.ResetCredits, 10) + "\n")
	}
	if usage.TokenSummary.LifetimeTokens != nil {
		sb.WriteString("- 累计 tokens：" + formatInteger(*usage.TokenSummary.LifetimeTokens) + "\n")
	}
	if usage.TokenSummary.PeakDailyTokens != nil {
		sb.WriteString("- 单日峰值 tokens：" + formatInteger(*usage.TokenSummary.PeakDailyTokens) + "\n")
	}
	if activity := usage.Activity; activity != nil {
		sb.WriteString("- 会话数：" + formatInteger(activity.Sessions) + "\n")
		sb.WriteString("- 消息数：" + formatInteger(activity.Messages) + "\n")
		sb.WriteString("- 输入 tokens：" + formatInteger(activity.InputTokens) + "\n")
		sb.WriteString("- 输出 tokens：" + formatInteger(activity.OutputTokens) + "\n")
		if activity.CachedInputTokens > 0 {
			sb.WriteString("- cache read：" + formatInteger(activity.CachedInputTokens) + "\n")
		}
		if activity.CacheWriteTokens > 0 {
			sb.WriteString("- cache write：" + formatInteger(activity.CacheWriteTokens) + "\n")
		}
		if activity.ReasoningOutputTokens > 0 {
			sb.WriteString("- reasoning tokens：" + formatInteger(activity.ReasoningOutputTokens) + "\n")
		}
		if activity.CostUSD > 0 {
			sb.WriteString(fmt.Sprintf("- 原生成本：`$%.4f`\n", activity.CostUSD))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatUsageWindow(window *agent.UsageWindow, now time.Time) string {
	label := formatWindowDuration(window.WindowDurationMin)
	remaining := 100 - window.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	text := fmt.Sprintf("%s：已用 %d%%，剩余 %d%%", label, window.UsedPercent, remaining)
	if window.ResetsAt > 0 {
		reset := time.Unix(window.ResetsAt, 0).In(now.Location())
		text += "；" + reset.Format("01-02 15:04") + " 重置"
	}
	return text
}

func formatWindowDuration(minutes int64) string {
	switch {
	case minutes > 0 && minutes%(24*60) == 0:
		return strconv.FormatInt(minutes/(24*60), 10) + " 天窗口"
	case minutes > 0 && minutes%60 == 0:
		return strconv.FormatInt(minutes/60, 10) + " 小时窗口"
	case minutes > 0:
		return strconv.FormatInt(minutes, 10) + " 分钟窗口"
	default:
		return "用量窗口"
	}
}

func formatInteger(value int64) string {
	raw := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(raw, "-") {
		start = 1
	}
	for i := len(raw) - 3; i > start; i -= 3 {
		raw = raw[:i] + "," + raw[i:]
	}
	return raw
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
	sb.WriteString("- scope: `" + scope + "`\n")
	if sess, ok := b.Sessions.Get(scope); ok && (sess.SessionID != "" || sess.ThreadID != "" || sess.Model != "") {
		id := sess.SessionID
		if id == "" {
			id = sess.ThreadID
		}
		if id != "" {
			sb.WriteString("- 会话: `" + id + "`\n")
		} else {
			sb.WriteString("- 会话: （尚未开始）\n")
		}
		if sess.Model != "" {
			sb.WriteString("- 模型: `" + sess.Model + "`\n")
		}
	} else {
		// 私聊主时间线每条消息是新话题/新会话；话题内才会续聊。
		sb.WriteString("- 会话: （无；私聊主时间线下一条新建，话题内续聊）\n")
	}
	oc := b.ownerControls()
	acc := b.policyAccess()
	owner := oc.BotOwnerID
	if owner == "" {
		owner = "（未解析，state=" + string(oc.OwnerState) + "）"
	}
	sb.WriteString("- access owner: `" + owner + "`\n")
	sb.WriteString(fmt.Sprintf("- access lists: users=%d chats=%d admins=%d\n",
		len(acc.AllowedUsers), len(acc.AllowedChats), len(acc.Admins)))
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

// requireAdmin replies and returns false when sender cannot manage access.
func (b *Bridge) requireAdmin(msg *Message, reply func(string)) bool {
	d := policy.CanRunAdminCommand(b.policyAccess(), b.ownerControls(), msg.SenderID)
	if d.OK {
		return true
	}
	reply("❌ 仅应用 owner 或管理员可执行此命令。")
	return false
}

func (b *Bridge) handleInvite(msg *Message, args []string, reply func(string)) {
	if !b.requireAdmin(msg, reply) {
		return
	}
	tokens := make([]string, 0, len(args))
	for _, a := range args {
		tokens = append(tokens, strings.ToLower(a))
	}

	// /invite all group — list API not wired; guide user.
	hasAll, hasGroup := false, false
	for _, t := range tokens {
		if t == "all" {
			hasAll = true
		}
		if t == "group" {
			hasGroup = true
		}
	}
	if hasAll && hasGroup {
		reply("当前版本请到目标群内发 `/invite group` 逐个加入（批量拉取 bot 所在群尚未接入）。")
		return
	}

	kind := ""
	for _, t := range tokens {
		switch t {
		case "user", "admin", "group":
			kind = t
		}
	}
	if kind == "" {
		reply("用法：\n" +
			"• `/invite user @某人` — 加入允许私聊\n" +
			"• `/invite admin @某人` — 加入管理员\n" +
			"• `/invite group` — 把当前群加入响应群名单")
		return
	}

	if kind == "group" {
		if msg.ChatType == "p2p" {
			reply("❌ `/invite group` 只能在群里发。")
			return
		}
		already := false
		if err := b.mutateAccess(func(a *config.ChatAccess) {
			next, added := addUnique(a.AllowedChats, msg.ChatID)
			a.AllowedChats = next
			already = !added
		}); err != nil {
			reply("❌ 保存失败：" + err.Error())
			return
		}
		if already {
			reply("✅ 当前群已在白名单里，无需重复添加。")
			return
		}
		reply("✅ 已把当前群（`" + msg.ChatID + "`）加入响应群名单。")
		return
	}

	targets := mentionUserTargets(msg)
	if len(targets) == 0 {
		reply("❌ 没检测到 @ 的用户。请像：`/invite " + kind + " @某人`（@ 用户不是 @ bot）。")
		return
	}
	var added, already []string
	err := b.mutateAccess(func(a *config.ChatAccess) {
		list := a.AllowedUsers
		if kind == "admin" {
			list = a.Admins
		}
		for _, t := range targets {
			next, wasAdded := addUnique(list, t.OpenID)
			list = next
			name := displayName(t)
			if wasAdded {
				added = append(added, name)
			} else {
				already = append(already, name)
			}
		}
		if kind == "admin" {
			a.Admins = list
		} else {
			a.AllowedUsers = list
		}
	})
	if err != nil {
		reply("❌ 保存失败：" + err.Error())
		return
	}
	label := "用户白名单"
	if kind == "admin" {
		label = "管理员"
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "✅ 已把 "+strings.Join(added, "、")+" 加入"+label+"。")
	}
	if len(already) > 0 {
		parts = append(parts, strings.Join(already, "、")+" 已在"+label+"里，跳过。")
	}
	reply(strings.Join(parts, "\n"))
}

func (b *Bridge) handleRemove(msg *Message, args []string, reply func(string)) {
	if !b.requireAdmin(msg, reply) {
		return
	}
	kind := ""
	for _, a := range args {
		switch strings.ToLower(a) {
		case "user", "admin", "group":
			kind = strings.ToLower(a)
		}
	}
	if kind == "" {
		reply("用法：\n" +
			"• `/remove user @某人`\n" +
			"• `/remove admin @某人`\n" +
			"• `/remove group` — 把当前群移出响应名单")
		return
	}
	if kind == "group" {
		if msg.ChatType == "p2p" {
			reply("`/remove group` 请在要移除的群里发。")
			return
		}
		missing := false
		if err := b.mutateAccess(func(a *config.ChatAccess) {
			next, removed := removeID(a.AllowedChats, msg.ChatID)
			a.AllowedChats = next
			missing = !removed
		}); err != nil {
			reply("❌ 保存失败：" + err.Error())
			return
		}
		if missing {
			reply("✅ 当前群本来就不在响应名单里。")
			return
		}
		reply("✅ 已把当前群移出响应群名单。")
		return
	}
	targets := mentionUserTargets(msg)
	if len(targets) == 0 {
		reply("请 @ 上要移除的人，例如：`/remove " + kind + " @某人`。")
		return
	}
	var removed, notThere []string
	err := b.mutateAccess(func(a *config.ChatAccess) {
		list := a.AllowedUsers
		if kind == "admin" {
			list = a.Admins
		}
		for _, t := range targets {
			next, was := removeID(list, t.OpenID)
			list = next
			name := displayName(t)
			if was {
				removed = append(removed, name)
			} else {
				notThere = append(notThere, name)
			}
		}
		if kind == "admin" {
			a.Admins = list
		} else {
			a.AllowedUsers = list
		}
	})
	if err != nil {
		reply("❌ 保存失败：" + err.Error())
		return
	}
	label := "用户白名单"
	if kind == "admin" {
		label = "管理员"
	}
	var parts []string
	if len(removed) > 0 {
		parts = append(parts, "✅ 已把 "+strings.Join(removed, "、")+" 移出"+label+"。")
	}
	if len(notThere) > 0 {
		parts = append(parts, strings.Join(notThere, "、")+" 本来就不在"+label+"里。")
	}
	reply(strings.Join(parts, "\n"))
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

// CardActionResult is the Feishu card.action.trigger response payload.
type CardActionResult struct {
	ToastKind string         // info | success | warning | error
	Toast     string         // empty = no toast
	Card      map[string]any // optional full card replace (ask settle / toggle)
}

// HandleCardAction processes card button callbacks (stop / refresh / model / ask).
// inputText / formValue carry card input element values (freeform asks).
func (b *Bridge) HandleCardAction(chatID, messageID, operatorID string, value map[string]any, inputText string, formValue map[string]any) CardActionResult {
	if !b.checkOperatorAccess(chatID, operatorID) {
		return CardActionResult{ToastKind: "error", Toast: "无权限操作"}
	}
	cmd, _ := value["cmd"].(string)
	switch cmd {
	case "stop":
		if b.stopRunByCardMessage(messageID) {
			return CardActionResult{ToastKind: "info", Toast: "已停止当前运行"}
		}
		return CardActionResult{ToastKind: "info", Toast: "该任务已结束（bridge 重启或卡片已过期），请发新消息重新开始"}
	case "refresh":
		return CardActionResult{ToastKind: "info", Toast: b.handleRefresh(chatID, messageID)}
	case modelSelectAction:
		return b.handleModelSelect(chatID, value)
	case ask.ActionSelect, ask.ActionToggle, ask.ActionSubmit, ask.ActionSubmitInput:
		kind, toast, card := b.HandleAskCardAction(operatorID, value, inputText, formValue)
		return CardActionResult{ToastKind: kind, Toast: toast, Card: card}
	default:
		return CardActionResult{}
	}
}

// handleRefresh re-renders the card in its current state with updated
// elapsed time, giving the user a live status check.
func (b *Bridge) handleRefresh(chatID, messageID string) string {
	b.runsMu.Lock()
	scope, ok := b.cardScopes[messageID]
	if !ok {
		b.runsMu.Unlock()
		return "该任务已结束（bridge 重启或卡片已过期），请发新消息重新开始"
	}
	ar, ok := b.runs[scope]
	b.runsMu.Unlock()
	if !ok || ar.runState == nil || ar.stream == nil {
		return "该任务已结束，请发新消息重新开始"
	}
	// Push a re-render with up-to-date elapsed time.
	ar.runState.Stats.DurationMs = time.Since(ar.startTime).Milliseconds()
	if tool := ar.runState.LastRunningTool(); tool != nil {
		tool.DurationMs = ar.runState.Stats.DurationMs
	}
	ar.stream.Update(card.Render(ar.runState, card.RenderOptions{StopButton: true}))
	return "🔄 已刷新"
}

// startQuickReply asks the user to reply in p2p, then routes their text
// back to the group chat. This avoids the @-mention requirement in groups.
func (b *Bridge) startQuickReply(groupChatID, cardMessageID, operatorID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Tell the user to reply in private chat (no @ needed in p2p).
	msgID, err := b.Lark.SendDirectText(ctx, operatorID,
		"💬 你想让机器人做什么？请直接回复这条消息。\n\n回复后我会在群里继续处理。")
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
		Model:     sess.Model,
		Access:    b.Profile.DefaultAccess(),
		SessionID: sess.SessionID,
		ThreadID:  sess.ThreadID,
	}

	run, err := b.Agent.Run(runOpts)
	if err != nil {
		log.Printf("[quick_reply] agent.Run: %v", err)
		return
	}

	startTime := time.Now()
	b.runsMu.Lock()
	b.runs[scope] = &activeRun{run: run, scope: scope}
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.runs, scope)
		b.runsMu.Unlock()
		if b.Ask != nil {
			b.Ask.InvalidateScope(scope, "run ended")
		}
	}()

	stream := b.makeRunCardStream(groupChatID, cardMessageID, false)
	runState := card.InitialState()
	if err := stream.Start(ctxb(), card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: true})); err != nil {
		run.Stop()
		log.Printf("[quick_reply] stream.Start: %v", err)
		return
	}
	b.runsMu.Lock()
	if ar, ok := b.runs[scope]; ok {
		ar.startTime = startTime
		ar.runState = runState
		ar.stream = stream
	}
	b.runsMu.Unlock()

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
		if evt.Type == agent.EventAskUser {
			if b.handleAskUser(scope, groupChatID, cardMessageID, false, evt) {
				if next, rotateErr := b.rotateRunCard(scope, groupChatID, cardMessageID, false, true, false, stream, runState); rotateErr != nil {
					log.Printf("[ask] rotate quick-reply run card failed: %v", rotateErr)
				} else {
					stream = next
				}
			}
			continue
		}
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

	runState.Stats.DurationMs = time.Since(startTime).Milliseconds()
	runState = runState.FinalizeIfRunning()
	stream.Update(card.Render(runState, card.RenderOptions{GroupChat: true}))
	stream.Finish(runState.TextContent())

	if latest, ok := b.Sessions.Get(scope); ok && latest.Model != sess.Model {
		newSess.Model = latest.Model
	}
	if newSess.SessionID != "" || newSess.ThreadID != "" {
		b.Sessions.Set(scope, newSess)
		b.recordGroupBinding(scope, groupChatID, "group", newSess)
		_ = b.Sessions.Flush()
		_ = b.Bindings.Flush()
	}
}

// ctxb returns a background context (convenience helper).
func ctxb() context.Context {
	return context.Background()
}
