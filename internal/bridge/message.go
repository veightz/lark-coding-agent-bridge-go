package bridge

import (
	"encoding/json"
	"regexp"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"lark-coding-agent-bridge-go/internal/media"
)

// larkimEvent aliases the SDK's message-receive event type.
type larkimEvent = larkim.P2MessageReceiveV1

// Mention is one account @-mentioned in a message.
type Mention struct {
	OpenID string
	Name   string
	Key    string // @_user_N placeholder
	IsBot  bool
}

// Message is a normalized incoming IM message.
type Message struct {
	MessageID    string
	ChatID       string
	ChatType     string // p2p / group / topic_group
	ThreadID     string
	RootID       string
	ParentID     string
	SenderID     string // open_id
	SenderType   string // user / app(...)
	Content      string // flattened text
	RawType      string // original message_type
	Mentions     []Mention
	MentionAll   bool
	MentionedBot bool
	Resources    []media.Resource
}

// TopicRootID is the Feishu message id that roots this conversation topic.
//
// P2P 话题模型：主时间线上的每条用户消息成为话题根；话题内后续消息带
// root_id 指向该根。群聊不走此模型（见 Scope）。
func (m *Message) TopicRootID() string {
	if m == nil {
		return ""
	}
	if m.RootID != "" {
		return m.RootID
	}
	// 已在飞书话题内但事件缺 root_id 时的兜底（少见）。
	if m.ThreadID != "" {
		return m.ThreadID
	}
	return m.MessageID
}

// Scope isolates agent sessions / debounce queues / reactions.
//
//   - 群聊：整群共享 chatID；已在飞书话题内时用 chatID:threadID。
//   - 私聊（话题-per-消息）：scope = chatID:topicRoot。
//     主时间线每条消息 → 独立 scope → agent 新会话（不续聊上一条）。
//     同一话题内后续消息 → 相同 topicRoot → 续同一 agent 会话。
func (m *Message) Scope() string {
	if m == nil {
		return ""
	}
	if m.ChatType == "p2p" {
		root := m.TopicRootID()
		if root == "" {
			return m.ChatID
		}
		return m.ChatID + ":" + root
	}
	if m.ThreadID != "" {
		return m.ChatID + ":" + m.ThreadID
	}
	return m.ChatID
}

// ReplyInThread reports whether bot replies for this chat type should use
// Feishu topic form (reply_in_thread). Private chats open a topic under the
// user's message so the main timeline stays clean; groups keep inline replies.
func ReplyInThread(chatType string) bool {
	return chatType == "p2p"
}

// ReplyInThread is true when this message arrived in a p2p chat.
func (m *Message) ReplyInThread() bool {
	if m == nil {
		return false
	}
	return ReplyInThread(m.ChatType)
}

var mentionAllRe = regexp.MustCompile(`@_all\b`)

// NormalizeMessage converts the SDK event into a Message.
func NormalizeMessage(event *larkim.P2MessageReceiveV1, botOpenID string) *Message {
	data := event.Event
	if data == nil || data.Message == nil || data.Sender == nil {
		return nil
	}
	msg := data.Message

	m := &Message{
		MessageID: strv(msg.MessageId),
		ChatID:    strv(msg.ChatId),
		ChatType:  strv(msg.ChatType),
		ThreadID:  strv(msg.ThreadId),
		RootID:    strv(msg.RootId),
		ParentID:  strv(msg.ParentId),
		RawType:   strv(msg.MessageType),
	}
	if msg.ChatType == nil {
		m.ChatType = "p2p"
	}
	if data.Sender.SenderId != nil {
		m.SenderID = strv(data.Sender.SenderId.OpenId)
		if m.SenderID == "" {
			m.SenderID = strv(data.Sender.SenderId.UserId)
		}
	}
	m.SenderType = strv(data.Sender.SenderType)

	rawContent := strv(msg.Content)
	m.MentionAll = mentionAllRe.MatchString(rawContent)

	for _, mt := range msg.Mentions {
		mention := Mention{
			Key:  strv(mt.Key),
			Name: strv(mt.Name),
		}
		if mt.Id != nil {
			mention.OpenID = strv(mt.Id.OpenId)
		}
		if mention.OpenID != "" && mention.OpenID == botOpenID {
			mention.IsBot = true
			m.MentionedBot = true
		}
		m.Mentions = append(m.Mentions, mention)
	}

	m.Content = flattenContent(m.RawType, rawContent, m.Mentions, botOpenID, m)
	return m
}

