// Command substrate-shim is the process-tier guest agent: a Go
// reimplementation of the guestd SGP/1 surface over a unix socket, used when
// the platform runs without KVM (dev boxes, CI). It implements path
// containment and process-group discipline exactly like guestd so host-side
// code cannot tell the tiers apart at the protocol level.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
)

type server struct {
	root  string
	token string

	mu     sync.Mutex
	pgidBy map[string]int
}

// resetForReconnect drops per-connection state after the host goes away.
//
// pgidBy maps exec ids to process groups. Those execs died with the connection
// (the v1 contract), so keeping the map would leak an entry per exec across
// every reconnect and, worse, let a recycled exec id signal a stale pgid --
// which after pid reuse is a signal delivered to an unrelated process group.
func (s *server) resetForReconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pgid := range s.pgidBy {
		// The host can no longer reach these; nothing will ever reap or
		// report them, so stop them rather than leaving them running against
		// the sandbox's process ceiling.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	s.pgidBy = map[string]int{}
}

func main() {
	// The agent socket must be private: anything that can dial it can run
	// commands as this user inside the sandbox. Restrict umask so the socket
	// inode lands at 0600 regardless of the spawner's umask.
	syscall.Umask(0o077)

	unixPath := flag.String("unix", "", "listen on this unix socket")
	connectPath := flag.String("connect", "", "dial this unix socket instead of listening")
	root := flag.String("root", ".", "filesystem root for fs ops and default cwd")
	tokenFile := flag.String("token-file", "", "read the agent token from this file, then unlink it")
	flag.Parse()
	if *unixPath == "" && *connectPath == "" {
		fmt.Fprintln(os.Stderr, "shim: --unix or --connect required")
		os.Exit(2)
	}
	s := &server{root: abs(*root), pgidBy: map[string]int{}}
	go reapOrphans()

	// Dial-out mode exists for the gVisor tier. A unix socket BOUND inside a
	// gVisor sandbox lives in the sentry's own VFS and is invisible on the
	// host — the gofer does not propagate it — so the listen-inside model
	// cannot work there. The host listens instead and the agent connects out;
	// SGP/1 is symmetric, so nothing else changes.
	if *connectPath != "" {
		// READ THE TOKEN AND UNLINK IT BEFORE SERVING ANYTHING.
		//
		// This runs as the sandbox's PID 1, and the OCI spec starts nothing
		// else: guest code exists only once this process spawns it, which
		// happens only in response to an exec request we have not begun
		// accepting yet. So this is the one window in the sandbox's life where
		// a secret can be handed in through the filesystem, and after the
		// unlink it lives only in this process's memory.
		//
		// Fail CLOSED: if the unlink does not happen the token is readable by
		// tenant code, and an agent whose identity the tenant can forge is
		// worse than no sandbox.
		if *tokenFile != "" {
			b, err := os.ReadFile(*tokenFile)
			if err != nil {
				fatal(fmt.Errorf("read agent token: %w", err))
			}
			if err := os.Remove(*tokenFile); err != nil {
				fatal(fmt.Errorf("could not unlink the agent token, refusing to run: %w", err))
			}
			s.token = strings.TrimSpace(string(b))
		}
		serveConnected(s, *connectPath)
		return
	}

	os.Remove(*unixPath)
	ln, err := net.Listen("unix", *unixPath)
	if err != nil {
		fatal(err)
	}
	// Belt-and-braces: enforce 0600 even if umask was overridden.
	if err := os.Chmod(*unixPath, 0o600); err != nil {
		fatal(err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

// dialWithRetry waits for the host's listener to be reachable. The sandbox can
// start before the host has accepted, and a one-shot dial would lose the race.
// serveConnected runs the dial-out agent for the life of the SANDBOX, not the
// life of one connection.
//
// It used to be a single dial followed by `s.handle(conn); return` -- and this
// process is the sandbox's PID 1, so returning killed the sandbox. Any
// transport hiccup at all (a mux desync, a slow reader, an unclean noded exit)
// therefore destroyed the sandbox AND everything the tenant had written into
// it, unrecoverably, with no way for the host to reattach. Measured before the
// fix: closing the host side of a healthy sandbox's connection left
// `runsc state` reporting "file does not exist".
//
// The host keeps its listener open and re-accepts, so the correct behaviour
// here is to reconnect and carry on. In-flight execs still die with their
// connection -- that is the documented v1 contract -- but the sandbox and its
// filesystem survive.
func serveConnected(s *server, path string) {
	for {
		conn, err := dialWithRetry(path, reconnectWindow)
		if err != nil {
			// The host is gone for good, or never came back. Exiting here is
			// correct: it stops the sandbox rather than leaving it running
			// with nothing supervising it.
			fatal(err)
		}
		s.handle(conn)
		// handle returned: the host went away. Reset per-connection state and
		// wait for it to come back.
		s.resetForReconnect()
	}
}

// reconnectWindow bounds how long the agent waits for the host to come back
// before giving up and letting the sandbox stop.
const reconnectWindow = 60 * time.Second

func dialWithRetry(path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("could not reach host agent socket %s: %v", path, last)
}

// reapedStatus holds exit statuses the orphan reaper collected, so an exec
// whose child the reaper happened to reach first can still report the right
// code instead of -1.
var reapedStatus struct {
	mu sync.Mutex
	m  map[int]syscall.WaitStatus
}

const maxReapedStatuses = 1024

func rememberReaped(pid int, ws syscall.WaitStatus) {
	reapedStatus.mu.Lock()
	defer reapedStatus.mu.Unlock()
	if reapedStatus.m == nil {
		reapedStatus.m = map[int]syscall.WaitStatus{}
	}
	if len(reapedStatus.m) >= maxReapedStatuses {
		// Bounded: entries are only ever read by an exec that lost the race,
		// so dropping the excess costs an exit code, never memory.
		for k := range reapedStatus.m {
			delete(reapedStatus.m, k)
			break
		}
	}
	reapedStatus.m[pid] = ws
}

// takeReaped waits briefly for the reaper to publish pid's status.
func takeReaped(pid int, within time.Duration) (syscall.WaitStatus, bool) {
	deadline := time.Now().Add(within)
	for {
		reapedStatus.mu.Lock()
		ws, ok := reapedStatus.m[pid]
		if ok {
			delete(reapedStatus.m, pid)
		}
		reapedStatus.mu.Unlock()
		if ok {
			return ws, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func forgetReaped(pid int) {
	reapedStatus.mu.Lock()
	delete(reapedStatus.m, pid)
	reapedStatus.mu.Unlock()
}

// reapOrphans waits for children the agent did not spawn.
//
// On the gVisor tier the agent is PID 1, so every process an exec backgrounds
// and abandons reparents here. Nothing waited for them, so each became a
// permanent zombie -- and they count against the sandbox's own process ceiling,
// so ordinary shell code (`sh -c 'work & exit'`, a test suite that spawns
// helpers) could fill it with unreapable entries and brick the sandbox: every
// later fork EAGAIN, every exec exit 126. Measured: 100 zombies from 60
// orphaned children, zero after this.
//
// It cannot tell an orphan from an exec's own child, so it RECORDS every status
// it takes. A plain wait4(-1) with no record races os/exec and destroys an
// exec's exit status -- observed directly as execs reporting -1 instead of
// their real code, which is how this was caught. The exec path consults the
// record when cmd.Wait() comes back empty-handed.
func reapOrphans() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGCHLD)
	for range ch {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			rememberReaped(pid, ws)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "shim:", err)
	os.Exit(1)
}

func abs(p string) string {
	a, _ := filepath.Abs(p)
	return a
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	mux := proto.NewMux(conn, false)

	// announce hello immediately
	hello := proto.Hello{Proto: 1, Agent: "substrate-shim", Version: "1.0.0", Token: s.token,
		BootID: readFile("/proc/sys/kernel/random/boot_id"), Features: []string{"exec", "fs"}}
	st, err := mux.Open(proto.OpHello, proto.Encode(hello))
	if err == nil {
		st.Close()
	}
	s.announcePorts(mux)
	portsDone := make(chan struct{})
	defer close(portsDone)
	go s.watchPorts(mux, portsDone)

	for {
		stream, err := mux.Accept()
		if err != nil {
			return
		}
		first, err := stream.Recv()
		if err != nil {
			continue
		}
		switch first.Op {
		case proto.OpExecReq:
			go s.exec(stream, first)
		case proto.OpFsReq:
			go s.fs(stream, first)
		case proto.OpPing:
			stream.SendFin(proto.OpPong, nil)
			stream.Close()
		case proto.OpReseed:
			var rs proto.Reseed
			json.Unmarshal(first.Payload, &rs)
			machineID := s.reseed(rs.VMGenID)
			ack := proto.ReseedAck{MachineID: machineID}
			stream.SendFin(proto.OpReseedAck, proto.Encode(ack))
			stream.Close()
		default:
			stream.Close()
		}
	}
}

func (s *server) announcePorts(mux *proto.Mux) {
	ports := s.currentPorts()
	if st, err := mux.Open(proto.OpPorts, proto.Encode(ports)); err == nil {
		st.Close()
	}
}

// watchPorts re-announces whenever the set of listening sockets changes.
//
// Ports used to be read ONCE, from a static .ports.json in the rootfs, at
// connection time. So the platform only ever knew about ports a template
// declared in advance -- anything the tenant actually started was invisible,
// and a preview URL for it could not be routed. Which is to say: you could run
// a web server in a sandbox and the platform would never know.
//
// Complexity: O(sockets) per poll, entirely INSIDE the sandbox. It costs the
// guest's own CPU budget, not the node's, which is why a poll is acceptable
// here and was not for host-side liveness.
func (s *server) watchPorts(mux *proto.Mux, done <-chan struct{}) {
	last := portKey(s.currentPorts())
	t := time.NewTicker(portPollInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		cur := s.currentPorts()
		k := portKey(cur)
		if k == last {
			continue
		}
		last = k
		st, err := mux.Open(proto.OpPorts, proto.Encode(cur))
		if err != nil {
			return // connection gone; the next one re-announces from scratch
		}
		st.Close()
	}
}

const portPollInterval = 2 * time.Second

// currentPorts reports what is actually listening, merged with any ports a
// template declared.
func (s *server) currentPorts() proto.PortsAnnounce {
	out := proto.PortsAnnounce{}
	seen := map[uint16]bool{}
	// Declared ports still count: a template may name a port whose server is
	// slow to bind, and dropping it would make the preview flap on restart.
	if b, err := os.ReadFile(filepath.Join(s.root, ".ports.json")); err == nil {
		var declared proto.PortsAnnounce
		if json.Unmarshal(b, &declared) == nil {
			for _, p := range declared.Ports {
				if !seen[p.Port] {
					seen[p.Port] = true
					out.Ports = append(out.Ports, p)
				}
			}
		}
	}
	for _, p := range listeningTCPPorts() {
		if !seen[p] {
			seen[p] = true
			out.Ports = append(out.Ports, proto.PortAnnounce{Port: p, Proto: "tcp"})
		}
	}
	sort.Slice(out.Ports, func(i, j int) bool { return out.Ports[i].Port < out.Ports[j].Port })
	return out
}

// listeningTCPPorts reads the guest's own listening sockets.
//
// LOOPBACK-ONLY LISTENERS ARE EXCLUDED. A server bound to 127.0.0.1 is
// deliberately not reachable from outside the sandbox, and announcing it would
// publish a preview URL that can never work -- worse, it would advertise a
// service the tenant chose to keep private.
func listeningTCPPorts() []uint16 {
	var out []uint16
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != tcpStateListen {
				continue
			}
			addr, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if isLoopbackHex(addr) {
				continue
			}
			n, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil || n <= 0 || n > 65535 {
				continue
			}
			out = append(out, uint16(n))
		}
	}
	return out
}

// tcpStateListen is TCP_LISTEN as /proc renders it.
const tcpStateListen = "0A"

// isLoopbackHex reports whether a /proc hex address is 127.0.0.1 or ::1.
// /proc writes IPv4 little-endian, so 127.0.0.1 is "0100007F".
func isLoopbackHex(addr string) bool {
	switch strings.ToUpper(addr) {
	case "0100007F": // 127.0.0.1
		return true
	case "00000000000000000000000001000000": // ::1
		return true
	}
	return false
}

// portKey renders an announcement for change detection.
func portKey(p proto.PortsAnnounce) string {
	var b strings.Builder
	for _, x := range p.Ports {
		fmt.Fprintf(&b, "%d/%s,", x.Port, x.Proto)
	}
	return b.String()
}

func (s *server) exec(stream *proto.Stream, first proto.Frame) {
	var req proto.ExecRequest
	if err := json.Unmarshal(first.Payload, &req); err != nil || len(req.Argv) == 0 {
		exit := proto.ExecExit{Code: 125}
		stream.SendFin(proto.OpExecExit, proto.Encode(exit))
		return
	}

	cwd := s.resolve(req.Cwd)
	if cwd == "" {
		cwd = s.root
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = envSlice(req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()
	shimTrace("spawning id=%s argv=%v", req.ExecID, req.Argv)
	if err := cmd.Start(); err != nil {
		frame := append([]byte{2}, []byte(err.Error())...)
		stream.SendBinary(proto.OpExecData, frame)
		stream.SendFin(proto.OpExecExit, proto.Encode(proto.ExecExit{Code: 126}))
		return
	}
	s.mu.Lock()
	s.pgidBy[req.ExecID] = cmd.Process.Pid
	s.mu.Unlock()

	done := make(chan int, 1)
	go func() {
		// Drain stdout and stderr CONCURRENTLY. Sequential draining (the old
		// code) deadlocks on chatty-stderr processes: stderr's pipe fills
		// while we are still blocked reading stdout, the child blocks on
		// write, stdout goes quiet, and both sides stall forever. guestd runs
		// one pump thread per fd for the same reason.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); pumpTagged(stdout, 1, stream) }()
		go func() { defer wg.Done(); pumpTagged(stderr, 2, stream) }()
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		} else if err != nil {
			// cmd.Wait() came back without a status: the orphan reaper may have
			// taken this child first. Recover the real code rather than
			// reporting -1 for a process that exited normally.
			if ws, ok := takeReaped(cmd.Process.Pid, 500*time.Millisecond); ok {
				switch {
				case ws.Signaled():
					code = 128 + int(ws.Signal())
				default:
					code = ws.ExitStatus()
				}
			} else {
				code = -1
			}
		}
		forgetReaped(cmd.Process.Pid)
		shimTrace("wait-done id=%s code=%d", req.ExecID, code)
		done <- code
	}()

	// stdin pump until FIN or exit
	go func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				stdin.Close()
				return
			}
			switch {
			case f.Op == proto.OpExecData && f.Flags&proto.FlagBinary != 0:
				if len(f.Payload) > 1 {
					stdin.Write(f.Payload[1:])
				}
				if f.Flags&proto.FlagFin != 0 {
					stdin.Close()
				}
			case f.Op == proto.OpExecSignal:
				var sig proto.ExecSignal
				json.Unmarshal(f.Payload, &sig)
				n := signalNum(sig.Signal)
				syscall.Kill(-cmd.Process.Pid, n)
			}
		}
	}()

	code := <-done
	shimTrace("sending-exit id=%s code=%d", req.ExecID, code)
	err := stream.SendFin(proto.OpExecExit, proto.Encode(proto.ExecExit{ExecID: req.ExecID, Code: code}))
	shimTrace("exit-sent err=%v", err)
	s.mu.Lock()
	delete(s.pgidBy, req.ExecID)
	s.mu.Unlock()
}

