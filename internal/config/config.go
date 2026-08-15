// Package config manages the bridge's on-disk configuration.
//
// Layout (root overridable via LARK_CODING_BRIDGE_HOME):
//
//	~/.lark-coding-agent-bridge/config.json            root config (profiles)
//	~/.lark-coding-agent-bridge/active-profile         last selected profile
//	~/.lark-coding-agent-bridge/profiles/<name>/       per-profile state
//	    sessions.json      agent session ids per chat scope
//	    workspaces.json    current + named workspace bindings
//	    media/             downloaded attachments
//	    logs/              run logs
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TenantBrand distinguishes Feishu (China) from Lark (international) tenants.
type TenantBrand string

const (
	TenantFeishu TenantBrand = "feishu"
	TenantLark   TenantBrand = "lark"
)

// AgentKind selects which local CLI agent the profile drives.
type AgentKind string

const (
	AgentClaude   AgentKind = "claude"
	AgentCodex    AgentKind = "codex"
	AgentPi       AgentKind = "pi"
	AgentOmp      AgentKind = "omp" // Oh My Pi（Pi fork；RPC 子集，见 ADR-0021）
	AgentOpenCode AgentKind = "opencode"
	AgentGrok     AgentKind = "grok"
	AgentKimi     AgentKind = "kimi"
	AgentCursor   AgentKind = "cursor" // Cursor CLI（ACP 主路径，见 ADR-0022）
)

// AccessLevel mirrors the original bridge's permission modes.
type AccessLevel string

const (
	AccessFull      AccessLevel = "full"      // claude bypassPermissions / codex danger-full-access
	AccessWorkspace AccessLevel = "workspace" // claude acceptEdits / codex workspace-write
	AccessReadOnly  AccessLevel = "read-only" // claude plan / codex read-only
)

// AppConfig holds the PersonalAgent app credentials.
type AppConfig struct {
	AppID     string      `json:"appId"`
	AppSecret string      `json:"appSecret"`
	Tenant    TenantBrand `json:"tenant,omitempty"`
}

// ChatAccess is the invite whitelist for who may drive the bot
// (ADR-0013, aligned with original bridge profile.access).
// Empty slices mean nobody from that list — never "open to all"
// (except when owner cannot be resolved yet; see policy fail-open).
type ChatAccess struct {
	// OwnerOpenID caches the Feishu app owner open_id so a temporary
	// application API failure does not lock the real owner out.
	OwnerOpenID string `json:"ownerOpenId,omitempty"`
	// AllowedUsers may DM the bot (open_id).
	AllowedUsers []string `json:"allowedUsers,omitempty"`
	// AllowedChats are group chat_ids where any member may use the bot.
	AllowedChats []string `json:"allowedChats,omitempty"`
	// Admins may DM, use any group, and run /invite /remove (open_id).
	Admins []string `json:"admins,omitempty"`
}

// AgentRuntime controls how the local agent CLI process is launched.
// It is intentionally stored in the local profile config so different
// machines can use different network/bootstrap commands.
type AgentRuntime struct {
	// CommandPrefix is evaluated before exec'ing the agent CLI in the same
	// short-lived shell, for example: "source ~/.proxy-env && proxy_on".
	CommandPrefix string `json:"commandPrefix,omitempty"`
	// Shell selects the interpreter for CommandPrefix (default: /bin/sh).
	Shell string `json:"shell,omitempty"`
	// ShellArgs selects how the command string is passed (default: ["-c"]).
	// Use ["-ic"] only when an interactive shell is required for aliases.
	ShellArgs []string `json:"shellArgs,omitempty"`
}

// LarkCLIConfig controls how lark-cli obtains its Feishu/Lark identity.
// The zero value deliberately means "share the Bridge app as bot" so every
// profile has one credential source unless an operator explicitly opts out.
type LarkCLIConfig struct {
	// SharedApp defaults to true. Set false only when lark-cli must use an
	// independently managed app configuration.
	SharedApp *bool `json:"sharedApp,omitempty"`
	// Identity is the lark-cli bind preset: bot-only (default) or user-default.
	Identity string `json:"identity,omitempty"`
}

// Profile is one bot instance's full configuration.
type Profile struct {
	AgentKind AgentKind      `json:"agentKind"`
	App       AppConfig      `json:"app"`
	Agent     *AgentRuntime  `json:"agent,omitempty"`
	LarkCLI   *LarkCLIConfig `json:"larkCli,omitempty"`

	// Access is chat access control (owner is not listed; resolved at runtime).
	Access ChatAccess `json:"access,omitempty"`

	Workspaces struct {
		Default string `json:"default,omitempty"`
	} `json:"workspaces,omitempty"`

	Permissions struct {
		DefaultAccess AccessLevel `json:"defaultAccess,omitempty"`
		MaxAccess     AccessLevel `json:"maxAccess,omitempty"`
	} `json:"permissions,omitempty"`

	Preferences struct {
		// IdleTimeoutMinutes kills a run whose agent emits nothing for N
		// minutes (0 → default 10; negative disables the watchdog).
		IdleTimeoutMinutes int `json:"idleTimeoutMinutes,omitempty"`
		// AllowAutoReply responds to all group messages without @-mention
		// (requires "读取群聊全部消息" permission in Lark console).
		AllowAutoReply bool `json:"allowAutoReply,omitempty"`
	} `json:"preferences,omitempty"`
}

// LarkCLISharedApp reports whether lark-cli must be bound to this profile's
// Bridge app. Sharing is the global default; only an explicit false opts out.
func (p *Profile) LarkCLISharedApp() bool {
	return p.LarkCLI == nil || p.LarkCLI.SharedApp == nil || *p.LarkCLI.SharedApp
}

