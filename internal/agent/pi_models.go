package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ListModels queries Pi's structured RPC catalog. A short-lived, sessionless
// process keeps model discovery independent from any chat scope and does not
// create conversation history.
func (a *PiAdapter) ListModels(ctx context.Context, cwd string) ([]ModelInfo, error) {
	responses, err := a.queryRPC(ctx, cwd,
		map[string]any{"id": "state", "type": "get_state"},
		map[string]any{"id": "models", "type": "get_available_models"},
	)
	if err != nil {
		return nil, err
	}
	defaultID := piModelFullID(responses["state"]["model"])
	models, _ := responses["models"]["models"].([]any)
	out := make([]ModelInfo, 0, len(models))
	seen := map[string]bool{}
	for _, item := range models {
		model, _ := item.(map[string]any)
		id := piModelFullID(model)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := stringField(model, "name")
		if name == "" {
			_, name, _ = parsePiModelRef(id)
		}
		provider := stringField(model, "provider")
		if provider != "" {
			name = provider + " · " + name
		}
		out = append(out, ModelInfo{
			ID:          id,
			DisplayName: name,
			Description: describePiModel(model),
			Default:     id == defaultID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("Pi 当前配置没有可用模型")
	}
	return out, nil
}

// queryRPC sends a small batch of read-only commands to a temporary Pi RPC
// process and returns each response's data keyed by request id.
func (a *PiAdapter) queryRPC(ctx context.Context, cwd string, commands ...map[string]any) (map[string]map[string]any, error) {
	binary := a.binary
	if binary == "" {
		binary = "pi"
	}
	cmd := agentCommandContext(ctx, a.runtime, binary, "--mode", "rpc", "--no-session")
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = mergeEnv(a.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 Pi RPC: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	wanted := make(map[string]bool, len(commands))
	for _, command := range commands {
		id, _ := command["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("Pi RPC 查询命令缺少 id")
		}
		wanted[id] = true
		data, err := json.Marshal(command)
		if err != nil {
			return nil, err
		}
		if _, err := stdin.Write(append(data, '\n')); err != nil {
			return nil, fmt.Errorf("写入 Pi RPC: %w", err)
		}
	}

	responses := make(map[string]map[string]any, len(commands))
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var raw map[string]any
		if json.Unmarshal(scanner.Bytes(), &raw) != nil || stringField(raw, "type") != "response" {
			// Pi extensions may emit fire-and-forget UI events during startup.
			continue
		}
		id := stringField(raw, "id")
		if !wanted[id] {
			continue
		}
		success, _ := raw["success"].(bool)
		if !success {
			return nil, fmt.Errorf("Pi RPC %s: %s", stringField(raw, "command"), errorMessage(raw, "命令失败"))
		}
		data, _ := raw["data"].(map[string]any)
		responses[id] = data
		if len(responses) == len(wanted) {
			return responses, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 Pi RPC: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if text := strings.TrimSpace(stderr.String()); text != "" {
		return nil, fmt.Errorf("Pi RPC 提前退出: %s", truncateRunes(text, 500))
	}
	return nil, fmt.Errorf("Pi RPC 在返回查询结果前退出")
}

func piModelFullID(value any) string {
	model, _ := value.(map[string]any)
	provider := stringField(model, "provider")
	id := stringField(model, "id")
	if provider == "" || id == "" {
		return ""
	}
	return provider + "/" + id
}

func parsePiModelRef(value string) (provider, modelID string, ok bool) {
	provider, modelID, ok = strings.Cut(value, "/")
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	return provider, modelID, ok && provider != "" && modelID != ""
}

func describePiModel(model map[string]any) string {
	var parts []string
	if n, ok := model["contextWindow"].(float64); ok && n > 0 {
		parts = append(parts, "上下文 "+formatPiInteger(int64(n))+" tokens")
	}
	if reasoning, _ := model["reasoning"].(bool); reasoning {
		parts = append(parts, "支持推理")
	}
	if inputs, ok := model["input"].([]any); ok {
		for _, input := range inputs {
			if input == "image" {
				parts = append(parts, "支持图片")
				break
			}
		}
	}
	return strings.Join(parts, " · ")
}

func formatPiInteger(value int64) string {
	digits := strconv.FormatInt(value, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}
