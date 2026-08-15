package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

type fakeRuntime struct {
	calls [][]string
}

func (f *fakeRuntime) LookPath(name string) (string, error) {
	if name != "lark-cli" {
		return "", errors.New("unexpected binary")
	}
	return "/fake/lark-cli", nil
}

func (f *fakeRuntime) Run(_ context.Context, _ string, args []string, _ map[string]string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if reflect.DeepEqual(args, []string{"-v"}) {
		return []byte("lark-cli version 1.0.82\n"), nil, nil
	}
	identity := "bot-only"
	for i := range args {
		if args[i] == "--identity" && i+1 < len(args) {
			identity = args[i+1]
		}
	}
	return []byte(fmt.Sprintf(`{"ok":true,"workspace":"lark-channel","app_id":"cli_same","identity":%q}`, identity)), nil, nil
}

func testProfile() config.Profile {
	return config.Profile{
		AgentKind: config.AgentCodex,
		App: config.AppConfig{
			AppID:     "cli_same",
			AppSecret: "do-not-copy-this-secret",
			Tenant:    config.TenantFeishu,
		},
	}
}

func TestEnsureSharedBindingDefaultBindsBotAndWritesSecretFreeProjection(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	profile := testProfile()
	cfg := &config.Config{SchemaVersion: 1, Profiles: map[string]config.Profile{"codex": profile}}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}

	synced, err := ensureSharedBinding(context.Background(), paths, "codex", &profile, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("expected first run to sync")
	}
	wantCalls := [][]string{
		{"-v"},
		{"config", "bind", "--source", "lark-channel", "--identity", "bot-only"},
	}
	if !reflect.DeepEqual(runtime.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", runtime.calls, wantCalls)
	}

	data, err := os.ReadFile(paths.LarkCLIProjectionFile("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), profile.App.AppSecret) {
		t.Fatal("projection must not duplicate App Secret")
	}
	var got projection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Accounts.App.ID != profile.App.AppID || got.Accounts.App.Secret.ID != "/profiles/codex/app/appSecret" {
		t.Fatalf("unexpected projection: %+v", got.Accounts.App)
	}
	if got.Secrets.Providers["bridge-config"].Path != paths.ConfigFile() {
		t.Fatalf("provider path = %q", got.Secrets.Providers["bridge-config"].Path)
	}
	info, err := os.Stat(paths.LarkCLIProjectionFile("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("projection mode = %o", info.Mode().Perm())
	}
}

func TestEnsureSharedBindingSkipsWhenMarkerAndBindingMatch(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	profile := testProfile()
	identity, _ := profile.LarkCLIIdentity()
	if err := config.WriteJSONAtomic(paths.LarkCLISyncFile("codex"), syncMarker{
		SchemaVersion: 1,
		Fingerprint:   sourceFingerprint(&profile, identity),
	}); err != nil {
		t.Fatal(err)
	}
	workspace := cliWorkspaceConfig{Apps: []cliAppConfig{{
		AppID:      profile.App.AppID,
		AppSecret:  json.RawMessage(`{"source":"keychain","id":"appsecret:cli_same"}`),
		Brand:      "feishu",
		DefaultAs:  "bot",
		StrictMode: "bot",
	}}}
	if err := config.WriteJSONAtomic(paths.LarkCLIWorkspaceConfig("codex"), workspace); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}

	synced, err := ensureSharedBinding(context.Background(), paths, "codex", &profile, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if synced {
		t.Fatal("matching binding should not be rewritten")
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("unexpected lark-cli calls: %#v", runtime.calls)
	}
}

func TestEnsureSharedBindingExplicitOptOutDoesNothing(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	profile := testProfile()
	shared := false
	profile.LarkCLI = &config.LarkCLIConfig{SharedApp: &shared}
	runtime := &fakeRuntime{}

	synced, err := ensureSharedBinding(context.Background(), paths, "codex", &profile, runtime)
	if err != nil || synced {
		t.Fatalf("synced=%v err=%v", synced, err)
	}
	if _, err := os.Stat(paths.LarkCLIProjectionFile("codex")); !os.IsNotExist(err) {
		t.Fatalf("projection should not be written, stat err=%v", err)
	}
}

func TestEnsureSharedBindingExplicitUserDefaultUsesForce(t *testing.T) {
	paths := config.Paths{Home: t.TempDir()}
	profile := testProfile()
	profile.LarkCLI = &config.LarkCLIConfig{Identity: "user-default"}
	runtime := &fakeRuntime{}

	synced, err := ensureSharedBinding(context.Background(), paths, "codex", &profile, runtime)
	if err != nil || !synced {
		t.Fatalf("synced=%v err=%v", synced, err)
	}
	want := []string{"config", "bind", "--source", "lark-channel", "--identity", "user-default", "--force"}
	if len(runtime.calls) != 2 || !reflect.DeepEqual(runtime.calls[1], want) {
		t.Fatalf("bind call = %#v, want %#v", runtime.calls, want)
	}
}

func TestEnvironmentOnlyExposesProjectionInSharedMode(t *testing.T) {
	paths := config.Paths{Home: filepath.Join(t.TempDir(), "bridge")}
	profile := testProfile()
	env := Environment(paths, "codex", &profile)
	if env["LARK_CHANNEL_CONFIG"] != paths.LarkCLIProjectionFile("codex") {
		t.Fatalf("LARK_CHANNEL_CONFIG = %q", env["LARK_CHANNEL_CONFIG"])
	}
	shared := false
	profile.LarkCLI = &config.LarkCLIConfig{SharedApp: &shared}
	env = Environment(paths, "codex", &profile)
	if _, ok := env["LARK_CHANNEL_CONFIG"]; ok {
		t.Fatal("independent mode must not expose the Bridge projection")
	}
}

func TestSupportsProjectionOverride(t *testing.T) {
	tests := map[string]bool{
		"lark-cli version 1.0.42": false,
		"lark-cli version 1.0.43": true,
		"lark-cli version 1.0.82": true,
		"lark-cli version 1.1.0":  true,
		"garbage":                 false,
	}
	for input, want := range tests {
		if got := supportsProjectionOverride(input); got != want {
			t.Errorf("supportsProjectionOverride(%q)=%v, want %v", input, got, want)
		}
	}
}
