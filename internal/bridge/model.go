package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
)

const (
	modelSelectAction = "model_select"
	modelPickerTTL    = 15 * time.Minute
)

func (b *Bridge) handleModel(msg *Message, reply func(string)) {
	provider, ok := b.Agent.(agent.ModelProvider)
	if !ok {
		reply("当前 agent（" + b.Agent.DisplayName() + "）暂不支持 /model。")
		return
	}
	scope := msg.Scope()
	current := ""
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cwd, err := b.resolveCwd()
	if err != nil {
		reply("⚠️ 获取工作目录失败：" + err.Error())
		return
	}
	if sess, ok := b.Sessions.Get(scope); ok {
		current = sess.Model
		if sess.Cwd != "" {
			cwd = sess.Cwd
		}
	}
	models, err := provider.ListModels(ctx, cwd)
	if err != nil {
		reply("⚠️ 获取模型列表失败：" + err.Error())
		return
	}

	nonce, err := newModelPickerNonce()
	if err != nil {
		reply("⚠️ 创建模型选择卡片失败：" + err.Error())
		return
	}
	b.registerModelPicker(nonce, scope, models)
	cardJSON := buildModelPickerCard(models, current, scope, nonce, "", b.Agent.DisplayName())
	cardID, err := b.Lark.CreateCard(ctx, cardJSON)
	if err == nil {
		_, err = b.Lark.SendCardByReference(ctx, msg.ChatID, cardID, msg.MessageID, msg.ReplyInThread())
	}
	if err != nil {
		b.deleteModelPicker(nonce)
		log.Printf("[model] send picker failed: %v", err)
		reply("⚠️ 模型选择卡片发送失败：" + err.Error())
	}
}

func (b *Bridge) registerModelPicker(nonce, scope string, models []agent.ModelInfo) {
	choices := make(map[string]agent.ModelInfo, len(models))
	for _, model := range models {
		if model.ID != "" {
			choices[model.ID] = model
		}
	}
	now := time.Now()
	b.modelPickersMu.Lock()
	defer b.modelPickersMu.Unlock()
	for key, picker := range b.modelPickers {
		if now.After(picker.ExpiresAt) {
			delete(b.modelPickers, key)
		}
	}
	b.modelPickers[nonce] = modelPicker{
		Scope:     scope,
		Models:    choices,
		ExpiresAt: now.Add(modelPickerTTL),
	}
}

func (b *Bridge) deleteModelPicker(nonce string) {
	b.modelPickersMu.Lock()
	delete(b.modelPickers, nonce)
	b.modelPickersMu.Unlock()
}

func (b *Bridge) handleModelSelect(chatID string, value map[string]any) CardActionResult {
	nonce, _ := value["nonce"].(string)
	modelID, _ := value["model"].(string)
	if nonce == "" || modelID == "" {
		return CardActionResult{ToastKind: "error", Toast: "模型选择参数无效"}
	}

	b.modelPickersMu.Lock()
	picker, ok := b.modelPickers[nonce]
	if ok && time.Now().After(picker.ExpiresAt) {
		delete(b.modelPickers, nonce)
		ok = false
	}
	model, modelOK := picker.Models[modelID]
	if ok && modelOK {
		delete(b.modelPickers, nonce)
	}
	b.modelPickersMu.Unlock()

	if !ok {
		return CardActionResult{ToastKind: "warning", Toast: "这张模型卡片已过期，请重新发送 /model"}
	}
	if !modelOK || !scopeBelongsToChat(picker.Scope, chatID) {
		return CardActionResult{ToastKind: "error", Toast: "模型选择无效"}
	}

	b.stopRun(picker.Scope)
	sess, _ := b.Sessions.Get(picker.Scope)
	sess.Model = model.ID
	b.Sessions.Set(picker.Scope, sess)
	if err := b.Sessions.Flush(); err != nil {
		return CardActionResult{ToastKind: "error", Toast: "保存模型失败：" + err.Error()}
	}
	agentName := ""
	if b.Agent != nil {
		agentName = b.Agent.DisplayName()
	}
	return CardActionResult{
		ToastKind: "success",
		Toast:     "已切换到 " + model.DisplayName,
		Card:      buildModelPickerCard(nil, "", picker.Scope, "", model.ID, agentName),
	}
}

func scopeBelongsToChat(scope, chatID string) bool {
	return chatID != "" && (scope == chatID || strings.HasPrefix(scope, chatID+":"))
}

func newModelPickerNonce() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("生成 nonce: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func buildModelPickerCard(models []agent.ModelInfo, current, scope, nonce, selected, agentName string) map[string]any {
	if agentName == "" {
		agentName = "当前 agent"
	}
	elements := []any{}
	if selected != "" {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": "当前话题后续消息将使用：\n\n**`" + selected + "`**",
		})
	} else {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": "选择当前话题后续消息使用的 " + agentName + " 模型。切换时会停止本话题中正在运行的任务。",
		})
		for _, model := range models {
			label := model.DisplayName
			if label == "" {
				label = model.ID
			}
			suffix := ""
			buttonType := "default"
			if model.ID == current {
				suffix = " · 当前"
				buttonType = "primary"
			} else if model.Default {
				suffix = " · 默认"
			}
			description := strings.TrimSpace(model.Description)
			if description != "" {
				elements = append(elements, map[string]any{
					"tag":     "markdown",
					"content": "**" + label + "**\n" + truncateModelText(description, 180),
				})
			}
			elements = append(elements, map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": truncateModelText(label+suffix, 40)},
				"type": buttonType,
				"behaviors": []map[string]any{{
					"type": "callback",
					"value": map[string]any{
						"cmd":   modelSelectAction,
						"scope": scope,
						"nonce": nonce,
						"model": model.ID,
					},
				}},
			})
		}
		elements = append(elements, map[string]any{
			"tag":       "markdown",
			"content":   "<font color='grey'>选项来自 " + agentName + " 当前可用配置；卡片 15 分钟后失效。</font>",
			"text_size": "notation",
		})
	}

	title := "🧠 选择模型"
	template := "blue"
	if selected != "" {
		title = "✅ 模型已切换"
		template = "green"
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			"update_multi":     true,
		},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": template,
		},
		"body": map[string]any{"elements": elements},
	}
}

func truncateModelText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
