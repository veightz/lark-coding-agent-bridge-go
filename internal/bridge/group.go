package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

// p2p 私聊任务自动建群流程（ADR-0006）：
//
//	用户私聊发出明确任务
//	  → 创建专属群（bot + 用户）
//	  → 私聊里发跳转卡片（一键进群）
//	  → 群里发欢迎消息 + 任务上下文
//	  → 任务消息转入群 scope 的队列，自动启动运行
//
// 建群失败（如缺 im:chat 权限）时降级为私聊里直接处理，不丢消息。

// escalationWindow 是「跟发消息并入同一群」的时间窗：建群后的窗口期
// 内，同一私聊的后续消息视为同一任务的一部分，进入同一个群。
const escalationWindow = 15 * time.Second

// escalation 记录一次私聊 → 群的升级。
type escalation struct {
	done    chan struct{} // 建群完成后关闭
	groupID string
	err     error
	at      time.Time
}

// escalateP2P 处理私聊消息的建群升级。返回 true 表示消息已被接管
// （转入群 scope 或排队等待建群结果），调用方不要再走私聊流程。
func (b *Bridge) escalateP2P(msg *Message) bool {
	// 窗口期内的跟发消息：并入刚刚创建的群。
	if esc := b.recentEscalation(msg.ChatID); esc != nil {
		select {
		case <-esc.done:
		case <-time.After(10 * time.Second):
		}
		if esc.err == nil && esc.groupID != "" {
			b.pushToGroup(esc.groupID, msg)
		} else {
			// 建群失败，回退到私聊直接处理。
			b.pending.Push(msg.Scope(), msg)
		}
		return true
	}

	// 私聊被显式绑定到某个 CLI 会话时，尊重绑定，不升级建群。
	if b.Bindings.HasScope(msg.ChatID) {
		return false
	}
	if !LooksLikeTask(msg.Content, len(msg.Resources) > 0) {
		return false
	}

	esc := &escalation{done: make(chan struct{}), at: time.Now()}
	b.escalationsMu.Lock()
	if b.escalations == nil {
		b.escalations = map[string]*escalation{}
	}
	b.escalations[msg.ChatID] = esc
	b.escalationsMu.Unlock()

	go b.runEscalation(msg, esc)
	return true
}

// recentEscalation 返回时间窗内该私聊的升级记录，顺带清理过期记录。
func (b *Bridge) recentEscalation(p2pChatID string) *escalation {
	b.escalationsMu.Lock()
	defer b.escalationsMu.Unlock()
	esc, ok := b.escalations[p2pChatID]
	if !ok {
		return nil
	}
	if time.Since(esc.at) > escalationWindow {
		delete(b.escalations, p2pChatID)
		return nil
	}
	return esc
}

func (b *Bridge) runEscalation(msg *Message, esc *escalation) {
	defer close(esc.done)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := groupNameFor(msg.Content)
	groupID, err := b.Lark.CreateChat(ctx, name, "由私聊任务自动创建", []string{msg.SenderID})
	if err != nil {
		esc.err = err
		log.Printf("[escalate] create chat failed: %v", err)
		_, _ = b.Lark.SendText(ctx, msg.ChatID,
			"⚠️ 自动建群失败（确认 bot 已开启 im:chat 权限），改为在私聊里处理。\n原因："+err.Error(),
			msg.MessageID)
		b.pending.Push(msg.Scope(), msg)
		return
	}
	esc.groupID = groupID
	log.Printf("[escalate] p2p %s → group %s (%s)", msg.ChatID, groupID, name)

	// 群里的第一条消息：欢迎 + 任务上下文。
	var welcome strings.Builder
	welcome.WriteString("🎉 群已建好")
	if cwd := b.Workspaces.Get(); cwd != "" {
		welcome.WriteString("，工作目录：`" + cwd + "`")
	}
	if task := strings.TrimSpace(msg.Content); task != "" {
		welcome.WriteString("\n\n📌 任务：" + task)
	}
	if _, err := b.Lark.SendText(ctx, groupID, welcome.String(), ""); err != nil {
		log.Printf("[escalate] welcome message failed: %v", err)
	}

	// 私聊里的跳转卡片。
	b.sendGroupJumpCardEx(ctx, msg.ChatID, msg.MessageID, groupID, name, false)

	// 任务转入群 scope，自动启动运行。
	b.pushToGroup(groupID, msg)
}

