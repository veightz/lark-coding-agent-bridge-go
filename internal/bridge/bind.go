// Session binding commands: /sessions, /bind, /unbind, /open (ADR-0005 / ADR-0007).
// They let a chat attach to an existing CLI-originated agent session,
// with session↔scope dedup so one session never binds two chats.
// /open (private chat) reuses or creates a group for a session.
package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/state"
)

const sessionListLimit = 8

// handleSessions lists recent CLI sessions for this profile's agent.
func (b *Bridge) handleSessions(msg *Message, reply func(string)) {
	sessions, err := b.listExternalSessions()
	if err != nil {
		reply("⚠️ 扫描会话失败：" + err.Error())
		return
	}
	if len(sessions) == 0 {
		reply(fmt.Sprintf("没有找到 %s 的历史会话。", b.Agent.DisplayName()))
		return
	}
	var sb strings.Builder
	sb.WriteString("**最近的 " + b.Agent.DisplayName() + " 会话**\n\n")
	for i, s := range sessions {
		bound := ""
		if bind, ok := b.lookupSessionBinding(s.ID); ok {
			if bind.ChatType == "group" || bind.ChatType == "" {
				bound = " 🔗群 " + b.chatLabel(bind.ChatID)
			} else {
				bound = " 🔗" + b.chatLabel(bind.ChatID)
			}
		}
		fmt.Fprintf(&sb, "%d. `%s` %s\n    %s · %s%s\n",
			i+1, s.ShortID(), orEmpty(s.Preview, "（无摘要）"),
			orEmpty(s.Cwd, "未知目录"), s.UpdatedAt.Format("01-02 15:04"), bound)
	}
	sb.WriteString("\n绑定到当前聊天：`/bind <序号或id前缀>`\n")
	sb.WriteString("在群里续聊（复用/建群）：`/open <序号或id前缀>`\n")
	sb.WriteString("解绑：`/unbind`")
	reply(sb.String())
}

// handleBind binds the current chat scope to a discovered session.
func (b *Bridge) handleBind(msg *Message, args []string, reply func(string)) {
	if len(args) == 0 {
		reply("用法：/bind <序号或id前缀> [--force]；先用 /sessions 查看列表")
		return
	}
	force := false
	var token string
	for _, a := range args {
		if a == "--force" {
			force = true
		} else if token == "" {
			token = a
		}
	}

	sessions, err := b.listExternalSessions()
	if err != nil {
		reply("⚠️ 扫描会话失败：" + err.Error())
		return
	}
	match, candidates := agent.MatchSession(sessions, token)
	if match == nil {
		if len(candidates) > 1 {
			var sb strings.Builder
			sb.WriteString("匹配到多个会话，请用更长的前缀：\n")
			for _, c := range candidates {
				fmt.Fprintf(&sb, "- `%s` %s\n", c.ShortID(), orEmpty(c.Preview, c.Cwd))
			}
			reply(sb.String())
			return
		}
		reply("没有找到匹配 `" + token + "` 的会话，/sessions 查看列表")
		return
	}

	key := state.SessionKey(b.kind(), match.ID)
	scope := msg.Scope()

	// Dedup: already bound somewhere else → report, don't rebind (ADR-0005).
	if bind, ok := b.Bindings.Get(key); ok && bind.Scope != scope {
		if !force {
			reply(fmt.Sprintf("⚠️ 会话 `%s` 已绑定到 %s，不能直接重复绑定。\n如要把绑定迁到当前聊天，发：\n`/bind %s --force`",
				match.ShortID(), b.chatLabel(bind.ChatID), token))
			return
		}
		// force: clear the old scope's session + binding before rebinding.
		b.stopRun(bind.Scope)
		b.Sessions.Delete(bind.Scope)
		b.Bindings.Delete(key)
	}

	b.stopRun(scope)
	sess := state.Session{Cwd: match.Cwd}
	if b.kind() == config.AgentCodex {
		sess.ThreadID = match.ID
	} else {
		sess.SessionID = match.ID
	}
	b.Sessions.Set(scope, sess)
	_ = b.Sessions.Flush()
	chatType := msg.ChatType
	if chatType == "" {
		chatType = "p2p"
	}
	b.Bindings.Set(key, state.Binding{Scope: scope, ChatID: msg.ChatID, ChatType: chatType})
	_ = b.Bindings.Flush()

	reply(fmt.Sprintf("✅ 已绑定会话 `%s`\n目录：%s\n摘要：%s\n之后在这个聊天里发消息就会续接该会话。/unbind 可解绑。",
		match.ShortID(), orEmpty(match.Cwd, "未知"), orEmpty(match.Preview, "（无摘要）")))
}

