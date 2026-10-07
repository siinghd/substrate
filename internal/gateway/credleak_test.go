package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fixedResolver struct{ r *Route }

func (f fixedResolver) Resolve(_ context.Context, _ string, _ int) (*Route, bool) {
	return f.r, f.r != nil
}

// TestGatewayStripsCallerCredentials: everything behind a preview URL is
// arbitrary end-user code. The gateway consumes the caller's credential to
// authorize the request, so it must not also hand that credential to the
// guest — a developer curling a preview URL with their org API key would
// otherwise be donating it to hostile code.
func TestGatewayStripsCallerCredentials(t *testing.T) {
	seen := make(chan http.Header, 1)
	seenURL := make(chan string, 1)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		seenURL <- r.URL.String()
		w.WriteHeader(200)
	}))
	defer guest.Close()
	target := strings.TrimPrefix(guest.URL, "http://")

	const tok = "pt_secret_preview_token"
	g := New(fixedResolver{&Route{SandboxID: "sb_0123456789abc", Port: 8080,
		Target: target, Public: false, Token: tok, State: "running"}}, "run.localhost", "", "")

	for _, tc := range []struct {
		name string
		set  func(*http.Request)
	}{
		{"authorization header", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }},
		{"x-sandbox-token header", func(r *http.Request) { r.Header.Set("X-Sandbox-Token", tok) }},
		{"query parameter", func(r *http.Request) {
			q := r.URL.Query()
			q.Set("sandbox_token", tok)
			r.URL.RawQuery = q.Encode()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/app?a=1", nil)
			req.Host = "8080-sb_0123456789abc.run.localhost"
			// A caller may also be carrying an unrelated org key.
			req.Header.Set("Proxy-Authorization", "Bearer sk_live_org_key")
			req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
			tc.set(req)

			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("proxy returned %d: %s", rec.Code, rec.Body.String())
			}

			h := <-seen
			gotURL := <-seenURL
			for _, bad := range []string{"Authorization", "X-Sandbox-Token", "Proxy-Authorization"} {
				if v := h.Get(bad); v != "" {
					t.Errorf("guest received %s: %q — credential leaked into the sandbox", bad, v)
				}
			}
			if strings.Contains(gotURL, "sandbox_token") || strings.Contains(gotURL, tok) {
				t.Errorf("guest received the token in the URL: %q", gotURL)
			}
			if u, err := url.Parse(gotURL); err == nil && u.Query().Get("a") != "1" {
				t.Errorf("stripping the token also dropped the caller's own query: %q", gotURL)
			}
			if xff := h.Values("X-Forwarded-For"); len(xff) > 0 && strings.Contains(xff[0], "10.0.0.1") {
				t.Errorf("guest received a client-spoofed X-Forwarded-For: %v", xff)
			}
		})
	}
}

// TestGatewayPublicRouteAlsoStrips: a public port needs no credential, but a
// caller may still be carrying one. It must not reach the guest either.
func TestGatewayPublicRouteAlsoStrips(t *testing.T) {
	seen := make(chan http.Header, 1)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer guest.Close()

	g := New(fixedResolver{&Route{SandboxID: "sb_0123456789abc", Port: 8080,
		Target: strings.TrimPrefix(guest.URL, "http://"), Public: true, State: "running"}},
		"run.localhost", "", "")

	req := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/", nil)
	req.Host = "8080-sb_0123456789abc.run.localhost"
	req.Header.Set("Authorization", "Bearer sk_live_org_key")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("proxy returned %d", rec.Code)
	}
	if v := (<-seen).Get("Authorization"); v != "" {
		t.Fatalf("public route leaked Authorization to the guest: %q", v)
	}
}

// mutableResolver lets a test change the route between requests, the way the
// control plane does when a port is closed or flipped private.
type mutableResolver struct {
	mu sync.Mutex
	r  *Route
}

func (m *mutableResolver) Resolve(context.Context, string, int) (*Route, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.r == nil {
		return nil, false
	}
	cp := *m.r
	return &cp, true
}

func (m *mutableResolver) set(r *Route) {
	m.mu.Lock()
	m.r = r
	m.mu.Unlock()
}

// TestGatewayDoesNotCacheAuthorization: the 30s route cache had no
// invalidation path, so a port flipped public->private stayed world-readable
// for the rest of the TTL, a closed port kept serving, and a rotated token kept
// authenticating against the stale value. Availability may be cached; the
// access decision may not.
func TestGatewayDoesNotCacheAuthorization(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer guest.Close()
	target := strings.TrimPrefix(guest.URL, "http://")

	res := &mutableResolver{r: &Route{SandboxID: "sb_0123456789abc", Port: 8080,
		Target: target, Public: true, State: "running"}}
	g := New(res, "run.localhost", "", "")

	get := func() int {
		req := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/", nil)
		req.Host = "8080-sb_0123456789abc.run.localhost"
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(); code != 200 {
		t.Fatalf("public route: %d", code)
	}
	// Customer notices and makes it private. The very next request must need
	// a credential — not 30 seconds later.
	res.set(&Route{SandboxID: "sb_0123456789abc", Port: 8080, Target: target,
		Public: false, Token: "tok_new", State: "running"})
	if code := get(); code != 401 {
		t.Fatalf("after flipping the port private, anonymous request got %d; want 401 — authorization was served from cache", code)
	}

	// Closing the port entirely must stop it serving immediately.
	res.set(nil)
	if code := get(); code == 200 {
		t.Fatal("closed port still served from cache")
	}
}

// TestGatewayCachedTargetSurvivesResolverOutageOnlyWhenPublic: the cache
// exists so ingress survives a control-plane outage (§5.6). That is fine for a
// public route; a private one cannot be authorized without a live token.
func TestGatewayCachedTargetSurvivesResolverOutageOnlyWhenPublic(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer guest.Close()
	target := strings.TrimPrefix(guest.URL, "http://")

	do := func(g *Server) int {
		req := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/", nil)
		req.Host = "8080-sb_0123456789abc.run.localhost"
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec.Code
	}

	// Public route, then the resolver goes away.
	res := &mutableResolver{r: &Route{SandboxID: "sb_0123456789abc", Port: 8080,
		Target: target, Public: true, State: "running"}}
	g := New(res, "run.localhost", "", "")
	if code := do(g); code != 200 {
		t.Fatalf("warm-up: %d", code)
	}
	res.set(nil)
	if code := do(g); code != 200 {
		t.Fatalf("public route did not survive a resolver outage: %d", code)
	}

	// Private route, then the resolver goes away: must fail closed.
	res2 := &mutableResolver{r: &Route{SandboxID: "sb_0123456789abc", Port: 8080,
		Target: target, Public: false, Token: "tok", State: "running"}}
	g2 := New(res2, "run.localhost", "", "")
	req := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/", nil)
	req.Host = "8080-sb_0123456789abc.run.localhost"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	g2.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("warm-up private: %d", rec.Code)
	}
	res2.set(nil)
	rec = httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "http://8080-sb_0123456789abc.run.localhost/", nil)
	req2.Host = "8080-sb_0123456789abc.run.localhost"
	req2.Header.Set("Authorization", "Bearer tok")
	g2.ServeHTTP(rec, req2)
	if rec.Code == 200 {
		t.Fatal("private route was authorized from a stale cached token during a resolver outage")
	}
}
