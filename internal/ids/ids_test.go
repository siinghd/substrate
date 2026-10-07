package ids

import (
	"strings"
	"testing"
)

func TestPrefixesAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for _, fn := range []func() string{Sandbox, Template, Snapshot, Exec, PortToken, Node, Org, Project, Event} {
		id := fn()
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
		prefix := id[:strings.Index(id, "_")]
		if !ValidID(id, prefix) {
			t.Fatalf("generated id %s fails its own validation", id)
		}
	}
	if !ValidSandboxID(Sandbox()) {
		t.Fatal("generated sandbox id fails validation")
	}
	if ValidSandboxID("tpl_abc") || ValidSandboxID("sb_short") {
		t.Fatal("validation accepted foreign id")
	}
	for _, evil := range []string{
		"sb_../../etc", "sb_..", "sb_a b c d e f gh", "SB_UPPERCASE123",
		"sb_0000000000000\n", "sb_0000000000000;rm", "", "sb_",
		"sb_00000000000000extra",
	} {
		if ValidSandboxID(evil) {
			t.Fatalf("hostile id accepted: %q", evil)
		}
	}
	if !ValidNodeID(Node()) || !ValidTemplateID(Template()) {
		t.Fatal("node/template validation broken")
	}
	if ValidNodeID("nd_../../../x") {
		t.Fatal("node traversal accepted")
	}
}

func TestValidIDAlphabet(t *testing.T) {
	if !ValidID("sb_0123456789abc", "sb") {
		t.Fatal("valid alphabet rejected")
	}
	if ValidID("sb_0123456789ab", "sb") || ValidID("sb_0123456789abcd", "sb") {
		t.Fatal("wrong length accepted")
	}
	if ValidID("sb_0123456789ab_c", "sb") {
		t.Fatal("underscore in body accepted")
	}
}

func TestKeyFormats(t *testing.T) {
	if !strings.HasPrefix(APIKey(), "sk_") {
		t.Fatal("api key prefix")
	}
	if !strings.HasPrefix(ProjectKey(), "pk_") {
		t.Fatal("project key prefix")
	}
	if len(APIKey()) < 20 || len(ProjectKey()) < 20 {
		t.Fatal("keys too short")
	}
}
