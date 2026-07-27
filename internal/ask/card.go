package ask

import (
	"fmt"
	"strings"
	"time"
)

// BuildCard renders a CardKit 2.0 JSON document for an ask (or its settled state).
func BuildCard(p *Pending, result *Result) map[string]any {
	title := "❓ 需要你的选择"
	headerTpl := "blue"
	if result != nil {
		switch result.Kind {
		case KindAnswered:
			title = "✅ 已选择"
			headerTpl = "green"
		case KindTimedOut:
			title = "⏰ 提问已超时"
			headerTpl = "orange"
		case KindInvalidated:
			title = "失效的提问"
			headerTpl = "grey"
		}
	}

	elements := []any{
		map[string]any{
			"tag": "markdown",
			"content": fmt.Sprintf(
				"**截止** %s · 来源 `%s`",
				p.DeadlineAt.Local().Format("15:04:05"),
				orDefault(p.Source, "agent"),
			),
		},
	}

	if result != nil {
		elements = append(elements, map[string]any{"tag": "hr"})
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": settleSummary(p, result),
		})
	} else {
		elements = append(elements, map[string]any{"tag": "hr"})
		requiresSubmit := len(p.Questions) > 1 || anyMulti(p.Questions)
		for i, q := range p.Questions {
			prompt := truncate(q.Prompt, 800)
			elements = append(elements, map[string]any{
				"tag":     "markdown",
				"content": fmt.Sprintf("**问题 %d**\n%s", i+1, prompt),
			})
			selected := setOf(nil)
			if i < len(p.Selections) {
				selected = setOf(p.Selections[i])
			}
			for _, opt := range q.Options {
				label := opt.Label
				if label == "" {
					label = opt.Key
				}
				btnType := "default"
				value := map[string]any{}
				if requiresSubmit {
					if _, ok := selected[opt.Key]; ok {
						btnType = "primary"
						if q.MultiSelect {
							label = "☑ " + label
						} else {
							label = "● " + label
						}
					} else if q.MultiSelect {
						label = "☐ " + label
					} else {
						label = "○ " + label
					}
					value = map[string]any{
						"cmd":            ActionToggle,
						"ask_id":         p.ID,
						"nonce":          p.Nonce,
						"question_index": i,
						"key":            opt.Key,
					}
				} else {
					value = map[string]any{
						"cmd":    ActionSelect,
						"ask_id": p.ID,
						"nonce":  p.Nonce,
						"key":    opt.Key,
					}
				}
				elements = append(elements, optionButton(label, btnType, value))
			}
		}
		if requiresSubmit {
			elements = append(elements, map[string]any{"tag": "hr"})
			elements = append(elements, optionButton("提交选择", "primary", map[string]any{
				"cmd":    ActionSubmit,
				"ask_id": p.ID,
				"nonce":  p.Nonce,
			}))
		}
		hint := fmt.Sprintf(
			"<font color='grey'>超时约 %s 后自动取消。点选后 agent 将继续。</font>",
			formatDuration(time.Until(p.DeadlineAt)),
		)
		if p.Freeform {
			hint = fmt.Sprintf(
				"<font color='grey'>也可直接在聊天里回复文字作答。超时约 %s 后自动取消。</font>",
				formatDuration(time.Until(p.DeadlineAt)),
			)
		}
		elements = append(elements, map[string]any{
			"tag":       "markdown",
			"content":   hint,
			"text_size": "notation",
		})
	}

	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
		},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": headerTpl,
		},
		"body": map[string]any{
			"elements": elements,
		},
	}
}

func optionButton(label, typ string, value map[string]any) map[string]any {
	if len([]rune(label)) > 40 {
		r := []rune(label)
		label = string(r[:37]) + "…"
	}
	return map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": label},
		"type": typ,
		"behaviors": []map[string]any{{
			"type":  "callback",
			"value": value,
		}},
	}
}

func settleSummary(p *Pending, result *Result) string {
	switch result.Kind {
	case KindTimedOut:
		return "未在时限内作答，agent 侧将按超时处理。"
	case KindInvalidated:
		reason := result.Reason
		if reason == "" {
			reason = "已取消"
		}
		return "提问已失效：" + reason
	case KindAnswered:
		var b strings.Builder
		for i, q := range p.Questions {
			prompt := truncate(q.Prompt, 120)
			b.WriteString(fmt.Sprintf("**Q%d** %s\n", i+1, prompt))
			keys := []string{}
			if i < len(result.Answers) {
				keys = result.Answers[i]
			}
			if len(keys) == 0 {
				if result.Comment != "" {
					b.WriteString("> " + result.Comment + "\n\n")
				} else {
					b.WriteString("> _(未选)_\n\n")
				}
				continue
			}
			labels := make([]string, 0, len(keys))
			for _, k := range keys {
				labels = append(labels, labelFor(q, k))
			}
			b.WriteString("> " + strings.Join(labels, ", ") + "\n\n")
		}
		return strings.TrimSpace(b.String())
	default:
		return ""
	}
}

func labelFor(q Question, key string) string {
	for _, o := range q.Options {
		if o.Key == key {
			if o.Label != "" {
				return o.Label
			}
			return o.Key
		}
	}
	return key
}

func anyMulti(qs []Question) bool {
	for _, q := range qs {
		if q.MultiSelect {
			return true
		}
	}
	return false
}

func setOf(keys []string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "已过期"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}

// ToastFor maps a click outcome to a short toast (empty = no toast).
func ToastFor(o ClickOutcome) (kind, content string) {
	switch o {
	case OutcomeAccepted, OutcomeToggled:
		return "", ""
	case OutcomeUnauthorized:
		return "warning", "你没有权限回答此提问"
	case OutcomeAlreadySettled:
		return "info", "该提问已结束"
	case OutcomeNeedSelection:
		return "warning", "请先至少选择一项"
	case OutcomeStale:
		return "info", "此提问已失效（可能 bridge 已重启）"
	default:
		return "info", string(o)
	}
}
