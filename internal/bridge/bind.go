// Session binding commands: /sessions, /bind, /unbind (ADR-0005).
// They let a chat attach to an existing CLI-originated agent session,
// with session↔scope dedup so one session never binds two chats.
package bridge

import (
	"context"
	"fmt"
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
		if bind, ok := b.Bindings.Get(state.SessionKey(b.kind(), s.ID)); ok {
			bound = " 🔗" + b.chatLabel(bind.ChatID)
		}
		fmt.Fprintf(&sb, "%d. `%s` %s\n    %s · %s%s\n",
			i+1, s.ShortID(), orEmpty(s.Preview, "（无摘要）"),
			orEmpty(s.Cwd, "未知目录"), s.UpdatedAt.Format("01-02 15:04"), bound)
	}
	sb.WriteString("\n绑定：`/bind <序号或id前缀>`；解绑：`/unbind`")
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
	b.Bindings.Set(key, state.Binding{Scope: scope, ChatID: msg.ChatID})
	_ = b.Bindings.Flush()

	reply(fmt.Sprintf("✅ 已绑定会话 `%s`\n目录：%s\n摘要：%s\n之后在这个聊天里发消息就会续接该会话。/unbind 可解绑。",
		match.ShortID(), orEmpty(match.Cwd, "未知"), orEmpty(match.Preview, "（无摘要）")))
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
