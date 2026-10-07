package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/config"
	metrics "github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/store"
)

func timeNow() time.Time { return time.Now() }

func authHashKey(k string) string { return auth.HashKey(k) }

func buildShimForTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "shim")
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/shim")
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	return bin
}

func execShim(shimBin, sock, root string) *exec.Cmd {
	return exec.Command(shimBin, "--unix", sock, "--root", root)
}

func newRequest(method, path, body string) (*http.Request, *stringsReader) {
	rd := stringsReader{body}
	req := httptest.NewRequest(method, path, &rd)
	return req, &rd
}

type stringsReader struct{ s string }

func (r *stringsReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

func recRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func extractJSON(body, key string) string {
	var m map[string]any
	if jsonUnmarshalStr(body, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func mintTokenForTest(secret, sandboxID string) string {
	tok, _ := auth.MintSandboxToken(secret, "substrate", sandboxID, []string{"exec"}, time.Hour)
	return tok
}

func forgeTokenWithSecret(secret, sandboxID string) string {
	return mintTokenForTest(secret, sandboxID)
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// helpers for pass-3 security tests
func configValidateForTest(cfg *config.Config) error { return config.ValidateForTest(cfg) }
func requestIDMiddlewareForTest(next http.Handler) http.Handler {
	return metrics.RequestIDMiddleware(next)
}
func requestIDFromCtx(ctx context.Context) string { return metrics.RequestIDFrom(ctx) }

func (s *Server) resolveProjectForTest(ctx context.Context, principal any, requested string) (*store.Project, bool) {
	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req = req.WithContext(context.WithValue(req.Context(), principalKey, principal))
	rec := httptest.NewRecorder()
	return s.resolveProject(rec, req, requested)
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

func DefaultCfgForTest() *config.Config {
	cfg := config.Defaults()
	cfg.API.JWTSecret = strings.Repeat("s", 32)
	cfg.Log.Level = "error"
	return cfg
}

func contextWithPrincipal(ctx context.Context, p *auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func substrateIdempotencyReplays() float64 {
	return testutil.ToFloat64(metrics.IdempotencyReplays)
}

func (e *attackEnv) doIdemRaw(method, path, body string) (int, string) {
	req, _ := newRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Idempotency-Key", "poison-raw-"+method+"-"+path)
	rec := recRecorder()
	e.srv.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
