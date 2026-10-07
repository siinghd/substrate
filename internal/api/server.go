// Package api serves the public v1 REST surface (plan §6.1): REST/JSON with
// SSE for streams, mandatory Idempotency-Key on mutations for API-key
// principals, and sandbox-scoped JWT auth for end-user agents.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

type Server struct {
	cfg   *config.Config
	log   *logging.Logger
	store store.Store
	sup   *reconcile.Supervisor

	execMu      sync.Mutex
	execHandles map[string]*vmm.ExecHandle
	execWaits   *execWaiter
}

func NewServer(cfg *config.Config, log *logging.Logger, st store.Store, sup *reconcile.Supervisor) *Server {
	return &Server{cfg: cfg, log: log, store: st, sup: sup, execHandles: map[string]*vmm.ExecHandle{}, execWaits: newExecWaiter()}
}

// ---- errors ----

type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

func errNotFound(what string) *apiError {
	return &apiError{Status: 404, Code: "not_found", Message: what + " does not exist"}
}
func errBadRequest(msg, fix string) *apiError {
	return &apiError{Status: 400, Code: "bad_request", Message: msg, Fix: fix}
}
func errUnauthorized() *apiError {
	return &apiError{Status: 401, Code: "unauthorized", Message: "missing or invalid credentials", Fix: "send Authorization: Bearer <key|token>"}
}
func errForbidden(why string) *apiError {
	return &apiError{Status: 403, Code: "forbidden", Message: why}
}
func errQuota(msg string) *apiError {
	return &apiError{Status: 402, Code: "quota_exceeded", Message: msg}
}

func errInternal(err error) *apiError {
	return &apiError{Status: 500, Code: "internal", Message: "internal error"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, e)
}

// maxJSONBody bounds a JSON request body. Reading exactly this many bytes and
// then parsing would silently truncate an over-large document into a confusing
// syntax error; read one byte past the limit so oversize is reported as 413.
const maxJSONBody = 4 << 20

func readJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		writeErr(w, errBadRequest("unreadable body", ""))
		return false
	}
	if len(body) > maxJSONBody {
		writeErr(w, &apiError{Status: 413, Code: "body_too_large",
			Message: "JSON body exceeds 4MiB",
			Fix:     "send large payloads through the fs endpoints, not the JSON body"})
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeErr(w, errBadRequest("invalid JSON: "+err.Error(), "check the request schema in the API reference"))
		return false
	}
	return true
}

// ---- middleware ----

type ctxKey string

const principalKey ctxKey = "principal"

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// sandbox tokens may also arrive as X-Sandbox-Token (preview URLs)
		hdr := r.Header.Get("Authorization")
		token := strings.TrimPrefix(hdr, "Bearer ")
		if token == "" {
			token = r.Header.Get("X-Sandbox-Token")
		}
		if token == "" {
			writeErr(w, errUnauthorized())
			return
		}

		switch {
		case strings.HasPrefix(token, "ak_"):
			// The admin credential lives in configuration, never the database.
			// Empty means the admin API is OFF -- fail closed, so a node that
			// simply never set one cannot be provisioned by anybody.
			admin := s.cfg.API.AdminKey
			if admin == "" || !auth.ConstantTimeEqual(token, admin) {
				writeErr(w, errUnauthorized())
				return
			}
			p := &auth.Principal{Kind: auth.KindAdmin}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		case strings.HasPrefix(token, "sk_") || strings.HasPrefix(token, "pk_"):
			key, err := s.store.GetAPIKeyByHash(r.Context(), auth.HashKey(token))
			if err != nil || key.RevokedAt != nil {
				writeErr(w, errUnauthorized())
				return
			}
			p := &auth.Principal{Kind: auth.KindOrg, OrgID: key.OrgID, ProjectID: key.ProjectID}
			if key.Kind == "project" {
				p.Kind = auth.KindProject
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		default:
			// sandbox JWT; audience resolved per-route from the path
			p := &auth.Principal{Kind: auth.KindSandbox, Scopes: []string{"*"}}
			sbID := sandboxIDFromPath(r.URL.Path)
			if sbID != "" {
				cl, err := auth.VerifySandboxToken(s.cfg.API.JWTSecret, token, sbID)
				if err != nil {
					writeErr(w, &apiError{Status: 401, Code: "unauthorized", Message: err.Error()})
					return
				}
				p.SandboxID = sbID
				p.Scopes = cl.Scopes
				sb, err := s.store.GetSandbox(r.Context(), sbID)
				if err != nil {
					writeErr(w, errNotFound("sandbox"))
					return
				}
				p.OrgID = sb.OrgID
				p.ProjectID = sb.ProjectID
			} else if r.URL.Path == "/v1/execs" || strings.HasPrefix(r.URL.Path, "/v1/execs/") {
				eid := execIDFromPath(r)
				ex, err := s.store.GetExec(r.Context(), eid)
				if err != nil {
					writeErr(w, errNotFound("exec"))
					return
				}
				cl, err := auth.VerifySandboxToken(s.cfg.API.JWTSecret, token, ex.SandboxID)
				if err != nil {
					writeErr(w, &apiError{Status: 401, Code: "unauthorized", Message: err.Error()})
					return
				}
				p.SandboxID = ex.SandboxID
				p.Scopes = cl.Scopes
			} else {
				writeErr(w, errForbidden("sandbox tokens are scoped to one sandbox; pass the sandbox id in the path"))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		}
	})
}

