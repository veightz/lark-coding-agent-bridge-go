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
	AgentOpenCode AgentKind = "opencode"
	AgentGrok     AgentKind = "grok"
	AgentKimi     AgentKind = "kimi"
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

// Profile is one bot instance's full configuration.
type Profile struct {
	AgentKind AgentKind `json:"agentKind"`
	App       AppConfig `json:"app"`

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