// flattenContent extracts readable text and attachment refs per message type.
func flattenContent(msgType, raw string, mentions []Mention, botOpenID string, m *Message) string {
	switch msgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return raw
		}
		return resolveMentions(body.Text, mentions, botOpenID)

	case "post":
		return flattenPost(raw, mentions, botOpenID, m)

	case "image":
		var body struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err == nil && body.ImageKey != "" {
			m.Resources = append(m.Resources, media.Resource{FileKey: body.ImageKey, Type: "image"})
		}
		return "[图片]"

	case "file":
		var body struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err == nil && body.FileKey != "" {
			m.Resources = append(m.Resources, media.Resource{FileKey: body.FileKey, Type: "file", Name: body.FileName})
		}
		if body.FileName != "" {
			return "[文件] " + body.FileName
		}
		return "[文件]"

	case "audio":
		return "[语音]"
	case "media":
		return "[视频]"
	case "sticker":
		return "[表情]"
	case "share_chat":
		return "[分享群聊]"
	case "share_user":
		return "[分享用户]"
	case "merge_forward":
		return "[合并转发消息]"
	default:
		return "[" + msgType + "]"
	}
}

// flattenPost extracts text/img refs from a rich-text (post) message.
func flattenPost(raw string, mentions []Mention, botOpenID string, m *Message) string {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return raw
	}
	// Localized payloads wrap the real doc: {"zh_cn": {...}} / {"en_us": {...}}
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
		return "[post]"
	}
	var title string
	if t, ok := doc["title"]; ok {
		_ = json.Unmarshal(t, &title)
	}

	var paragraphs [][]struct {
		Tag      string `json:"tag"`
		Text     string `json:"text"`
		Href     string `json:"href"`
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		ImageKey string `json:"image_key"`
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal(contentRaw, &paragraphs); err != nil {
		return "[post]"
	}

	var sb strings.Builder
	if title != "" {
		sb.WriteString(title)
		sb.WriteString("\n")
	}
	for _, para := range paragraphs {
		for _, el := range para {
			switch el.Tag {
			case "text":
				sb.WriteString(el.Text)
			case "a":
				sb.WriteString("[" + el.Text + "](" + el.Href + ")")
			case "at":
				if el.UserID == botOpenID {
					break // strip bot self-mention
				}
				name := el.UserName
				sb.WriteString("@" + name)
			case "img":
				if el.ImageKey != "" {
					m.Resources = append(m.Resources, media.Resource{FileKey: el.ImageKey, Type: "image"})
					sb.WriteString("[图片]")
				}
			case "media":
				if el.FileKey != "" {
					m.Resources = append(m.Resources, media.Resource{FileKey: el.FileKey, Type: "file", Name: el.FileName})
					sb.WriteString("[文件]")
				}
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// resolveMentions replaces @_user_N placeholders with readable names and
// strips mentions of the bridge bot itself.
func resolveMentions(text string, mentions []Mention, botOpenID string) string {
	for _, mt := range mentions {
		if mt.Key == "" {
			continue
		}
		if mt.OpenID == botOpenID {
			// Strip the bot mention plus one following space.
			text = strings.ReplaceAll(text, mt.Key+" ", "")
			text = strings.ReplaceAll(text, mt.Key, "")
			continue
		}
		text = strings.ReplaceAll(text, mt.Key, "@"+mt.Name)
	}
	return strings.TrimSpace(text)
}

func strv(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
