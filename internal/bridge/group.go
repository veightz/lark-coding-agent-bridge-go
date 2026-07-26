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
	b.sendGroupJumpCard(ctx, msg.ChatID, msg.MessageID, groupID, name)

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

// sendGroupJumpCard 在私聊里发一张卡片，带一键跳转群会话的按钮。
func (b *Bridge) sendGroupJumpCard(ctx context.Context, p2pChatID, replyTo, groupID, groupName string) {
	cardJSON := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": "🚀 任务已在新群中启动"},
			"template": "blue",
		},
		"body": map[string]any{"elements": []map[string]any{
			{
				"tag":     "markdown",
				"content": "已创建群 **" + groupName + "**，任务正在群里运行，回复和进度都会更新到群里。",
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
		log.Printf("[escalate] jump card failed, fallback to text: %v", err)
		_, _ = b.Lark.SendText(ctx, p2pChatID,
			"🚀 已创建群「"+groupName+"」，任务正在群里运行。", replyTo)
	}
}

// groupNameFor 用任务文本生成群名（截断 20 字）。
func groupNameFor(content string) string {
	text := strings.Join(strings.Fields(strings.TrimSpace(content)), " ")
	if text == "" {
		return defaultGroupName()
	}
	if utf8.RuneCountInString(text) > 20 {
		text = string([]rune(text)[:20]) + "…"
	}
	return text
}

func defaultGroupName() string {
	now := time.Now()
	return fmt.Sprintf("任务 · %d-%d %02d:%02d",
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

	b.sendGroupJumpCard(ctx, msg.ChatID, msg.MessageID, groupID, name)
}
