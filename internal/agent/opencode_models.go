package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

type ocProvidersResponse struct {
	Providers []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Models map[string]struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Status      string `json:"status"`
		} `json:"models"`
	} `json:"providers"`
	Default map[string]string `json:"default"`
}

// ListModels returns the models enabled for the current OpenCode project.
// Provider configuration is directory-scoped, so cwd is part of the query.
func (a *OpenCodeAdapter) ListModels(ctx context.Context, cwd string) ([]ModelInfo, error) {
	srv, err := a.ensureServer()
	if err != nil {
		return nil, err
	}
	endpoint := srv.base + "/config/providers"
	if cwd != "" {
		endpoint += "?directory=" + urlQueryEscape(cwd)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := srv.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 OpenCode 模型: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询 OpenCode 模型: HTTP %d", resp.StatusCode)
	}
	var payload ocProvidersResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析 OpenCode 模型: %w", err)
	}

	var out []ModelInfo
	for _, provider := range payload.Providers {
		for key, model := range provider.Models {
			if model.Status == "disabled" || model.Status == "deprecated" {
				continue
			}
			id := model.ID
			if id == "" {
				id = key
			}
			if provider.ID == "" || id == "" {
				continue
			}
			name := model.Name
			if name == "" {
				name = id
			}
			if provider.Name != "" {
				name = provider.Name + " · " + name
			}
			out = append(out, ModelInfo{
				ID:          provider.ID + "/" + id,
				DisplayName: name,
				Description: model.Description,
				Default:     payload.Default[provider.ID] == id,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("OpenCode 当前项目没有可用模型")
	}
	return out, nil
}
