// Package bridge routes incoming Lark messages to the local agent CLI:
// normalize → debounce queue → agent run → streaming card reply, with
// per-scope sessions and slash commands. Ported from the original
// channel.ts run flow (IM scope only; cloud-doc comments are out of scope).
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/ask"
	"lark-coding-agent-bridge-go/internal/card"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/lark"
	"lark-coding-agent-bridge-go/internal/media"
	"lark-coding-agent-bridge-go/internal/policy"
	"lark-coding-agent-bridge-go/internal/state"
)

// activeRun tracks a running agent invocation for /stop, stop button,
// and refresh (re-render the card from the current run state).
type activeRun struct {
	run       agent.Run
	scope     string
	startTime time.Time
	runState  *card.RunState
	stream    *card.Stream
}

// activeCardEntry persists one in-flight streaming card so the bridge can
// clean it up on restart.
type activeCardEntry struct {
	ChatID string `json:"chatId"`
	CardID string `json:"cardId"`
}

const debounceMs = 600

// pendingReply tracks a quick_reply button click awaiting the user's p2p reply.
type pendingReply struct {
	groupChatID   string
	cardMessageID string
	promptMsgID   string // the "please reply" p2p message ID
}

// workingReaction tracks a Typing reaction added as instant ack.
type workingReaction struct {
	messageID  string
	reactionID string
}

type modelPicker struct {
	Scope     string
	Models    map[string]agent.ModelInfo
	ExpiresAt time.Time
}

// Bridge bundles every dependency of the message pipeline.
type Bridge struct {
	Paths       config.Paths
	ProfileName string
	Profile     *config.Profile
	Lark        *lark.Client
	Agent       agent.Adapter
	Bot         *lark.BotInfo

	Sessions   *state.SessionStore
	Workspaces *state.WorkspaceStore
	Bindings   *state.BindingStore
	Media      *media.Cache

	pending *PendingQueue

	runsMu     sync.Mutex
	runs       map[string]*activeRun // scope → run
	cardScopes map[string]string     // card message id → scope (stop button)

	activeCardsMu   sync.Mutex
	activeCardsPath string

	pendingRepliesMu sync.Mutex
	pendingReplies   map[string]*pendingReply // operatorID → pending (quick_reply)

	reactionsMu sync.Mutex
	reactions   map[string]workingReaction // scope → working reaction

	modelPickersMu sync.Mutex
	modelPickers   map[string]modelPicker // one-time nonce → advertised choices

	// Ask broker + per-scope chat routes for Claude hooks / OpenCode questions (ADR-0008).
	Ask         *ask.Broker
	AskURL      string // loopback base URL for hooks (empty if not started)
	routesMu    sync.Mutex
	scopeRoutes map[string]ask.Route

	// Chat access owner (ADR-0013); refreshed from application OpenAPI.
	ownerMu    sync.Mutex
	botOwnerID string
	ownerState policy.OwnerState
	ownerError string
}

// NewBridge wires the pipeline. Call HandleMessage / HandleCardAction.
func NewBridge(
	paths config.Paths,
	profileName string,
	profile *config.Profile,
	larkClient *lark.Client,
	agentAdapter agent.Adapter,
	bot *lark.BotInfo,
) (*Bridge, error) {
	sessions, err := state.LoadSessions(paths.SessionsFile(profileName))
	if err != nil {
		return nil, err
	}
	workspaces, err := state.LoadWorkspaces(paths.WorkspacesFile(profileName))
	if err != nil {
		return nil, err
	}
	bindings, err := state.LoadBindings(paths.BindingsFile(profileName))
	if err != nil {
		return nil, err
	}
	b := &Bridge{
		Paths:           paths,
		ProfileName:     profileName,
		Profile:         profile,
		Lark:            larkClient,
		Agent:           agentAdapter,
		Bot:             bot,
		Sessions:        sessions,
		Workspaces:      workspaces,
		Bindings:        bindings,
		Media:           media.NewCache(paths.MediaDir(profileName)),
		runs:            map[string]*activeRun{},
		cardScopes:      map[string]string{},
		Ask:             ask.NewBroker(),
		scopeRoutes:     map[string]ask.Route{},
		modelPickers:    map[string]modelPicker{},
		activeCardsPath: paths.ActiveCardsFile(profileName),
	}
	b.Ask.SetDispatcher(&larkAskDispatcher{b: b})
	if configurable, ok := agentAdapter.(agent.AccessConfigurer); ok {
		configurable.SetDefaultAccess(profile.DefaultAccess())
	}
	b.pending = NewPendingQueue(debounceMs*time.Millisecond, b.flush)

	if workspaces.Get() == "" {
		dir := profile.Workspaces.Default
		if dir == "" {
			dir, err = paths.DefaultWorkspace(profileName)
			if err != nil {
				return nil, err
			}
		}
		workspaces.SetCurrent(dir)
		_ = workspaces.Flush()
	}
	if err := os.MkdirAll(paths.MediaDir(profileName), 0o755); err != nil {
		return nil, err
	}
	return b, nil
}

