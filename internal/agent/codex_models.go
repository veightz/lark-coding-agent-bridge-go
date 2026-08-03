package agent

import (
	"context"
	"fmt"
)

type codexModelListResponse struct {
	Data       []codexModelEntry `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

type codexModelEntry struct {
	ID          string `json:"id"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Hidden      bool   `json:"hidden"`
	IsDefault   bool   `json:"isDefault"`
}

// ListModels asks Codex app-server for the authenticated account's current
// picker catalog. It does not create a thread or consume a model turn.
func (a *CodexAdapter) ListModels(ctx context.Context, _ string) ([]ModelInfo, error) {
	binary := a.binary
	if binary == "" {
		binary = "codex"
	}
	rpc, err := startCodexUsageRPC(ctx, binary, mergeEnv(a.Env), a.runtime)
	if err != nil {
		return nil, fmt.Errorf("启动 Codex app-server: %w", err)
	}
	defer rpc.close()

	var initialized map[string]any
	if err := rpc.request("initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "lark-coding-agent-bridge-go",
			"title":   "Lark Coding Agent Bridge",
			"version": "1",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &initialized); err != nil {
		return nil, err
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}

	var out []ModelInfo
	var cursor *string
	for {
		params := map[string]any{
			"includeHidden": false,
			"limit":         100,
		}
		if cursor != nil && *cursor != "" {
			params["cursor"] = *cursor
		}
		var page codexModelListResponse
		if err := rpc.request("model/list", params, &page); err != nil {
			return nil, err
		}
		for _, model := range page.Data {
			if model.Hidden {
				continue
			}
			id := model.Model
			if id == "" {
				id = model.ID
			}
			if id == "" {
				continue
			}
			name := model.DisplayName
			if name == "" {
				name = id
			}
			out = append(out, ModelInfo{
				ID:          id,
				DisplayName: name,
				Description: model.Description,
				Default:     model.IsDefault,
			})
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Codex app-server 未返回可用模型")
	}
	return out, nil
}
