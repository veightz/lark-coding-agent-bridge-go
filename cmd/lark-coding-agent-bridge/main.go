// lark-coding-agent-bridge bridges Feishu/Lark messenger with local
// coding-agent CLIs (Claude Code, Codex CLI, pi, opencode) — a Go port of
// lark-channel-bridge (github.com/zarazhangrui/lark-coding-agent-bridge).
//
// Subcommands:
//
//	run (default)   start the bot in the foreground
//	dashboard       list running bridge instances and their versions
//	upgrade         pull the source repo, rebuild and swap the binary
//	version         print build info
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/ask"
	"lark-coding-agent-bridge-go/internal/bridge"
	"lark-coding-agent-bridge-go/internal/buildinfo"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/lark"
	"lark-coding-agent-bridge-go/internal/onboard"
	"lark-coding-agent-bridge-go/internal/registry"
	"lark-coding-agent-bridge-go/internal/supervisor"
	"lark-coding-agent-bridge-go/internal/upgrade"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[bridge] ")

	cmd := "run"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "run", "start":
		err = cmdRun(args)
	case "hook":
		err = cmdHook(args)
	case "dashboard", "ps":
		err = cmdDashboard()
	case "upgrade", "update":
		err = cmdUpgrade(args)
	case "version", "--version", "-v":
		printVersion()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`用法: lark-coding-agent-bridge [命令] [参数]

命令:
  run         启动 bot（默认命令）  --profile --agent --workspace --app-id
  hook        Claude PreToolUse 钩子入口（AskUserQuestion → 飞书卡片）  claude
  dashboard   查看正在运行的 bridge 实例及其版本
  upgrade     从源码仓库拉取最新代码并重建升级  [--check] [--source <dir>]
  version     打印版本信息`)
}

func printVersion() {
	info := buildinfo.Current()
	fmt.Printf("lark-coding-agent-bridge %s\n", info.Short())
	if info.Commit != "" {
		fmt.Printf("commit: %s (%s)\n", info.Commit, info.CommitTime)
	}
	fmt.Printf("go:     %s\n", info.GoVersion)
}

// ─── run ───────────────────────────────────────────────────────────

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		profileFlag   = fs.String("profile", "", "使用指定 profile（必填）")
		agentFlag     = fs.String("agent", "", "agent 类型：claude | codex | pi | opencode")
		workspaceFlag = fs.String("workspace", "", "初始工作目录")
		appIDFlag     = fs.String("app-id", "", "已有应用的 App ID（跳过扫码创建）")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return run(*profileFlag, *agentFlag, *workspaceFlag, *appIDFlag)
}

