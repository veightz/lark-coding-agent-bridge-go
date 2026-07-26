package bridge

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"lark-coding-agent-bridge-go/internal/media"
)

// promptMention mirrors BridgePromptMention in the original prompt.ts.
type promptMention struct {
	OpenID string `json:"openId,omitempty"`
	Name   string `json:"name,omitempty"`
	IsBot  bool   `json:"isBot,omitempty"`
}

type promptContext struct {
	ChatID     string          `json:"chatId"`
	ChatType   string          `json:"chatType"`
	SenderID   string          `json:"senderId"`
	SenderType string          `json:"senderType,omitempty"`
	BotOpenID  string          `json:"botOpenId,omitempty"`
	Mentions   []promptMention `json:"mentions,omitempty"`
	ThreadID   string          `json:"threadId,omitempty"`
	MessageIDs []string        `json:"messageIds,omitempty"`
	Source     string          `json:"source"`
}

type promptAttachment struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type promptUserInput struct {
	Text        string             `json:"text"`
	Attachments []promptAttachment `json:"attachments,omitempty"`
}

// buildPrompt assembles the agent prompt with bridge_context metadata and
// the merged user input. Ported from prompt.ts buildAgentPrompt.
func (b *Bridge) buildPrompt(ctx context.Context, batch []*Message, attachments []media.Attachment) string {
	first := batch[0]

	pc := promptContext{
		ChatID:     first.ChatID,
		ChatType:   first.ChatType,
		SenderID:   first.SenderID,
		SenderType: senderTypeOf(first),
		BotOpenID:  b.botOpenID(),
		ThreadID:   first.ThreadID,
		Source:     "im",
	}
	mentionsSeen := map[string]bool{}
	for _, m := range batch {
		pc.MessageIDs = append(pc.MessageIDs, m.MessageID)
		for _, mt := range m.Mentions {
			key := mt.OpenID
			if key == "" {
				key = mt.Name
			}
			if key == "" || mentionsSeen[key] {
				continue
			}
			mentionsSeen[key] = true
			pc.Mentions = append(pc.Mentions, promptMention{OpenID: mt.OpenID, Name: mt.Name, IsBot: mt.IsBot})
		}
	}

	// Fetch quoted message content when the user is replying to a message.
	var quotedSection string
	if first.ParentID != "" {
		if msgType, content, err := b.Lark.GetMessage(ctx, first.ParentID); err == nil {
			quotedText := flattenQuoted(msgType, content)
			if quotedText != "" {
				quotedSection = promptSection("quoted_message", map[string]string{
					"id":      first.ParentID,
					"type":    msgType,
					"content": quotedText,
				}) + "\n\n"
			}
		} else {
			log.Printf("[prompt] GetMessage(%s) failed: %v", first.ParentID, err)
		}
	}

	var text string
	if len(batch) == 1 {
		text = strings.TrimSpace(batch[0].Content)
	} else {
		var parts []string
		for _, m := range batch {
			annotation := senderAnnotation(m)
			content := strings.TrimSpace(m.Content)
			if content == "" {
				continue
			}
			parts = append(parts, annotation+":\n    "+strings.ReplaceAll(content, "\n", "\n    "))
		}
		text = strings.Join(parts, "\n")
	}

	input := promptUserInput{Text: text}
	for _, att := range attachments {
		input.Attachments = append(input.Attachments, promptAttachment{
			Path: att.Path,
			Kind: att.Kind,
			Name: att.Name,
		})
	}

	return promptSection("bridge_context", pc) + "\n\n" + quotedSection + promptSection("user_input", input)
}

func senderTypeOf(m *Message) string {
	if m.SenderType == "" || m.SenderType == "user" {
		return "user"
	}
	return "bot"
}

func senderAnnotation(m *Message) string {
	id := m.SenderID
	if id == "" {
		id = "unknown"
	}
	return "[" + id + " (" + senderTypeOf(m) + ")]"
}

// flattenQuoted extracts readable text from a fetched message's raw content.
func flattenQuoted(msgType, raw string) string {
	switch msgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err == nil {
			return body.Text
		}
	case "post":
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return raw
		}
		contentRaw, ok := doc["content"]
		if !ok {
			for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
				if sub, ok := doc[lang]; ok {
					var subDoc map[string]json.RawMessage
					if err := json.Unmarshal(sub, &subDoc); err == nil {
						if c, ok := subDoc["content"]; ok {
							contentRaw = c
							break
						}
					}
				}
			}
		}
		if contentRaw == nil {
			return raw
		}
		var paragraphs [][]struct {
			Tag  string `json:"tag"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(contentRaw, &paragraphs); err != nil {
			return raw
		}
		var sb strings.Builder
		for _, para := range paragraphs {
			for _, el := range para {
				if el.Tag == "text" {
					sb.WriteString(el.Text)
				}
			}
			sb.WriteString("\n")
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	return raw
}

// promptSection wraps a value in an XML-ish tag with safe JSON inside.
func promptSection(tag string, value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte("null")
	}
	// Match the original's escaping so a stray "</bridge_context>" in user
	// text can't break out of the section.
	s := strings.NewReplacer(
		"<", ``,
		">", `>`,
		"&", `&`,
	).Replace(string(data))
	return "<" + tag + ">\n" + s + "\n</" + tag + ">"
}