// HandleMessage is the entry point for im.message.receive_v1 events.
func (b *Bridge) HandleMessage(event *larkimEvent) {
	msg := NormalizeMessage(event, b.botOpenID())
	if msg == nil {
		return
	}
	// Never react to our own messages (loop guard).
	if msg.SenderID != "" && msg.SenderID == b.botOpenID() {
		return
	}

	// Access control (ADR-0013): default private — only owner / invite lists.
	if !b.checkMessageAccess(msg) {
		return
	}

	// P2P quick-reply: if this user clicked "💬 继续对话", forward their
	// text back to the group chat (no @ mention needed in p2p).
	if msg.ChatType == "p2p" && msg.SenderID != "" {
		if pr := b.consumePendingReply(msg.SenderID); pr != nil {
			content := strings.TrimSpace(msg.Content)
			if content != "" {
				// Forward to the original group chat.
				b.forwardToGroup(pr.groupChatID, pr.cardMessageID, msg.SenderID, content)
			} else if len(msg.Resources) > 0 {
				b.forwardToGroup(pr.groupChatID, pr.cardMessageID, msg.SenderID, msg.Content)
			}
			return
		}
	}

	// Groups require an @bot mention (unless auto-reply is enabled). @all alone doesn't count.
	if msg.ChatType != "p2p" && !msg.MentionedBot && !b.Profile.AutoReplyEnabled() {
		return
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" && len(msg.Resources) == 0 {
		return
	}

	// pi input/editor freeform: chat text settles the pending ask card (ADR-0008).
	if content != "" && !strings.HasPrefix(content, "/") {
		if b.tryAnswerAskWithText(msg.ChatID, msg.SenderID, content) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = b.Lark.SendText(ctx, msg.ChatID, "✅ 已收到你的回答，agent 继续中…", msg.MessageID, msg.ReplyInThread())
			return
		}
	}

	if strings.HasPrefix(content, "/") {
		go b.handleCommand(msg, content)
		return
	}

	// 私聊统一话题回复（reply_in_thread），不再按任务意图自动建群（ADR-0012）。
	// 需要群聊时用 /new chat 或 /open 显式操作。

	log.Printf("[intake] chat=%s type=%s scope=%s root=%s thread=%s chars=%d resources=%d",
		msg.ChatID, msg.ChatType, msg.Scope(), msg.TopicRootID(), msg.ThreadID, len(content), len(msg.Resources))
	b.pending.Push(msg.Scope(), msg)
	if id := b.addWorkingReaction(msg.MessageID); id != "" {
		b.setScopeReaction(msg.Scope(), msg.MessageID, id)
	}
}

func (b *Bridge) botOpenID() string {
	if b.Bot == nil {
		return ""
	}
	return b.Bot.OpenID
}

