package ask

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseClaudeHookPayload extracts questions from a Claude Code PreToolUse /
// PermissionRequest hook stdin payload for tool AskUserQuestion.
// Returns (nil, nil) when the payload is not an AskUserQuestion (passthrough).
func ParseClaudeHookPayload(raw []byte) (questions []Question, rawQuestions []map[string]any, err error) {
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("invalid json: %w", err)
	}
	event, _ := p["hook_event_name"].(string)
	if event != "PreToolUse" && event != "PermissionRequest" {
		return nil, nil, nil
	}
	tool, _ := p["tool_name"].(string)
	if tool != "AskUserQuestion" {
		return nil, nil, nil
	}
	toolInput, _ := p["tool_input"].(map[string]any)
	if toolInput == nil {
		return nil, nil, nil
	}
	rawList, _ := toolInput["questions"].([]any)
	if len(rawList) == 0 {
		return nil, nil, nil
	}
	for _, item := range rawList {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rawQuestions = append(rawQuestions, m)
		prompt, _ := m["question"].(string)
		if prompt == "" {
			prompt, _ = m["prompt"].(string)
		}
		multi, _ := m["multiSelect"].(bool)
		var opts []Option
		if arr, ok := m["options"].([]any); ok {
			for _, o := range arr {
				om, ok := o.(map[string]any)
				if !ok {
					continue
				}
				label, _ := om["label"].(string)
				key, _ := om["key"].(string)
				if key == "" {
					key = label
				}
				if key == "" {
					continue
				}
				opts = append(opts, Option{Key: key, Label: label})
			}
		}
		if prompt == "" || len(opts) == 0 {
			continue
		}
		questions = append(questions, Question{Prompt: prompt, Options: opts, MultiSelect: multi})
	}
	if len(questions) == 0 {
		return nil, nil, nil
	}
	return questions, rawQuestions, nil
}

// FormatClaudeAnswer builds the hookSpecificOutput directive Claude expects.
// Empty answers with no comment → still allow with empty map (should not be used
// for passthrough; passthrough is empty stdout).
func FormatClaudeAnswer(eventName string, rawQuestions []map[string]any, questions []Question, result Result) string {
	if eventName == "" {
		eventName = "PreToolUse"
	}
	answers := map[string]string{}
	comment := strings.TrimSpace(result.Comment)
	for i, q := range questions {
		var keys []string
		if i < len(result.Answers) {
			keys = result.Answers[i]
		}
		if len(keys) > 0 {
			labels := make([]string, 0, len(keys))
			for _, k := range keys {
				labels = append(labels, labelFor(q, k))
			}
			answers[q.Prompt] = joinComma(labels)
		} else if comment != "" {
			answers[q.Prompt] = comment
		}
	}
	var directive map[string]any
	if eventName == "PermissionRequest" {
		directive = map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName": "PermissionRequest",
				"decision": map[string]any{
					"behavior": "allow",
					"updatedInput": map[string]any{
						"questions": rawQuestions,
						"answers":   answers,
					},
				},
			},
		}
	} else {
		directive = map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":      "PreToolUse",
				"permissionDecision": "allow",
				"updatedInput": map[string]any{
					"questions": rawQuestions,
					"answers":   answers,
				},
			},
		}
	}
	b, _ := json.Marshal(directive)
	return string(b)
}

// ParseOpenCodeQuestions normalizes OpenCode question.asked properties.
func ParseOpenCodeQuestions(props map[string]any) (id string, questions []Question) {
	id, _ = props["id"].(string)
	if id == "" {
		id, _ = props["questionID"].(string)
	}
	rawList, _ := props["questions"].([]any)
	for _, item := range rawList {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		prompt, _ := m["question"].(string)
		if prompt == "" {
			prompt, _ = m["header"].(string)
		}
		multi, _ := m["multiple"].(bool)
		if !multi {
			multi, _ = m["multiSelect"].(bool)
		}
		var opts []Option
		if arr, ok := m["options"].([]any); ok {
			for _, o := range arr {
				om, ok := o.(map[string]any)
				if !ok {
					continue
				}
				label, _ := om["label"].(string)
				if label == "" {
					continue
				}
				opts = append(opts, Option{Key: label, Label: label})
			}
		}
		if prompt == "" {
			continue
		}
		// OpenCode allows free-form; ensure at least a placeholder option set
		// so the card is usable. If no options, synthesize Yes/Skip is wrong —
		// require options from the model; skip empty.
		if len(opts) == 0 {
			continue
		}
		questions = append(questions, Question{Prompt: prompt, Options: opts, MultiSelect: multi})
	}
	return id, questions
}

// FormatOpenCodeAnswers maps settled keys → label arrays for POST /question/{id}/reply.
func FormatOpenCodeAnswers(questions []Question, result Result) [][]string {
	out := make([][]string, len(questions))
	comment := strings.TrimSpace(result.Comment)
	for i, q := range questions {
		var keys []string
		if i < len(result.Answers) {
			keys = result.Answers[i]
		}
		if len(keys) == 0 {
			if comment != "" {
				out[i] = []string{comment}
			} else {
				out[i] = []string{""}
			}
			continue
		}
		labels := make([]string, 0, len(keys))
		for _, k := range keys {
			labels = append(labels, labelFor(q, k))
		}
		out[i] = labels
	}
	return out
}

// FormatAnswersWithComment prefers selected keys (as labels), else freeform comment.
// Used by OpenCode and pi (select value / input text).
func FormatAnswersWithComment(questions []Question, result Result) [][]string {
	return FormatOpenCodeAnswers(questions, result)
}

func joinComma(ss []string) string {
	return strings.Join(ss, ", ")
}
