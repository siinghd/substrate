//go:build e2e

package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestE2EGracefulShutdown: SIGTERM must produce a clean exit (0) within the
// drain window, not a signal kill.
func TestE2EGracefulShutdown(t *testing.T) {
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	bin := filepath.Join(t.TempDir(), "noded")
	build := exec.Command("go", "build", "-o", bin, "./cmd/noded")
	build.Dir = wd
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build noded: %v\n%s", err, out)
	}

	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
mode: standalone
node:
  data_dir: `+dataDir+`
api:
  listen: 127.0.0.1:0
gateway:
  enabled: false
driver:
  name: process
  allow_process: true
`), 0o644)

	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Env = append(os.Environ(), "SUBSTRATE_INSECURE_DEV=1", "SUBSTRATE_API_KEY=sk_term_test")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond) // let it boot
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatal("noded was signal-killed instead of draining")
			}
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("noded did not exit within 10s of SIGTERM")
	}
}