// flush is the PendingQueue callback: one batch per scope at a time.
func (b *Bridge) flush(scope string, batch []*Message) {
	if len(batch) == 0 {
		return
	}
	b.pending.Block(scope)
	go func() {
		defer b.pending.Unblock(scope)
		if err := b.runBatch(scope, batch); err != nil {
			log.Printf("[run] scope=%s failed: %v", scope, err)
			chatID := batch[0].ChatID
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = b.Lark.SendText(ctx, chatID, "⚠️ 运行失败："+err.Error(), batch[0].MessageID, batch[0].ReplyInThread())
		}
	}()
}

// runBatch executes one agent run for a merged message batch and streams
// the reply into a CardKit card.
func (b *Bridge) runBatch(scope string, batch []*Message) error {
	first := batch[0]
	ctx := context.Background()

	cwd, err := b.resolveCwd()
	if err != nil {
		return err
	}
	// /bind 绑定的会话记录了自己的 cwd（CLI 里发起会话的目录），
	// 优先于聊天当前工作目录。
	if boundSess, ok := b.Sessions.Get(scope); ok && boundSess.Cwd != "" {
		if info, statErr := os.Stat(boundSess.Cwd); statErr == nil && info.IsDir() {
			cwd = boundSess.Cwd
		}
	}

	// Download attachments from every message in the batch.
	var attachments []media.Attachment
	var images []string
	for _, m := range batch {
		if len(m.Resources) == 0 {
			continue
		}
		for _, att := range b.Media.Download(ctx, b.Lark, m.MessageID, m.Resources) {
			attachments = append(attachments, att)
			if att.Kind == "image" {
				images = append(images, att.Path)
			}
		}
	}

	sess, _ := b.Sessions.Get(scope)
	runOpts := agent.RunOptions{
		RunID:     fmt.Sprintf("%s-%d", scope, time.Now().UnixNano()),
		Scope:     scope,
		Prompt:    b.buildPrompt(ctx, batch, attachments),
		Cwd:       cwd,
		Model:     sess.Model,
		Images:    images,
		Access:    b.Profile.DefaultAccess(),
		SessionID: sess.SessionID,
		ThreadID:  sess.ThreadID,
	}
	// Claude AskUserQuestion hook routing (ADR-0008).
	// Root message id: p2p 话题根（TopicRootID），便于卡片/回复挂在同一话题下。
	configureClaudeAskRunOptions(&runOpts, b.Profile.AgentKind, b.AskURL, b.Paths, b.ProfileName, scope, first)
	inThread := first.ReplyInThread()
	b.SetScopeRoute(scope, first.ChatID, first.MessageID, inThread)
	defer b.ClearScopeRoute(scope)

	run, err := b.Agent.Run(runOpts)
	if err != nil {
		return fmt.Errorf("启动 agent 失败: %w", err)
	}

	startTime := time.Now()
	// 私聊：以用户原消息为根创建话题，流式卡片回复在话题内。
	stream := card.NewStream(b.Lark, first.ChatID, first.MessageID, inThread)
	runState := card.InitialState()
	runState.Stats.Cwd = cwd
	// 续聊时立刻带上已知 session，卡片首帧就能显示 🆔，不必等跑完。
	if sess.SessionID != "" {
		runState.Stats.SessionID = sess.SessionID
	}
	if sess.ThreadID != "" {
		runState.Stats.ThreadID = sess.ThreadID
	}

	b.runsMu.Lock()
	b.runs[scope] = &activeRun{run: run, scope: scope, startTime: startTime, runState: runState, stream: stream}
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.runs, scope)
		b.runsMu.Unlock()
		if b.Ask != nil {
			b.Ask.InvalidateScope(scope, "run ended")
		}
	}()

	if err := stream.Start(ctx, card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: first.ChatType != "p2p"})); err != nil {
		run.Stop()
		return fmt.Errorf("创建回复卡片失败: %w", err)
	}
	b.saveActiveCard(scope, first.ChatID, stream.CardID())
	b.clearScopeReaction(scope)
	b.runsMu.Lock()
	b.cardScopes[stream.MessageID()] = scope
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.cardScopes, stream.MessageID())
		b.runsMu.Unlock()
	}()

	newSess := state.Session{Cwd: cwd, Model: sess.Model}
	stopped := false
	idleFired := false
	eventsCh := run.Events()

	heartbeat := time.NewTicker(1 * time.Second)
	defer heartbeat.Stop()

	idleMins := b.Profile.IdleTimeout()
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	resetIdle := func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		if idleMins > 0 {
			idleTimer = time.NewTimer(time.Duration(idleMins) * time.Minute)
			idleC = idleTimer.C
		} else {
			idleC = nil
		}
	}
	resetIdle()

