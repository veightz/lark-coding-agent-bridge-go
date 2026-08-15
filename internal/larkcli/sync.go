// Package larkcli keeps the lark-cli workspace launched by Bridge aligned
// with the current Bridge profile's Feishu/Lark app identity.
package larkcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

const (
	minBindMajor = 1
	minBindMinor = 0
	minBindPatch = 43 // LARK_CHANNEL_CONFIG source override landed in v1.0.43.
)

// Environment is injected into every agent child. LARK_CHANNEL_CONFIG is only
// present in shared mode, where it points at a secret-free projection of the
// canonical Bridge profile config.
func Environment(paths config.Paths, profileName string, profile *config.Profile) map[string]string {
	env := map[string]string{
		"LARK_CHANNEL":             "1",
		"LARK_CHANNEL_HOME":        paths.Home,
		"LARK_CHANNEL_PROFILE":     profileName,
		"LARKSUITE_CLI_CONFIG_DIR": paths.LarkCLIBaseDir(profileName),
	}
	if profile.LarkCLISharedApp() {
		env["LARK_CHANNEL_CONFIG"] = paths.LarkCLIProjectionFile(profileName)
	}
	return env
}

// EnsureSharedBinding reconciles lark-cli with the Bridge app when shared mode
// is enabled. A missing lark-cli binary is not fatal because Bridge itself does
// not require the optional CLI; an installed but incompatible/misconfigured
// CLI fails closed so it cannot silently run as another Feishu subject.
func EnsureSharedBinding(ctx context.Context, paths config.Paths, profileName string, profile *config.Profile) (bool, error) {
	return ensureSharedBinding(ctx, paths, profileName, profile, osRuntime{})
}

type commandRuntime interface {
	LookPath(string) (string, error)
	Run(context.Context, string, []string, map[string]string) ([]byte, []byte, error)
}

type osRuntime struct{}