// LarkCLIIdentity returns the configured bind identity preset. An empty value
// is intentionally bot-only so lark-cli calls use the same bot subject as the
// Bridge connection by default.
func (p *Profile) LarkCLIIdentity() (string, error) {
	if p.LarkCLI == nil || p.LarkCLI.Identity == "" {
		return "bot-only", nil
	}
	switch p.LarkCLI.Identity {
	case "bot-only", "user-default":
		return p.LarkCLI.Identity, nil
	default:
		return "", fmt.Errorf("larkCli.identity 必须是 bot-only 或 user-default，当前为 %q", p.LarkCLI.Identity)
	}
}

// IdleTimeout returns the per-run idle watchdog in minutes.
func (p *Profile) IdleTimeout() int {
	if p.Preferences.IdleTimeoutMinutes == 0 {
		return 10
	}
	return p.Preferences.IdleTimeoutMinutes
}

// AutoReplyEnabled reports whether the bot responds without @-mention.
func (p *Profile) AutoReplyEnabled() bool {
	return p.Preferences.AllowAutoReply
}

// AgentRuntimeConfig returns the optional per-machine launch settings.
func (p *Profile) AgentRuntimeConfig() AgentRuntime {
	if p.Agent == nil {
		return AgentRuntime{}
	}
	return *p.Agent
}

// DefaultAccess returns the effective access level (full when unset).
func (p *Profile) DefaultAccess() AccessLevel {
	if p.Permissions.DefaultAccess == "" {
		return AccessFull
	}
	return p.Permissions.DefaultAccess
}

// Tenant normalizes the tenant brand (defaults to feishu).
func (p *Profile) Tenant() TenantBrand {
	if p.App.Tenant == TenantLark {
		return TenantLark
	}
	return TenantFeishu
}

// BaseURL is the OpenAPI base URL for the tenant.
func (p *Profile) BaseURL() string {
	if p.Tenant() == TenantLark {
		return "https://open.larksuite.com"
	}
	return "https://open.feishu.cn"
}

// Config is the root config.json document.
type Config struct {
	SchemaVersion int                `json:"schemaVersion"`
	ActiveProfile string             `json:"activeProfile,omitempty"`
	Profiles      map[string]Profile `json:"profiles"`
}

// Active returns the active profile's name and config.
func (c *Config) Active() (string, *Profile, error) {
	name := c.ActiveProfile
	if name == "" && len(c.Profiles) == 1 {
		for n := range c.Profiles {
			name = n
		}
	}
	if name == "" {
		return "", nil, fmt.Errorf("没有激活的 profile")
	}
	p, ok := c.Profiles[name]
	if !ok {
		return "", nil, fmt.Errorf("profile %q 不存在", name)
	}
	return name, &p, nil
}

// Paths resolves all on-disk locations for the bridge.
type Paths struct {
	Home string
}

// HomeDir returns the bridge state root, honoring LARK_CODING_BRIDGE_HOME.
func HomeDir() string {
	if h := os.Getenv("LARK_CODING_BRIDGE_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".lark-coding-agent-bridge")
}

func NewPaths() Paths { return Paths{Home: HomeDir()} }

func (p Paths) ConfigFile() string { return filepath.Join(p.Home, "config.json") }

func (p Paths) ProfileDir(name string) string { return filepath.Join(p.Home, "profiles", name) }

// LarkCLIBaseDir is the profile-isolated lark-cli base directory. lark-cli
// adds its own lark-channel workspace segment below this path.
func (p Paths) LarkCLIBaseDir(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "lark-cli")
}

// LarkCLIWorkspaceConfig is lark-cli's effective config under LARK_CHANNEL=1.
func (p Paths) LarkCLIWorkspaceConfig(profile string) string {
	return filepath.Join(p.LarkCLIBaseDir(profile), "lark-channel", "config.json")
}

// LarkCLIProjectionFile is the secret-free lark-channel source projected from
// the canonical Bridge profile config for `lark-cli config bind`.
func (p Paths) LarkCLIProjectionFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "lark-cli-source.json")
}

// LarkCLISyncFile stores a non-secret fingerprint of the last successful bind.
func (p Paths) LarkCLISyncFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "lark-cli-sync.json")
}

func (p Paths) SessionsFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "sessions.json")
}

func (p Paths) WorkspacesFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "workspaces.json")
}

func (p Paths) BindingsFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "bindings.json")
}

func (p Paths) UserGroupsFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "user-groups.json")
}

func (p Paths) MediaDir(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "media")
}

func (p Paths) LogsDir(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "logs")
}

func (p Paths) ActiveCardsFile(profile string) string {
	return filepath.Join(p.ProfileDir(profile), "active-cards.json")
}

// DefaultWorkspace returns (and creates) the profile-managed default working directory.
func (p Paths) DefaultWorkspace(profile string) (string, error) {
	dir := filepath.Join(p.Home, "workspaces", profile, "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Load reads config.json. Missing file returns (nil, nil).
func Load(paths Paths) (*Config, error) {
	data, err := os.ReadFile(paths.ConfigFile())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", paths.ConfigFile(), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	return &cfg, nil
}

// Save atomically writes config.json.
func Save(paths Paths, cfg *Config) error {
	return WriteJSONAtomic(paths.ConfigFile(), cfg)
}

// WriteJSONAtomic serializes v as indented JSON and atomically replaces path.
func WriteJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ValidProfileName reports whether name is safe to use as a directory name.
func ValidProfileName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return !strings.Contains(name, "..")
}