var shimDebug = os.Getenv("SUBSTRATE_DEBUG_SSE") == "1"

func shimTrace(format string, args ...any) {
	if shimDebug {
		fmt.Fprintf(os.Stderr, "SHIM-TRACE "+format+"\n", args...)
	}
}

func pumpTagged(r io.Reader, tag byte, stream *proto.Stream) {
	buf := make([]byte, 16<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			frame := append([]byte{tag}, buf[:n]...)
			if stream.SendBinary(proto.OpExecData, frame) != nil {
				return
			}
		}
		if err != nil {
			meta := proto.ExecDataMeta{FD: fdName(tag), EOF: true}
			stream.Send(proto.OpExecData, proto.Encode(meta))
			return
		}
	}
}

func fdName(tag byte) string {
	if tag == 2 {
		return "err"
	}
	return "out"
}

func signalNum(name string) syscall.Signal {
	switch strings.ToUpper(name) {
	case "HUP":
		return syscall.SIGHUP
	case "INT":
		return syscall.SIGINT
	case "KILL":
		return syscall.SIGKILL
	case "TERM", "":
		return syscall.SIGTERM
	case "USR1":
		return syscall.SIGUSR1
	default:
		if n, err := strconv.Atoi(name); err == nil && n > 0 && n < 64 {
			return syscall.Signal(n)
		}
		return syscall.SIGTERM
	}
}

