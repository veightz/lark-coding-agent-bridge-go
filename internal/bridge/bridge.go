// Package bridge routes incoming Lark messages to the local agent CLI:
// normalize → debounce queue → agent run → streaming card reply, with
// per-scope sessions and slash commands. Ported from the original
// channel.ts run flow (IM scope only; cloud-doc comments are out of scope).
package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"lark-coding-agent-bridge-go/internal/agent"
	"lark-coding-agent-bridge-go/internal/card"
	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/lark"
	"lark-coding-agent-bridge-go/internal/media"
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

const debounceMs = 600

// pendingReply tracks a quick_reply button click awaiting the user's p2p reply.
type pendingReply struct {
	groupChatID   string
	cardMessageID string
	promptMsgID   string // the "please reply" p2p message ID
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

	pendingRepliesMu sync.Mutex
	pendingReplies   map[string]*pendingReply // operatorID → pending (quick_reply)

	escalationsMu sync.Mutex
	escalations   map[string]*escalation // p2p chatID → 进行中的建群升级（ADR-0006）
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

	b := &Bridge{
		Paths:       paths,
		ProfileName: profileName,
		Profile:     profile,
		Lark:        larkClient,
		Agent:       agentAdapter,
		Bot:         bot,
		Sessions:    sessions,
		Workspaces:  workspaces,
		Bindings:    bindings,
		Media:       media.NewCache(paths.MediaDir(profileName)),
		runs:        map[string]*activeRun{},
		cardScopes:  map[string]string{},
	}
	b.pending = NewPendingQueue(debounceMs*time.Millisecond, b.flush)
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

	if strings.HasPrefix(content, "/") {
		go b.handleCommand(msg, content)
		return
	}

	// 私聊里的明确任务：自动建群并把任务转过去（ADR-0006）。
	if msg.ChatType == "p2p" && b.escalateP2P(msg) {
		return
	}

	log.Printf("[intake] chat=%s scope=%s type=%s chars=%d resources=%d",
		msg.ChatID, msg.Scope(), msg.RawType, len(content), len(msg.Resources))
	b.pending.Push(msg.Scope(), msg)
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
			_, _ = b.Lark.SendText(ctx, chatID, "⚠️ 运行失败："+err.Error(), batch[0].MessageID)
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

	run, err := b.Agent.Run(runOpts)
	if err != nil {
		return fmt.Errorf("启动 agent 失败: %w", err)
	}

	startTime := time.Now()
	stream := card.NewStream(b.Lark, first.ChatID, first.MessageID)
	runState := card.InitialState()

	b.runsMu.Lock()
	b.runs[scope] = &activeRun{run: run, scope: scope, startTime: startTime, runState: runState, stream: stream}
	b.runsMu.Unlock()
	defer func() {
		b.runsMu.Lock()
		delete(b.runs, scope)
		b.runsMu.Unlock()
	}()

	if err := stream.Start(ctx, card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: first.ChatType != "p2p"})); err != nil {
		run.Stop()
		return fmt.Errorf("创建回复卡片失败: %w", err)
	}
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
eventLoop:
	for {
		idleMins := b.Profile.IdleTimeout()
		var idleC <-chan time.Time
		var idleTimer *time.Timer
		if idleMins > 0 {
			idleTimer = time.NewTimer(time.Duration(idleMins) * time.Minute)
			idleC = idleTimer.C
		}
		select {
		case evt, ok := <-eventsCh:
			if idleTimer != nil {
				idleTimer.Stop()
			}
			if !ok {
				break eventLoop
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
			case agent.EventDone:
				if evt.SessionID != "" {
					newSess.SessionID = evt.SessionID
				}
				if evt.ThreadID != "" {
					newSess.ThreadID = evt.ThreadID
				}
			}
			stream.Update(card.Render(runState, card.RenderOptions{StopButton: true, GroupChat: first.ChatType != "p2p"}))
			// Sync runState for card refresh button (HandleCardAction).
			b.runsMu.Lock()
			if ar, ok := b.runs[scope]; ok {
				ar.runState = runState
			}
			b.runsMu.Unlock()
		case <-idleC:
			// Idle watchdog: the agent emitted nothing for the whole
			// window — kill the run and annotate the card.
			log.Printf("[run] scope=%s idle watchdog fired after %dmin", scope, idleMins)
			idleFired = true
			b.runsMu.Lock()
			delete(b.runs, scope)
			b.runsMu.Unlock()
			run.Stop()
			// Keep reading until the events channel closes.
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
	stream.Update(card.Render(runState, card.RenderOptions{GroupChat: first.ChatType != "p2p"}))
	stream.Finish(summaryOf(runState))

	if newSess.SessionID != "" || newSess.ThreadID != "" {
		b.Sessions.Set(scope, newSess)
	}
	// If the run produced no session ids (e.g. it failed before init), the
	// previous session stays intact so the next message can resume it.
	if err := b.Sessions.Flush(); err != nil {
		log.Printf("[run] sessions flush failed: %v", err)
	}
	return nil
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
		if state.Stats.UsageAvailable || state.Stats.InputTokens+state.Stats.OutputTokens > 0 {
			total := state.Stats.InputTokens + state.Stats.OutputTokens
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
