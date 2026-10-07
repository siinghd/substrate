// Package gateway serves preview URLs (plan §4.4):
//
//	https://{port}-{sandbox_id}.run.{region}.example.dev
//
// Default-private with real bearer auth (the Codespaces model, not
// capability URLs); a port becomes public only when explicitly exposed.
// The route cache is read-through so ingress keeps serving during
// control-plane outages (§5.6).
package gateway

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Route struct {
	SandboxID string
	Port      int
	Target    string // host:port to proxy to
	Public    bool
	Token     string
	Tier      string
	State     string // sandbox state; paused/destroyed routes fail fast
}

// ErrPaused is returned by resolvers when the sandbox cannot serve traffic.
var ErrPaused = httpError{503, "sandbox is paused; any API call resumes it"}

type httpError struct {
	status  int
	message string
}

func (e httpError) Error() string { return e.message }

type Resolver interface {
	Resolve(ctx context.Context, sandboxID string, port int) (*Route, bool)
}

type Server struct {
	resolver   Resolver
	baseDomain string
	tlsCert    string
	tlsKey     string

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	route   *Route
	expires time.Time
}

func New(resolver Resolver, baseDomain, tlsCert, tlsKey string) *Server {
	return &Server{
		resolver:   resolver,
		baseDomain: baseDomain,
		tlsCert:    tlsCert,
		tlsKey:     tlsKey,
		cache:      map[string]cacheEntry{},
	}
}

// parseHost extracts (port, sandboxID) from "{port}-{sb_xxx}.domain".
// Strict grammar: 1-5 digit port, sb_ + platform alphabet — hostile Host
// headers must never reach the resolver.
func parseHost(host, baseDomain string) (int, string, bool) {
	host = strings.ToLower(strings.Split(host, ":")[0])
	suffix := "." + strings.ToLower(baseDomain)
	if !strings.HasSuffix(host, suffix) {
		return 0, "", false
	}
	label := strings.TrimSuffix(host, suffix)
	dash := strings.Index(label, "-")
	if dash <= 0 || dash > 5 {
		return 0, "", false
	}
	port := 0
	for i := 0; i < dash; i++ {
		c := label[i]
		if c < '0' || c > '9' {
			return 0, "", false
		}
		port = port*10 + int(c-'0')
	}
	sbID := label[dash+1:]
	if port < 1 || port > 65535 || len(sbID) != 16 || !strings.HasPrefix(sbID, "sb_") {
		return 0, "", false
	}
	for i := 3; i < len(sbID); i++ {
		c := sbID[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return 0, "", false
		}
	}
	return port, sbID, true
}

// ParseHostForTest exposes the strict grammar to tests.
func ParseHostForTest(host, domain string) (int, string, bool) { return parseHost(host, domain) }

func (g *Server) cached(sandboxID string, port int) *Route {
	key := sandboxID + ":" + itoa(port)
	g.mu.RLock()
	e, ok := g.cache[key]
	g.mu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		return e.route
	}
	return nil
}

// Invalidate drops a cached route immediately. Called on port close and on
// visibility or token changes so a revocation does not wait out the TTL.
func (g *Server) Invalidate(sandboxID string, port int) {
	key := sandboxID + ":" + itoa(port)
	g.mu.Lock()
	delete(g.cache, key)
	g.mu.Unlock()
}

// InvalidateSandbox drops every cached route for one sandbox (destroy/pause).
func (g *Server) InvalidateSandbox(sandboxID string) {
	prefix := sandboxID + ":"
	g.mu.Lock()
	for k := range g.cache {
		if strings.HasPrefix(k, prefix) {
			delete(g.cache, k)
		}
	}
	g.mu.Unlock()
}

func (g *Server) storeCache(r *Route) {
	key := r.SandboxID + ":" + itoa(r.Port)
	g.mu.Lock()
	if len(g.cache) >= cacheMaxEntries {
		g.sweepLocked()
	}
	g.cache[key] = cacheEntry{route: r, expires: time.Now().Add(30 * time.Second)}
	g.mu.Unlock()
}

