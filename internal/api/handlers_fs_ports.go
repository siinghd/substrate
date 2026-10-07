package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/store"
)

func (s *Server) handleFsRead(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	if err := s.resumeIfNeeded(r, sb); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: errString(err)})
		return
	}
	m, live := s.sup.Get(sb.ID)
	if !live {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "machine not live"})
		return
	}
	s.touch(r, sb.ID)
	path := "/" + r.PathValue("path")

	q := r.URL.Query()
	if q.Get("list") == "true" || strings.HasSuffix(path, "/") {
		entries, err := m.Fs().List(r.Context(), path)
		if err != nil {
			writeErr(w, &apiError{Status: 404, Code: "fs_error", Message: errString(err),
				Fix: "paths are absolute inside the sandbox; list the parent to discover names"})
			return
		}
		limit := atoiDefault(q.Get("limit"), 200)
		truncated := false
		if len(entries) > limit {
			entries = entries[:limit]
			truncated = true
		}
		writeJSON(w, 200, map[string]any{"path": path, "entries": entries,
			"truncated": truncated, "count": len(entries)})
		return
	}

	offset := int64(atoiDefault(q.Get("offset"), 0))
	length := int64(atoiDefault(q.Get("length"), 0))
	data, err := m.Fs().Read(r.Context(), path, offset, length)
	if err != nil {
		writeErr(w, &apiError{Status: 404, Code: "fs_error", Message: errString(err)})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(200)
	w.Write(data)
}

func (s *Server) handleFsWrite(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	if err := s.resumeIfNeeded(r, sb); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: errString(err)})
		return
	}
	m, live := s.sup.Get(sb.ID)
	if !live {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "machine not live"})
		return
	}
	s.touch(r, sb.ID)
	path := "/" + r.PathValue("path")
	// One byte past the limit so an oversize write is REFUSED, not silently
	// shortened: a truncated file written with a 201 is data corruption the
	// caller has no way to detect.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeErr(w, errBadRequest("unreadable body", ""))
		return
	}
	if int64(len(body)) > maxRequestBody {
		writeErr(w, &apiError{Status: 413, Code: "body_too_large",
			Message: "file write exceeds the 64MiB limit",
			Fix:     "write in chunks with ?append=true"})
		return
	}
	appendMode := r.URL.Query().Get("append") == "true"
	if err := m.Fs().Write(r.Context(), path, body, appendMode); err != nil {
		writeErr(w, &apiError{Status: 400, Code: "fs_error", Message: errString(err)})
		return
	}
	writeJSON(w, 201, map[string]any{"path": path, "size": len(body)})
}

func (s *Server) handleFsDelete(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	m, live := s.sup.Get(sb.ID)
	if !live {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "machine not live"})
		return
	}
	path := "/" + r.PathValue("path")
	if err := m.Fs().Remove(r.Context(), path); err != nil {
		writeErr(w, &apiError{Status: 404, Code: "fs_error", Message: errString(err)})
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": path})
}

// ---- ports / preview URLs ----

type portReq struct {
	Port   int  `json:"port"`
	Public bool `json:"public"`
}

func (s *Server) handleExposePort(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	var req portReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeErr(w, errBadRequest("port out of range", "1-65535"))
		return
	}
	p := principal(r)
	if req.Public && p.Kind == "sandbox" {
		writeErr(w, errForbidden("sandbox tokens cannot make ports public"))
		return
	}
	tok := ids.PortToken()
	pm := store.PortMapping{Port: req.Port, Proto: "tcp", Public: req.Public, Token: tok}
	if err := s.store.UpsertPort(r.Context(), sb.ID, pm); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	url := s.previewURL(sb.ID, req.Port)
	resp := map[string]any{"port": req.Port, "url": url, "public": req.Public}
	if !req.Public {
		resp["token"] = tok
	}
	writeJSON(w, 201, resp)
}

func (s *Server) handleClosePort(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, errBadRequest("bad port", ""))
		return
	}
	if err := s.store.DeletePort(r.Context(), sb.ID, port, "tcp"); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, 200, map[string]any{"closed": port})
}

func (s *Server) previewURL(sandboxID string, port int) string {
	base := s.cfg.Gateway.BaseDomain
	host := fmt.Sprintf("%d-%s.%s", port, sandboxID, base)
	scheme := "http"
	if s.cfg.Gateway.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + host
}

var _ = time.Now
