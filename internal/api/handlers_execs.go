package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

type execReq struct {
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd"`
	Env  map[string]string `json:"env"`
	TTY  bool              `json:"tty"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	dbgSSE := os.Getenv("SUBSTRATE_DEBUG_SSE") == "1"
	var req execReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Argv) == 0 {
		writeErr(w, errBadRequest("argv required", `{"argv": ["bash", "-lc", "make test"]}`))
		return
	}
	ctx := r.Context()
	if err := s.resumeIfNeeded(r, sb); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "sandbox is not running: " + errString(err)})
		return
	}
	m, live := s.sup.Get(sb.ID)
	if !live {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "sandbox machine not live yet; retry shortly"})
		return
	}
	s.touch(r, sb.ID)

	// Same exec handle on retry: an agent reissuing `rm -rf build && make`
	// twice concurrently must not corrupt its own workspace (§6.1).
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if prior, err := s.store.GetExecByIdempotencyKey(ctx, principalKeyFor(r)+":"+key); err == nil && prior.SandboxID == sb.ID {
			writeJSON(w, 201, map[string]any{"exec_id": prior.ID, "state": prior.State, "replayed": true})
			return
		}
	}

	execID := ids.Exec()
	idemKey := principalKeyFor(r) + ":" + r.Header.Get("Idempotency-Key")

	ex := &store.Exec{
		ID: execID, SandboxID: sb.ID, Argv: req.Argv,
		State: "running", TTY: req.TTY,
		LogPath:   filepath.Join(s.dataDir(), "execs", execID+".log"),
		CreatedAt: time.Now(),
	}
	ex.IdempotencyKey = idemKey
	if err := s.store.CreateExec(ctx, ex); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	os.MkdirAll(filepath.Dir(ex.LogPath), 0o755)
	logFile, err := os.OpenFile(ex.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// A distinct code here turns "why do all execs 500?" into an
		// immediate diagnosis: the node's log storage is unwritable.
		writeErr(w, &apiError{Status: 500, Code: "exec_log_unavailable",
			Message: "cannot record exec output (check node data_dir writability): " + errString(err)})
		return
	}

	// Exec lifetime is deliberately DETACHED from this request's context:
	// net/http cancels r.Context() as soon as ServeHTTP returns, which would
	// SIGKILL the just-started exec milliseconds after its 201. Exes are
	// async objects — stream/stdin/DELETE /v1/execs/{id} own the lifecycle.
	h, err := m.Exec(context.Background(), vmm.ExecSpec{Argv: req.Argv, Cwd: req.Cwd, Env: req.Env, TTY: req.TTY})
	if err != nil {
		logFile.Close()
		ex.State = "error"
		_ = s.store.UpdateExec(ctx, ex)
		writeErr(w, &apiError{Status: 500, Code: "exec_failed", Message: errString(err)})
		return
	}

	go func() {
		defer logFile.Close()
		// Hard cap on recorded output: a runaway `yes` would otherwise fill
		// the data-dir volume and take the whole node down (the retention
		// prune only removes OLD logs; size is bounded here at write time).
		// The process itself keeps running; only recording stops.
		copyExecOutput(logFile, h.Stdout)
	}()
	// Single exit consumer: persists the terminal state AND removes the live
	// handle. (Two receivers on one unbuffered-value channel raced here — the
	// cleaner could steal the exit value and leave the row "running".)
	s.execMu.Lock()
	s.execHandles[ex.ID] = h
	s.execMu.Unlock()
	go func() {
		ei := <-h.ExitCh
		now := time.Now()
		ex.State = "exited"
		if ei.Killed {
			ex.State = "killed"
		}
		ex.ExitCode = ei.Code
		ex.FinishedAt = &now
		bgCtx, cancel := contextWithTimeout(10 * time.Second)
		defer cancel()
		start := time.Now()
		if err := s.store.UpdateExec(bgCtx, ex); err != nil {
			s.log.Error("exec exit persist failed", "exec", ex.ID, "err", err.Error())
		}
		// Wake any SSE streams tailing this exec so they observe the terminal
		// state without polling the store.
		s.execWaits.notify(ex.ID)
		if d := time.Since(start); d > 2*time.Second {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "SLOW-UPDATE %v — ALL STACKS:\n%s\n", d, buf[:n])
		}
		if dbgSSE {
			fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"exec-exit-persisted\",\"exec\":%q,\"code\":%d}\n", ex.ID, ei.Code)
		}
		s.execMu.Lock()
		delete(s.execHandles, ex.ID)
		s.execMu.Unlock()
	}()

	if dbgSSE {
		fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"exec-open\",\"exec\":%q,\"sandbox\":%q}\n", ex.ID, sb.ID)
	}

	writeJSON(w, 201, map[string]any{"exec_id": ex.ID, "state": "running"})
}

// maxExecLogBytes bounds one exec's recorded output (var: tests shrink it).
var maxExecLogBytes int64 = 64 << 20

const truncationMarker = "\n[substrate: output truncated]\n"

// copyExecOutput streams exec stdout to the log, stopping recording at the
// cap. It keeps draining after the cap so the guest-side pump never wedges;
// excess bytes are discarded. Returns bytes written and whether truncation
// occurred.
func copyExecOutput(w io.Writer, r io.Reader) (int64, bool) {
	var written int64
	truncated := false
	markerWritten := false
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if written >= maxExecLogBytes {
				truncated = true
			} else {
				room := maxExecLogBytes - written
				nn := int64(n)
				if nn > room {
					nn = room
					truncated = true
				}
				w.Write(buf[:nn])
				written += nn
			}
			if truncated && !markerWritten {
				io.WriteString(w, truncationMarker)
				written += int64(len(truncationMarker))
				markerWritten = true
			}
		}
		if err != nil {
			return written, truncated
		}
	}
}

func principalKeyFor(r *http.Request) string {
	p := principal(r)
	if p == nil {
		return "anon"
	}
	return p.OrgID
}

func (s *Server) loadExec(w http.ResponseWriter, r *http.Request) (*store.Exec, bool) {
	id := r.PathValue("id")
	ex, err := s.store.GetExec(r.Context(), id)
	if err != nil {
		writeErr(w, errNotFound("exec "+id))
		return nil, false
	}
	p := principal(r)
	if p.Kind == "sandbox" && p.SandboxID != ex.SandboxID {
		writeErr(w, errForbidden("token is scoped to a different sandbox"))
		return nil, false
	}
	// Object-level authz for org/project principals too: an exec belongs to
	// its sandbox's tenant. Without this, any org key could read/stream/
	// kill another org's exec by id.
	sb, err := s.store.GetSandbox(r.Context(), ex.SandboxID)
	if err != nil {
		writeErr(w, errNotFound("exec "+id))
		return nil, false
	}
	if !authorizeSandboxRecord(w, p, sb) {
		return nil, false
	}
	return ex, true
}

func (s *Server) handleGetExec(w http.ResponseWriter, r *http.Request) {
	ex, ok := s.loadExec(w, r)
	if !ok {
		return
	}
	v := map[string]any{
		"id": ex.ID, "sandbox_id": ex.SandboxID, "argv": ex.Argv,
		"state": ex.State, "exit_code": ex.ExitCode, "created_at": ex.CreatedAt,
	}
	if ex.FinishedAt != nil {
		v["finished_at"] = *ex.FinishedAt
	}
	writeJSON(w, 200, v)
}

func (s *Server) loadLiveExec(w http.ResponseWriter, r *http.Request) (*store.Exec, *vmm.ExecHandle, bool) {
	ex, ok := s.loadExec(w, r)
	if !ok {
		return nil, nil, false
	}
	s.execMu.Lock()
	h := s.execHandles[ex.ID]
	s.execMu.Unlock()
	if h == nil {
		writeErr(w, &apiError{Status: 409, Code: "exec_finished", Message: "exec already finished; output is available via GET /v1/execs/{id}/stream"})
		return nil, nil, false
	}
	return ex, h, true
}

func (s *Server) handleExecStdin(w http.ResponseWriter, r *http.Request) {
	_, h, ok := s.loadLiveExec(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, errBadRequest("unreadable body", ""))
		return
	}
	if err := h.WriteStdin(body); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "stdin_closed", Message: errString(err)})
		return
	}
	writeJSON(w, 200, map[string]any{"written": len(body)})
}

func (s *Server) handleExecKill(w http.ResponseWriter, r *http.Request) {
	ex, h, ok := s.loadLiveExec(w, r)
	if !ok {
		return
	}
	// A failed signal must not masquerade as a successful kill (202): the
	// client would stop polling while the process keeps running.
	if err := h.Signal("KILL"); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "kill_failed", Message: errString(err)})
		return
	}
	writeJSON(w, 202, map[string]any{"id": ex.ID, "state": "killing"})
}

// handleExecStream serves SSE with resumable offsets (§6.1). The `id:` field
// carries the exact raw log-byte offset, so clients resume precisely even
// when escaping changed payload lengths.
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	ex, ok := s.loadExec(w, r)
	if !ok {
		return
	}
	fl, canFlush := w.(http.Flusher)
	offset := int64(atoiDefault(r.URL.Query().Get("offset"), 0))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	if canFlush {
		fl.Flush()
	}

	logPath := ex.LogPath
	if logPath == "" {
		fmt.Fprintf(w, "event: error\ndata: no log recorded\n\n")
		return
	}

	// Wake on exec-exit notification; 5s fallback tick bounds any missed
	// wakeup. The old loop polled status at 10Hz per stream — dozens of open
	// streams serialized the whole API behind SQLite.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	wakeCh, unsubscribe := s.execWaits.subscribe(ex.ID)
	defer unsubscribe()
	deadline := time.Now().Add(30 * time.Minute)
	dbgSSE := os.Getenv("SUBSTRATE_DEBUG_SSE") == "1"
	ticks := 0
	if dbgSSE {
		fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"sse-open\",\"exec\":%q,\"log\":%q}\n", ex.ID, logPath)
		defer fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"sse-close\",\"exec\":%q,\"state\":%q}\n", ex.ID, ex.State)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		case <-wakeCh:
		}
		if fi, err := os.Stat(logPath); err == nil && fi.Size() > offset {
			f, err := os.Open(logPath)
			if err != nil {
				return
			}
			buf := make([]byte, 32<<10)
			f.Seek(offset, 0)
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					fmt.Fprintf(w, "id: %d\nevent: output\ndata: %s\n\n", offset+int64(n), sseEscape(buf[:n]))
					offset += int64(n)
					if canFlush {
						fl.Flush()
					}
				}
				if rerr != nil {
					break
				}
			}
			f.Close()
		}
		// Re-fetch status: the exec may finish after the stream opened.
		if fresh, err := s.store.GetExec(r.Context(), ex.ID); err == nil {
			ex = fresh
		} else if dbgSSE {
			fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"sse-getexec-err\",\"exec\":%q,\"err\":%q}\n", ex.ID, err.Error())
		}
		if dbgSSE && ticks%50 == 0 {
			fmt.Fprintf(os.Stderr, "{\"lvl\":\"debug\",\"msg\":\"sse-tick\",\"exec\":%q,\"state\":%q,\"offset\":%d}\n", ex.ID, ex.State, offset)
		}
		ticks++
		if ex.State == "exited" || ex.State == "killed" || ex.State == "error" {
			fmt.Fprintf(w, "id: %d\nevent: exit\ndata: {\"exit_code\":%d,\"state\":\"%s\"}\n\n", offset, ex.ExitCode, ex.State)
			if canFlush {
				fl.Flush()
			}
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(w, "event: timeout\ndata: {}\n\n")
			return
		}
	}
}

func sseEscape(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
