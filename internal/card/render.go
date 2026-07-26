package card

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	reasoningMax          = 1500
	collapseToolThreshold = 3
	headerSummaryMax      = 80
	bodyFieldMax          = 600
	outputMax             = 1200
	bodyTotalMax          = 2500
)

// RenderOptions customizes the rendered card.
type RenderOptions struct {
	// StopButton adds the ⏹ callback button while running.
	StopButton bool
	// GroupChat adds the "继续对话" quick-reply button (group chats only).
	GroupChat bool
}

// Render builds the CardKit 2.0 card JSON for the current run state.
func Render(state *RunState, opts RenderOptions) map[string]any {
	var elements []map[string]any

	if state.Reasoning.Content != "" {
		title := "🧠 **思考完成，点击查看**"
		if state.Reasoning.Active {
			title = "🧠 **思考中**"
		}
		elements = append(elements, collapsiblePanel(title, state.Reasoning.Active, "grey",
			truncate(state.Reasoning.Content, reasoningMax)))
	}

	for _, group := range groupBlocks(state.Blocks) {
		if group.text != nil {
			if strings.TrimSpace(*group.text) != "" {
				elements = append(elements, markdown(*group.text))
			}
		} else {
			elements = append(elements, renderToolGroup(group.tools, state.Terminal != TerminalRunning)...)
		}
	}

	// Add stats line when run is finished.
	if state.Terminal != TerminalRunning && state.Stats.DurationMs > 0 {
		elements = append(elements, statsLine(&state.Stats))
	}

	switch state.Terminal {
	case TerminalInterrupted:
		elements = append(elements, noteMd("_⏹ 已被中断_"))
	case TerminalIdleTimeout:
		elements = append(elements, noteMd("_⏱ 长时间无响应，已自动终止_"))
	case TerminalError:
		if state.ErrorMsg != "" {
			elements = append(elements, noteMd("⚠️ agent 失败："+state.ErrorMsg))
		}
	case TerminalDone:
		if len(elements) == 0 {
			elements = append(elements, noteMd("_（未返回内容）_"))
		}
	}

	if state.Terminal == TerminalRunning {
		if state.Footer != FooterNone {
			elements = append(elements, footerStatus(state.Footer))
		}
		if opts.StopButton {
			elements = append(elements, stopButton())
		}
	} else if state.Terminal == TerminalDone && opts.GroupChat {
		// Finished card in a group chat: add quick-reply button so users
		// don't need to @-mention the bot. Clicks trigger a p2p prompt.
		elements = append(elements, quickReplyButton())
	}

	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"streaming_mode": state.Terminal == TerminalRunning,
			"summary":        map[string]string{"content": summaryText(state)},
		},
		"body": map[string]any{"elements": elements},
	}
}

type blockGroup struct {
	text  *string
	tools []*ToolEntry
}

func groupBlocks(blocks []Block) []blockGroup {
	var groups []blockGroup
	var toolBuf []*ToolEntry
	flushTools := func() {
		if len(toolBuf) > 0 {
			groups = append(groups, blockGroup{tools: toolBuf})
			toolBuf = nil
		}
	}
	for i := range blocks {
		b := &blocks[i]
		if b.Kind == BlockTool {
			toolBuf = append(toolBuf, b.Tool)
		} else {
			flushTools()
			content := b.Content
			groups = append(groups, blockGroup{text: &content})
		}
	}
	flushTools()
	return groups
}

func renderToolGroup(tools []*ToolEntry, finalized bool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	if len(tools) < collapseToolThreshold {
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			out = append(out, toolPanel(t, false))
		}
		return out
	}
	if finalized {
		return []map[string]any{collapsedToolSummary(tools, true)}
	}
	// Running: collapse prior tools, keep the latest visible.
	out := []map[string]any{collapsedToolSummary(tools[:len(tools)-1], false)}
	out = append(out, toolPanel(tools[len(tools)-1], true))
	return out
}

func toolPanel(tool *ToolEntry, expanded bool) map[string]any {
	border := "grey"
	if tool.Status == ToolError {
		border = "red"
	}
	body := toolBodyMd(tool)
	if body == "" {
		body = "_无输出_"
	}
	return collapsiblePanel(toolHeaderText(tool), expanded, border, body)
}

