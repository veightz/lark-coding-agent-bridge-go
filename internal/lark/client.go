// Package lark wraps the Feishu/Lark OpenAPI surface the bridge needs:
// sending messages, driving CardKit streaming cards, and downloading
// message attachments. Long-connection event intake lives in the bridge.
package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Client bundles the SDK client plus app credentials.
type Client struct {
	SDK       *lark.Client
	AppID     string
	AppSecret string
	BaseURL   string

	httpClient *http.Client
}

func NewClient(appID, appSecret, baseURL string) *Client {
	return &Client{
		SDK: lark.NewClient(appID, appSecret,
			lark.WithOpenBaseUrl(baseURL),
			lark.WithLogLevel(larkcore.LogLevelWarn),
		),
		AppID:      appID,
		AppSecret:  appSecret,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func apiErr(op string, code int, msg string) error {
	return fmt.Errorf("%s failed: code=%d msg=%s", op, code, msg)
}

// SendText sends a plain text message to a chat; replyTo threads it under a message.
func (c *Client) SendText(ctx context.Context, chatID, text, replyTo string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	if replyTo != "" {
		resp, err := c.SDK.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
			MessageId(replyTo).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType("text").
				Content(string(content)).
				Build()).
			Build())
		if err != nil {
			return "", err
		}
		if !resp.Success() {
			return "", apiErr("message.reply", resp.Code, resp.Msg)
		}
		return str(resp.Data.MessageId), nil
	}
	return c.createMessage(ctx, "chat_id", chatID, "text", string(content))
}

// SendDirectText sends a plain text message to a user by open_id (p2p).
func (c *Client) SendDirectText(ctx context.Context, openID, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	return c.createMessage(ctx, "open_id", openID, "text", string(content))
}

// createMessage is the shared helper for sending a message with arbitrary receive_id_type.
func (c *Client) createMessage(ctx context.Context, receiveIDType, receiveID, msgType, content string) (string, error) {
	resp, err := c.SDK.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("message.create", resp.Code, resp.Msg)
	}
	return str(resp.Data.MessageId), nil
}

// CreateChat creates a private group chat with the bot as owner and the
// given users (open_id) invited. Requires the im:chat scope.
func (c *Client) CreateChat(ctx context.Context, name, description string, inviteOpenIDs []string) (string, error) {
	resp, err := c.SDK.Im.V1.Chat.Create(ctx, larkim.NewCreateChatReqBuilder().
		UserIdType("open_id").
		Body(larkim.NewCreateChatReqBodyBuilder().
			Name(name).
			Description(description).
			ChatMode("group").
			ChatType("private").
			UserIdList(inviteOpenIDs).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("chat.create", resp.Code, resp.Msg)
	}
	if resp.Data.ChatId == nil {
		return "", fmt.Errorf("chat.create returned no chat_id")
	}
	return *resp.Data.ChatId, nil
}

// CreateCard instantiates a CardKit 2.0 card entity and returns its card_id.
func (c *Client) CreateCard(ctx context.Context, card any) (string, error) {
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	resp, err := c.SDK.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(string(data)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("cardkit.card.create", resp.Code, resp.Msg)
	}
	if resp.Data.CardId == nil {
		return "", fmt.Errorf("cardkit.card.create returned no card_id")
	}
	return *resp.Data.CardId, nil
}

// cardRef is the fixed JSON payload for referencing a cardKit card.
type cardRef struct {
	Type string      `json:"type"`
	Data cardRefData `json:"data"`
}
type cardRefData struct {
	CardID string `json:"card_id"`
}

// SendCardByReference sends an interactive message referencing a card_id.
func (c *Client) SendCardByReference(ctx context.Context, chatID, cardID, replyTo string) (string, error) {
	content, _ := json.Marshal(cardRef{
		Type: "card",
		Data: cardRefData{CardID: cardID},
	})
	if replyTo != "" {
		resp, err := c.SDK.Im.V1.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
			MessageId(replyTo).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType("interactive").
				Content(string(content)).
				Build()).
			Build())
		if err != nil {
			return "", err
		}
		if !resp.Success() {
			return "", apiErr("message.reply(card)", resp.Code, resp.Msg)
		}
		return str(resp.Data.MessageId), nil
	}
	resp, err := c.SDK.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("interactive").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", apiErr("message.create(card)", resp.Code, resp.Msg)
	}
	return str(resp.Data.MessageId), nil
}