// execIDFromPath resolves the exec id for the sandbox-JWT audience check.
//
// It used to take the LAST path segment, which is the exec id only for
// /v1/execs/{id} — for /v1/execs/{id}/stream and /v1/execs/{id}/stdin it
// picked up "stream" and "stdin", so GetExec failed and the middleware
// answered 404 before the handler ran. That broke the entire exec loop for
// sandbox-scoped tokens (both SDKs poll /stream for output), and the 404 read
// as "the exec was deleted", pointing debugging at a retention bug that does
// not exist. The route pattern already carries the id; prefer it.
func execIDFromPath(r *http.Request) string {
	if id := r.PathValue("id"); id != "" {
		return id
	}
	return lastPathSegment(r.URL.Path)
}

func sandboxIDFromPath(p string) string {
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, "sb_") {
			return part
		}
	}
	return ""
}

func lastPathSegment(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	buf    bytesBuffer
}

type bytesBuffer struct {
	data []byte
}

const maxCachedBody = 1 << 20

func (b *bytesBuffer) append(p []byte) (int, error) {
	if len(b.data) < maxCachedBody {
		room := maxCachedBody - len(b.data)
		if len(p) < room {
			room = len(p)
		}
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil // always report full write to the handler
}

// Write captures the response body for idempotency caching AND forwards it
// to the underlying writer. Named-field methods don't promote in Go, so this
// must be explicit — without it the cache silently stays empty.
func (rr *responseRecorder) Write(p []byte) (int, error) {
	if _, err := rr.buf.append(p); err != nil {
		return 0, err
	}
	return rr.ResponseWriter.Write(p)
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// idempotencyMiddleware replays identical mutations within 24h keyed on
// (principal, key, path) with a request-body hash guard; replaying an exec
// returns the SAME exec handle (§6.1). Response bodies up to 1MB are cached.
func (s *Server) idempotencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		p := principal(r)
		if key == "" {
			if p.Kind == auth.KindOrg || p.Kind == auth.KindProject {
				writeErr(w, &apiError{
					Status: 400, Code: "idempotency_key_required",
					Message: "mutating requests require an Idempotency-Key header",
					Fix:     "SDKs set this automatically; generate a UUID per logical operation",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// The body has to be read here to hash it for the idempotency claim,
		// and then replayed to the handler. It must NEVER be truncated: the
		// previous io.LimitReader(4MiB) cut every fs write above 4MiB and the
		// handler happily wrote the short file with a 201 — silent corruption
		// that only org/project keys hit, because only they are required to
		// send an Idempotency-Key.
		mem, spill, reqHash, err := bufferBody(r)
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				writeErr(w, &apiError{Status: 413, Code: "body_too_large",
					Message: "request body exceeds the 64MiB limit",
					Fix:     "split the write, or stream it in chunks"})
				return
			}
			writeErr(w, errBadRequest("unreadable body", ""))
			return
		}
		if spill != nil {
			defer spill.Close()
			r.Body = io.NopCloser(spill)
		} else {
			r.Body = io.NopCloser(bytes.NewReader(mem))
		}

		// Principal identity MUST be part of the cache key: a sandbox-token
		// denial must never poison an org-key request on the same path.
		idemKey := string(p.Kind) + ":" + p.OrgID + ":" + p.ProjectID + ":" + p.SandboxID + ":" + key

		// Fast path: completed response already cached → replay.
		if rec, err := s.store.GetIdempotency(r.Context(), idemKey, r.URL.Path); err == nil &&
			rec.Status != 0 && rec.ReqHash == reqHash && time.Since(rec.CreatedAt) < 24*time.Hour {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(rec.Status)
			w.Write([]byte(rec.Body))
			metrics.IncIdempotencyReplay()
			return
		}

		// Claim: exactly one executor per (key,path). Losers poll for the
		// winner's response with exponential backoff (50ms→1s ≈ 16 queries
		// over the window vs ~60 at a fixed 250ms) — concurrent identical
		// retries can never double-execute (plan §6.1).
		claimed, cerr := s.store.ClaimIdempotency(r.Context(), idemKey, r.URL.Path, reqHash)
		if cerr != nil || !claimed {
			pollDeadline := time.Now().Add(15 * time.Second)
			delay := 50 * time.Millisecond
			for time.Now().Before(pollDeadline) {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
				}
				if delay < time.Second {
					delay *= 2
				}
				rec, err := s.store.GetIdempotency(r.Context(), idemKey, r.URL.Path)
				if err == nil && rec.Status != 0 {
					if rec.ReqHash == reqHash {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Idempotent-Replayed", "true")
						w.WriteHeader(rec.Status)
						w.Write([]byte(rec.Body))
						metrics.IncIdempotencyReplay()
						return
					}
					break // same key, different body: execute uncached
				}
				if err != nil {
					break // winner deleted the row (non-2xx): execute uncached
				}
			}
			// claim timeout or winner-failed: execute uncached below
		}

		isClaimant := cerr == nil && claimed
		rr := &responseRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rr, r)

		// Only successful responses are replayed. Denials (401/403) and
		// state-dependent failures (404/409) must re-execute on retry.
		if isClaimant {
			if rr.status >= 200 && rr.status < 300 && len(rr.buf.data) > 0 &&
				strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
				if err := s.store.CompleteIdempotency(r.Context(), idemKey, r.URL.Path,
					rr.status, string(rr.buf.data), reqHash); err != nil {
					s.log.Error("idempotency cache failed", "path", r.URL.Path, "err", err.Error())
				}
			} else {
				// release the claim so concurrent/future retries re-execute
				_ = s.store.DeleteIdempotency(r.Context(), idemKey, r.URL.Path)
			}
		}
	})
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// maxRequestBody is the hard ceiling on any mutating request body. It matches
// the largest handler limit (fs write) so the middleware never becomes the
// silent bottleneck the handler doesn't know about. A var, not a const, so
// tests can exercise the ceiling without moving 64MiB (same idiom as
// maxExecLogBytes).
var maxRequestBody int64 = 64 << 20

