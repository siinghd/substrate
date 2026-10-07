// Package billing turns measured usage into money.
//
// Usage was measured well and stopped there: per-sandbox CPU, memory and
// bytes, interval-stamped and bounded, with no way to price any of it. You
// could bill; you could not charge.
//
// MONEY IS INTEGER MINOR UNITS EVERYWHERE. Not float64. A cent is exact; a
// float is not, and the error compounds across every line of every invoice
// until totals disagree with the sum of their parts and no one can say which
// is right.
package billing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// Unit is a billable dimension.
type Unit string

const (
	// UnitSandboxSecond is wall-clock residency: the sandbox existed and held
	// resources, whether or not it burned CPU.
	UnitSandboxSecond Unit = "sandbox_second"
	// UnitCPUSecond is CPU actually consumed.
	UnitCPUSecond Unit = "cpu_second"
	// UnitGiBHour is memory held over time (peak MiB x hours).
	UnitGiBHour Unit = "gib_hour"
	// UnitEgressGB is outbound bytes, the dimension that costs real money to
	// a host and is the usual vector for abuse.
	UnitEgressGB Unit = "egress_gb"
)

// Price is the cost of one unit, in NANO minor units (1e-9 of a cent).
//
// Per-unit prices are tiny -- a sandbox-second might be 0.0003 cents -- so a
// price expressed in whole cents would round every rate to zero. Nanos keep
// the arithmetic exact in int64 and are converted to cents once, at the end,
// where the rounding rule is stated.
type Price struct {
	Unit      Unit  `json:"unit"`
	NanoCents int64 `json:"nano_cents"`
}

// Plan is what an org is charged on.
type Plan struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Currency string  `json:"currency"`
	Prices   []Price `json:"prices"`
	// IncludedCents is a credit applied to the total (a free tier).
	IncludedCents int64 `json:"included_cents"`
	// MinimumCents floors a non-zero invoice, e.g. a payment processor's own
	// minimum charge. Applied only when there is something to charge.
	MinimumCents int64 `json:"minimum_cents"`
}

// Line is one priced dimension on an invoice.
type Line struct {
	Unit      Unit    `json:"unit"`
	Quantity  float64 `json:"quantity"`
	NanoCents int64   `json:"unit_nano_cents"`
	// AmountNanoCents is kept unrounded so the total is the sum of exact line
	// values, rounded once. Rounding each line and summing produces a total
	// that disagrees with an independently computed one.
	AmountNanoCents int64 `json:"amount_nano_cents"`
	AmountCents     int64 `json:"amount_cents"`
}