func run(profileName, agentKind, workspace, appID string) error {
	paths := config.NewPaths()
	if profileName == "" {
		return errMissingProfile(paths)
	}
	if !config.ValidProfileName(profileName) {
		return fmt.Errorf("非法 profile 名: %q", profileName)
	}

	cfg, profile, err := loadOrOnboard(paths, profileName, agentKind, workspace, appID)
	if err != nil {
		return err
	}
	_ = cfg // config is persisted by loadOrOnboard; only the profile is needed

	larkClient := lark.NewClient(profile.App.AppID, profile.App.AppSecret, profile.BaseURL())

	// Resolve the bot's own identity for prompt injection and loop guards.
	bot, err := larkClient.GetBotInfo(context.Background())
	if err != nil {
		return fmt.Errorf("获取 bot 身份失败（检查 App ID/Secret 与网络）: %w", err)
	}

	adapter := agent.NewAdapter(profile.AgentKind)
	adapter.SetBotIdentity(agent.BotIdentity{OpenID: bot.OpenID, Name: bot.AppName})
	injectAgentEnv(adapter, paths, profileName)

	br, err := bridge.NewBridge(paths, profileName, profile, larkClient, adapter, bot)
	if err != nil {
		return err
	}
	// Chat access owner (ADR-0013): resolve Feishu app owner for whitelist.
	br.StartOwnerRefresh()

	// Ask IPC + Claude hook install (ADR-0008). Best-effort: failure only
	// disables Claude AskUserQuestion takeover, not the whole bridge.
	askSrv := &ask.Server{Broker: br.Ask, Resolve: br.ResolveScopeRoute}
	if askURL, aerr := askSrv.StartListen(); aerr != nil {
		log.Printf("[ask] loopback 服务启动失败: %v", aerr)
	} else {
		br.AskURL = askURL
		defer askSrv.Close()
		if self, e := os.Executable(); e == nil {
			if path, ierr := ask.InstallClaudeAskHook(paths, profileName, self); ierr != nil {
				log.Printf("[ask] 安装 Claude hook 失败: %v", ierr)
			} else {
				log.Printf("[ask] Claude AskUserQuestion hook → %s (ipc %s)", path, askURL)
			}
		}
	}

	// Register in the local process registry (dashboard reads this).
	deregister := registry.Register(paths, profileName, adapter.ID(), br.Workspaces.Get())

	// Clean up streaming cards left behind by a previous ungraceful shutdown.
	br.CleanupStaleCards()
	defer deregister()

	// Event intake over the WebSocket long connection, wrapped by the
	// supervisor for probe-based half-dead detection and auto reconnect.
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			br.HandleMessage(event)
			return nil
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return handleCardAction(br, event), nil
		})

	sup := supervisor.New(supervisor.Config{
		AppID:     profile.App.AppID,
		AppSecret: profile.App.AppSecret,
		Domain:    profile.BaseURL(),
		Events:    eventHandler,
		Probe: func(ctx context.Context) error {
			_, err := larkClient.GetBotInfo(ctx)
			return err
		},
		OnStateChange: func(state supervisor.State) {
			if state == supervisor.StateConnected {
				log.Printf("[ws] 连接正常 (bot=%s)", bot.AppName)
			}
		},
	})

	fmt.Printf("profile:   %s\n", profileName)
	fmt.Printf("agent:     %s (%s)\n", adapter.DisplayName(), adapter.ID())
	fmt.Printf("bot:       %s (%s)\n", bot.AppName, bot.OpenID)
	fmt.Printf("workspace: %s\n", br.Workspaces.Get())
	fmt.Printf("version:   %s\n", buildinfo.Current().Short())
	fmt.Println("\n正在监听消息。按 Ctrl+C 退出。")

	// Graceful shutdown: stop the supervisor (which stops accepting new
	// events), then flush state. The running goroutines will finish with
	// their deferred card cleanup before the process exits.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n正在退出…")
		sup.Stop()
		br.CleanupStaleCards()
		br.Flush()
	}()

	sup.Run(context.Background())
	return nil
}

func handleCardAction(br *bridge.Bridge, event *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	if event == nil || event.Event == nil {
		return nil
	}
	var chatID, messageID, operatorID string
	var value map[string]any
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
		messageID = event.Event.Context.OpenMessageID
	}
	if event.Event.Operator != nil {
		operatorID = event.Event.Operator.OpenID
	}
	if event.Event.Action != nil {
		value = event.Event.Action.Value
	}
	res := br.HandleCardAction(chatID, messageID, operatorID, value)
	if res.Toast == "" && res.Card == nil {
		return nil
	}
	out := &callback.CardActionTriggerResponse{}
	if res.Toast != "" {
		kind := res.ToastKind
		if kind == "" {
			kind = "info"
		}
		out.Toast = &callback.Toast{Type: kind, Content: res.Toast}
	}
	if res.Card != nil {
		out.Card = &callback.Card{Type: "raw", Data: res.Card}
	}
	return out
}

// errMissingProfile returns a clear error when --profile is omitted.
func errMissingProfile(paths config.Paths) error {
	msg := "请指定 --profile <name>（例如: lark-coding-agent-bridge run --profile oc）"
	if cfg, err := config.Load(paths); err == nil && cfg != nil && len(cfg.Profiles) > 0 {
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		msg += "\n已有 profile: " + strings.Join(names, ", ")
	}
	return fmt.Errorf("%s", msg)
}

