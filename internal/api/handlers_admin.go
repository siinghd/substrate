package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/store"
)

// The tenant-provisioning surface.
//
// Before this, orgs, projects and keys were created by ONE bootstrap path at
// first start: a single org, a single project, a single key. Onboarding a
// customer meant hand-writing rows into the database. Every operation the store
// needed already existed; nothing exposed them.
//
// These routes are gated on a credential that is deliberately NOT in the
// database (see auth.KindAdmin): a bug that leaks tenant data cannot leak the
// ability to mint tenants, and a database restore cannot resurrect a rotated
// admin key.

// requireAdmin rejects anything that is not the admin principal.
func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if p == nil || p.Kind != auth.KindAdmin {
			// 404, not 403: an org key probing /v1/admin should not learn that
			// the surface exists or that its own credential is the wrong kind.
			writeErr(w, errNotFound("not found"))
			return
		}
		next(w, r)
	}
}

type createOrgReq struct {
	Name string `json:"name"`
}

func (s *Server) handleAdminCreateOrg(w http.ResponseWriter, r *http.Request) {
	var req createOrgReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, errBadRequest("invalid json body", "send {\"name\": \"...\"}"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 128 {
		writeErr(w, errBadRequest("name must be 1-128 characters", "pick a shorter organisation name"))
		return
	}
	org := &store.Org{ID: ids.Org(), Name: name, CreatedAt: time.Now()}
	if err := s.store.CreateOrg(r.Context(), org); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

func (s *Server) handleAdminListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.store.ListOrgs(r.Context())
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": orgs})
}

type createProjectReq struct {
	OrgID        string `json:"org_id"`
	Name         string `json:"name"`
	MaxSandboxes int    `json:"max_sandboxes"`
}

func (s *Server) handleAdminCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, errBadRequest("invalid json body", "send {\"org_id\":\"...\",\"name\":\"...\"}"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 128 {
		writeErr(w, errBadRequest("name must be 1-128 characters", "pick a shorter project name"))
		return
	}
	if _, err := s.store.GetOrg(r.Context(), req.OrgID); err != nil {
		writeErr(w, errBadRequest("unknown org_id "+req.OrgID, "create the organisation first"))
		return
	}
	max := req.MaxSandboxes
	if max <= 0 {
		max = s.cfg.Limits.MaxSandboxesPerProject
	}
	if ceiling := s.cfg.Limits.MaxSandboxesPerProject; ceiling > 0 && max > ceiling {
		writeErr(w, errBadRequest("max_sandboxes exceeds the node limit", "raise limits.max_sandboxes_per_project"))
		return
	}
	p := &store.Project{ID: ids.Project(), OrgID: req.OrgID, Name: name, MaxSandboxes: max, CreatedAt: time.Now()}
	if err := s.store.CreateProject(r.Context(), p); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleAdminListProjects(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		writeErr(w, errBadRequest("org_id is required", "pass ?org_id=org_..."))
		return
	}
	ps, err := s.store.ListProjects(r.Context(), orgID)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": ps})
}

type createKeyReq struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id"`
}

// handleAdminCreateKey mints a tenant credential and returns the plaintext
// EXACTLY ONCE. Only the hash is stored, so it cannot be shown again and a
// database dump does not yield usable credentials.
func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, errBadRequest("invalid json body", "send {\"org_id\":\"...\"}"))
		return
	}
	if _, err := s.store.GetOrg(r.Context(), req.OrgID); err != nil {
		writeErr(w, errBadRequest("unknown org_id "+req.OrgID, "create the organisation first"))
		return
	}
	kind, prefix := "org", "sk"
	if req.ProjectID != "" {
		proj, err := s.store.GetProject(r.Context(), req.ProjectID)
		if err != nil {
			writeErr(w, errBadRequest("unknown project_id "+req.ProjectID, "create the project first"))
			return
		}
		// A project key scoped to another org's project would be a
		// cross-tenant credential. Refuse rather than trust the caller's pair.
		if proj.OrgID != req.OrgID {
			writeErr(w, errBadRequest("project does not belong to that org", "check org_id and project_id"))
			return
		}
		kind, prefix = "project", "pk"
	}
	plain, err := auth.NewAPIKey(prefix)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	k := &store.APIKey{
		ID: ids.New("key"), OrgID: req.OrgID, ProjectID: req.ProjectID,
		Kind: kind, KeyHash: auth.HashKey(plain), CreatedAt: time.Now(),
	}
	if err := s.store.CreateAPIKey(r.Context(), k); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	// `key` appears in this response and nowhere else, ever.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "org_id": k.OrgID, "project_id": k.ProjectID, "kind": k.Kind,
		"created_at": k.CreatedAt, "key": plain,
		"warning": "this is the only time the key is shown; store it now",
	})
}

func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		writeErr(w, errBadRequest("org_id is required", "pass ?org_id=org_..."))
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), orgID)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	// store.APIKey tags KeyHash `json:"-"`, so no secret material can leak
	// through this listing even if a key row somehow held plaintext.
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, errBadRequest("key id is required", "DELETE /v1/admin/keys/{id}"))
		return
	}
	if err := s.store.RevokeAPIKey(r.Context(), id); err != nil {
		writeErr(w, errInternal(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type suspendReq struct {
	Suspended bool   `json:"suspended"`
	Reason    string `json:"reason"`
}

// handleAdminSuspendOrg is the abuse and non-payment lever.
//
// Suspension is a reversible STATE, not a deletion: the audit trail and the
// usage records survive, so a wrongly-suspended tenant can be restored and a
// disputed bill can still be reconstructed. Running sandboxes are stopped by
// the reconciler on its next pass -- blocking new creations while leaving a
// miner running would not be a control at all.
func (s *Server) handleAdminSuspendOrg(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req suspendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, errBadRequest("invalid json body", "send {\"suspended\":true,\"reason\":\"...\"}"))
		return
	}
	if len(req.Reason) > 512 {
		writeErr(w, errBadRequest("reason too long", "keep it under 512 characters"))
		return
	}
	if err := s.store.SetOrgSuspended(r.Context(), id, req.Suspended, strings.TrimSpace(req.Reason)); err != nil {
		writeErr(w, errNotFound("org "+id))
		return
	}
	org, err := s.store.GetOrg(r.Context(), id)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, org)
}
