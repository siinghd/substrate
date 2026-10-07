package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// authorizeSandboxRecord enforces tenant isolation between the authenticated
// principal and a fetched sandbox: org/project principals must own it
// (404-masked), sandbox tokens must be audience-bound to exactly it.
func authorizeSandboxRecord(w http.ResponseWriter, p *auth.Principal, sb *store.Sandbox) bool {
	switch p.Kind {
	case auth.KindSandbox:
		if p.SandboxID != sb.ID {
			writeErr(w, errForbidden("token is scoped to a different sandbox"))
			return false
		}
	case auth.KindProject:
		if p.ProjectID != sb.ProjectID {
			writeErr(w, errNotFound("sandbox "+sb.ID))
			return false
		}
	case auth.KindOrg:
		if p.OrgID != sb.OrgID {
			writeErr(w, errNotFound("sandbox "+sb.ID))
			return false
		}
	}
	return true
}

// loadSandbox fetches and authorizes.
func (s *Server) loadSandbox(w http.ResponseWriter, r *http.Request) (*store.Sandbox, bool) {
	id := r.PathValue("id")
	sb, err := s.store.GetSandbox(r.Context(), id)
	if err != nil {
		writeErr(w, errNotFound("sandbox "+id))
		return nil, false
	}
	if !authorizeSandboxRecord(w, principal(r), sb) {
		return nil, false
	}
	return sb, true
}

func (s *Server) resolveProject(w http.ResponseWriter, r *http.Request, requested string) (*store.Project, bool) {
	p := principal(r)
	if p.Kind == auth.KindProject && p.ProjectID != "" {
		proj, err := s.store.GetProject(r.Context(), p.ProjectID)
		if err != nil {
			writeErr(w, errNotFound("project"))
			return nil, false
		}
		return proj, true
	}
	if requested == "" {
		projects, _ := s.store.ListProjects(r.Context(), p.OrgID)
		if len(projects) == 1 {
			return projects[0], true
		}
		writeErr(w, errBadRequest("project_id required", "org keys must name the project; project keys are pre-scoped"))
		return nil, false
	}
	proj, err := s.store.GetProject(r.Context(), requested)
	if err != nil || proj.OrgID != p.OrgID {
		writeErr(w, errNotFound("project"))
		return nil, false
	}
	return proj, true
}

type createSandboxReq struct {
	TemplateID      string            `json:"template_id"`
	TemplateName    string            `json:"template_name"`
	ProjectID       string            `json:"project_id"`
	ExternalUserID  string            `json:"external_user_id"`
	Env             map[string]string `json:"env"`
	MemMiB          int               `json:"mem_mib"`
	VCPUs           int               `json:"vcpus"`
	TTLIdleSecs     int               `json:"ttl_idle_secs"`
	TTLAbsoluteSecs int               `json:"ttl_absolute_secs"`
	// Isolation is the tier the CALLER asked for. The field was absent
	// entirely, so a request for "hv" was silently dropped and the sandbox came
	// back stamped with whatever tier the node happened to run -- on a
	// KVM-less box, a bare host process. The cell's scheduler already reads
	// this field (internal/cell/proxy.go) and openapi.yaml documents it as
	// "Never silently downgraded (plan §2.4)", so the node was the one link in
	// the chain that ignored it.
	Isolation string `json:"isolation"`
	WaitMs    int    `json:"-"`
}

