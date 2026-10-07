package api

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestAttackShimSocketPerms: a world-connectable agent socket is a local
// privilege escalation (any user could exec commands). The shim must chmod
// it to 0600 regardless of umask.
func TestAttackShimSocketPerms(t *testing.T) {
	shim := buildShimForTest(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "agent.sock")

	cmd := execShim(shim, sock, dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })

	// wait for socket to appear
	deadline := 50
	for i := 0; i < deadline; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		sleepMS(20)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal("socket never appeared")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("agent socket is %o — other users can connect and execute commands", perm)
	}

	// and prove an actual connection works for the OWNER (functional check)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("owner cannot connect: %v", err)
	}
	conn.Close()
}