eventLoop:
	for {
		pendingAsk := b.Ask != nil && b.Ask.PendingCountForScope(scope) > 0
		select {
		case evt, ok := <-eventsCh:
			if !pendingAsk {
				resetIdle()
			}
			if !ok {
				break eventLoop
			}
			if evt.Type == agent.EventAskUser {
				if idleTimer != nil {
					idleTimer.Stop()
				}
				b.handleAskUser(scope, first.ChatID, first.MessageID, inThread, evt)
				resetIdle()
				continue
			}
			runState = runState.Reduce(evt)
			runState.Stats.DurationMs = time.Since(startTime).Milliseconds()
			if tool := runState.LastRunningTool(); tool != nil {
				tool.DurationMs = runState.Stats.DurationMs
			}
			switch evt.Type {
			case agent.EventSystem:
				if evt.SessionID != "" {
					newSess.SessionID = evt.SessionID
				}
				if evt.ThreadID != "" {
					newSess.ThreadID = evt.ThreadID
				}
				if evt.Model != "" {
					newSess.Model = evt.Model
				}
			case agent.EventDone:
				if evt.SessionID != "" {
					newSess.SessionID = evt.SessionID
				}
				if evt.ThreadID != "" {
					newSess.ThreadID = evt.ThreadID
				}
				if evt.Model != "" {
					newSess.Model = evt.Model
				}
			}
			stream.Update(card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: first.ChatType != "p2p"}))
			b.runsMu.Lock()
			if ar, ok := b.runs[scope]; ok {
				ar.runState = runState
			}
			b.runsMu.Unlock()
		case <-heartbeat.C:
			if runState.Terminal != card.TerminalRunning || pendingAsk {
				continue
			}
			runState.Stats.DurationMs = time.Since(startTime).Milliseconds()
			if tool := runState.LastRunningTool(); tool != nil {
				tool.DurationMs = runState.Stats.DurationMs
			}
			stream.Update(card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: first.ChatType != "p2p"}))
			b.runsMu.Lock()
			if ar, ok := b.runs[scope]; ok {
				ar.runState = runState
			}
			b.runsMu.Unlock()
		case <-idleC:
			if pendingAsk {
				resetIdle()
				continue
			}
			log.Printf("[run] scope=%s idle watchdog fired after %dmin", scope, idleMins)
			idleFired = true
			b.runsMu.Lock()
			delete(b.runs, scope)
			b.runsMu.Unlock()
			run.Stop()
		}
	}

	b.runsMu.Lock()
	if _, ok := b.runs[scope]; !ok {
		stopped = true // removed by stopRun (or the watchdog) before we finished
	}
	b.runsMu.Unlock()
	// Record duration before finalizing.
	elapsed := time.Since(startTime).Milliseconds()
	switch {
	case idleFired && runState.Terminal == card.TerminalRunning:
		runState = runState.MarkIdleTimeout()
	case stopped && runState.Terminal == card.TerminalRunning:
		runState = runState.MarkInterrupted()
	default:
		runState = runState.FinalizeIfRunning()
	}
	runState.Stats.DurationMs = elapsed
	// 兜底：部分 agent 只在 Done 时带回 id，或仅写入 newSess 未进 Reduce。
	if runState.Stats.SessionID == "" && newSess.SessionID != "" {
		runState.Stats.SessionID = newSess.SessionID
	}
	if runState.Stats.ThreadID == "" && newSess.ThreadID != "" {
		runState.Stats.ThreadID = newSess.ThreadID
	}
	stream.Update(card.Render(runState, card.RenderOptions{GroupChat: first.ChatType != "p2p"}))
	b.removeActiveCard(scope)
	stream.Finish(summaryOf(runState))

	if latest, ok := b.Sessions.Get(scope); ok && latest.Model != sess.Model {
		newSess.Model = latest.Model
	}
	if newSess.SessionID != "" || newSess.ThreadID != "" {
		b.Sessions.Set(scope, newSess)
		// 群聊跑出 session 后自动写入 bindings.json（ADR-0007），
		// 便于私聊 /sessions + /open 复用已有群。
		// 私聊话题 scope 不写 bindings（recordGroupBinding 已跳过 p2p）。
		b.recordGroupBinding(scope, first.ChatID, first.ChatType, newSess)
	}
	// If the run produced no session ids (e.g. it failed before init), the
	// previous session stays intact so the next message can resume it.
	if err := b.Sessions.Flush(); err != nil {
		log.Printf("[run] sessions flush failed: %v", err)
	}
	if err := b.Bindings.Flush(); err != nil {
		log.Printf("[run] bindings flush failed: %v", err)
	}
	return nil
}