// knownTiers is the isolation enum from openapi.yaml.
var knownTiers = map[string]bool{"hv": true, "kernel": true, "process": true}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxReq
	if !readJSON(w, r, &req) {
		return
	}
	proj, ok := s.resolveProject(w, r, req.ProjectID)
	if !ok {
		return
	}
	ctx := r.Context()

	var tpl *store.Template
	var err error
	if req.TemplateID != "" {
		tpl, err = s.store.GetTemplate(ctx, req.TemplateID)
	} else if req.TemplateName != "" {
		tpl, err = s.store.GetTemplateByName(ctx, proj.ID, req.TemplateName)
	} else {
		writeErr(w, errBadRequest("template_id or template_name required", "list templates via GET /v1/templates"))
		return
	}
	if err != nil || tpl.ProjectID != proj.ID {
		writeErr(w, errNotFound("template"))
		return
	}
	// Honour or refuse the requested tier -- never quietly substitute one.
	// Handing a caller who asked for hardware virtualisation a shared-kernel
	// host process, with a 201 and no warning, is the failure mode the plan
	// calls out by name.
	if req.Isolation != "" {
		if !knownTiers[req.Isolation] {
			writeErr(w, errBadRequest("unknown isolation tier "+req.Isolation, "one of: hv, kernel, process"))
			return
		}
		if actual := s.driverTier(); req.Isolation != actual {
			writeErr(w, &apiError{
				Status: 409, Code: "isolation_unavailable",
				Message: fmt.Sprintf("this node provides isolation tier %q, not %q", actual, req.Isolation),
				Fix:     "place on a node with the requested tier, or ask for " + actual + " explicitly",
			})
			return
		}
	}
	if req.ExternalUserID != "" && len(req.ExternalUserID) > 256 {
		writeErr(w, errBadRequest("external_user_id too long", "keep it under 256 characters"))
		return
	}

	// SUSPENSION IS CHECKED BEFORE ANY QUOTA. A suspended org must not be able
	// to start work at all -- that is the whole point of the lever, whether it
	// was pulled for abuse or for non-payment.
	if org, oerr := s.store.GetOrg(ctx, proj.OrgID); oerr == nil && org.SuspendedAt != nil {
		reason := org.SuspendReason
		if reason == "" {
			reason = "contact support"
		}
		writeErr(w, &apiError{
			Status: 403, Code: "org_suspended",
			Message: "this organisation is suspended: " + reason,
			Fix:     "resolve the issue with your provider; existing sandboxes have been stopped",
		})
		return
	}
	if n, _ := s.store.CountSandboxesByProject(ctx, proj.ID); n >= proj.MaxSandboxes {
		writeErr(w, errQuota(fmt.Sprintf("project sandbox limit reached (%d)", proj.MaxSandboxes)))
		return
	}
	if req.ExternalUserID != "" && s.cfg.Limits.MaxSandboxesPerUser > 0 {
		if n, _ := s.store.CountSandboxesByUser(ctx, proj.ID, req.ExternalUserID); n >= s.cfg.Limits.MaxSandboxesPerUser {
			writeErr(w, errQuota(fmt.Sprintf("per-end-user limit reached (%d)", s.cfg.Limits.MaxSandboxesPerUser)))
			return
		}
	}

	// Resource-shaped input needs a ceiling. mem_mib becomes a cgroup memory
	// limit and vcpus a CPU share, and neither had any bound anywhere in the
	// stack: one request could ask for terabytes of memory or thousands of
	// vCPUs, and the node would try to honour it -- on a host packing 2,000
	// sandboxes, that is a denial of service from a single API call.
	mem := tpl.MemMiB
	if req.MemMiB > 0 {
		mem = req.MemMiB
	}
	if max := s.cfg.Limits.MaxMemMiB; max > 0 && mem > max {
		writeErr(w, errBadRequest(fmt.Sprintf("mem_mib %d exceeds the limit of %d", mem, max),
			"request less memory, or ask your operator to raise limits.max_mem_mib"))
		return
	}
	vcpus := tpl.VCPUs
	if req.VCPUs > 0 {
		vcpus = req.VCPUs
	}
	if max := s.cfg.Limits.MaxVCPUs; max > 0 && vcpus > max {
		writeErr(w, errBadRequest(fmt.Sprintf("vcpus %d exceeds the limit of %d", vcpus, max),
			"request fewer vcpus, or ask your operator to raise limits.max_vcpus"))
		return
	}
	if max := s.cfg.Limits.MaxTTLAbsoluteSecs; max > 0 && req.TTLAbsoluteSecs > max {
		writeErr(w, errBadRequest(fmt.Sprintf("ttl_absolute_secs %d exceeds the limit of %d", req.TTLAbsoluteSecs, max),
			"a sandbox may not pin resources indefinitely"))
		return
	}
	// Same ceiling for the idle ttl. Both are multiplied by time.Second before
	// comparison, and time.Duration is int64 NANOseconds: above ~9.2e9 seconds
	// that wraps negative, which flips the idle check to always-true and pauses
	// the sandbox instantly. Only the tenant's own sandbox suffers, but a value
	// that means the opposite of what was asked for is not a limit, it is a bug.
	if max := s.cfg.Limits.MaxTTLAbsoluteSecs; max > 0 && req.TTLIdleSecs > max {
		writeErr(w, errBadRequest(fmt.Sprintf("ttl_idle_secs %d exceeds the limit of %d", req.TTLIdleSecs, max),
			"an idle ttl longer than the absolute ttl ceiling has no effect"))
		return
	}
	if req.MemMiB < 0 || req.VCPUs < 0 || req.TTLAbsoluteSecs < 0 || req.TTLIdleSecs < 0 {
		writeErr(w, errBadRequest("negative resource or ttl value", "all of mem_mib, vcpus and the ttls must be >= 0"))
		return
	}
	idle := req.TTLIdleSecs
	if idle == 0 {
		idle = s.cfg.Limits.DefaultIdlePauseSecs
	}

	now := time.Now()
	sb := &store.Sandbox{
		ID: ids.Sandbox(), OrgID: proj.OrgID, ProjectID: proj.ID,
		TemplateID: tpl.ID, ExternalUserID: req.ExternalUserID,
		Desired: "running", State: store.StateCreating,
		Tier: s.driverTier(), MemMiB: mem, VCPUs: vcpus,
		Env: req.Env, LineageID: "lin_" + sbLineage(),
		TTLIdleSecs: idle, TTLAbsoluteSecs: req.TTLAbsoluteSecs,
		LastActivityMS: now.UnixMilli(), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateSandbox(ctx, sb); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	_ = s.store.AppendEvent(ctx, &store.Event{ID: ids.Event(), TS: now, OrgID: sb.OrgID, ProjectID: sb.ProjectID, SandboxID: sb.ID, Type: "created"})

	wait := 10 * time.Second
	go func() {
		bgCtx, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()
		if err := s.sup.EnsureRunning(bgCtx, sb, tpl, nil); err != nil {
			s.log.Warn("async start failed", "sandbox", sb.ID, "err", err)
			got, gerr := s.store.GetSandbox(bgCtx, sb.ID)
			if gerr == nil {
				got.State = store.StateError
				got.Error = err.Error()
				_ = s.store.UpdateSandbox(bgCtx, got)
			}
		}
	}()

	// Poll with gentle backoff: 25ms → 200ms. Worst case ~60 queries on the
	// create path instead of 400, invisible to the caller either way.
	delay := 25 * time.Millisecond
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		got, err := s.store.GetSandbox(ctx, sb.ID)
		if err != nil {
			break
		}
		if got.State == store.StateRunning || got.State == store.StateError {
			writeJSON(w, 201, sandboxView(got))
			return
		}
		sleepMS(int(delay.Milliseconds()))
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
	got, _ := s.store.GetSandbox(ctx, sb.ID)
	writeJSON(w, 202, sandboxView(got))
}