// pushToGroup 把一条私聊消息改写为群消息，送入群 scope 的去抖队列。
// MessageID/Resources 保留原值，附件仍可从原私聊消息下载。
func (b *Bridge) pushToGroup(groupID string, msg *Message) {
	synthetic := *msg
	synthetic.ChatID = groupID
	synthetic.ChatType = "group"
	synthetic.ThreadID = ""
	b.pending.Push(synthetic.Scope(), &synthetic)
}

// sendGroupJumpCard 在私聊里发一张卡片，带一键跳转群会话的按钮（新建群）。
func (b *Bridge) sendGroupJumpCard(ctx context.Context, p2pChatID, replyTo, groupID, groupName string) {
	b.sendGroupJumpCardEx(ctx, p2pChatID, replyTo, groupID, groupName, false)
}

// sendGroupJumpCardEx 发送跳转卡片。reused=true 表示复用已有绑定群（不新建）。
func (b *Bridge) sendGroupJumpCardEx(ctx context.Context, p2pChatID, replyTo, groupID, groupName string, reused bool) {
	title := "🚀 任务已在新群中启动"
	body := "已创建群 **" + groupName + "**，任务正在群里运行，回复和进度都会更新到群里。"
	fallback := "🚀 已创建群「" + groupName + "」，任务正在群里运行。"
	if reused {
		title = "✅ 群已存在，可直接进入"
		body = "会话已绑定到群 **" + groupName + "**，无需新建。点击下方按钮进入群聊续聊。"
		fallback = "✅ 会话已绑定群「" + groupName + "」，请直接进入该群续聊。"
	}
	cardJSON := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": "blue",
		},
		"body": map[string]any{"elements": []map[string]any{
			{
				"tag":     "markdown",
				"content": body,
			},
			{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": "👉 进入群聊"},
				"type": "primary",
				"behaviors": []map[string]any{{
					"type":        "open_url",
					"default_url": "https://applink.feishu.cn/client/chat/open?openChatId=" + groupID,
				}},
			},
		}},
	}
	cardID, err := b.Lark.CreateCard(ctx, cardJSON)
	if err == nil {
		_, err = b.Lark.SendCardByReference(ctx, p2pChatID, cardID, replyTo)
	}
	if err != nil {
		log.Printf("[jump-card] failed, fallback to text: %v", err)
		_, _ = b.Lark.SendText(ctx, p2pChatID, fallback, replyTo)
	}
}

// groupNameMaxRunes 是群名中「标题」部分的最大字数（不含「任务 · 」前缀）。
// 飞书群名上限更宽，这里刻意压短，方便在会话列表里扫一眼。
const groupNameMaxRunes = 16

// leadInPrefixes 是任务句常见的起手废话，生成群名时剥掉。
var leadInPrefixes = []string{
	"帮我", "麻烦你", "麻烦", "请帮我", "请帮", "请你", "帮忙",
	"能不能", "可以帮我", "劳驾", "拜托",
	"hey ", "hi ", "hello ", "please ",
}

// groupNameFor 从用户消息提炼短群名：任务 · {标题}。
// 避免把整段闲聊/联调话原样塞进群名（ADR-0006 体验补丁）。
func groupNameFor(content string) string {
	title := extractTaskTitle(content)
	if title == "" {
		return defaultGroupName()
	}
	return "任务 · " + truncateRunes(title, groupNameMaxRunes)
}