func (osRuntime) LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil || home == "" {
		return "", err
	}
	// 与 agent CLI 的启动定位保持一致，覆盖 launchd 精简 PATH。
	for _, dir := range []string{
		filepath.Join(home, ".volta", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, "Library", "pnpm"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
	} {
		candidate := filepath.Join(dir, name)
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", err
}

func (osRuntime) Run(ctx context.Context, path string, args []string, overrides map[string]string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = mergeEnvironment(os.Environ(), overrides)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func ensureSharedBinding(ctx context.Context, paths config.Paths, profileName string, profile *config.Profile, runtime commandRuntime) (bool, error) {
	if !profile.LarkCLISharedApp() {
		return false, nil
	}
	identity, err := profile.LarkCLIIdentity()
	if err != nil {
		return false, err
	}

	cliPath, err := runtime.LookPath("lark-cli")
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查找 lark-cli 失败: %w", err)
	}

	if err := writeProjection(paths, profileName, profile); err != nil {
		return false, fmt.Errorf("生成 lark-cli 统一配置投影失败: %w", err)
	}
	fingerprint := sourceFingerprint(profile, identity)
	if markerMatches(paths.LarkCLISyncFile(profileName), fingerprint) &&
		bindingMatches(paths.LarkCLIWorkspaceConfig(profileName), profile, identity) {
		return false, nil
	}

	env := Environment(paths, profileName, profile)
	bindCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr, err := runtime.Run(bindCtx, cliPath, []string{"-v"}, env)
	if err != nil {
		return false, fmt.Errorf("读取 lark-cli 版本失败: %w: %s", err, diagnostic(stdout, stderr, profile.App.AppSecret))
	}
	if !supportsProjectionOverride(string(stdout)) {
		return false, fmt.Errorf("lark-cli 版本过旧（需要 >= 1.0.43，当前 %s），请先运行 lark-cli update", strings.TrimSpace(string(stdout)))
	}

	args := []string{"config", "bind", "--source", "lark-channel", "--identity", identity}
	if identity == "user-default" {
		// profile 中的显式 user-default 已经是操作者的主动授权配置。
		args = append(args, "--force")
	}
	stdout, stderr, err = runtime.Run(bindCtx, cliPath, args, env)
	if err != nil {
		return false, fmt.Errorf("同步 lark-cli 主体失败: %w: %s", err, diagnostic(stdout, stderr, profile.App.AppSecret))
	}
	var result struct {
		OK       bool   `json:"ok"`
		AppID    string `json:"app_id"`
		Identity string `json:"identity"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return false, fmt.Errorf("lark-cli 同步结果无法解析: %w: %s", err, diagnostic(stdout, stderr, profile.App.AppSecret))
	}
	if !result.OK || result.AppID != profile.App.AppID || result.Identity != identity {
		return false, fmt.Errorf("lark-cli 同步后主体校验失败: app_id=%q identity=%q", result.AppID, result.Identity)
	}
	if err := config.WriteJSONAtomic(paths.LarkCLISyncFile(profileName), syncMarker{
		SchemaVersion: 1,
		Fingerprint:   fingerprint,
	}); err != nil {
		return false, fmt.Errorf("写入 lark-cli 同步标记失败: %w", err)
	}
	return true, nil
}

type projection struct {
	Accounts struct {
		App projectionApp `json:"app"`
	} `json:"accounts"`
	Secrets struct {
		Providers map[string]projectionProvider `json:"providers"`
	} `json:"secrets"`
}

type projectionApp struct {
	ID     string              `json:"id"`
	Secret projectionSecretRef `json:"secret"`
	Tenant string              `json:"tenant"`
}

type projectionSecretRef struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

type projectionProvider struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
}

func writeProjection(paths config.Paths, profileName string, profile *config.Profile) error {
	var p projection
	p.Accounts.App = projectionApp{
		ID: profile.App.AppID,
		Secret: projectionSecretRef{
			Source:   "file",
			Provider: "bridge-config",
			ID:       "/profiles/" + profileName + "/app/appSecret",
		},
		Tenant: string(profile.Tenant()),
	}
	p.Secrets.Providers = map[string]projectionProvider{
		"bridge-config": {
			Source: "file",
			Path:   paths.ConfigFile(),
			Mode:   "json",
		},
	}
	return config.WriteJSONAtomic(paths.LarkCLIProjectionFile(profileName), p)
}

type syncMarker struct {
	SchemaVersion int    `json:"schemaVersion"`
	Fingerprint   string `json:"fingerprint"`
}

func sourceFingerprint(profile *config.Profile, identity string) string {
	h := sha256.New()
	for _, part := range []string{profile.App.AppID, profile.App.AppSecret, string(profile.Tenant()), identity} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func markerMatches(path, fingerprint string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var marker syncMarker
	return json.Unmarshal(data, &marker) == nil && marker.SchemaVersion == 1 && marker.Fingerprint == fingerprint
}

type cliWorkspaceConfig struct {
	CurrentApp string         `json:"currentApp"`
	Apps       []cliAppConfig `json:"apps"`
}

type cliAppConfig struct {
	Name       string          `json:"name,omitempty"`
	AppID      string          `json:"appId"`
	AppSecret  json.RawMessage `json:"appSecret"`
	Brand      string          `json:"brand"`
	DefaultAs  string          `json:"defaultAs"`
	StrictMode string          `json:"strictMode"`
}

func bindingMatches(path string, profile *config.Profile, identity string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg cliWorkspaceConfig
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Apps) == 0 {
		return false
	}
	app := &cfg.Apps[0]
	if cfg.CurrentApp != "" {
		app = nil
		for i := range cfg.Apps {
			if cfg.Apps[i].Name == cfg.CurrentApp || cfg.Apps[i].AppID == cfg.CurrentApp {
				app = &cfg.Apps[i]
				break
			}
		}
		if app == nil {
			return false
		}
	}
	if app.AppID != profile.App.AppID || app.Brand != string(profile.Tenant()) {
		return false
	}
	var secretRef struct {
		Source string `json:"source"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(app.AppSecret, &secretRef) != nil || secretRef.Source != "keychain" || secretRef.ID != "appsecret:"+profile.App.AppID {
		return false
	}
	if identity == "user-default" {
		return app.DefaultAs == "user" && app.StrictMode == "off"
	}
	return app.DefaultAs == "bot" && app.StrictMode == "bot"
}

func supportsProjectionOverride(versionOutput string) bool {
	fields := strings.Fields(versionOutput)
	for _, field := range fields {
		parts := strings.Split(strings.TrimPrefix(field, "v"), ".")
		if len(parts) != 3 {
			continue
		}
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		patchText := parts[2]
		if i := strings.IndexByte(patchText, '-'); i >= 0 {
			patchText = patchText[:i]
		}
		patch, err3 := strconv.Atoi(patchText)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if major != minBindMajor {
			return major > minBindMajor
		}
		if minor != minBindMinor {
			return minor > minBindMinor
		}
		return patch >= minBindPatch
	}
	return false
}

func diagnostic(stdout, stderr []byte, secret string) string {
	value := strings.TrimSpace(string(stderr))
	if value == "" {
		value = strings.TrimSpace(string(stdout))
	}
	if secret != "" {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	if len(value) > 2048 {
		value = value[:2048] + "…"
	}
	return value
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for key, value := range overrides {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}
