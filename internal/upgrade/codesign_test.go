package upgrade

import "testing"

func TestIdentityListed(t *testing.T) {
	out := `     1) ABCDEF0123456789 "veightz-lark-bridge-codesign"
     1 valid identities found`
	if !identityListed(out, localCodesignIdentity) {
		t.Fatal("expected local identity to be listed")
	}
	if identityListed(out, "Apple Development: someone") {
		t.Fatal("should not match a different identity")
	}
	if identityListed("     0 valid identities found", localCodesignIdentity) {
		t.Fatal("empty list should not match")
	}
}