// resolve maps an absolute guest path into the sandbox root, refusing
// escapes — including via symlinks planted in the workdir that point outside
// (a template can carry them). EvalSymlinks resolves the real target first;
// O(path depth).
//
// Known limitation (accepted for the non-isolated dev tier): resolve-then-open
// has a TOCTOU window against a concurrent symlink swap. Closing it properly
// needs openat2(RESOLVE_BENEATH) at every open site — scheduled with the
// hardened-tier work. This tier provides NO security boundary by design
// (plan §2.1); the hv tier is unaffected (paths never touch a shared host fs).
func (s *server) resolve(p string) string {
	if p == "" {
		return ""
	}
	clean := filepath.Clean("/" + p)
	full := filepath.Join(s.root, clean)

	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		full = resolved
	} else {
		// not-yet-existing path: resolve the deepest existing ancestor so a
		// symlinked parent directory is caught too.
		dir := filepath.Dir(full)
		if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
			full = filepath.Join(resolvedDir, filepath.Base(full))
		}
	}
	// The prefix must not double the separator. With --root "/" (the gVisor
	// tier, where the sandbox IS the whole filesystem) s.root+"/" was "//",
	// which prefixes nothing, so EVERY path was rejected as escaping and the
	// entire fs surface returned "path escapes sandbox" on that tier while exec
	// kept working. Containment there is gVisor's job; this check is
	// defence-in-depth for roots that are a subdirectory.
	prefix := s.root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if full != s.root && !strings.HasPrefix(full, prefix) {
		return ""
	}
	return full
}

