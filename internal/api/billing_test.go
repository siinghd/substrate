package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestInvoiceIsScopedToTheCallersOrg: the invoice endpoint must never take an
// org from the request. A tenant reading another tenant's spend is the same
// cross-tenant read the cell proxy counts refusals of.
func TestInvoiceIsScopedToTheCallersOrg(t *testing.T) {
	e, _ := limitsEnv(t)
	for _, q := range []string{
		"?org_id=org_someoneelse",
		"?org_id=org_someoneelse&from=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	} {
		code, body := e.do("GET", "/v1/usage/invoice"+q, "", e.apiKey, false)
		if code != 200 {
			t.Fatalf("invoice: %d %s", code, body)
		}
		var inv struct {
			OrgID string `json:"org_id"`
		}
		json.Unmarshal([]byte(body), &inv)
		if inv.OrgID == "org_someoneelse" {
			t.Fatalf("org_id from the query string was honoured: %s", body)
		}
	}
}

// TestInvoiceRejectsUnboundedPeriods: the period is the one input a tenant
// controls that turns into database work.
func TestInvoiceRejectsUnboundedPeriods(t *testing.T) {
	e, _ := limitsEnv(t)
	for _, q := range []string{
		"?from=1970-01-01T00:00:00Z&to=2099-01-01T00:00:00Z",
		"?from=2030-01-01T00:00:00Z&to=2020-01-01T00:00:00Z",
		"?from=not-a-date",
	} {
		code, body := e.do("GET", "/v1/usage/invoice"+q, "", e.apiKey, false)
		if code < 400 {
			t.Errorf("accepted period %q: %d %s", q, code, body)
		}
	}
}

// TestInvoiceRequiresCredentials.
func TestInvoiceRequiresCredentials(t *testing.T) {
	e, _ := limitsEnv(t)
	if code, _ := e.do("GET", "/v1/usage/invoice", "", "", false); code != 401 {
		t.Fatalf("unauthenticated invoice request returned %d", code)
	}
}

// TestZeroPriceConfigProducesZeroInvoice: a deployment that has not set prices
// must bill nothing, not guess.
func TestZeroPriceConfigProducesZeroInvoice(t *testing.T) {
	e, _ := limitsEnv(t)
	code, body := e.do("GET", "/v1/usage/invoice", "", e.apiKey, false)
	if code != 200 {
		t.Fatalf("invoice: %d %s", code, body)
	}
	if !strings.Contains(body, `"total_cents":0`) {
		t.Fatalf("unpriced deployment produced a non-zero invoice: %s", body)
	}
}