// extractTaskTitle 从任务原文抽出适合当群名的短语。
func extractTaskTitle(content string) string {
	text := strings.TrimSpace(content)
	if text == "" {
		return ""
	}
	// 只取第一行 / 第一句，丢掉后文铺垫。
	text = firstSegment(text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}

	// 剥「帮我/请帮…」等起手。
	runes := []rune(text)
	lower := strings.ToLower(text)
	for _, p := range leadInPrefixes {
		pl := strings.ToLower(p)
		if strings.HasPrefix(lower, pl) {
			rest := strings.TrimSpace(string(runes[len([]rune(p)):]))
			// 去掉起手后可能残留的「一下/下」
			rest = strings.TrimPrefix(rest, "一下")
			rest = strings.TrimPrefix(rest, "下")
			rest = strings.TrimSpace(rest)
			if rest != "" {
				text = rest
				lower = strings.ToLower(text)
				runes = []rune(text)
			}
			break
		}
	}

	// 若正文里仍有动作关键词，从最早命中处起取（「先说明背景，帮我修复 X」→ 再剥起手）。
	if idx := earliestKeywordIndex(lower); idx > 0 {
		// idx 是 byte index on lower；本关键词表大小写折叠不改变长度，可按 byte 对齐。
		cut := byteIndexToRuneOffset(text, idx)
		if cut > 0 && cut < len(runes) {
			text = strings.TrimSpace(string(runes[cut:]))
			lower = strings.ToLower(text)
			runes = []rune(text)
		}
	}

	// 关键词切片后可能又以「帮我」开头，再剥一次。
	for _, p := range leadInPrefixes {
		pl := strings.ToLower(p)
		if strings.HasPrefix(lower, pl) {
			rest := strings.TrimSpace(string(runes[len([]rune(p)):]))
			rest = strings.TrimPrefix(rest, "一下")
			rest = strings.TrimPrefix(rest, "下")
			rest = strings.TrimSpace(rest)
			if rest != "" {
				text = rest
			}
			break
		}
	}

	text = strings.TrimSpace(text)
	// 去掉收尾标点
	text = strings.TrimRight(text, "。.!！?？…~～,，、;；:：")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return text
}

// firstSegment 取第一行，或到第一个强句号为止。
func firstSegment(s string) string {
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	// 中英文句号 / 问号 / 感叹号
	for _, sep := range []string{"。", "！", "？", "!", "?"} {
		if i := strings.Index(s, sep); i >= 0 {
			// 保留很短前缀时不切（避免「OK。帮我…」被切成 OK）
			head := strings.TrimSpace(s[:i])
			if utf8.RuneCountInString(head) >= 4 {
				return head
			}
		}
	}
	return s
}

// earliestKeywordIndex 返回 taskKeywords 在 lower 中最早出现的 byte 下标，未命中 -1。
func earliestKeywordIndex(lower string) int {
	best := -1
	for _, kw := range taskKeywords {
		k := strings.ToLower(kw)
		if i := strings.Index(lower, k); i >= 0 {
			if best < 0 || i < best {
				best = i
			}
		}
	}
	return best
}

func byteIndexToRuneOffset(s string, byteIdx int) int {
	if byteIdx <= 0 {
		return 0
	}
	if byteIdx >= len(s) {
		return utf8.RuneCountInString(s)
	}
	return utf8.RuneCountInString(s[:byteIdx])
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

func defaultGroupName() string {
	now := time.Now()
	return fmt.Sprintf("任务 · %d/%d %02d:%02d",
		int(now.Month()), now.Day(), now.Hour(), now.Minute())
}

// handleNewChat 实现 /new chat [name]：显式建群（对齐原版 TS 行为）。
func (b *Bridge) handleNewChat(msg *Message, name string, reply func(string)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if name == "" {
		name = defaultGroupName()
	}
	groupID, err := b.Lark.CreateChat(ctx, name, "", []string{msg.SenderID})
	if err != nil {
		reply("❌ 创建群失败：" + err.Error() + "\n\n确认 bot 已开启 `im:chat` 权限。")
		return
	}

	var welcome strings.Builder
	welcome.WriteString("🎉 群已建好")
	if cwd := b.Workspaces.Get(); cwd != "" {
		welcome.WriteString("，工作目录：`" + cwd + "`")
	}
	welcome.WriteString("\n\n@我 + 任意消息开始对话。")
	if _, err := b.Lark.SendText(ctx, groupID, welcome.String(), ""); err != nil {
		log.Printf("[new-chat] welcome message failed: %v", err)
	}

	b.sendGroupJumpCardEx(ctx, msg.ChatID, msg.MessageID, groupID, name, false)
}