// configureClaudeAskRunOptions injects the Claude-only hook environment and
// --settings flag. Codex/OpenCode/Pi handle asks in-process and must never
// receive Claude CLI arguments.
func configureClaudeAskRunOptions(opts *agent.RunOptions, kind config.AgentKind, askURL string, paths config.Paths, profileName, scope string, first *Message) {
	if opts == nil || first == nil || askURL == "" || kind != config.AgentClaude {
		return
	}
	rootMsgID := first.MessageID
	if r := first.TopicRootID(); r != "" {
		rootMsgID = r
	}
	opts.Env = map[string]string{
		ask.EnvAskURL:           askURL,
		ask.EnvAskScope:         scope,
		ask.EnvAskChatID:        first.ChatID,
		ask.EnvAskRootMessageID: rootMsgID,
		ask.EnvAskProfile:       profileName,
	}
	if settings := ask.ClaudeSettingsPath(paths, profileName); settings != "" {
		opts.ExtraArgs = []string{"--settings", settings}
	}
}

// recordGroupBinding writes the reverse session→group link after a group
// run. Does not overwrite an explicit binding to a different chat.
func (b *Bridge) recordGroupBinding(scope, chatID, chatType string, sess state.Session) {
	if chatType == "p2p" || chatID == "" {
		return
	}
	agentID := sess.SessionID
	if agentID == "" {
		agentID = sess.ThreadID
	}
	if agentID == "" {
		return
	}
	key := state.SessionKey(b.kind(), agentID)
	ok := b.Bindings.SetIfAbsent(key, state.Binding{
		Scope:    scope,
		ChatID:   chatID,
		ChatType: "group",
	})
	if !ok {
		log.Printf("[bind] skip auto-record %s: already bound elsewhere", key)
	}
}

// resetSession clears the stored session AND any persistent agent process
// for the scope (/new, /cd, /ws use).
func (b *Bridge) resetSession(scope string) {
	b.Sessions.Delete(scope)
	_ = b.Sessions.Flush()
	b.Bindings.DeleteByScope(scope)
	_ = b.Bindings.Flush()
	if r, ok := b.Agent.(agent.SessionResetter); ok {
		r.ResetSession(scope)
	}
}
func (b *Bridge) stopRun(scope string) bool {
	b.runsMu.Lock()
	ar, ok := b.runs[scope]
	if ok {
		delete(b.runs, scope)
	}
	b.runsMu.Unlock()
	if !ok {
		return false
	}
	if b.Ask != nil {
		b.Ask.InvalidateScope(scope, "run stopped")
	}
	ar.run.Stop()
	return true
}

// stopRunByCardMessage maps a stop-button click back to its run.
func (b *Bridge) stopRunByCardMessage(messageID string) bool {
	b.runsMu.Lock()
	scope, ok := b.cardScopes[messageID]
	b.runsMu.Unlock()
	if !ok {
		return false
	}
	return b.stopRun(scope)
}