func collapsedToolSummary(tools []*ToolEntry, finalized bool) map[string]any {
	suffix := ""
	if finalized {
		suffix = "（已结束）"
	}
	title := "☕ **" + itoa(len(tools)) + " 个工具调用" + suffix + "**"
	var lines []string
	for _, t := range tools {
		lines = append(lines, "- "+toolHeaderText(t))
	}
	return map[string]any{
		"tag":      "collapsible_panel",
		"expanded": false,
		"header":   panelHeader(title),
		"border":   map[string]any{"color": "blue", "corner_radius": "5px"},
		"elements": []map[string]any{notationMd(strings.Join(lines, "\n"))},
	}
}

func collapsiblePanel(title string, expanded bool, border, body string) map[string]any {
	return map[string]any{
		"tag":      "collapsible_panel",
		"expanded": expanded,
		"header":   panelHeader(title),
		"border":   map[string]any{"color": border, "corner_radius": "5px"},
		"elements": []map[string]any{notationMd(body)},
	}
}

func panelHeader(titleMd string) map[string]any {
	return map[string]any{
		"title":          map[string]any{"tag": "markdown", "content": titleMd},
		"vertical_align": "center",
		"icon": map[string]any{
			"tag":   "standard_icon",
			"token": "down-small-ccm_outlined",
			"size":  "16px 16px",
		},
		"icon_position":       "follow_text",
		"icon_expanded_angle": -180,
	}
}

func markdown(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": MaskEmails(content)}
}

func noteMd(content string) map[string]any {
	return map[string]any{"tag": "markdown", "content": MaskEmails(content), "text_size": "notation"}
}

func notationMd(content string) map[string]any {
	return noteMd(content)
}

func stopButton() map[string]any {
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": "⏹ 终止"},
		"type": "danger",
		"behaviors": []map[string]any{{
			"type":  "callback",
			"value": map[string]any{"cmd": "stop"},
		}},
	}
}

func quickReplyButton() map[string]any {
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": "💬 继续对话"},
		"type": "primary",
		"behaviors": []map[string]any{{
			"type":  "callback",
			"value": map[string]any{"cmd": "quick_reply"},
		}},
	}
}

func footerStatus(status FooterStatus) map[string]any {
	text := "✍️ 正在输出"
	switch status {
	case FooterThinking:
		text = "🧠 正在思考"
	case FooterToolRunning:
		text = "🧰 正在调用工具"
	}
	return noteMd(text)
}

func summaryText(state *RunState) string {
	switch state.Terminal {
	case TerminalInterrupted:
		return "已中断"
	case TerminalIdleTimeout:
		return "已超时"
	case TerminalError:
		return "出错"
	case TerminalDone:
		if state.Stats.DurationMs > 0 {
			dur := formatDuration(state.Stats.DurationMs)
			if state.Stats.UsageAvailable {
				return formatRunStats(dur, &state.Stats)
			}
			return "已完成 · " + dur
		}
		return "已完成"
	}
	switch state.Footer {
	case FooterToolRunning:
		return "正在调用工具"
	case FooterStreaming:
		return "正在输出"
	}
	return "思考中"
}

func statsLine(stats *RunStats) map[string]any {
	parts := []string{"⏱ " + formatDuration(stats.DurationMs)}
	if stats.UsageAvailable || stats.InputTokens+stats.OutputTokens > 0 {
		total := stats.InputTokens + stats.OutputTokens
		if stats.CachedInputTokens > 0 {
			parts = append(parts, fmt.Sprintf("🔤 %d token (缓存 %d)", total, stats.CachedInputTokens))
		} else {
			parts = append(parts, fmt.Sprintf("🔤 %d token", total))
		}
		if stats.CostUSD > 0 {
			parts = append(parts, fmt.Sprintf("💰 $%.4f", stats.CostUSD))
		}
	}
	return noteMd(strings.Join(parts, " · "))
}

func formatDuration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		m := ms / 60000
		s := (ms % 60000) / 1000
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
}