func sbLineage() string { return strings.ToLower(ids.Sandbox()[3:]) }

func (s *Server) driverTier() string {
	if s.sup == nil {
		return "process"
	}
	return s.sup.Tier()
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	s.touch(r, sb.ID)
	writeJSON(w, 200, sandboxView(sb))
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()
	f := store.SandboxFilter{
		State:          q.Get("state"),
		ExternalUserID: q.Get("external_user_id"),
		Limit:          atoiCapped(q.Get("limit"), 100, 500),
	}
	switch p.Kind {
	case auth.KindProject:
		f.ProjectID = p.ProjectID
	default:
		if pid := q.Get("project_id"); pid != "" {
			// Object-level authz: a project filter is honored only when the
			// project belongs to the caller's org — otherwise ListSandboxes'
			// project-only WHERE clause would leak cross-org rows.
			proj, err := s.store.GetProject(r.Context(), pid)
			if err != nil || proj.OrgID != p.OrgID {
				writeErr(w, errNotFound("project"))
				return
			}
			f.ProjectID = pid
		} else {
			f.OrgID = p.OrgID
		}
	}
	sbs, err := s.store.ListSandboxes(r.Context(), f)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	out := make([]map[string]any, 0, len(sbs))
	for _, sb := range sbs {
		out = append(out, sandboxView(sb))
	}
	writeJSON(w, 200, map[string]any{"sandboxes": out})
}

func (s *Server) handleDestroySandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	sb.Desired = "destroyed"
	sb.State = store.StateDestroying
	if err := s.store.UpdateSandbox(r.Context(), sb); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	go func() {
		bgCtx, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()
		_ = s.sup.Destroy(bgCtx, sb.ID)
	}()
	_ = s.store.AppendEvent(r.Context(), &store.Event{ID: ids.Event(), TS: time.Now(), SandboxID: sb.ID, Type: "destroying"})
	writeJSON(w, 202, map[string]any{"id": sb.ID, "state": "destroying"})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	if err := s.sup.Pause(r.Context(), sb.ID); err != nil {
		if errors.Is(err, vmm.ErrUnsupported) {
			writeErr(w, &apiError{Status: 409, Code: "pause_unsupported", Message: "this isolation tier cannot pause", Fix: "use the hv tier or destroy instead"})
			return
		}
		writeErr(w, &apiError{Status: 409, Code: "pause_failed", Message: errString(err)})
		return
	}
	got, _ := s.store.GetSandbox(r.Context(), sb.ID)
	writeJSON(w, 200, sandboxView(got))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	if err := s.resumeIfNeeded(r, sb); err != nil {
		writeErr(w, &apiError{Status: 409, Code: "resume_failed", Message: errString(err)})
		return
	}
	got, _ := s.store.GetSandbox(r.Context(), sb.ID)
	writeJSON(w, 200, sandboxView(got))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	m, live := s.sup.Get(sb.ID)
	if !live {
		writeErr(w, &apiError{Status: 409, Code: "not_running", Message: "snapshot requires a running sandbox"})
		return
	}
	dest := s.dataDir() + "/sandboxes/" + sb.ID + "/snapshots/" + time.Now().UTC().Format("20060102T150405")
	ref, err := m.Pause(r.Context(), dest)
	if err != nil {
		writeErr(w, &apiError{Status: 409, Code: "snapshot_failed", Message: errString(err)})
		return
	}
	sn := &store.Snapshot{
		ID: ids.Snapshot(), SandboxID: sb.ID, Kind: "user",
		Ref: ref, Shared: false, LineageID: sb.LineageID, CreatedAt: time.Now(),
	}
	if err := s.store.CreateSnapshot(r.Context(), sn); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	_ = s.store.AppendEvent(r.Context(), &store.Event{ID: ids.Event(), TS: time.Now(), SandboxID: sb.ID, Type: "snapshotted"})
	writeJSON(w, 201, map[string]any{"id": sn.ID, "kind": sn.Kind, "lineage": sn.LineageID})
}