// loadOrOnboard loads the profile, running the QR wizard (or --app-id
// prompt) when no credentials exist yet.
func loadOrOnboard(paths config.Paths, profileName, agentKind, workspace, appID string) (*config.Config, *config.Profile, error) {
	cfg, err := config.Load(paths)
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		cfg = &config.Config{SchemaVersion: 1, Profiles: map[string]config.Profile{}}
	}

	profile, exists := cfg.Profiles[profileName]
	if exists && profile.App.AppID != "" && profile.App.AppSecret != "" {
		if agentKind != "" {
			profile.AgentKind = config.AgentKind(agentKind)
			cfg.Profiles[profileName] = profile
			if err := config.Save(paths, cfg); err != nil {
				return nil, nil, err
			}
		}
		return cfg, &profile, nil
	}

	// First run: gather app credentials.
	var appCfg config.AppConfig
	if appID != "" {
		secret, err := promptSecret("请输入 App Secret: ")
		if err != nil {
			return nil, nil, err
		}
		appCfg = config.AppConfig{AppID: appID, AppSecret: secret, Tenant: config.TenantFeishu}
	} else {
		result, err := onboard.RunWizard(context.Background())
		if err != nil {
			return nil, nil, err
		}
		appCfg = config.AppConfig{
			AppID:     result.ClientID,
			AppSecret: result.ClientSecret,
			Tenant:    config.TenantBrand(result.TenantBrand),
		}
	}

	kind := config.AgentKind(agentKind)
	if kind == "" {
		kind = chooseAgent()
	}

	profile = config.Profile{AgentKind: kind, App: appCfg}
	profile.Permissions.DefaultAccess = config.AccessFull
	profile.Permissions.MaxAccess = config.AccessFull
	if workspace != "" {
		profile.Workspaces.Default = workspace
	}

	cfg.Profiles[profileName] = profile
	cfg.ActiveProfile = profileName
	if err := config.Save(paths, cfg); err != nil {
		return nil, nil, fmt.Errorf("写入配置失败: %w", err)
	}
	fmt.Printf("配置已写入 %s\n\n", paths.ConfigFile())
	return cfg, &profile, nil
}

// chooseAgent picks an installed agent CLI, asking when several exist.
func chooseAgent() config.AgentKind {
	var installed []config.AgentKind
	for _, candidate := range []struct {
		kind   config.AgentKind
		binary string
	}{
		{config.AgentClaude, "claude"},
		{config.AgentCodex, "codex"},
		{config.AgentPi, "pi"},
		{config.AgentOpenCode, "opencode"},
	} {
		if _, err := exec.LookPath(candidate.binary); err == nil {
			installed = append(installed, candidate.kind)
		}
	}
	switch len(installed) {
	case 0:
		fmt.Println("⚠️ 未检测到 claude / codex / pi / opencode CLI，默认使用 claude（请之后安装并登录）。")
		return config.AgentClaude
	case 1:
		return installed[0]
	}
	fmt.Printf("检测到多个 agent: %s\n", joinKinds(installed))
	fmt.Printf("使用哪个？[默认 %s]: ", installed[0])
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	choice := strings.TrimSpace(line)
	for _, k := range installed {
		if string(k) == choice {
			return k
		}
	}
	return installed[0]
}

func joinKinds(kinds []config.AgentKind) string {
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

func promptSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(line)
	if secret == "" {
		return "", fmt.Errorf("App Secret 不能为空")
	}
	return secret, nil
}