func formatRunStats(dur string, stats *RunStats) string {
	total := stats.InputTokens + stats.OutputTokens
	if total == 0 {
		return "已完成 · " + dur
	}
	summary := fmt.Sprintf("已完成 · %s · %d token", dur, total)
	if stats.CostUSD > 0 {
		summary += fmt.Sprintf(" · $%.4f", stats.CostUSD)
	}
	return summary
}

// ─── tool body rendering (ported from tool-render.ts) ─────────────

func toolHeaderText(tool *ToolEntry) string {
	icon := "⏳"
	switch tool.Status {
	case ToolDone:
		icon = "✅"
	case ToolError:
		icon = "❌"
	}
	if summary := summarizeInput(tool.Name, tool.Input); summary != "" {
		return icon + " **" + tool.Name + "** — " + summary
	}
	return icon + " **" + tool.Name + "**"
}

func toolBodyMd(tool *ToolEntry) string {
	var parts []string
	if inputMd := renderInput(tool); inputMd != "" {
		parts = append(parts, inputMd)
	}
	if tool.Output != "" {
		truncated := truncate(tool.Output, outputMax)
		if tool.Status == ToolError {
			parts = append(parts, "**Error**\n```\n"+truncated+"\n```")
		} else {
			parts = append(parts, "**Output**\n```\n"+truncated+"\n```")
		}
	} else if tool.Status == ToolRunning {
		parts = append(parts, "_运行中…_")
	}
	body := strings.Join(parts, "\n\n")
	if len(body) <= bodyTotalMax {
		return body
	}
	return body[:bodyTotalMax] + "…\n\n_（body 已截断）_"
}

func inputString(input any, key string, max int) string {
	rec, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	v, ok := rec[key].(string)
	if !ok {
		return ""
	}
	oneLine := strings.Join(strings.Fields(v), " ")
	return truncate(oneLine, max)
}

func summarizeInput(name string, input any) string {
	pick := func(key string, max ...int) string {
		m := headerSummaryMax
		if len(max) > 0 {
			m = max[0]
		}
		return inputString(input, key, m)
	}
	switch name {
	case "Bash", "command_execution":
		return pick("command")
	case "Read", "Edit", "Write", "NotebookEdit":
		return pick("file_path")
	case "Grep":
		pat := pick("pattern", 40)
		if path := pick("path", 30); path != "" {
			return pat + " in " + path
		}
		return pat
	case "Glob":
		return pick("pattern")
	case "WebFetch":
		return pick("url")
	case "WebSearch":
		return pick("query", 60)
	case "Agent", "Task":
		if d := pick("description"); d != "" {
			return d
		}
		return pick("subagent_type")
	default:
		for _, k := range []string{"command", "file_path", "path", "query"} {
			if v := pick(k); v != "" {
				return v
			}
		}
		return ""
	}
}

func renderInput(tool *ToolEntry) string {
	str := func(k string) string { return inputString(tool.Input, k, 1<<30) }
	switch tool.Name {
	case "Bash", "command_execution":
		if cmd := str("command"); cmd != "" {
			return "**Command**\n```bash\n" + truncate(cmd, bodyFieldMax) + "\n```"
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if fp := str("file_path"); fp != "" {
			return "**File** `" + fp + "`"
		}
	case "Grep":
		var lines []string
		if p := str("pattern"); p != "" {
			lines = append(lines, "**Pattern** `"+p+"`")
		}
		if p := str("path"); p != "" {
			lines = append(lines, "**Path** `"+p+"`")
		}
		return strings.Join(lines, "\n")
	case "WebFetch":
		if u := str("url"); u != "" {
			return "**URL** " + u
		}
	case "WebSearch":
		if q := str("query"); q != "" {
			return "**Query** " + truncate(q, bodyFieldMax)
		}
	}
	return ""
}

// ─── helpers ──────────────────────────────────────────────────────

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// emailRe matches email addresses for masking. Feishu's tenant audit
// rejects streamed cards containing raw emails (400 EMAIL_ADDRESS).
var emailRe = regexp.MustCompile(`([A-Za-z0-9._%+-]{2})[A-Za-z0-9._%+-]*(@[A-Za-z0-9.-]+\.[A-Za-z]{2,})`)

// MaskEmails masks email local-parts in s: "ab***@example.com".
func MaskEmails(s string) string {
	return emailRe.ReplaceAllString(s, "${1}***${2}")
}
