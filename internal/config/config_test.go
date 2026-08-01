package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	home := t.TempDir()
	paths := Paths{Home: home}

	cfg := &Config{
		SchemaVersion: 1,
		ActiveProfile: "default",
		Profiles: map[string]Profile{
			"default": {
				AgentKind: AgentClaude,
				App:       AppConfig{AppID: "cli_x", AppSecret: "sec", Tenant: TenantFeishu},
				Agent: &AgentRuntime{
					CommandPrefix: "source ~/.proxy-env && proxy_on",
					Shell:         "/bin/zsh",
					ShellArgs:     []string{"-ic"},
				},
			},
		},
	}
	prof := cfg.Profiles["default"]
	prof.Permissions.DefaultAccess = AccessFull
	cfg.Profiles["default"] = prof

	if err := Save(paths, cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	name, profile, err := loaded.Active()
	if err != nil {
		t.Fatal(err)
	}
	if name != "default" || profile.App.AppID != "cli_x" || profile.App.AppSecret != "sec" {
		t.Errorf("loaded = %+v (%s)", profile, name)
	}
	if profile.DefaultAccess() != AccessFull {
		t.Errorf("access = %v", profile.DefaultAccess())
	}
	if profile.BaseURL() != "https://open.feishu.cn" {
		t.Errorf("base url = %v", profile.BaseURL())
	}
	runtime := profile.AgentRuntimeConfig()
	if runtime.CommandPrefix != "source ~/.proxy-env && proxy_on" ||
		runtime.Shell != "/bin/zsh" ||
		len(runtime.ShellArgs) != 1 || runtime.ShellArgs[0] != "-ic" {
		t.Errorf("agent runtime = %+v", runtime)
	}
}

func TestAgentRuntimeConfigDefaultsEmpty(t *testing.T) {
	if got := (&Profile{}).AgentRuntimeConfig(); got.CommandPrefix != "" || got.Shell != "" || got.ShellArgs != nil {
		t.Fatalf("runtime = %+v", got)
	}
}

func TestLoadMissing(t *testing.T) {
	cfg, err := Load(Paths{Home: t.TempDir()})
	if err != nil || cfg != nil {
		t.Errorf("missing config should be (nil, nil), got (%v, %v)", cfg, err)
	}
}

func TestPathsLayout(t *testing.T) {
	p := Paths{Home: "/x"}
	if p.ConfigFile() != "/x/config.json" {
		t.Errorf("config file = %s", p.ConfigFile())
	}
	if p.SessionsFile("p1") != filepath.Join("/x", "profiles", "p1", "sessions.json") {
		t.Errorf("sessions = %s", p.SessionsFile("p1"))
	}
	dir, err := Paths{Home: t.TempDir()}.DefaultWorkspace("p1")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("default workspace not created: %s", dir)
	}
}

func TestValidProfileName(t *testing.T) {
	if !ValidProfileName("default") || !ValidProfileName("claude-2") {
		t.Error("valid names rejected")
	}
	if ValidProfileName("") || ValidProfileName("a/b") || ValidProfileName("a b") {
		t.Error("invalid names accepted")
	}
}
