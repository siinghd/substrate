package gvisor

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// maxProcs caps processes per sandbox. Generous for real workloads (a build,
// a language server, a test run), small enough that a fork bomb cannot consume
// the node's memory through sentry task overhead.
const maxProcs = 512

// writeOCIConfig emits the runtime spec for one sandbox.
//
// Written by hand rather than shelled out to `runsc spec` so the security
// posture is explicit and reviewable in one place: no host mounts beyond the
// single socket directory, no ambient capabilities, no privileged devices.
func writeOCIConfig(bundle, sockDir, shimBin, rootfsPath string, spec vmm.Spec) error {
	// netnsPath, when set, makes the sandbox JOIN an existing namespace that
	// already has an addressed veth in it, instead of getting a fresh empty
	// one. That is how this tier gets a network at all: runsc attaches its
	// netstack to whatever interface it finds in the namespace.
	netnsPath := ""
	if spec.Net != nil {
		netnsPath = spec.Net.NSPath
	}
	mem := spec.Template.MemMiB
	if mem <= 0 {
		mem = 512
	}
	vcpus := spec.Template.VCPUs
	if vcpus <= 0 {
		vcpus = 1
	}

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}

	cfg := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			// The agent is the sandbox's init: it holds the SGP/1 connection and
			// spawns every exec, so the container lives exactly as long as it.
			// It CONNECTS OUT to a socket the host already created -- see the
			// comment in Create for why the reverse cannot work under gVisor.
			// --token-file names a secret the agent reads and UNLINKS before
			// it serves anything. It is not passed in argv or env: both are
			// readable by tenant code through /proc/1, which would make the
			// token worthless the moment guest code ran.
			"args": []string{"/" + shimInGuest,
				"--connect", sockDirGuest + "/" + agentSock,
				"--token-file", sockDirGuest + "/" + agentToken,
				"--root", "/"},
			"env": env,
			"cwd": "/",
			"capabilities": map[string]any{
				// Empty across the board. Guest code is untrusted by
				// definition, and gVisor's own kernel provides everything the
				// workload legitimately needs.
				"bounding":    []string{},
				"effective":   []string{},
				"inheritable": []string{},
				"permitted":   []string{},
				"ambient":     []string{},
			},
			"rlimits": []any{
				map[string]any{"type": "RLIMIT_NOFILE", "hard": 8192, "soft": 8192},
				// Without a process ceiling `ulimit -u` inside the sandbox
				// reports "unlimited" and a fork bomb is unbounded. Each task
				// is a sentry task backed by HOST memory, which the guest's
				// own memory limit does not cover -- so the blast radius is
				// the node, not the sandbox.
				map[string]any{"type": "RLIMIT_NPROC", "hard": maxProcs, "soft": maxProcs},
			},
			"noNewPrivileges": true,
		},
		// An ABSOLUTE path to a SHARED, node-prepared template, not a
		// per-sandbox copy. Every sandbox using this template names the same
		// directory; runsc stacks a private, size-capped overlay on top of it
		// (see Create), so writes never reach it. See prepareTemplate.
		"root":     map[string]any{"path": rootfsPath, "readonly": false},
		"hostname": "sandbox",
		"mounts": []any{
			map[string]any{"destination": "/proc", "type": "proc", "source": "proc"},
			map[string]any{
				"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
				"options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
			map[string]any{
				"destination": "/dev/pts", "type": "devpts", "source": "devpts",
				"options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"},
			},
			map[string]any{
				"destination": "/dev/shm", "type": "tmpfs", "source": "shm",
				"options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
			},
			map[string]any{
				"destination": "/sys", "type": "sysfs", "source": "sysfs",
				"options": []string{"nosuid", "noexec", "nodev", "ro"},
			},
			// The ONLY host path the sandbox can WRITE to: the directory
			// holding its own agent socket.
			map[string]any{
				"destination": sockDirGuest, "type": "bind",
				"source":  sockDir,
				"options": []string{"rbind", "rw"},
			},
			// The agent, bind-mounted READ-ONLY straight from the host.
			//
			// It used to be COPIED into a per-sandbox rootfs the guest could
			// write, which is what made the shim-staging symlink attack
			// possible in the first place. Mounted ro it cannot be replaced,
			// relinked, or truncated from inside the sandbox at all -- the
			// attack has no surface left rather than a guarded one -- and
			// nothing is copied per sandbox.
			map[string]any{
				"destination": "/" + shimInGuest, "type": "bind",
				"source":  shimBin,
				"options": []string{"bind", "ro"},
			},
		},
		"linux": map[string]any{
			"resources": map[string]any{
				"memory": map[string]any{"limit": int64(mem) * 1024 * 1024},
				"cpu":    map[string]any{"shares": uint64(vcpus) * 1024},
				// Belt and braces with RLIMIT_NPROC above: the rlimit is
				// enforced by the sentry per-user, the pids cgroup by the host.
				"pids": map[string]any{"limit": int64(maxProcs)},
			},
			"namespaces": []any{
				map[string]any{"type": "pid"},
				map[string]any{"type": "ipc"},
				map[string]any{"type": "uts"},
				map[string]any{"type": "mount"},
				func() map[string]any {
					ns := map[string]any{"type": "network"}
					if netnsPath != "" {
						ns["path"] = netnsPath
					}
					return ns
				}(),
			},
		},
	}

	blob, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundle, "config.json"), blob, 0o644)
}
