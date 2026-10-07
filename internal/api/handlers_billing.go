package api

import (
	"net/http"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/billing"
	"github.com/substrate-sh/substrate/internal/store"
)

// planFromConfig builds the price list. Node-wide for now; per-org plans need
// a schema migration and are the next step.
func (s *Server) planFromConfig() billing.Plan {
	b := s.cfg.Billing
	return billing.Plan{
		ID: b.PlanID, Name: b.PlanID, Currency: b.Currency,
		IncludedCents: b.IncludedCents, MinimumCents: b.MinimumCents,
		Prices: []billing.Price{
			{Unit: billing.UnitSandboxSecond, NanoCents: b.SandboxSecondNanoCents},
			{Unit: billing.UnitCPUSecond, NanoCents: b.CPUSecondNanoCents},
			{Unit: billing.UnitGiBHour, NanoCents: b.GiBHourNanoCents},
			{Unit: billing.UnitEgressGB, NanoCents: b.EgressGBNanoCents},
		},
	}
}

// handleInvoice prices the CALLER'S OWN usage for a period.
//
// Scoped to the principal's org, never to a query parameter: an org_id taken
// from the request would let any tenant read another tenant's usage and
// spending, which is exactly the cross-tenant read the cell proxy already
// counts refusals of.
func (s *Server) handleInvoice(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p == nil || p.OrgID == "" {
		writeErr(w, errUnauthorized())
		return
	}
	from, to, err := parsePeriod(r)
	if err != nil {
		writeErr(w, errBadRequest(err.Error(), "pass ?from=RFC3339&to=RFC3339, at most 366 days apart"))
		return
	}
	f := store.UsageFilter{
		OrgID: p.OrgID, GroupBy: "project_id", From: from, To: to,
		Limit: 1000,
	}
	if p.Kind == auth.KindProject && p.ProjectID != "" {
		f.ProjectID = p.ProjectID
	}
	rows, err := s.store.UsageRollup(r.Context(), f)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	inv, err := billing.Rate(p.OrgID, s.planFromConfig(), billing.FromRows(rows), from, to)
	if err != nil {
		writeErr(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// parsePeriod reads and BOUNDS the billing window.
//
// An unbounded range is an unbounded scan of the usage table on a request
// path: the one place a tenant can make the database do arbitrary work.
func parsePeriod(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	to := now
	from := now.AddDate(0, -1, 0)
	if v := r.URL.Query().Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errParse("from")
		}
		from = t.UTC()
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errParse("to")
		}
		to = t.UTC()
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errParse("to must be after from")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, errParse("period longer than 366 days")
	}
	return from, to, nil
}

type parseErr string

func (e parseErr) Error() string { return string(e) }
func errParse(what string) error { return parseErr("invalid billing period: " + what) }
