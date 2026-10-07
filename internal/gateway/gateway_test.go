package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mapResolver struct{ routes map[string]*Route }

func (m mapResolver) Resolve(_ context.Context, sb string, port int) (*Route, bool) {
	r, ok := m.routes[fmt.Sprintf("%s:%d", sb, port)]
	return r, ok
}

func TestParseHost(t *testing.T) {
	cases := []struct {
		host, domain string
		port         int
		sb           string
		ok           bool
	}{
		{"3000-sb_abc1234567890.run.localhost", "run.localhost", 3000, "sb_abc1234567890", true},
		{"8080-sb_xyz2345678901.run.localhost:9911", "run.localhost", 8080, "sb_xyz2345678901", true},
		{"sb_abc1234567890.run.localhost", "run.localhost", 0, "", false},
		{"evil.com", "run.localhost", 0, "", false},
		{"3000-tpl_x.run.localhost", "run.localhost", 0, "", false},
	}
	for _, c := range cases {
		port, sb, ok := parseHost(c.host, c.domain)
		if ok != c.ok || port != c.port || sb != c.sb {
			t.Fatalf("parseHost(%q): got %d %q %v want %d %q %v", c.host, port, sb, ok, c.port, c.sb, c.ok)
		}
	}
}

func TestPrivatePortRequiresToken(t *testing.T) {
	res := mapResolver{routes: map[string]*Route{
		"sb_priv123456789:3000": {SandboxID: "sb_priv123456789", Port: 3000, Target: "127.0.0.1:1", Public: false, Token: "pxt_secret"},
	}}
	g := New(res, "run.localhost", "", "")

	req := httptest.NewRequest("GET", "http://3000-sb_priv123456789.run.localhost/", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated access got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "http://3000-sb_priv123456789.run.localhost/?sandbox_token=WRONG", nil)
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "http://3000-sb_priv123456789.run.localhost/", nil)
	req.Header.Set("Authorization", "Bearer pxt_secret")
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid token rejected")
	}
}

func TestPublicPortOpenAndProxying(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-from-sandbox")
	}))
	defer backend.Close()

	target := strings.TrimPrefix(backend.URL, "http://")
	res := mapResolver{routes: map[string]*Route{
		"sb_pub1234567890:8080": {SandboxID: "sb_pub1234567890", Port: 8080, Target: target, Public: true},
	}}
	g := New(res, "run.localhost", "", "")

	req := httptest.NewRequest("GET", "http://8080-sb_pub1234567890.run.localhost/path?q=1", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello-from-sandbox") {
		t.Fatalf("proxy failed: %d %q", rec.Code, rec.Body.String())
	}
}

func TestUnknownHostAndPort(t *testing.T) {
	g := New(mapResolver{routes: map[string]*Route{}}, "run.localhost", "", "")
	req := httptest.NewRequest("GET", "http://unknown.host/", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown host: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "http://9999-sb_missing123456.run.localhost/", nil)
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("missing port: %d", rec.Code)
	}
}

// Regression: a private route with an EMPTY stored token must never accept
// an empty presented token (ConstantTimeCompare("","") == 1 bypass).
func TestPrivatePortEmptyStoredTokenDenied(t *testing.T) {
	res := mapResolver{routes: map[string]*Route{
		"sb_empty00000001:3000": {SandboxID: "sb_empty00000001", Port: 3000, Target: "127.0.0.1:1", Public: false, Token: ""},
	}}
	g := New(res, "run.localhost", "", "")
	req := httptest.NewRequest("GET", "http://3000-sb_empty00000001.run.localhost/", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty-token private route accepted unauthenticated request: %d", rec.Code)
	}
}

// Regression: proxy dial failures must not leak internal target addresses
// to internet-facing clients.
func TestProxyErrorDoesNotLeakTarget(t *testing.T) {
	res := mapResolver{routes: map[string]*Route{
		"sb_pub0000000001:3000": {SandboxID: "sb_pub0000000001", Port: 3000, Target: "127.0.0.1:1", Public: true},
	}}
	g := New(res, "run.localhost", "", "")
	req := httptest.NewRequest("GET", "http://3000-sb_pub0000000001.run.localhost/", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") || strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("internal dial detail leaked to client: %s", rec.Body.String())
	}
}
