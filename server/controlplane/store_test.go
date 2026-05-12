package controlplane

import (
	"testing"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

func TestStore_AddAndGet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	client := &api.Client{
		ID:          "client-1",
		WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TunnelCIDR:  "10.99.1.0/24",
		CreatedAt:   "2025-01-01T00:00:00Z",
	}
	hash := HashToken("test-api-token")
	if err := s.AddClient(client, hash); err != nil {
		t.Fatal(err)
	}

	got := s.GetClient("client-1")
	if got == nil {
		t.Fatal("expected client, got nil")
	}
	if got.ID != "client-1" {
		t.Fatalf("expected ID client-1, got %s", got.ID)
	}
	if got.TunnelCIDR != "10.99.1.0/24" {
		t.Fatalf("expected CIDR 10.99.1.0/24, got %s", got.TunnelCIDR)
	}
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	client := &api.Client{
		ID:          "client-1",
		WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TunnelCIDR:  "10.99.1.0/24",
		CreatedAt:   "2025-01-01T00:00:00Z",
	}
	hash := HashToken("persist-token")
	if err := s1.AddClient(client, hash); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.GetClient("client-1")
	if got == nil {
		t.Fatal("expected persisted client after reload, got nil")
	}
	if got.TunnelCIDR != "10.99.1.0/24" {
		t.Fatalf("expected CIDR 10.99.1.0/24 after reload, got %s", got.TunnelCIDR)
	}

	cid, ok := s2.ValidateTokenHash(hash)
	if !ok {
		t.Fatal("expected token hash to be valid after reload")
	}
	if cid != "client-1" {
		t.Fatalf("expected client-1, got %s", cid)
	}
}

func TestStore_RemoveClient(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	client := &api.Client{
		ID:          "client-1",
		WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TunnelCIDR:  "10.99.1.0/24",
		CreatedAt:   "2025-01-01T00:00:00Z",
	}
	hash := HashToken("remove-token")
	if err := s.AddClient(client, hash); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveClient("client-1"); err != nil {
		t.Fatal(err)
	}
	if s.GetClient("client-1") != nil {
		t.Fatal("expected client to be removed")
	}
	if _, ok := s.ValidateTokenHash(hash); ok {
		t.Fatal("expected token hash to be removed")
	}
}

func TestStore_RemoveClient_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveClient("nonexistent"); err == nil {
		t.Fatal("expected error for removing nonexistent client")
	}
}

func TestStore_ValidateTokenHash(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	hash := HashToken("api-token-1")
	client := &api.Client{
		ID:          "client-1",
		WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TunnelCIDR:  "10.99.1.0/24",
		CreatedAt:   "2025-01-01T00:00:00Z",
	}
	if err := s.AddClient(client, hash); err != nil {
		t.Fatal(err)
	}

	cid, ok := s.ValidateTokenHash(hash)
	if !ok || cid != "client-1" {
		t.Fatalf("expected (client-1, true), got (%s, %v)", cid, ok)
	}

	_, ok = s.ValidateTokenHash(HashToken("wrong-token"))
	if ok {
		t.Fatal("expected unknown hash to fail validation")
	}
}

func TestStore_ClientCount(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if s.ClientCount() != 0 {
		t.Fatalf("expected 0 clients, got %d", s.ClientCount())
	}

	for i := range 3 {
		c := &api.Client{
			ID:          "client-" + string(rune('1'+i)),
			WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			TunnelCIDR:  "10.99.1.0/24",
		}
		if err := s.AddClient(c, HashToken(c.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if s.ClientCount() != 3 {
		t.Fatalf("expected 3 clients, got %d", s.ClientCount())
	}

	if err := s.RemoveClient("client-1"); err != nil {
		t.Fatal(err)
	}
	if s.ClientCount() != 2 {
		t.Fatalf("expected 2 clients after removal, got %d", s.ClientCount())
	}
}

func TestStore_ListClients(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"client-1", "client-2"} {
		c := &api.Client{
			ID:          id,
			WGPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			TunnelCIDR:  "10.99.1.0/24",
		}
		if err := s.AddClient(c, HashToken(id)); err != nil {
			t.Fatal(err)
		}
	}

	clients := s.ListClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
}