func (s *server) fs(stream *proto.Stream, first proto.Frame) {
	var req proto.FsRequest
	if err := json.Unmarshal(first.Payload, &req); err != nil {
		stream.SendFin(proto.OpFsResp, proto.Encode(proto.FsResponse{OK: false, Error: "bad request"}))
		return
	}
	path := s.resolve(req.Path)
	if path == "" {
		stream.SendFin(proto.OpFsResp, proto.Encode(proto.FsResponse{OK: false, Error: "path escapes sandbox"}))
		return
	}

	resp := proto.FsResponse{}
	switch req.Op {
	case "read":
		// Streams the file in <= MaxFrameSize chunks exactly like guestd
		// (fsops.rs): one whole-file frame both exceeded MaxFrameSize — so
		// SendBinary failed silently and the client got OK + size with NO
		// data — and materialized arbitrarily large files in RAM.
		const maxRead = 32 << 20 // matches guestd DEFAULT_MAX_READ
		f, err := os.Open(path)
		if err != nil {
			resp.OK, resp.Error = false, err.Error()
			break
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			resp.OK, resp.Error = false, err.Error()
			break
		}
		if fi.IsDir() {
			f.Close()
			resp.OK, resp.Error = false, "is a directory"
			break
		}
		if req.Offset > 0 {
			if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
				f.Close()
				resp.OK, resp.Error = false, "seek failed"
				break
			}
		}
		want := int64(maxRead)
		if req.Length > 0 && req.Length < want {
			want = req.Length
		}
		var sent int64
		buf := make([]byte, proto.MaxFrameSize)
		ok := true
		for sent < want {
			n, rerr := io.ReadFull(io.LimitReader(f, want-sent), buf)
			if n > 0 {
				if stream.SendBinary(proto.OpFsData, buf[:n]) != nil {
					ok = false
					break
				}
				sent += int64(n)
			}
			if rerr != nil {
				break // EOF or read error: deliver what we have
			}
		}
		f.Close()
		resp.OK, resp.Size = ok, sent
	case "write":
		var payload []byte
		for {
			f, rerr := stream.Recv()
			if rerr != nil {
				break
			}
			if f.Op == proto.OpFsData && f.Flags&proto.FlagBinary != 0 {
				payload = append(payload, f.Payload...)
			}
			if f.Flags&proto.FlagFin != 0 {
				break
			}
		}
		mode := os.FileMode(req.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if parent := filepath.Dir(path); parent != "." {
			os.MkdirAll(parent, 0o755)
		}
		var err error
		if req.Append {
			// Append must touch the original file: tmp+rename would discard
			// prior content.
			err = writeAppend(path, payload, mode)
		} else {
			err = writeAtomic(path, payload, mode)
		}
		if err != nil {
			resp.OK, resp.Error = false, err.Error()
		} else {
			resp.OK, resp.Size = true, int64(len(payload))
		}
	case "list":
		entries, err := os.ReadDir(path)
		if err != nil {
			resp.OK, resp.Error = false, err.Error()
			break
		}
		// Cap matches guestd MAX_ENTRIES: an unbounded listing overruns the
		// 1MiB frame cap on large directories and the response is lost.
		const maxEntries = 2000
		truncated := false
		for _, e := range entries {
			if len(resp.Entries) >= maxEntries {
				truncated = true
				break
			}
			info, _ := e.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			resp.Entries = append(resp.Entries, proto.FsEntry{Name: e.Name(), Size: size, Dir: e.IsDir()})
		}
		_ = truncated // size field carries the count; API layer applies its own limit
		resp.OK = true
	case "remove":
		if err := os.RemoveAll(path); err != nil {
			resp.OK, resp.Error = false, err.Error()
		} else {
			resp.OK = true
		}
	case "mkdir":
		if err := os.MkdirAll(path, 0o755); err != nil {
			resp.OK, resp.Error = false, err.Error()
		} else {
			resp.OK = true
		}
	case "stat":
		info, err := os.Stat(path)
		if err != nil {
			resp.OK, resp.Error = false, err.Error()
			break
		}
		resp.OK = true
		resp.Entries = []proto.FsEntry{{Name: filepath.Base(path), Size: info.Size(), Dir: info.IsDir()}}
	default:
		resp.OK, resp.Error = false, "unknown op "+req.Op
	}
	stream.SendFin(proto.OpFsResp, proto.Encode(resp))
}

func (s *server) reseed(vmgenid string) string {
	id := randomHex(16)
	_ = os.WriteFile(filepath.Join(s.root, "etc/machine-id"), []byte(id+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(s.root, "run/substrate-vmgenid"), []byte(vmgenid+"\n"), 0o644)
	return id
}

func envSlice(env map[string]string) []string {
	out := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

var hexChars = "0123456789abcdef"

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0xf]
	}
	return string(out)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path+".tmp", data, mode); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func writeAppend(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