func (s *Server) handleMintToken(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.loadSandbox(w, r)
	if !ok {
		return
	}
	var req struct {
		Scopes  []string `json:"scopes"`
		TTLSecs int      `json:"ttl_secs"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"exec", "fs", "ports"}
	}
	for _, sc := range scopes {
		if sc == "destroy" || sc == "*" || sc == "lifecycle" || sc == "create" {
			writeErr(w, errBadRequest("scope "+sc+" not grantable to end users", "choose from exec, fs, ports"))
			return
		}
	}
	// Privilege containment: a sandbox token may only re-mint subsets of the
	// scopes it already holds — never escalate.
	if p := principal(r); p.Kind == auth.KindSandbox {
		held := map[string]bool{}
		for _, heldScope := range p.Scopes {
			held[heldScope] = true
		}
		for _, sc := range scopes {
			if !held[sc] {
				writeErr(w, errForbidden("token cannot grant scope "+sc+" which it does not hold"))
				return
			}
		}
	}
	ttl := 24 * time.Hour
	if req.TTLSecs > 0 {
		ttl = time.Duration(req.TTLSecs) * time.Second
	}
	if ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour
	}
	tok, err := auth.MintSandboxToken(s.cfg.API.JWTSecret, "substrate", sb.ID, scopes, ttl)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, 201, map[string]any{
		"token":      tok,
		"sandbox_id": sb.ID,
		"scopes":     scopes,
		"expires_at": time.Now().Add(ttl).UTC(),
	})
}

// resumeIfNeeded implements implicit resume: any API touch on a paused
// sandbox restores it; callers just see a slower first response (§3.6).
func (s *Server) resumeIfNeeded(r *http.Request, sb *store.Sandbox) error {
	if sb.State != store.StatePaused {
		return nil
	}
	tpl, err := s.store.GetTemplate(r.Context(), sb.TemplateID)
	if err != nil {
		return fmt.Errorf("template missing: %w", err)
	}
	if err := s.sup.Resume(r.Context(), sb, tpl, nil); err != nil {
		return err
	}
	got, err := s.store.GetSandbox(r.Context(), sb.ID)
	if err != nil {
		return err
	}
	*sb = *got
	return nil
}

// touch marks activity via a single-column UPDATE: a read-modify-write of the
// whole row here raced the reconciler and could revert a just-applied state
// transition with stale values (lost update).
func (s *Server) touch(r *http.Request, id string) {
	_ = s.store.TouchSandbox(r.Context(), id, time.Now().UnixMilli())
}

func (s *Server) dataDir() string { return s.cfg.Node.DataDir }

func sandboxView(sb *store.Sandbox) map[string]any {
	v := map[string]any{
		"id":               sb.ID,
		"template_id":      sb.TemplateID,
		"external_user_id": sb.ExternalUserID,
		"state":            string(sb.State),
		"isolation":        sb.Tier,
		"mem_mib":          sb.MemMiB,
		"vcpus":            sb.VCPUs,
		"ttl_idle_secs":    sb.TTLIdleSecs,
		"created_at":       sb.CreatedAt,
	}
	if sb.Error != "" {
		v["error"] = sb.Error
	}
	if sb.StartedAt != nil {
		v["started_at"] = *sb.StartedAt
	}
	if len(sb.Env) > 0 {
		v["env"] = sb.Env
	}
	return v
}

func atoiDefault(s string, def int) int {
	return atoiCapped(s, def, 1<<30)
}

// atoiCapped parses s bounded by cap; anything invalid or over-cap returns def.
func atoiCapped(s string, def, cap int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > cap {
			return def
		}
	}
	if n == 0 {
		return def
	}
	return n
}

func errString(err error) string { return err.Error() }

var _ = sha256.Sum256
var _ = io.Discard
