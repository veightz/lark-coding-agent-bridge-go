package bridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/policy"
)

const ownerRefreshInterval = 30 * time.Minute

const nonAllowedGroupHint = "当前群尚未加入响应列表，所以 bot 不会处理消息。\n" +
	"Bot owner/管理员可在本群发 /invite group 加入白名单。"

// policyAccess maps live profile access into policy.Access.
func (b *Bridge) policyAccess() policy.Access {
	if b.Profile == nil {
		return policy.Access{}
	}
	a := b.Profile.Access
	return policy.Access{
		AllowedUsers: append([]string(nil), a.AllowedUsers...),
		AllowedChats: append([]string(nil), a.AllowedChats...),
		Admins:       append([]string(nil), a.Admins...),
	}
}

// ownerControls is a snapshot of runtime owner identity.
func (b *Bridge) ownerControls() policy.Controls {
	b.ownerMu.Lock()
	defer b.ownerMu.Unlock()
	return policy.Controls{
		BotOwnerID: b.botOwnerID,
		OwnerState: b.ownerState,
		OwnerError: b.ownerError,
	}
}

// RefreshOwner fetches the Feishu app owner open_id (ADR-0013).
// On success, caches ownerOpenId into config.json so a later API failure
// does not lock the operator out.
func (b *Bridge) RefreshOwner(ctx context.Context) {
	if b.Lark == nil {
		return
	}
	id, err := b.Lark.GetAppOwnerOpenID(ctx)
	b.ownerMu.Lock()
	defer b.ownerMu.Unlock()
	if err != nil {
		b.ownerState = policy.OwnerFailed
		b.ownerError = err.Error()
		// Prefer in-memory, else config cache — never clear a known owner.
		if b.botOwnerID == "" && b.Profile != nil {
			b.botOwnerID = b.Profile.Access.OwnerOpenID
		}
		if b.botOwnerID != "" {
			log.Printf("[access] owner 刷新失败（沿用缓存 %s）: %v", tailID(b.botOwnerID), err)
		} else {
			log.Printf("[access] owner 刷新失败（无缓存，暂 fail-open）: %v", err)
		}
		return
	}
	prev := b.botOwnerID
	b.botOwnerID = id
	b.ownerState = policy.OwnerOK
	b.ownerError = ""
	log.Printf("[access] owner = %s", id)
	// Persist cache outside the lock.
	if prev != id || (b.Profile != nil && b.Profile.Access.OwnerOpenID != id) {
		go b.persistOwnerCache(id)
	}
}

func (b *Bridge) persistOwnerCache(ownerID string) {
	if err := b.mutateAccess(func(a *config.ChatAccess) {
		a.OwnerOpenID = ownerID
	}); err != nil {
		log.Printf("[access] 缓存 ownerOpenId 失败: %v", err)
	}
}

// StartOwnerRefresh seeds owner from config cache, then refreshes from API
// and starts a background ticker. Call once after NewBridge.
func (b *Bridge) StartOwnerRefresh() {
	b.ownerMu.Lock()
	if b.ownerState == "" {
		b.ownerState = policy.OwnerUnknown
	}
	// Seed from persisted cache before the network call so the first
	// inbound message is not race-denied.
	if b.botOwnerID == "" && b.Profile != nil && b.Profile.Access.OwnerOpenID != "" {
		b.botOwnerID = b.Profile.Access.OwnerOpenID
		b.ownerState = policy.OwnerFailed // cached, not yet confirmed this run
		log.Printf("[access] 使用缓存 owner = %s", b.botOwnerID)
	}
	b.ownerMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	b.RefreshOwner(ctx)
	cancel()

	go func() {
		t := time.NewTicker(ownerRefreshInterval)
		defer t.Stop()
		for range t.C {
			cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
			b.RefreshOwner(cctx)
			ccancel()
		}
	}()
}

// checkMessageAccess applies canUseDm / canUseGroup. Returns false to drop.
// On denied-chat + @bot, sends a one-line invite hint.
func (b *Bridge) checkMessageAccess(msg *Message) bool {
	if msg == nil || msg.SenderID == "" {
		return false
	}
	d := policy.CanUseChat(b.policyAccess(), b.ownerControls(), msg.ChatType, msg.ChatID, msg.SenderID)
	if d.OK {
		return true
	}
	log.Printf("[access] deny sender=%s chat=%s type=%s reason=%s",
		tailID(msg.SenderID), msg.ChatID, msg.ChatType, d.Reason)
	if msg.ChatType != "p2p" && d.Reason == policy.ReasonDeniedChat && msg.MentionedBot {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := b.Lark.SendText(ctx, msg.ChatID, nonAllowedGroupHint, msg.MessageID, false); err != nil {
			log.Printf("[access] hint failed: %v", err)
		}
	}
	return false
}

// checkOperatorAccess for card callbacks (stop / ask).
// Card events omit chat_type and p2p/group chat_ids both look like oc_*,
// so pass if either DM or group rules allow the operator.
func (b *Bridge) checkOperatorAccess(chatID, operatorID string) bool {
	if operatorID == "" {
		return false
	}
	acc := b.policyAccess()
	c := b.ownerControls()
	if policy.CanUseDM(acc, c, operatorID).OK {
		return true
	}
	if chatID != "" && policy.CanUseGroup(acc, c, chatID, operatorID).OK {
		return true
	}
	return false
}

// mutateAccess loads config.json, mutates profile.access, saves, and
// updates the live Profile pointer.
func (b *Bridge) mutateAccess(fn func(*config.ChatAccess)) error {
	cfg, err := config.Load(b.Paths)
	if err != nil {
		return err
	}
	if cfg == nil {
		return fmt.Errorf("config.json 不存在")
	}
	prof, ok := cfg.Profiles[b.ProfileName]
	if !ok {
		return fmt.Errorf("profile %q 不存在", b.ProfileName)
	}
	fn(&prof.Access)
	// Normalize nils to empty for stable JSON.
	if prof.Access.AllowedUsers == nil {
		prof.Access.AllowedUsers = []string{}
	}
	if prof.Access.AllowedChats == nil {
		prof.Access.AllowedChats = []string{}
	}
	if prof.Access.Admins == nil {
		prof.Access.Admins = []string{}
	}
	cfg.Profiles[b.ProfileName] = prof
	if err := config.Save(b.Paths, cfg); err != nil {
		return err
	}
	if b.Profile != nil {
		b.Profile.Access = prof.Access
	}
	return nil
}

func tailID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

// mentionUserTargets returns non-bot @mentions (for /invite user @x).
func mentionUserTargets(msg *Message) []Mention {
	var out []Mention
	for _, m := range msg.Mentions {
		if m.IsBot || m.OpenID == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func addUnique(list []string, id string) (next []string, added bool) {
	for _, x := range list {
		if x == id {
			return list, false
		}
	}
	return append(list, id), true
}

func removeID(list []string, id string) (next []string, removed bool) {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x == id {
			removed = true
			continue
		}
		out = append(out, x)
	}
	return out, removed
}

func displayName(m Mention) string {
	if strings.TrimSpace(m.Name) != "" {
		return m.Name
	}
	return m.OpenID
}