// injectAgentEnv hands the agent subprocess the lark-channel context
// variables (profile-local lark-cli config dir etc.).
func injectAgentEnv(adapter agent.Adapter, paths config.Paths, profileName string) {
	larkCliDir := paths.ProfileDir(profileName) + "/lark-cli"
	_ = os.MkdirAll(larkCliDir, 0o755)
	env := map[string]string{
		"LARK_CHANNEL":             "1",
		"LARK_CHANNEL_HOME":        paths.Home,
		"LARK_CHANNEL_PROFILE":     profileName,
		"LARKSUITE_CLI_CONFIG_DIR": larkCliDir,
	}
	switch a := adapter.(type) {
	case *agent.ClaudeAdapter:
		a.Env = env
	case *agent.CodexAdapter:
		a.Env = env
	case *agent.PiAdapter:
		a.Env = env
	case *agent.OpenCodeAdapter:
		a.Env = env
	}
}

// ─── hook (Claude AskUserQuestion → Feishu card, ADR-0008) ────────

// cmdHook is invoked by Claude Code PreToolUse for AskUserQuestion.
// stdin: hook JSON payload; stdout: allow directive with answers, or empty
// passthrough when the bridge is unreachable / payload is not our concern.
func cmdHook(args []string) error {
	if len(args) == 0 || args[0] != "claude" {
		return fmt.Errorf("用法: lark-coding-agent-bridge hook claude")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil // passthrough
	}

	// Non-AskUserQuestion → empty stdout (Claude runs the tool normally).
	qs, _, err := ask.ParseClaudeHookPayload(raw)
	if err != nil || qs == nil {
		return nil
	}

	askURL := os.Getenv(ask.EnvAskURL)
	if askURL == "" {
		// Bridge not running or env not injected — passthrough to terminal.
		return nil
	}

	body := map[string]any{
		"scope":         os.Getenv(ask.EnvAskScope),
		"chatId":        os.Getenv(ask.EnvAskChatID),
		"rootMessageId": os.Getenv(ask.EnvAskRootMessageID),
		"source":        "claude-hook",
		"timeoutMs":     int((30 * time.Minute) / time.Millisecond),
		"raw":           json.RawMessage(raw),
	}
	payload, _ := json.Marshal(body)
	client := &http.Client{Timeout: 35 * time.Minute}
	resp, err := client.Post(strings.TrimRight(askURL, "/")+"/v1/ask", "application/json", bytes.NewReader(payload))
	if err != nil {
		// Daemon unreachable → passthrough (botmux 同款降级).
		return nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var parsed struct {
		Kind      string `json:"kind"`
		Directive string `json:"directive"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil
	}
	if parsed.Kind == "passthrough" || parsed.Directive == "" {
		return nil
	}
	_, _ = os.Stdout.WriteString(parsed.Directive)
	if !strings.HasSuffix(parsed.Directive, "\n") {
		_, _ = os.Stdout.WriteString("\n")
	}
	return nil
}

// ─── dashboard ─────────────────────────────────────────────────────

func cmdDashboard() error {
	paths := config.NewPaths()
	entries, err := registry.List(paths)
	if err != nil {
		return err
	}
	self := buildinfo.Current()
	fmt.Printf("本机二进制: %s\n\n", self.Short())
	if len(entries) == 0 {
		fmt.Println("没有正在运行的 bridge 实例。")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tAGENT\tPID\tVERSION\t运行时长\t心跳\tBINARY")
	for _, e := range entries {
		uptime := time.Since(e.StartedAt).Round(time.Second)
		heartbeat := time.Since(e.HeartbeatAt).Round(time.Second)
		version := e.Version.Short()
		if e.Version.Commit != self.Commit || e.Binary != currentExe() {
			version += " ⚠︎"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s前\t%s\n",
			e.Profile, e.Agent, e.PID, version, uptime, heartbeat, e.Binary)
	}
	w.Flush()
	fmt.Println("\n⚠︎ = 与当前二进制/源码版本不一致（可能是旧版或开发分支构建）")
	return nil
}

func currentExe() string {
	self, _ := os.Executable()
	return self
}

// ─── upgrade ───────────────────────────────────────────────────────

func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "只检查是否有新版本，不执行升级")
	source := fs.String("source", "", "源码仓库目录（默认自动探测）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return upgrade.Run(upgrade.Options{
		CheckOnly: *check,
		SourceDir: *source,
		Stdout:    os.Stdout,
	})
}
