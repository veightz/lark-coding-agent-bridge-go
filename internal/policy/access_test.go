package policy

import "testing"

func TestCanUseDM(t *testing.T) {
	c := Controls{BotOwnerID: "ou_owner", OwnerState: OwnerOK}
	access := Access{
		AllowedUsers: []string{"ou_user"},
		Admins:       []string{"ou_admin"},
	}
	if d := CanUseDM(access, c, "ou_owner"); !d.OK || d.Reason != ReasonOwner {
		t.Fatalf("owner: %+v", d)
	}
	if d := CanUseDM(access, c, "ou_user"); !d.OK || d.Reason != ReasonAllowedUser {
		t.Fatalf("user: %+v", d)
	}
	if d := CanUseDM(access, c, "ou_admin"); !d.OK || d.Reason != ReasonAllowedAdmin {
		t.Fatalf("admin: %+v", d)
	}
	if d := CanUseDM(access, c, "ou_stranger"); d.OK || d.Reason != ReasonDeniedUser {
		t.Fatalf("stranger: %+v", d)
	}
}

func TestCanUseGroup(t *testing.T) {
	c := Controls{BotOwnerID: "ou_owner", OwnerState: OwnerOK}
	access := Access{
		AllowedChats: []string{"oc_ok"},
		Admins:       []string{"ou_admin"},
	}
	// Owner any group
	if d := CanUseGroup(access, c, "oc_other", "ou_owner"); !d.OK {
		t.Fatalf("owner any group: %+v", d)
	}
	// Admin any group
	if d := CanUseGroup(access, c, "oc_other", "ou_admin"); !d.OK {
		t.Fatalf("admin any group: %+v", d)
	}
	// Allowed chat: any sender
	if d := CanUseGroup(access, c, "oc_ok", "ou_stranger"); !d.OK || d.Reason != ReasonAllowedChat {
		t.Fatalf("allowed chat: %+v", d)
	}
	// Not allowed
	if d := CanUseGroup(access, c, "oc_no", "ou_stranger"); d.OK || d.Reason != ReasonDeniedChat {
		t.Fatalf("denied chat: %+v", d)
	}
}

func TestIsCreatorMatchesByID(t *testing.T) {
	// Any non-empty owner id matches, regardless of refresh state.
	c := Controls{BotOwnerID: "ou_owner", OwnerState: OwnerUnknown}
	if !IsCreator(c, "ou_owner") {
		t.Fatal("known id should match even if state unknown")
	}
	c.OwnerState = OwnerFailed
	if !IsCreator(c, "ou_owner") {
		t.Fatal("failed with known id should still match creator")
	}
	c.BotOwnerID = ""
	if IsCreator(c, "ou_owner") {
		t.Fatal("empty owner id must not match")
	}
}

func TestFailOpenWithoutTrustAnchor(t *testing.T) {
	c := Controls{OwnerState: OwnerFailed} // no botOwnerID
	access := Access{}
	if d := CanUseDM(access, c, "ou_anyone"); !d.OK {
		t.Fatal("empty anchor must fail-open so solo owner is not locked out")
	}
	if d := CanUseGroup(access, c, "oc_x", "ou_anyone"); !d.OK {
		t.Fatal("group fail-open")
	}
	// Once we know the real owner, strangers are denied.
	c.BotOwnerID = "ou_owner"
	if d := CanUseDM(access, c, "ou_stranger"); d.OK {
		t.Fatal("with owner set, stranger must be denied")
	}
	if d := CanUseDM(access, c, "ou_owner"); !d.OK {
		t.Fatal("owner must pass")
	}
}

func TestCanRunAdminCommand(t *testing.T) {
	c := Controls{BotOwnerID: "ou_owner", OwnerState: OwnerOK}
	access := Access{Admins: []string{"ou_admin"}, AllowedUsers: []string{"ou_user"}}
	if d := CanRunAdminCommand(access, c, "ou_user"); d.OK {
		t.Fatal("plain user must not run admin cmds")
	}
	if d := CanRunAdminCommand(access, c, "ou_admin"); !d.OK {
		t.Fatal("admin should run admin cmds")
	}
}