// UpdateCard replaces a card entity's full content (sequence must increase).
func (c *Client) UpdateCard(ctx context.Context, cardID string, card any, sequence int) error {
	data, err := json.Marshal(card)
	if err != nil {
		return err
	}
	resp, err := c.SDK.Cardkit.V1.Card.Update(ctx, larkcardkit.NewUpdateCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewUpdateCardReqBodyBuilder().
			Card(larkcardkit.NewCardBuilder().
				Type("card_json").
				Data(string(data)).
				Build()).
			Uuid(fmt.Sprintf("u_%s_%d", cardID, sequence)).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("cardkit.card.update", resp.Code, resp.Msg)
	}
	return nil
}

// cardSettings is the payload for CardKit card.settings (streaming mode etc.).
type cardSettings struct {
	Config cardSettingsConfig `json:"config"`
}
type cardSettingsConfig struct {
	StreamingMode bool              `json:"streaming_mode"`
	Summary       map[string]string `json:"summary"`
}

// FinishStreamingCard flips streaming_mode off and sets the preview summary.
func (c *Client) FinishStreamingCard(ctx context.Context, cardID string, sequence int, summary string) error {
	settings, _ := json.Marshal(cardSettings{
		Config: cardSettingsConfig{
			StreamingMode: false,
			Summary:       map[string]string{"content": summary},
		},
	})
	resp, err := c.SDK.Cardkit.V1.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(string(settings)).
			Uuid(fmt.Sprintf("s_%s_%d", cardID, sequence)).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return apiErr("cardkit.card.settings", resp.Code, resp.Msg)
	}
	return nil
}

// DownloadResource fetches an image/file attachment from a message.
// resourceType is "image" or "file".
func (c *Client) DownloadResource(ctx context.Context, messageID, fileKey, resourceType string) (io.ReadCloser, string, error) {
	resp, err := c.SDK.Im.V1.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type(resourceType).
		Build())
	if err != nil {
		return nil, "", err
	}
	if !resp.Success() {
		return nil, "", apiErr("messageResource.get", resp.Code, resp.Msg)
	}
	return io.NopCloser(resp.File), resp.FileName, nil
}

// BotInfo is the bot's own IM identity from /open-apis/bot/v3/info.
type BotInfo struct {
	OpenID  string
	AppName string
}

// GetBotInfo resolves the bot identity. Uses raw HTTP because the SDK has
// no typed wrapper for this endpoint.
func (c *Client) GetBotInfo(ctx context.Context) (*BotInfo, error) {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/open-apis/bot/v3/info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, apiErr("bot.info", out.Code, out.Msg)
	}
	return &BotInfo{OpenID: out.Bot.OpenID, AppName: out.Bot.AppName}, nil
}

// GetMessage fetches a single message's raw content by ID.
// Returns the message type, text content, and any error.
func (c *Client) GetMessage(ctx context.Context, messageID string) (msgType, content string, err error) {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/open-apis/im/v1/messages/"+messageID, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				MessageType string `json:"msg_type"`
				Body        struct {
					Content string `json:"content"`
				} `json:"body"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if out.Code != 0 {
		return "", "", apiErr("message.get", out.Code, out.Msg)
	}
	if len(out.Data.Items) == 0 {
		return "", "", fmt.Errorf("message %s not found", messageID)
	}
	return out.Data.Items[0].MessageType, out.Data.Items[0].Body.Content, nil
}

// GetChatName resolves a chat's display name ("" for p2p chats or errors).
func (c *Client) GetChatName(ctx context.Context, chatID string) string {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/open-apis/im/v1/chats/"+chatID, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Code int `json:"code"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Code != 0 {
		return ""
	}
	return out.Data.Name
}

func (c *Client) tenantAccessToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("app_id", c.AppID)
	form.Set("app_secret", c.AppSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/open-apis/auth/v3/tenant_access_token/internal",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", apiErr("tenant_access_token", out.Code, out.Msg)
	}
	return out.TenantAccessToken, nil
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
