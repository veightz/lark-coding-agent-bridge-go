package ask

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lark-coding-agent-bridge-go/internal/config"
)

// Env keys injected into agent child processes for the ask hook IPC.
const (
	EnvAskURL           = "LARK_BRIDGE_ASK_URL"
	EnvAskScope         = "LARK_BRIDGE_SCOPE"
	EnvAskChatID        = "LARK_BRIDGE_CHAT_ID"
	EnvAskRootMessageID = "LARK_BRIDGE_ROOT_MESSAGE_ID"
	EnvAskProfile       = "LARK_BRIDGE_PROFILE"
)

// ClaudeSettingsPath is the per-profile Claude settings file used with --settings.
func ClaudeSettingsPath(paths config.Paths, profile string) string {
	return filepath.Join(paths.ProfileDir(profile), "claude-settings.json")
}

// InstallClaudeAskHook writes a PreToolUse hook for AskUserQuestion into the
// profile's claude-settings.json. Idempotent: rewrites only when the hook
// command changes. binary is the absolute path of this bridge executable.
func InstallClaudeAskHook(paths config.Paths, profile, binary string) (string, error) {
	if binary == "" {
		return "", fmt.Errorf("ask: binary path required for hook install")
	}
	path := ClaudeSettingsPath(paths, profile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	hookCmd := fmt.Sprintf("%q hook claude", binary)
	// Also accept unquoted when path has no spaces (Claude shell-splits).
	if !strings.ContainsAny(binary, " \t\"'") {
		hookCmd = binary + " hook claude"
	}

	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &doc)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	group := map[string]any{
		"matcher": "AskUserQuestion",
		"hooks": []map[string]any{{
			"type":    "command",
			"command": hookCmd,
			// Feishu answers can take a while; Claude default hook timeout is short.
			"timeout": 3600,
		}},
	}

	// Replace any existing bridge ask hook groups (matcher AskUserQuestion + our command).
	pre, _ := hooks["PreToolUse"].([]any)
	filtered := make([]any, 0, len(pre)+1)
	for _, g := range pre {
		gm, ok := g.(map[string]any)
		if !ok {
			filtered = append(filtered, g)
			continue
		}
		if isBridgeAskGroup(gm) {
			continue
		}
		filtered = append(filtered, g)
	}
	filtered = append(filtered, group)
	hooks["PreToolUse"] = filtered
	doc["hooks"] = hooks

	if err := config.WriteJSONAtomic(path, doc); err != nil {
		return "", err
	}
	return path, nil
}

func isBridgeAskGroup(g map[string]any) bool {
	matcher, _ := g["matcher"].(string)
	if matcher != "AskUserQuestion" {
		return false
	}
	hooks, _ := g["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, "hook claude") &&
			(strings.Contains(cmd, "lark-coding-agent-bridge-go") || strings.Contains(cmd, "LARK_BRIDGE") || strings.Contains(cmd, "hook claude")) {
			return true
		}
		if strings.Contains(cmd, "hook claude") {
			return true
		}
	}
	return false
}