// sweepLocked drops expired entries; called only when the map is full, so
// amortized cost is O(1) per insert.
func (g *Server) sweepLocked() {
	now := time.Now()
	for k, e := range g.cache {
		if now.After(e.expires) {
			delete(g.cache, k)
		}
	}
	if len(g.cache) >= cacheMaxEntries {
		g.cache = map[string]cacheEntry{} // pathological churn: reset
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// constantTimeEqual avoids leaking the preview token via response timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

const cacheMaxEntries = 65536

func (g *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	port, sbID, ok := parseHost(r.Host, g.baseDomain)
	if !ok {
		http.Error(w, "no such preview host", http.StatusNotFound)
		return
	}

	// The cache exists so ingress keeps serving during a control-plane outage
	// (§5.6). That justifies caching AVAILABILITY data — where to send the
	// request — but not AUTHORIZATION data. Caching Public and Token meant a
	// port flipped private stayed world-readable for the full TTL, a closed
	// port kept serving, and a rotated token kept authenticating against the
	// old value. Availability is cached; the access decision is re-read.
	cached := g.cached(sbID, port)
	fresh, found := g.resolver.Resolve(r.Context(), sbID, port)
	var route *Route
	switch {
	case found:
		route = fresh
		g.storeCache(fresh)
	case cached != nil:
		// Resolver is down: serve the cached TARGET, but only for a route
		// that was public. A private route cannot be authorized without a
		// live token, and a stale token is not an acceptable substitute.
		if !cached.Public {
			http.Error(w, "preview temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		route = cached
	default:
		http.Error(w, "port not exposed", http.StatusNotFound)
		return
	}

	if route.State == "paused" || route.State == "archived" {
		http.Error(w, ErrPaused.message+"; any API call resumes it", http.StatusServiceUnavailable)
		return
	}
	if route.State == "destroyed" || route.State == "destroying" {
		http.Error(w, "sandbox no longer exists", http.StatusNotFound)
		return
	}

	if !route.Public {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get("X-Sandbox-Token")
		}
		if token == "" {
			// browsers cannot set headers; accept ?sandbox_token= but never log it
			token = r.URL.Query().Get("sandbox_token")
		}
		// An empty presented token matches an empty stored token under
		// ConstantTimeCompare; a private route must always require real
		// credentials on BOTH sides.
		if token == "" || route.Token == "" || !constantTimeEqual(token, route.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="substrate-preview"`)
			http.Error(w, "this preview is private; pass the sandbox token", http.StatusUnauthorized)
			return
		}
	}

	// The gateway CONSUMED the caller's credential; the sandbox must never see
	// it. Everything past this line is arbitrary end-user code, so forwarding
	// Authorization verbatim handed a hostile guest whatever the caller
	// happened to send — and an org API key is exactly what a developer tries
	// first, because every other endpoint wants one.
	stripCredentials(r)

	target := &url.URL{Scheme: "http", Host: route.Target}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = guestTransport
	// Stream responses through instead of buffering: a guest serving SSE or a
	// long download must not be held in gateway memory.
	proxy.FlushInterval = 200 * time.Millisecond
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Detail stays server-side: dial errors name internal addresses and
		// must not reach internet-facing clients.
		http.Error(w, "sandbox port unreachable", http.StatusBadGateway)
	}
	r.URL.Host = target.Host
	r.URL.Scheme = target.Scheme
	r.Host = target.Host
	proxy.ServeHTTP(w, r)
}

// credentialHeaders are consumed by the gateway and stripped before the
// request reaches the guest.
var credentialHeaders = []string{
	"Authorization",
	"X-Sandbox-Token",
	"Proxy-Authorization",
}

// stripCredentials removes every form the preview credential can arrive in —
// header and query parameter alike. The query string matters as much as the
// header: the 401 body tells browsers to use ?sandbox_token=, and a query
// parameter lands in the guest's own access log.
func stripCredentials(r *http.Request) {
	for _, h := range credentialHeaders {
		r.Header.Del(h)
	}
	// Forwarding headers are ours to set. ReverseProxy APPENDS to any inbound
	// X-Forwarded-For, so a client-supplied chain would reach the guest as if
	// we had vouched for it; dropping them first makes the guest see exactly
	// one hop, the real one.
	r.Header.Del("X-Forwarded-For")
	r.Header.Del("X-Forwarded-Host")
	r.Header.Del("X-Forwarded-Proto")
	r.Header.Del("Forwarded")
	if q := r.URL.Query(); q.Has("sandbox_token") {
		q.Del("sandbox_token")
		r.URL.RawQuery = q.Encode()
	}
}

// guestTransport talks to untrusted guests, so every phase is bounded. The
// default transport has no response-header timeout at all: a guest that
// accepts the connection and never answers pinned a gateway goroutine, an fd
// and an upstream connection forever — and the gateway is SHARED, so one
// tenant's guest could exhaust ingress for every tenant.
var guestTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	IdleConnTimeout:       60 * time.Second,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   4,
	// Guests are plain HTTP on a point-to-point /32; no proxy, no TLS.
	Proxy: nil,
}

func (g *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		// Without these a client (or a guest holding the response open) keeps
		// a connection and its goroutine indefinitely.
		ReadTimeout: 5 * time.Minute,
		IdleTimeout: 2 * time.Minute,
	}
	if g.tlsCert != "" && g.tlsKey != "" {
		return srv.ServeTLS(ln, g.tlsCert, g.tlsKey)
	}
	return srv.Serve(ln)
}
