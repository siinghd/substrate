// Package auth implements the three credential grades (plan §6.4):
// org API keys (sk_), project keys (pk_), and short-lived sandbox-scoped JWTs
// whose audience is exactly one sandbox_id — the credential handed to
// end-user agents. It never lets a sandbox token see the fleet.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBadToken      = errors.New("auth: invalid token")
	ErrExpired       = errors.New("auth: token expired")
	ErrScope         = errors.New("auth: scope not granted")
	ErrWrongAudience = errors.New("auth: token audience mismatch")
)

type Kind string

const (
	KindOrg     Kind = "org"
	KindProject Kind = "project"
	KindSandbox Kind = "sandbox"
	// KindAdmin provisions tenants. It is NOT stored in the database: it is a
	// single out-of-band secret, so a database compromise cannot mint one and
	// there is no admin row for a tenant-scoped bug to reach.
	KindAdmin Kind = "admin"
)

// Principal is the authenticated caller.
type Principal struct {
	Kind      Kind
	OrgID     string
	ProjectID string   // project keys + sandbox tokens
	SandboxID string   // sandbox tokens only
	Scopes    []string // sandbox tokens: exec, fs, ports
}

func (p *Principal) Can(scope string) bool {
	if p.Kind != KindSandbox {
		return true
	}
	for _, s := range p.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

// NewAPIKey mints a tenant credential. 160 bits from crypto/rand: these are
// bearer secrets with no rate limit in front of the hash lookup, so they must
// be unguessable rather than merely unique.
func NewAPIKey(prefix string) (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

// ConstantTimeEqual compares two secrets without leaking their common prefix
// through timing. Used for the admin key, which is compared on every admin
// request and can be retried for free.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum)
}

// ---- minimal HS256 JWT (no external deps; audience-bound per RFC 8707) ----

type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"` // sandbox_id
	Audience  []string `json:"aud"`
	Scopes    []string `json:"scp"`
	ExpiresAt int64    `json:"exp"`
	IssuedAt  int64    `json:"iat"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func MintSandboxToken(secret, issuer, sandboxID string, scopes []string, ttl time.Duration) (string, error) {
	// An empty HMAC key produces tokens anyone can forge; every caller must
	// supply a real secret (fail closed at the primitive, not just config).
	if secret == "" {
		return "", fmt.Errorf("auth: refusing to mint token under an empty secret")
	}
	now := time.Now()
	cl := Claims{
		Issuer: issuer, Subject: sandboxID,
		Audience:  []string{"sandbox:" + sandboxID},
		Scopes:    scopes,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	hdr := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(cl)
	if err != nil {
		return "", err
	}
	payload := hdr + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + b64(mac.Sum(nil)), nil
}

func VerifySandboxToken(secret, token, sandboxID string) (*Claims, error) {
	if secret == "" {
		// Mirror of the mint-side guard: verification under an empty key
		// would accept attacker-signed tokens.
		return nil, ErrBadToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrBadToken
	}
	payload := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrBadToken
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrBadToken
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrBadToken
	}
	var cl Claims
	if err := json.Unmarshal(body, &cl); err != nil {
		return nil, ErrBadToken
	}
	if time.Now().Unix() > cl.ExpiresAt {
		return nil, ErrExpired
	}
	want := "sandbox:" + sandboxID
	ok := false
	for _, a := range cl.Audience {
		if a == want {
			ok = true
		}
	}
	if !ok {
		return nil, ErrWrongAudience
	}
	return &cl, nil
}
