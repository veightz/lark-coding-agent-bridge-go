package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

// 群聊相关：显式建群（/new chat）与 /open 跳转卡片。
// 私聊任务自动建群已由 ADR-0012 取消——私聊统一话题回复，不再启发式升级。

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
		// 私聊跳转卡：挂在用户原消息的话题下。
		_, err = b.Lark.SendCardByReference(ctx, p2pChatID, cardID, replyTo, true)
	}
	if err != nil {
		log.Printf("[jump-card] failed, fallback to text: %v", err)
		_, _ = b.Lark.SendText(ctx, p2pChatID, fallback, replyTo, true)
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

// titleKeywords 用于从长句里切出「动作词起」的标题段（仅 /new chat、/open 命名，不触发建群）。
var titleKeywords = []string{
	"帮我", "麻烦", "请帮", "帮忙",
	"实现", "修复", "写一个", "写个", "写一篇", "写一下",
	"创建", "新建", "部署", "分析", "总结", "翻译",
	"解释", "排查", "调试", "优化", "重构", "审查",
	"查一下", "查看", "看一下", "看看", "检查",
	"改成", "修改", "加上", "新增", "删除", "去掉",
	"跑一下", "运行", "执行", "整理", "对比", "比较", "设计",
	"fix", "bug", "implement", "create", "write", "refactor",
	"debug", "deploy", "review", "summarize", "translate",
	"analyze", "analyse", "optimize", "add", "remove",
	"update", "explain", "build",
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

// earliestKeywordIndex 返回 titleKeywords 在 lower 中最早出现的 byte 下标，未命中 -1。
func earliestKeywordIndex(lower string) int {
	best := -1
	for _, kw := range titleKeywords {
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
	if _, err := b.Lark.SendText(ctx, groupID, welcome.String(), "", false); err != nil {
		log.Printf("[new-chat] welcome message failed: %v", err)
	}

	b.sendGroupJumpCardEx(ctx, msg.ChatID, msg.MessageID, groupID, name, false)
}
