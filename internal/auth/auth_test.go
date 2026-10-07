package auth

import (
	"testing"
	"time"
)

func TestSandboxTokenRoundTrip(t *testing.T) {
	tok, err := MintSandboxToken("secret", "substrate", "sb_abc123", []string{"exec", "fs"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := VerifySandboxToken("secret", tok, "sb_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if cl.Subject != "sb_abc123" || len(cl.Scopes) != 2 {
		t.Fatalf("claims: %+v", cl)
	}
}

func TestTokenAudienceBinding(t *testing.T) {
	tok, _ := MintSandboxToken("secret", "substrate", "sb_abc123", []string{"exec"}, time.Hour)
	if _, err := VerifySandboxToken("secret", tok, "sb_OTHER"); err == nil {
		t.Fatal("token for sb_abc123 accepted for sb_OTHER — audience binding broken")
	}
}

func TestTokenExpiryAndTamper(t *testing.T) {
	tok, _ := MintSandboxToken("secret", "substrate", "sb_x", []string{"exec"}, -time.Minute)
	if _, err := VerifySandboxToken("secret", tok, "sb_x"); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	tok2, _ := MintSandboxToken("secret", "substrate", "sb_x", []string{"exec"}, time.Hour)
	if _, err := VerifySandboxToken("wrong-secret", tok2, "sb_x"); err == nil {
		t.Fatal("forged signature accepted")
	}
	if _, err := VerifySandboxToken("secret", tok2+"x", "sb_x"); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestScopes(t *testing.T) {
	p := &Principal{Kind: KindSandbox, Scopes: []string{"exec"}}
	if !p.Can("exec") || p.Can("fs") || p.Can("destroy") {
		t.Fatal("scope enforcement wrong")
	}
	org := &Principal{Kind: KindOrg}
	if !org.Can("anything") {
		t.Fatal("org principal should be unrestricted")
	}
}

// Fail-closed primitives: an empty HMAC secret must be refused at mint AND
// verify time — tokens under the empty key are forgeable by anyone.
func TestEmptySecretRefused(t *testing.T) {
	if _, err := MintSandboxToken("", "iss", "sb_x", []string{"exec"}, time.Hour); err == nil {
		t.Fatal("mint under empty secret succeeded")
	}
	tok, err := MintSandboxToken("real-secret-at-least-something", "iss", "sb_x", []string{"exec"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySandboxToken("", tok, "sb_x"); err != ErrBadToken {
		t.Fatalf("verify under empty secret returned %v, want ErrBadToken", err)
	}
}