// bodySpillThreshold is how much of a body the middleware keeps in RAM.
// Anything larger spools to an unlinked temp file, so a 64MiB upload costs
// O(1) heap instead of O(body) x in-flight requests.
const bodySpillThreshold = 1 << 20

var errBodyTooLarge = errors.New("request body too large")

// bufferBody reads the request body in full so it can be both hashed for the
// idempotency claim and replayed to the handler, without truncating it and
// without holding it all in memory. Returns either an in-memory body or an
// open, already-unlinked temp file positioned at offset 0 — never both.
func bufferBody(r *http.Request) ([]byte, *os.File, string, error) {
	h := sha256.New()
	lim := io.LimitReader(r.Body, maxRequestBody+1)

	var head bytes.Buffer
	n, err := io.CopyN(&head, lim, bodySpillThreshold)
	if err != nil && err != io.EOF {
		return nil, nil, "", err
	}
	h.Write(head.Bytes())
	if n < bodySpillThreshold {
		return head.Bytes(), nil, hex.EncodeToString(h.Sum(nil)), nil
	}

	f, ferr := os.CreateTemp("", "substrate-body-*")
	if ferr != nil {
		return nil, nil, "", ferr
	}
	// Unlink now: the open fd keeps the data alive, and nothing can leak the
	// file if this process dies mid-request.
	_ = os.Remove(f.Name())
	if _, err := f.Write(head.Bytes()); err != nil {
		f.Close()
		return nil, nil, "", err
	}
	rest, cerr := io.Copy(io.MultiWriter(f, h), lim)
	if cerr != nil {
		f.Close()
		return nil, nil, "", cerr
	}
	if n+rest > maxRequestBody {
		f.Close()
		return nil, nil, "", errBodyTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, "", err
	}
	return nil, f, hex.EncodeToString(h.Sum(nil)), nil
}

