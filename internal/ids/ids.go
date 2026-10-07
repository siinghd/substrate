// Package ids generates the platform's identifiers. All IDs are prefixed
// (sb_, tpl_, snp_, ex_, px_) so a leaked credential's scope is obvious in
// logs, and use crypto/rand — never counters that leak fleet volume.
package ids

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

var enc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func random(n int) string {
	b := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return enc.EncodeToString(b)[:n]
}

func New(prefix string) string {
	return prefix + "_" + random(13)
}

func Sandbox() string     { return New("sb") }
func Template() string    { return New("tpl") }
func Snapshot() string    { return New("snp") }
func Exec() string        { return New("ex") }
func PortToken() string   { return New("pxt") }
func Node() string        { return New("nd") }
func Org() string         { return New("org") }
func Project() string     { return New("prj") }
func APIKey() string      { return "sk_" + random(24) }
func ProjectKey() string  { return "pk_" + random(24) }
func Event() string       { return New("ev") }
func Idempotency() string { return New("idm") }

// ValidID reports whether id is exactly a platform-generated identifier with
// the given prefix. This is a security boundary, not cosmetics: sandbox IDs
// become jailer chroot directories, netns names and tap names, so anything
// outside this strict alphabet must never reach those layers.
func ValidID(id, prefix string) bool {
	suffix := prefix + "_"
	if !strings.HasPrefix(id, suffix) {
		return false
	}
	rest := id[len(suffix):]
	if len(rest) != 13 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

func ValidSandboxID(id string) bool  { return ValidID(id, "sb") }
func ValidTemplateID(id string) bool { return ValidID(id, "tpl") }
func ValidNodeID(id string) bool     { return ValidID(id, "nd") }
