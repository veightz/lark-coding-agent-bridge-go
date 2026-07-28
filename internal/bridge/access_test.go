package bridge

import (
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
	"lark-coding-agent-bridge-go/internal/policy"
)

func TestCheckMessageAccessOwner(t *testing.T) {
	b := &Bridge{
		Profile:    &config.Profile{},
		ownerState: policy.OwnerOK,
		botOwnerID: "ou_owner",
	}
	msg := &Message{ChatType: "group", ChatID: "oc_1", SenderID: "ou_owner"}
	if !b.checkMessageAccess(msg) {
		t.Fatal("owner should pass group")
	}
	msg.SenderID = "ou_other"
	if b.checkMessageAccess(msg) {
		t.Fatal("stranger should be denied without invite")
	}
}

func TestCheckMessageAccessAllowedChat(t *testing.T) {
	b := &Bridge{
		Profile: &config.Profile{
			Access: config.ChatAccess{AllowedChats: []string{"oc_open"}},
		},
		ownerState: policy.OwnerOK,
		botOwnerID: "ou_owner",
	}
	msg := &Message{ChatType: "group", ChatID: "oc_open", SenderID: "ou_stranger"}
	if !b.checkMessageAccess(msg) {
		t.Fatal("allowed chat should open to any member")
	}
}

func TestAddRemoveUnique(t *testing.T) {
	list, added := addUnique(nil, "a")
	if !added || len(list) != 1 {
		t.Fatalf("%v %v", list, added)
	}
	list, added = addUnique(list, "a")
	if added {
		t.Fatal("duplicate")
	}
	list, removed := removeID(list, "a")
	if !removed || len(list) != 0 {
		t.Fatalf("%v %v", list, removed)
	}
}

func TestCheckOperatorAccessAllowedUser(t *testing.T) {
	b := &Bridge{
		Profile: &config.Profile{
			Access: config.ChatAccess{AllowedUsers: []string{"ou_user"}},
		},
		ownerState: policy.OwnerOK,
		botOwnerID: "ou_owner",
	}
	// allowedUsers may press cards even when chat_id looks like a group id.
	if !b.checkOperatorAccess("oc_p2p_or_group", "ou_user") {
		t.Fatal("allowed user should pass operator check via DM rules")
	}
	if b.checkOperatorAccess("oc_x", "ou_stranger") {
		t.Fatal("stranger must fail")
	}
}
