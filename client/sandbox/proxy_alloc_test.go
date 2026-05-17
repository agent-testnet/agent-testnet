package sandbox

import (
	"strings"
	"testing"
)

func TestProxyAllocator_PublicSequential(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	got := []string{}
	for i := 0; i < 3; i++ {
		ip, err := a.AllocatePublic()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip)
	}

	want := []string{"83.150.255.1", "83.150.255.2", "83.150.255.3"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("public alloc %d: got %s, want %s", i, got[i], w)
		}
	}
}

func TestProxyAllocator_PrivateSequential(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"172.16.10.10", "172.16.10.11", "172.16.10.12"}
	for i, w := range want {
		ip, err := a.AllocatePrivate(10)
		if err != nil {
			t.Fatalf("private alloc %d: %v", i, err)
		}
		if ip != w {
			t.Fatalf("private alloc %d: got %s, want %s", i, ip, w)
		}
	}
}

func TestProxyAllocator_PrivatePerVMIsolated(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	// Two distinct vmIndexes get distinct ranges so allocations don't
	// collide.
	ip10, err := a.AllocatePrivate(10)
	if err != nil {
		t.Fatal(err)
	}
	ip11, err := a.AllocatePrivate(11)
	if err != nil {
		t.Fatal(err)
	}
	if ip10 == ip11 {
		t.Fatalf("expected different IPs across vmIndexes, both got %s", ip10)
	}
	if !strings.HasPrefix(ip10, "172.16.10.") {
		t.Fatalf("vmIndex=10 IP %s not in expected /24", ip10)
	}
	if !strings.HasPrefix(ip11, "172.16.11.") {
		t.Fatalf("vmIndex=11 IP %s not in expected /24", ip11)
	}
}

func TestProxyAllocator_ClaimAvoidsCollision(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	// Pre-claim the first three public IPs (e.g. as if reloaded from disk).
	a.Claim("83.150.255.1")
	a.Claim("83.150.255.2")
	a.Claim("83.150.255.3")

	ip, err := a.AllocatePublic()
	if err != nil {
		t.Fatal(err)
	}
	if ip != "83.150.255.4" {
		t.Fatalf("expected 83.150.255.4 after pre-claims, got %s", ip)
	}
}

func TestProxyAllocator_ReleaseAllowsReuse(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	ip1, err := a.AllocatePublic()
	if err != nil {
		t.Fatal(err)
	}
	a.Release(ip1)

	ip2, err := a.AllocatePublic()
	if err != nil {
		t.Fatal(err)
	}
	if ip1 != ip2 {
		t.Fatalf("after release expected %s to be re-handed-out, got %s", ip1, ip2)
	}
}

func TestProxyAllocator_PublicExhaustion(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for {
		_, err := a.AllocatePublic()
		if err != nil {
			break
		}
		count++
	}
	// 254 usable IPs in a /24 (.1..254).
	if count != 254 {
		t.Fatalf("expected 254 public allocations before exhaustion, got %d", count)
	}
	if _, err := a.AllocatePublic(); err == nil {
		t.Fatal("expected exhaustion error after the range is full")
	}
}

func TestProxyAllocator_AllocateDispatch(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}

	ip, err := a.Allocate("public", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ip, "83.150.255.") {
		t.Fatalf("public Allocate returned %s, expected 83.150.255.x", ip)
	}

	ip, err = a.Allocate("private", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ip, "172.16.10.") {
		t.Fatalf("private Allocate returned %s, expected 172.16.10.x", ip)
	}

	if _, err := a.Allocate("nonsense", 10); err == nil {
		t.Fatal("expected error for unknown visibility")
	}
}

func TestProxyAllocator_PrivateInvalidVMIndex(t *testing.T) {
	a, err := NewProxyAllocator()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocatePrivate(0); err == nil {
		t.Fatal("expected error for vmIndex=0")
	}
	if _, err := a.AllocatePrivate(255); err == nil {
		t.Fatal("expected error for vmIndex=255")
	}
}