// handleOpen reuses or creates a group for a discovered session (ADR-0007).
// Intended for p2p: list with /sessions, then /open <n> to jump into the
// group that owns that session. Already-bound groups are never recreated.
func (b *Bridge) handleOpen(msg *Message, args []string, reply func(string)) {
	if len(args) == 0 {
		reply("用法：/open <序号或id前缀>；先用 /sessions 查看列表")
		return
	}
	token := args[0]
	sessions, err := b.listExternalSessions()
	if err != nil {
		reply("⚠️ 扫描会话失败：" + err.Error())
		return
	}
	match, candidates := agent.MatchSession(sessions, token)
	if match == nil {
		if len(candidates) > 1 {
			var sb strings.Builder
			sb.WriteString("匹配到多个会话，请用更长的前缀：\n")
			for _, c := range candidates {
				fmt.Fprintf(&sb, "- `%s` %s\n", c.ShortID(), orEmpty(c.Preview, c.Cwd))
			}
			reply(sb.String())
			return
		}
		reply("没有找到匹配 `" + token + "` 的会话，/sessions 查看列表")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1) 已有群绑定且群仍可用 → 直接复用，发跳转卡片。
	if bind, ok := b.lookupSessionBinding(match.ID); ok {
		if groupID, name, reusable := b.reusableGroup(ctx, bind); reusable {
			// 确保 sessions.json 也指向该群（/open 后在群里续聊）。
			b.ensureGroupSession(groupID, match)
			b.sendGroupJumpCardEx(ctx, msg.ChatID, msg.MessageID, groupID, name, true)
			return
		}
		// 绑定指向的群已失效（解散/bot 被踢）→ 清掉旧绑定，走新建。
		if bind.ChatType == "group" || bind.ChatType == "" {
			log.Printf("[open] bound group %s unreachable, recreating for %s", bind.ChatID, match.ShortID())
			b.Bindings.Delete(state.SessionKey(b.kind(), match.ID))
			if bind.Scope != "" {
				b.Sessions.Delete(bind.Scope)
			}
		}
	}

	// 2) 新建群并绑定 session。
	name := groupNameFor(orEmpty(match.Preview, match.ShortID()))
	groupID, err := b.Lark.CreateChat(ctx, name, "会话续聊群 · "+match.ShortID(), []string{msg.SenderID})
	if err != nil {
		reply("❌ 创建群失败：" + err.Error() + "\n\n确认 bot 已开启 `im:chat` 权限。")
		return
	}
	b.ensureGroupSession(groupID, match)
	key := state.SessionKey(b.kind(), match.ID)
	// 若 session 曾绑在私聊等别处，换绑到新群（/open 的语义就是给 session 一个群）。
	if old, ok := b.Bindings.Get(key); ok && old.Scope != groupID {
		b.Sessions.Delete(old.Scope)
		_ = b.Sessions.Flush()
	}
	b.Bindings.Set(key, state.Binding{Scope: groupID, ChatID: groupID, ChatType: "group"})
	_ = b.Bindings.Flush()

	var welcome strings.Builder
	welcome.WriteString("🎉 群已建好，已绑定会话 `" + match.ShortID() + "`")
	if match.Cwd != "" {
		welcome.WriteString("\n工作目录：`" + match.Cwd + "`")
	}
	if match.Preview != "" {
		welcome.WriteString("\n摘要：" + match.Preview)
	}
	welcome.WriteString("\n\n@我 + 任意消息即可续聊该会话。")
	if _, err := b.Lark.SendText(ctx, groupID, welcome.String(), "", false); err != nil {
		log.Printf("[open] welcome message failed: %v", err)
	}
	b.sendGroupJumpCardEx(ctx, msg.ChatID, msg.MessageID, groupID, name, false)
	log.Printf("[open] session %s → group %s (%s)", match.ShortID(), groupID, name)
}

