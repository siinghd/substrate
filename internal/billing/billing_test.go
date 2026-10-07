package billing

import (
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

func testPlan() Plan {
	return Plan{
		ID: "plan_std", Name: "standard", Currency: "usd",
		Prices: []Price{
			{Unit: UnitSandboxSecond, NanoCents: 300},      // 0.0000003 USD/s
			{Unit: UnitCPUSecond, NanoCents: 2_000},        // 0.000002 USD/s
			{Unit: UnitGiBHour, NanoCents: 1_200_000_000},  // $0.012/GiB-hour
			{Unit: UnitEgressGB, NanoCents: 9_000_000_000}, // $0.09/GB
		},
	}
}

// TestRateIsExactAtScale: the reason money is integer nano-cents. Price a
// month of real usage and check the total against an independently computed
// value, not against the code's own arithmetic.
func TestRateIsExactAtScale(t *testing.T) {
	p := testPlan()
	u := Usage{
		RunningSeconds: 2_592_000, // one sandbox-month
		CPUSeconds:     100_000,
		MemPeakKB:      2 * 1024 * 1024, // 2 GiB
		TxBytes:        50e9,            // 50 GB egress
	}
	now := time.Now()
	inv, err := Rate("org_1", p, u, now.Add(-720*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	// Computed by hand:
	//  residency 2,592,000 s * 300 nc      =     777,600,000 nc =    0.78 c
	//  cpu         100,000 s * 2,000 nc    =     200,000,000 nc =    0.20 c
	//  memory  2 GiB * 720 h * 1.2e9 nc    = 1,728,000,000,000 nc = 1728.00 c
	//  egress       50 GB * 9e9 nc         =   450,000,000,000 nc =  450.00 c
	//  subtotal                            = 2,178,977,600,000 nc = 2178.98 c
	const wantSubtotal = 2179
	if inv.SubtotalCents != wantSubtotal {
		t.Fatalf("subtotal = %d cents, want %d\nlines: %+v", inv.SubtotalCents, wantSubtotal, inv.Lines)
	}
	if inv.TotalCents != wantSubtotal {
		t.Fatalf("total = %d, want %d", inv.TotalCents, wantSubtotal)
	}
	if inv.Currency != "USD" {
		t.Errorf("currency = %q, want normalised USD", inv.Currency)
	}
}

// TestRateRoundsOnceNotPerLine: rounding each line and summing gives a total
// that disagrees with the exact one. Four lines that each round down by just
// under half a cent lose nearly two cents per invoice, every invoice.
func TestRateRoundsOnceNotPerLine(t *testing.T) {
	p := Plan{ID: "p", Currency: "usd", Prices: []Price{
		{Unit: UnitSandboxSecond, NanoCents: 499_000_000}, // 0.499 c each
		{Unit: UnitCPUSecond, NanoCents: 499_000_000},
		{Unit: UnitEgressGB, NanoCents: 499_000_000},
	}}
	u := Usage{RunningSeconds: 1, CPUSeconds: 1, TxBytes: 1e9}
	inv, err := Rate("org", p, u, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Exact: 3 * 0.499 = 1.497 c -> 1 cent.
	// Per-line rounding: each 0.499 -> 0 cents -> total 0. That is the bug.
	if inv.SubtotalCents != 1 {
		t.Fatalf("subtotal = %d, want 1: rounding is being applied per line", inv.SubtotalCents)
	}
}

// TestInboundBytesAreNotBilled: charging for received bytes means anyone on
// the internet can run up a tenant's bill by sending them traffic.
func TestInboundBytesAreNotBilled(t *testing.T) {
	p := Plan{ID: "p", Currency: "usd", Prices: []Price{{Unit: UnitEgressGB, NanoCents: 9_000_000_000}}}
	inv, err := Rate("org", p, Usage{RxBytes: 500e9}, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalCents != 0 {
		t.Fatalf("500 GB of INBOUND traffic was billed %d cents", inv.TotalCents)
	}
}

// TestNegativeUsageNeverCredits: a corrupt or backwards sample must not reduce
// a real bill.
func TestNegativeUsageNeverCredits(t *testing.T) {
	p := testPlan()
	inv, err := Rate("org", p, Usage{RunningSeconds: -1e9, CPUSeconds: -1e9, TxBytes: -1e12}, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalCents < 0 {
		t.Fatalf("negative usage produced a credit of %d cents", -inv.TotalCents)
	}
}

// TestFreeTierCannotGoNegative: an included credit larger than the bill must
// zero it, not owe the customer money.
func TestFreeTierCannotGoNegative(t *testing.T) {
	p := testPlan()
	p.IncludedCents = 100_000
	inv, err := Rate("org", p, Usage{RunningSeconds: 60}, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalCents != 0 {
		t.Fatalf("total = %d, want 0", inv.TotalCents)
	}
	if inv.CreditCents > inv.SubtotalCents {
		t.Fatalf("credited %d against a %d subtotal", inv.CreditCents, inv.SubtotalCents)
	}
}

// TestMinimumDoesNotChargeIdleCustomers: a floor must apply to a bill that
// exists, not create one.
func TestMinimumDoesNotChargeIdleCustomers(t *testing.T) {
	p := testPlan()
	p.MinimumCents = 500
	inv, err := Rate("org", p, Usage{}, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalCents != 0 {
		t.Fatalf("an idle org was charged %d cents by the minimum", inv.TotalCents)
	}
}

// TestPeakMemoryIsNotSummedAcrossSandboxes: peaks are maxima. Summing them
// bills a tenant for memory that never coexisted.
func TestPeakMemoryIsNotSummedAcrossSandboxes(t *testing.T) {
	rows := []store.UsageRow{
		{RunningSeconds: 3600, MemPeakKB: 1024 * 1024},
		{RunningSeconds: 3600, MemPeakKB: 1024 * 1024},
		{RunningSeconds: 3600, MemPeakKB: 512 * 1024},
	}
	u := FromRows(rows)
	if u.MemPeakKB != 1024*1024 {
		t.Fatalf("MemPeakKB = %d, want the maximum 1048576", u.MemPeakKB)
	}
	if u.RunningSeconds != 10800 {
		t.Fatalf("RunningSeconds = %v, want 10800 (residency DOES sum)", u.RunningSeconds)
	}
}

// TestRateRejectsBackwardsPeriod: a period that ends before it starts is a
// caller bug that would silently produce a zero invoice.
func TestRateRejectsBackwardsPeriod(t *testing.T) {
	now := time.Now()
	if _, err := Rate("org", testPlan(), Usage{}, now, now.Add(-time.Hour)); err == nil {
		t.Fatal("accepted a period that ends before it starts")
	}
}

func TestNanoToCentsRoundsHalfAwayFromZero(t *testing.T) {
	for _, tc := range []struct {
		nano int64
		want int64
	}{
		{0, 0}, {499_999_999, 0}, {500_000_000, 1}, {1_499_999_999, 1}, {1_500_000_000, 2},
		{-500_000_000, -1}, {-499_999_999, 0},
	} {
		if got := nanoToCents(tc.nano); got != tc.want {
			t.Errorf("nanoToCents(%d) = %d, want %d", tc.nano, got, tc.want)
		}
	}
}
