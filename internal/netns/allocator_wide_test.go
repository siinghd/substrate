package netns

import (
	"net"
	"testing"
)

// TestAllocatorWideBlockNoCollisions pins the /10 correctness fix: the plan's
// default block is 100.64.0.0/10 (4,194,304 addresses). The old per-octet
// byte-carry arithmetic truncated at 65536 allocations and silently handed
// out duplicate IPs.
func TestAllocatorWideBlockNoCollisions(t *testing.T) {
	a, err := NewAllocator("100.64.0.0/10")
	if err != nil {
		t.Fatal(err)
	}
	const n = 70000 // past the old 16-bit truncation point
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		sub, err := a.Allocate()
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		k := sub.GuestIP.String()
		if _, dup := seen[k]; dup {
			t.Fatalf("collision at allocation %d: %s allocated twice", i, k)
		}
		// The host end must be distinct too: both live on the wire, and a
		// collision there is just as fatal as a guest-side one.
		if _, dup := seen[sub.HostIP.String()]; dup {
			t.Fatalf("host-side collision at allocation %d: %s", i, sub.HostIP)
		}
		seen[k] = struct{}{}
		seen[sub.HostIP.String()] = struct{}{}
		if !a.InBlock(sub.GuestIP) {
			t.Fatalf("allocated IP %s not reported in block", k)
		}
	}
}

// TestAllocatorReleaseRoundTrip: after FULL exhaustion, releasing one address
// and allocating again must return exactly it — proving Release's index math
// stays correct for offsets carrying into octet 2 (past the old truncation
// point). The allocator is a rotating cursor, so mid-stream release does not
// guarantee immediate reuse.
func TestAllocatorReleaseRoundTrip(t *testing.T) {
	a, err := NewAllocator("100.64.0.0/15") // 131072 addrs — spans octet 2 fully
	if err != nil {
		t.Fatal(err)
	}
	const releasedIdx = 20000 // deep past the old 16-bit truncation point
	var released Subnet
	seen := make(map[string]struct{})
	// /15 = 131072 addresses = 32768 subnets, slot 0 reserved.
	for i := 0; i < 32767; i++ {
		sub, err := a.Allocate()
		if err != nil {
			t.Fatalf("allocation %d failed early: %v", i, err)
		}
		if _, dup := seen[sub.CIDR()]; dup {
			t.Fatalf("collision at allocation %d", i)
		}
		seen[sub.CIDR()] = struct{}{}
		if i == releasedIdx {
			released = sub
			a.Release(sub.GuestIP) // bitmap slot freed while cursor marches on
		}
	}
	// Block now has exactly one free slot.
	got, err := a.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got.CIDR() != released.CIDR() {
		t.Fatalf("final allocation = %v, want the released %v", got.CIDR(), released.CIDR())
	}
	// Releasing by EITHER end must free the same subnet: callers hold the
	// guest address, teardown paths sometimes hold the host address.
	a.Release(got.HostIP)
	again, err := a.Allocate()
	if err != nil || again.CIDR() != got.CIDR() {
		t.Fatalf("release by host address did not free the subnet: got %v (%v)", again.CIDR(), err)
	}
}

// TestAllocatorSmallBlockStillWorks guards the common /24-style config.
func TestAllocatorSmallBlockStillWorks(t *testing.T) {
	a, err := NewAllocator("100.64.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	gw := net.ParseIP("100.64.0.0")
	sub, err := a.Allocate()
	// Slot 0 is reserved, so the first sandbox gets the SECOND /30.
	if err != nil || sub.CIDR() != "100.64.0.4/30" ||
		sub.HostIP.String() != "100.64.0.5" || sub.GuestIP.String() != "100.64.0.6" {
		t.Fatalf("first allocation = %+v (%v), want 100.64.0.4/30 host .5 guest .6", sub, err)
	}
	if a.InBlock(gw) == false {
		t.Fatal("gateway should be inside block")
	}
	outside := net.ParseIP("100.65.0.1")
	if a.InBlock(outside) {
		t.Fatal("/24 block wrongly claims address outside it")
	}
}
