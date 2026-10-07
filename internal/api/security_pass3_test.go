package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/substrate-sh/substrate/internal/config"
	fc "github.com/substrate-sh/substrate/internal/vmm/firecracker"
)

// TestJailerNeverRoot: per plan §7.2 a VMM escape must land in an
// unprivileged context — allocated UIDs must never be 0 and must be unique.
func TestJailerNeverRoot(t *testing.T) {
	d := fc.NewDriver(fc.Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: t.TempDir()})
	seen := map[uint32]bool{}
	for i := 0; i < 100; i++ {
		uid, gid := d.UIDForTest("sb_0123456789012")
		if uid == 0 || gid == 0 {
			t.Fatalf("root uid/gid allocated: %d/%d", uid, gid)
		}
		if seen[uid] {
			t.Fatalf("uid %d reused across sandboxes", uid)
		}
		seen[uid] = true
	}
}

// TestJWTSecretFloorEnforced: short secrets must be rejected outside dev mode.
func TestJWTSecretFloorEnforced(t *testing.T) {
	os.Unsetenv("SUBSTRATE_INSECURE_DEV")
	cfg := config.Defaults()
	cfg.Mode = "standalone"
	cfg.Driver.Name = "process"
	cfg.Driver.AllowProcess = true
	cfg.API.JWTSecret = "short"

	if err := configValidateForTest(cfg); err == nil || !containsStr(err.Error(), "32 characters") {
		t.Fatalf("short secret accepted: %v", err)
	}
	os.Setenv("SUBSTRATE_INSECURE_DEV", "1")
	defer os.Unsetenv("SUBSTRATE_INSECURE_DEV")
	if err := configValidateForTest(cfg); err != nil {
		t.Fatalf("dev mode should allow short secret: %v", err)
	}
}

func TestRequestIDInjectionResistant(t *testing.T) {
	hostile := []string{"id with spaces", "../../etc", "<script>", "id\ninjection-header"}
	for _, h := range hostile {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.Header.Set("X-Request-Id", h)
		rec := httptest.NewRecorder()
		var got string
		mw := requestIDMiddlewareForTest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = requestIDFromCtx(r.Context())
		}))
		mw.ServeHTTP(rec, req)

		echoed := rec.Header().Get("X-Request-Id")
		if echoed == h {
			t.Fatalf("hostile id %q echoed verbatim", h)
		}
		if got != echoed {
			t.Fatalf("ctx/header mismatch: %q vs %q", got, echoed)
		}
		if stringsContainsAny(echoed, " \n\r<>/") {
			t.Fatalf("echoed id contains unsafe chars: %q", echoed)
		}
	}
}

func stringsContainsAny(s, chars string) bool {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return true
			}
		}
	}
	return false
}

var _ = os.Getenv