// Invoice is one org's charge for one period.
type Invoice struct {
	OrgID         string    `json:"org_id"`
	PlanID        string    `json:"plan_id"`
	Currency      string    `json:"currency"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	Lines         []Line    `json:"lines"`
	SubtotalCents int64     `json:"subtotal_cents"`
	CreditCents   int64     `json:"credit_cents"`
	TotalCents    int64     `json:"total_cents"`
}

// Usage is the metered input for one period, already aggregated.
type Usage struct {
	RunningSeconds float64
	CPUSeconds     float64
	MemPeakKB      int64
	RxBytes        int64
	TxBytes        int64
}

// FromRows folds a rollup into a single period total.
func FromRows(rows []store.UsageRow) Usage {
	var u Usage
	for _, r := range rows {
		u.RunningSeconds += r.RunningSeconds
		u.CPUSeconds += r.CPUSeconds
		u.RxBytes += r.RxBytes
		u.TxBytes += r.TxBytes
		// Peak memory is a MAXIMUM, not a sum: adding peaks across sandboxes
		// would bill a tenant for memory that never existed at the same time.
		if r.MemPeakKB > u.MemPeakKB {
			u.MemPeakKB = r.MemPeakKB
		}
	}
	return u
}

// quantity returns how many of `unit` this usage represents.
func (u Usage) quantity(unit Unit) float64 {
	switch unit {
	case UnitSandboxSecond:
		return u.RunningSeconds
	case UnitCPUSecond:
		return u.CPUSeconds
	case UnitGiBHour:
		// Peak GiB held for the running duration.
		return (float64(u.MemPeakKB) / (1024 * 1024)) * (u.RunningSeconds / 3600)
	case UnitEgressGB:
		// EGRESS only. Charging for inbound bytes lets anyone on the internet
		// run up a tenant's bill by sending them traffic.
		return float64(u.TxBytes) / 1e9
	}
	return 0
}

// Rate prices usage against a plan.
//
// Negative quantities are clamped to zero: a corrupt or backwards sample must
// never produce a credit that silently reduces a real bill.
func Rate(orgID string, plan Plan, u Usage, from, to time.Time) (Invoice, error) {
	if plan.Currency == "" {
		return Invoice{}, fmt.Errorf("plan %q has no currency", plan.ID)
	}
	if to.Before(from) {
		return Invoice{}, fmt.Errorf("period ends (%s) before it starts (%s)", to, from)
	}
	inv := Invoice{
		OrgID: orgID, PlanID: plan.ID, Currency: strings.ToUpper(plan.Currency),
		PeriodStart: from, PeriodEnd: to,
	}
	// Deterministic line order: an invoice that reorders between renders looks
	// like it changed.
	prices := append([]Price(nil), plan.Prices...)
	sort.Slice(prices, func(i, j int) bool { return prices[i].Unit < prices[j].Unit })

	var subtotalNano int64
	for _, p := range prices {
		q := u.quantity(p.Unit)
		if q < 0 {
			q = 0
		}
		if q == 0 && p.NanoCents == 0 {
			continue
		}
		amountNano := int64(q * float64(p.NanoCents))
		subtotalNano += amountNano
		inv.Lines = append(inv.Lines, Line{
			Unit: p.Unit, Quantity: q, NanoCents: p.NanoCents,
			AmountNanoCents: amountNano,
			AmountCents:     nanoToCents(amountNano),
		})
	}
	// ROUND ONCE, at the subtotal. Half-up, and only here.
	inv.SubtotalCents = nanoToCents(subtotalNano)

	credit := plan.IncludedCents
	if credit > inv.SubtotalCents {
		credit = inv.SubtotalCents
	}
	if credit < 0 {
		credit = 0
	}
	inv.CreditCents = credit
	inv.TotalCents = inv.SubtotalCents - credit
	// A minimum applies only to a bill that exists. Applying it to a zero
	// invoice charges idle customers.
	if inv.TotalCents > 0 && inv.TotalCents < plan.MinimumCents {
		inv.TotalCents = plan.MinimumCents
	}
	return inv, nil
}

// nanoToCents converts nano-cents to cents, rounding half away from zero.
func nanoToCents(nano int64) int64 {
	const scale = 1_000_000_000
	if nano >= 0 {
		return (nano + scale/2) / scale
	}
	return -((-nano + scale/2) / scale)
}

// PaymentProvider charges an invoice. Substrate never stores a card: the
// provider holds the customer record and substrate holds only its id.
//
// Deliberately an interface with a no-op default. Wiring a real processor means
// live credentials and real money movement; that is an explicit, operator-made
// decision, not something a service should acquire by being deployed.
type PaymentProvider interface {
	// Charge submits an invoice. It MUST be idempotent on idempotencyKey:
	// substrate retries, and a retry that charges twice is the worst bug this
	// package can have.
	Charge(ctx Context, customerRef string, inv Invoice, idempotencyKey string) (string, error)
	Name() string
}

// Context is the minimal context the provider needs; kept local so this
// package does not force a dependency on callers' context type.
type Context interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

// NoopProvider records invoices without moving money. It is the default so a
// deployment that has not chosen a processor still produces correct invoices
// and simply does not charge for them.
type NoopProvider struct{}

func (NoopProvider) Name() string { return "noop" }
func (NoopProvider) Charge(_ Context, customerRef string, inv Invoice, idem string) (string, error) {
	return "noop_" + idem, nil
}
