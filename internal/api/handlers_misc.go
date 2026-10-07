package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/store"
)

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var projectID string
	switch p.Kind {
	case "project":
		projectID = p.ProjectID
	default:
		if pid := r.URL.Query().Get("project_id"); pid != "" {
			// Same org-scope guard as sandboxes: never honor a foreign
			// project_id, whatever the caller's kind.
			proj, err := s.store.GetProject(r.Context(), pid)
			if err != nil || proj.OrgID != p.OrgID {
				writeErr(w, errNotFound("project"))
				return
			}
			projectID = pid
		}
	}
	if projectID == "" {
		projects, _ := s.store.ListProjects(r.Context(), p.OrgID)
		if len(projects) > 0 {
			projectID = projects[0].ID
		}
	}
	tpls, err := s.store.ListTemplates(r.Context(), projectID)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	out := make([]map[string]any, 0, len(tpls))
	for _, t := range tpls {
		out = append(out, templateView(t))
	}
	writeJSON(w, 200, map[string]any{"templates": out})
}

func (s *Server) handlePutTemplate(w http.ResponseWriter, r *http.Request) {
	// End-user agents (sandbox JWTs) must never register templates: the
	// rootfs/kernel paths name HOST filesystem locations that a later boot
	// reads into a VM — a tenant-controlled template is a host-disclosure
	// vector.
	if p := principal(r); p.Kind == "sandbox" {
		writeErr(w, errForbidden("sandbox tokens cannot create templates"))
		return
	}
	var req struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ProjectID  string `json:"project_id"`
		RootfsPath string `json:"rootfs_path"`
		KernelPath string `json:"kernel_path"`
		MemMiB     int    `json:"mem_mib"`
		VCPUs      int    `json:"vcpus"`
		WarmCmd    string `json:"warm_cmd"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	proj, ok := s.resolveProject(w, r, req.ProjectID)
	if !ok {
		return
	}
	if req.Name == "" {
		writeErr(w, errBadRequest("name required", ""))
		return
	}
	for label, p := range map[string]string{"rootfs_path": req.RootfsPath, "kernel_path": req.KernelPath} {
		if err := validateHostArtifactPath(p); err != nil {
			writeErr(w, errBadRequest(label+" rejected: "+err.Error(), "use an absolute path with no .. or symlink-escape segments"))
			return
		}
	}
	if req.MemMiB == 0 {
		req.MemMiB = 512
	}
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	t := &store.Template{
		ID: orNew(req.ID, ids.Template), ProjectID: proj.ID, Name: req.Name,
		RootfsPath: req.RootfsPath, KernelPath: req.KernelPath,
		MemMiB: req.MemMiB, VCPUs: req.VCPUs, WarmCmd: req.WarmCmd,
		CreatedAt: time.Now(),
	}
	if err := s.store.PutTemplate(r.Context(), t); err != nil {
		// A supplied id that belongs to another project is a tenancy refusal,
		// not a server fault: say so instead of returning 500.
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, &apiError{Status: 409, Code: "template_id_taken",
				Message: "that template id belongs to another project",
				Fix:     "omit id to have one generated, or use an id from this project"})
			return
		}
		writeErr(w, errInternal(err))
		return
	}
	// A changed template must take effect on this node's next sandbox start;
	// the supervisor caches templates and had no invalidation path.
	s.sup.InvalidateTemplate(t.ID)
	writeJSON(w, 201, templateView(t))
}

func orNew(id string, gen func() string) string {
	if id != "" {
		return id
	}
	return gen()
}

// validateHostArtifactPath rejects path shapes that would let a template
// registration reach outside the operator-intended artifact tree: relative
// paths and any traversal segment. (Full allowlisting of artifact roots is a
// deployment-level decision; this closes the traversal hole without breaking
// operators who build rootfs/kernel images in their own trees.)
func validateHostArtifactPath(p string) error {
	if p == "" {
		return nil // optional field
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("must be absolute")
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("must be canonical (no . or .. segments)")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("traversal not allowed")
		}
	}
	return nil
}

func templateView(t *store.Template) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "mem_mib": t.MemMiB, "vcpus": t.VCPUs,
		"warm_cmd": t.WarmCmd, "created_at": t.CreatedAt,
	}
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()
	f := store.EventFilter{
		SandboxID: q.Get("sandbox_id"),
		Type:      q.Get("type"),
		Limit:     atoiCapped(q.Get("limit"), 200, 1000),
	}
	switch p.Kind {
	case "project":
		f.ProjectID = p.ProjectID
	case "sandbox":
		f.SandboxID = p.SandboxID
	default:
		f.OrgID = p.OrgID
	}
	if ts := q.Get("after"); ts != "" {
		if ms, err := parseMS(ts); err == nil {
			f.After = time.UnixMilli(ms)
		}
	}
	evs, err := s.store.ListEvents(r.Context(), f)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		// Internal observability spans (stage timings) stay server-side:
		// they are diagnostics, not customer-facing ledger entries.
		if strings.HasPrefix(e.Type, "stage.") || strings.Contains(e.Type, ".stage.") {
			continue
		}
		out = append(out, map[string]any{
			"id": e.ID, "ts": e.TS, "type": e.Type,
			"sandbox_id": e.SandboxID, "data": jsonRaw(e.Data),
		})
	}
	writeJSON(w, 200, map[string]any{"events": out})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()
	f := store.UsageFilter{
		GroupBy:   q.Get("group_by"),
		ProjectID: q.Get("project_id"),
	}
	switch p.Kind {
	case "project":
		f.ProjectID = p.ProjectID
	case "sandbox":
		writeErr(w, errForbidden("sandbox tokens cannot read usage"))
		return
	default:
		// Org principals are ALWAYS org-scoped: usage rollups grouped by
		// external_user_id or sandbox_id must never cross the tenant line,
		// whatever filters were or weren't supplied.
		f.OrgID = p.OrgID
		if f.ProjectID != "" {
			// Narrowing to a project requires owning it.
			proj, err := s.store.GetProject(r.Context(), f.ProjectID)
			if err != nil || proj.OrgID != p.OrgID {
				writeErr(w, errNotFound("project"))
				return
			}
		}
	}
	if v := q.Get("from"); v != "" {
		if ms, err := parseMS(v); err == nil {
			f.From = time.UnixMilli(ms)
		}
	}
	if v := q.Get("to"); v != "" {
		if ms, err := parseMS(v); err == nil {
			f.To = time.UnixMilli(ms)
		}
	}
	now := time.Now()
	if f.To.IsZero() {
		f.To = now
	}
	// An unbounded rollup aggregated the ENTIRE ledger — every usage row ever
	// written for the tenant, on a request path, with no index able to help
	// past the project scope. Default to the last billing period and refuse
	// spans wider than a quarter; both are explicit in the response so a
	// caller can never mistake a defaulted window for "all time".
	if f.From.IsZero() {
		f.From = f.To.Add(-defaultUsageWindow)
	}
	if f.To.Before(f.From) {
		writeErr(w, errBadRequest("to is before from", "pass millisecond timestamps"))
		return
	}
	if f.To.Sub(f.From) > maxUsageWindow {
		writeErr(w, &apiError{Status: 400, Code: "window_too_wide",
			Message: "usage window exceeds 92 days",
			Fix:     "request narrower windows and sum them client-side"})
		return
	}
	f.After = q.Get("after")
	f.Limit = atoiCapped(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	rows, err := s.store.UsageRollup(r.Context(), f)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	resp := map[string]any{
		"group_by": f.GroupBy,
		"rows":     rows,
		"from":     f.From.UnixMilli(),
		"to":       f.To.UnixMilli(),
	}
	// A full page means there may be more: hand back the cursor rather than
	// silently truncating the tenant's bill.
	if len(rows) == f.Limit && f.Limit > 0 {
		resp["next_cursor"] = rows[len(rows)-1].Group
	}
	writeJSON(w, 200, resp)
}

const (
	// defaultUsageWindow is one billing period.
	defaultUsageWindow = 30 * 24 * time.Hour
	maxUsageWindow     = 92 * 24 * time.Hour
	defaultUsageLimit  = 1000
	maxUsageLimit      = 10000
)

func parseMS(s string) (int64, error) {
	n := int64(0)
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadNumber
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }

const errBadNumber = strErr("not a number")

func jsonRaw(b []byte) any {
	var v any
	jsonUnmarshal(b, &v)
	return v
}
