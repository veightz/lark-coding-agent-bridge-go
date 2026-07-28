// Package policy implements chat access control (owner / invite lists),
// ported from the original bridge's src/policy/access.ts.
package policy

// OwnerState tracks whether we successfully resolved the Feishu app owner.
type OwnerState string

const (
	OwnerUnknown OwnerState = "unknown"
	OwnerOK      OwnerState = "ok"
	OwnerFailed  OwnerState = "failed"
)

// Controls is runtime owner identity (not persisted; refreshed from OpenAPI).
type Controls struct {
	BotOwnerID string
	OwnerState OwnerState
	OwnerError string
}

// Access is the persisted invite whitelist on a profile.
type Access struct {
	AllowedUsers []string
	AllowedChats []string
	Admins       []string
}

// Reason classifies allow/deny decisions (for logs / metrics).
type Reason string

const (
	ReasonOwner        Reason = "owner"
	ReasonAllowedUser  Reason = "allowed-user"
	ReasonAllowedAdmin Reason = "allowed-admin"
	ReasonAllowedChat  Reason = "allowed-chat"
	ReasonDeniedUser   Reason = "denied-user"
	ReasonDeniedChat   Reason = "denied-chat"
	ReasonDeniedAdmin  Reason = "denied-admin"
)

// Decision is the result of an access check.
type Decision struct {
	OK     bool
	Reason Reason
}

// IsCreator reports whether sender matches the known app owner open_id.
// Any non-empty BotOwnerID is trusted (ok or failed-with-cache); empty id
// never grants creator (including unknown state).
func IsCreator(c Controls, senderID string) bool {
	return c.BotOwnerID != "" && senderID != "" && c.BotOwnerID == senderID
}

// HasTrustAnchor reports whether access can be enforced strictly: we know
// an owner and/or at least one invite list entry exists.
func HasTrustAnchor(access Access, c Controls) bool {
	if c.BotOwnerID != "" {
		return true
	}
	return len(access.AllowedUsers) > 0 || len(access.AllowedChats) > 0 || len(access.Admins) > 0
}

// CanUseDM: owner, allowedUsers, or admins.
// If we have no owner and no lists (API still down on a fresh install),
// allow so the real operator is never locked out of a solo bot.
func CanUseDM(access Access, c Controls, senderID string) Decision {
	if IsCreator(c, senderID) {
		return allow(ReasonOwner)
	}
	if contains(access.AllowedUsers, senderID) {
		return allow(ReasonAllowedUser)
	}
	if contains(access.Admins, senderID) {
		return allow(ReasonAllowedAdmin)
	}
	if !HasTrustAnchor(access, c) {
		return allow(ReasonOwner) // open bootstrap — no anchor yet
	}
	return deny(ReasonDeniedUser)
}

// CanUseGroup: owner, admins, or chat is in allowedChats (any member).
func CanUseGroup(access Access, c Controls, chatID, senderID string) Decision {
	if IsCreator(c, senderID) {
		return allow(ReasonOwner)
	}
	if contains(access.Admins, senderID) {
		return allow(ReasonAllowedAdmin)
	}
	if contains(access.AllowedChats, chatID) {
		return allow(ReasonAllowedChat)
	}
	if !HasTrustAnchor(access, c) {
		return allow(ReasonOwner) // open bootstrap
	}
	return deny(ReasonDeniedChat)
}

// CanRunAdminCommand: owner or admins (for /invite /remove).
// When no trust anchor exists, allow so the operator can /invite themselves
// into a durable list (then enforcement becomes strict).
func CanRunAdminCommand(access Access, c Controls, senderID string) Decision {
	if IsCreator(c, senderID) {
		return allow(ReasonOwner)
	}
	if contains(access.Admins, senderID) {
		return allow(ReasonAllowedAdmin)
	}
	if !HasTrustAnchor(access, c) {
		return allow(ReasonOwner)
	}
	return deny(ReasonDeniedAdmin)
}

// CanUseChat dispatches by chat type.
func CanUseChat(access Access, c Controls, chatType, chatID, senderID string) Decision {
	if chatType == "p2p" {
		return CanUseDM(access, c, senderID)
	}
	return CanUseGroup(access, c, chatID, senderID)
}

func allow(r Reason) Decision { return Decision{OK: true, Reason: r} }
func deny(r Reason) Decision  { return Decision{OK: false, Reason: r} }

func contains(list []string, id string) bool {
	if id == "" {
		return false
	}
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}