var _ = vmm.ErrUnsupported

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				writeErr(w, errInternal(errors.New("panic")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireScope guards sandbox-token principals.
func requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if !p.Can(scope) {
			writeErr(w, errForbidden("token lacks scope "+scope))
			return
		}
		next(w, r)
	}
}

// ---- bootstrap ----

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "time": time.Now().UTC()})
	})

	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			s.idempotencyMiddleware(h).ServeHTTP(w, r)
		}))
	}

	route("POST /v1/sandboxes", requireScope("create", s.handleCreateSandbox))
	mux.Handle("GET /v1/sandboxes", s.authMiddleware(s.handleListSandboxes))
	mux.Handle("GET /v1/sandboxes/{id}", s.authMiddleware(s.handleGetSandbox))
	route("DELETE /v1/sandboxes/{id}", requireScope("destroy", s.handleDestroySandbox))

	route("POST /v1/sandboxes/{id}/exec", requireScope("exec", s.handleExec))
	route("POST /v1/sandboxes/{id}/pause", requireScope("lifecycle", s.handlePause))
	route("POST /v1/sandboxes/{id}/resume", requireScope("lifecycle", s.handleResume))
	route("POST /v1/sandboxes/{id}/snapshots", requireScope("lifecycle", s.handleSnapshot))

	mux.Handle("GET /v1/execs/{id}", s.authMiddleware(s.handleGetExec))
	mux.Handle("GET /v1/execs/{id}/stream", s.authMiddleware(s.handleExecStream))
	route("POST /v1/execs/{id}/stdin", requireScope("exec", s.handleExecStdin))
	route("DELETE /v1/execs/{id}", requireScope("exec", s.handleExecKill))

	mux.Handle("GET /v1/sandboxes/{id}/fs/{path...}", s.authMiddleware(requireScope("fs", s.handleFsRead)))
	route("PUT /v1/sandboxes/{id}/fs/{path...}", requireScope("fs", s.handleFsWrite))
	route("DELETE /v1/sandboxes/{id}/fs/{path...}", requireScope("fs", s.handleFsDelete))

	route("POST /v1/sandboxes/{id}/ports", requireScope("ports", s.handleExposePort))
	route("DELETE /v1/sandboxes/{id}/ports/{port}", requireScope("ports", s.handleClosePort))

	route("POST /v1/sandboxes/{id}/tokens", s.handleMintToken)

	mux.Handle("GET /v1/templates", s.authMiddleware(s.handleListTemplates))
	route("POST /v1/templates", s.handlePutTemplate)

	mux.Handle("GET /v1/events", s.authMiddleware(s.handleListEvents))
	mux.Handle("GET /v1/usage", s.authMiddleware(s.handleUsage))
	mux.Handle("GET /v1/usage/invoice", s.authMiddleware(s.handleInvoice))

	// Tenant provisioning. Every route is admin-only; a non-admin credential
	// gets 404 so the surface is not discoverable from a tenant key.
	mux.Handle("POST /v1/admin/orgs", s.authMiddleware(requireAdmin(s.handleAdminCreateOrg)))
	mux.Handle("GET /v1/admin/orgs", s.authMiddleware(requireAdmin(s.handleAdminListOrgs)))
	mux.Handle("POST /v1/admin/projects", s.authMiddleware(requireAdmin(s.handleAdminCreateProject)))
	mux.Handle("GET /v1/admin/projects", s.authMiddleware(requireAdmin(s.handleAdminListProjects)))
	mux.Handle("POST /v1/admin/keys", s.authMiddleware(requireAdmin(s.handleAdminCreateKey)))
	mux.Handle("GET /v1/admin/keys", s.authMiddleware(requireAdmin(s.handleAdminListKeys)))
	mux.Handle("DELETE /v1/admin/keys/{id}", s.authMiddleware(requireAdmin(s.handleAdminRevokeKey)))
	mux.Handle("POST /v1/admin/orgs/{id}/suspend", s.authMiddleware(requireAdmin(s.handleAdminSuspendOrg)))

	return s.recoverMiddleware(mux)
}

var _ = ids.Sandbox
