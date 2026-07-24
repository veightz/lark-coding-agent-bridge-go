package bridge

import (
	"encoding/json"
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
func (b *Bridge) buildPrompt(batch []*Message, attachments []media.Attachment) string {
	first := batch[0]

	ctx := promptContext{
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
		ctx.MessageIDs = append(ctx.MessageIDs, m.MessageID)
		for _, mt := range m.Mentions {
			key := mt.OpenID
			if key == "" {
				key = mt.Name
			}
			if key == "" || mentionsSeen[key] {
				continue
			}
			mentionsSeen[key] = true
			ctx.Mentions = append(ctx.Mentions, promptMention{OpenID: mt.OpenID, Name: mt.Name, IsBot: mt.IsBot})
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

	return promptSection("bridge_context", ctx) + "\n\n" + promptSection("user_input", input)
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
