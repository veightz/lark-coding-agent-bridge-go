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
	resp, err := c.SDK.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
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

// SendCardByReference sends an interactive message referencing a card_id.
func (c *Client) SendCardByReference(ctx context.Context, chatID, cardID, replyTo string) (string, error) {
	content, _ := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
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

// FinishStreamingCard flips streaming_mode off and sets the preview summary.
func (c *Client) FinishStreamingCard(ctx context.Context, cardID string, sequence int, summary string) error {
	settings, _ := json.Marshal(map[string]any{
		"config": map[string]any{
			"streaming_mode": false,
			"summary":        map[string]string{"content": summary},
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
