package controlplane

import (
	"testing"
)

func TestVIPAllocator_Sequential(t *testing.T) {
	va, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{"83.150.0.2", "83.150.0.3", "83.150.0.4"}
	for i, want := range expected {
		ip, err := va.AllocateVIP("node-" + string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if ip.String() != want {
			t.Fatalf("VIP %d: got %s, want %s", i, ip.String(), want)
		}
	}
}

func TestVIPAllocator_Idempotent(t *testing.T) {
	va, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}

	ip1, err := va.AllocateVIP("same-key")
	if err != nil {
		t.Fatal(err)
	}
	ip2, err := va.AllocateVIP("same-key")
	if err != nil {
		t.Fatal(err)
	}
	if !ip1.Equal(ip2) {
		t.Fatalf("expected same VIP for same key, got %s and %s", ip1, ip2)
	}
}

func TestVIPAllocator_SkipsDNSVIP(t *testing.T) {
	va, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}

	allocs := va.AllAllocations()
	for _, ip := range allocs {
		if ip.String() == "83.150.0.1" {
			t.Fatal("DNS VIP should never be allocated to a key")
		}
	}

	// Allocate many VIPs and ensure DNS VIP is never among them
	for i := range 10 {
		ip, err := va.AllocateVIP(string(rune('a' + i)))
		if err != nil {
			t.Fatal(err)
		}
		if ip.String() == "83.150.0.1" {
			t.Fatal("allocated the DNS VIP to a node")
		}
	}
}

func TestVIPAllocator_DNSVIP(t *testing.T) {
	va, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	dns := va.DNSVIP()
	if dns.String() != "83.150.0.1" {
		t.Fatalf("expected DNS VIP 83.150.0.1, got %s", dns)
	}
}

func TestVIPAllocator_Release(t *testing.T) {
	va, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}

	ip, err := va.AllocateVIP("node-x")
	if err != nil {
		t.Fatal(err)
	}
	if ip == nil {
		t.Fatal("expected VIP, got nil")
	}

	va.ReleaseVIP("node-x")
	if va.GetVIP("node-x") != nil {
		t.Fatal("expected nil after release")
	}

	// Allocating a new key should NOT reuse the released IP
	ip2, err := va.AllocateVIP("node-y")
	if err != nil {
		t.Fatal(err)
	}
	if ip.Equal(ip2) {
		t.Fatal("released VIP should not be reused")
	}
}

func TestVIPAllocator_InvalidSubnet(t *testing.T) {
	_, err := NewVIPAllocator("invalid", "83.150.0.1")
	if err == nil {
		t.Fatal("expected error for invalid subnet")
	}
}

func TestVIPAllocator_InvalidDNSVIP(t *testing.T) {
	_, err := NewVIPAllocator("83.150.0.0/16", "invalid")
	if err == nil {
		t.Fatal("expected error for invalid DNS VIP")
	}
}