// lookupSessionBinding finds the binding for an agent session id, falling
// back to sessions.json when bindings.json has no entry (legacy data).
func (b *Bridge) lookupSessionBinding(sessionID string) (state.Binding, bool) {
	key := state.SessionKey(b.kind(), sessionID)
	if bind, ok := b.Bindings.Get(key); ok {
		return bind, true
	}
	// 回退：sessions.json 里 scope→sessionId，反查后补写 bindings。
	if scope, _, ok := b.Sessions.FindByAgentID(sessionID); ok {
		// scope 可能是 "chatID:threadID"；群 chatID 取冒号前缀。
		chatID := scope
		if i := strings.IndexByte(scope, ':'); i >= 0 {
			chatID = scope[:i]
		}
		// 私聊 scope 也可能落在 sessions 里，不当作可复用群。
		// 用 GetChat 判断：chat_mode=group 才算。
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		info, exists := b.Lark.GetChat(ctx, chatID)
		if !exists || info.ChatMode == "p2p" {
			return state.Binding{}, false
		}
		bind := state.Binding{Scope: scope, ChatID: chatID, ChatType: "group"}
		b.Bindings.Set(key, bind)
		_ = b.Bindings.Flush()
		return bind, true
	}
	return state.Binding{}, false
}

// reusableGroup reports whether a binding points at a live group chat.
func (b *Bridge) reusableGroup(ctx context.Context, bind state.Binding) (groupID, name string, ok bool) {
	if bind.ChatID == "" {
		return "", "", false
	}
	// 明确标成 p2p 的绑定不能当群复用。
	if bind.ChatType == "p2p" {
		return "", "", false
	}
	info, exists := b.Lark.GetChat(ctx, bind.ChatID)
	if !exists {
		return "", "", false
	}
	if info.ChatMode == "p2p" {
		return "", "", false
	}
	name = info.Name
	if name == "" {
		name = "会话群"
	}
	return bind.ChatID, name, true
}

// ensureGroupSession writes sessions.json so the group scope resumes the
// given external session (cwd + session/thread id).
func (b *Bridge) ensureGroupSession(groupID string, match *agent.ExternalSession) {
	sess := state.Session{Cwd: match.Cwd}
	if b.kind() == config.AgentCodex {
		sess.ThreadID = match.ID
	} else {
		sess.SessionID = match.ID
	}
	b.Sessions.Set(groupID, sess)
	_ = b.Sessions.Flush()
}

// handleUnbind detaches the current scope from its bound session.
func (b *Bridge) handleUnbind(msg *Message, reply func(string)) {
	scope := msg.Scope()
	removed := b.Bindings.DeleteByScope(scope)
	_ = b.Bindings.Flush()
	b.Sessions.Delete(scope)
	_ = b.Sessions.Flush()
	if len(removed) > 0 {
		reply("✅ 已解绑，下条消息将开启新会话")
	} else {
		reply("当前聊天没有绑定关系；已清除会话，下条消息重新开始")
	}
}

// listExternalSessions enumerates CLI sessions for this profile's agent.
func (b *Bridge) listExternalSessions() ([]agent.ExternalSession, error) {
	if lister, ok := b.Agent.(agent.SessionLister); ok {
		return lister.ListSessions(sessionListLimit)
	}
	return agent.ListSessions(b.kind(), sessionListLimit)
}

func (b *Bridge) kind() config.AgentKind {
	return b.Profile.AgentKind
}

// chatLabel renders a chat for dedup messages: name when resolvable.
func (b *Bridge) chatLabel(chatID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if name := b.Lark.GetChatName(ctx, chatID); name != "" {
		return "「" + name + "」(" + chatID + ")"
	}
	return "聊天 " + chatID
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