// resolveCwd returns the current working directory, validating it exists.
func (b *Bridge) resolveCwd() (string, error) {
	cwd := b.Workspaces.Get()
	if cwd == "" {
		var err error
		cwd, err = b.Paths.DefaultWorkspace(b.ProfileName)
		if err != nil {
			return "", err
		}
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("工作目录 %s 不存在，请用 /cd 切换", cwd)
	}
	return cwd, nil
}

// CleanupStaleCards finalizes any streaming cards that were left in-flight
// by the previous bridge instance (crash / ungraceful shutdown).
func (b *Bridge) CleanupStaleCards() {
	data, err := os.ReadFile(b.activeCardsPath)
	if err != nil {
		return // file absent or unreadable — nothing to clean
	}
	var entries map[string]activeCardEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[cards] 解析旧 active-cards 失败: %v", err)
		_ = os.Remove(b.activeCardsPath)
		return
	}
	if len(entries) == 0 {
		return
	}
	log.Printf("[cards] 发现 %d 张残留卡片，正在清理…", len(entries))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for scope, e := range entries {
		// Send a final update removing interactive elements first, then
		// close streaming mode. Ignore errors — old cards may already
		// be expired.
		update := card.Render(card.InitialState().MarkInterrupted(), card.RenderOptions{})
		_ = b.Lark.UpdateCard(ctx, e.CardID, update, 0)
		_ = b.Lark.FinishStreamingCard(ctx, e.CardID, 1, "bridge 已重启 — 任务中断")
		log.Printf("[cards] 已清理残留卡片 scope=%s card=%s", scope, e.CardID)
	}
	_ = os.Remove(b.activeCardsPath)
}

func (b *Bridge) saveActiveCard(scope, chatID, cardID string) {
	entries := b.loadActiveCards()
	entries[scope] = activeCardEntry{ChatID: chatID, CardID: cardID}
	b.writeActiveCards(entries)
}

func (b *Bridge) removeActiveCard(scope string) {
	entries := b.loadActiveCards()
	delete(entries, scope)
	b.writeActiveCards(entries)
}

func (b *Bridge) loadActiveCards() map[string]activeCardEntry {
	data, err := os.ReadFile(b.activeCardsPath)
	if err != nil {
		return map[string]activeCardEntry{}
	}
	var entries map[string]activeCardEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return map[string]activeCardEntry{}
	}
	return entries
}

func (b *Bridge) writeActiveCards(entries map[string]activeCardEntry) {
	if len(entries) == 0 {
		_ = os.Remove(b.activeCardsPath)
		return
	}
	data, _ := json.Marshal(entries)
	_ = os.WriteFile(b.activeCardsPath, data, 0o644)
}

// Flush persists mutable stores; call on shutdown.
func (b *Bridge) Flush() {
	if err := b.Sessions.Flush(); err != nil {
		log.Printf("[shutdown] sessions flush: %v", err)
	}
	if err := b.Workspaces.Flush(); err != nil {
		log.Printf("[shutdown] workspaces flush: %v", err)
	}
	if err := b.Bindings.Flush(); err != nil {
		log.Printf("[shutdown] bindings flush: %v", err)
	}
}

func formatDuration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		m := ms / 60000
		s := (ms % 60000) / 1000
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
}

func summaryOf(state *card.RunState) string {
	// Use the card summary (duration/tokens) when available,
	// fall back to text preview for notification display.
	text := state.TextContent()
	if text == "" {
		return ""
	}
	// Prepend stats if the run has duration info.
	if state.Stats.DurationMs > 0 {
		dur := formatDuration(state.Stats.DurationMs)
		if state.Stats.UsageAvailable || state.Stats.TotalTokens() > 0 {
			total := state.Stats.TotalTokens()
			return fmt.Sprintf("⏱ %s  🔤 %d token  %s", dur, total, truncatePreview(text, 60))
		}
		return fmt.Sprintf("⏱ %s  %s", dur, truncatePreview(text, 60))
	}
	return truncatePreview(text, 80)
}

func truncatePreview(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
