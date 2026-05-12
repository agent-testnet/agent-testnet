package controlplane

import (
	"strings"
	"testing"
)

func setupRegistry(t *testing.T) (*Registry, *Auth) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewRegistry(store, auth), auth
}

func TestRegisterClient_Success(t *testing.T) {
	reg, auth := setupRegistry(t)

	client, apiToken, err := reg.RegisterClient(auth.JoinToken(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	if client.ID != "client-1" {
		t.Fatalf("expected client-1, got %s", client.ID)
	}
	if client.TunnelCIDR != "10.99.1.0/24" {
		t.Fatalf("expected 10.99.1.0/24, got %s", client.TunnelCIDR)
	}
	if apiToken == "" {
		t.Fatal("expected non-empty API token")
	}
}

func TestRegisterClient_InvalidToken(t *testing.T) {
	reg, _ := setupRegistry(t)

	_, _, err := reg.RegisterClient("wrong-token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if !strings.Contains(err.Error(), "invalid join token") {
		t.Fatalf("expected 'invalid join token' error, got: %v", err)
	}
}

func TestRegisterClient_CIDRAllocation(t *testing.T) {
	reg, auth := setupRegistry(t)
	token := auth.JoinToken()

	expected := []string{"10.99.1.0/24", "10.99.2.0/24", "10.99.3.0/24"}
	for i, want := range expected {
		client, _, err := reg.RegisterClient(token, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
		if err != nil {
			t.Fatal(err)
		}
		if client.TunnelCIDR != want {
			t.Fatalf("client %d: expected CIDR %s, got %s", i+1, want, client.TunnelCIDR)
		}
	}
}

func TestRegisterClient_MaxClients(t *testing.T) {
	reg, auth := setupRegistry(t)
	reg.nextCIDR = 255 // beyond 254 limit

	_, _, err := reg.RegisterClient(auth.JoinToken(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err == nil {
		t.Fatal("expected error when max clients reached")
	}
	if !strings.Contains(err.Error(), "maximum clients reached") {
		t.Fatalf("expected 'maximum clients reached', got: %v", err)
	}
}

func TestDeregisterClient(t *testing.T) {
	reg, auth := setupRegistry(t)

	client, _, err := reg.RegisterClient(auth.JoinToken(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.DeregisterClient(client.ID); err != nil {
		t.Fatal(err)
	}

	if reg.store.GetClient(client.ID) != nil {
		t.Fatal("expected client to be removed from store")
	}
}

func TestValidateAPIToken(t *testing.T) {
	reg, auth := setupRegistry(t)

	_, apiToken, err := reg.RegisterClient(auth.JoinToken(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}

	cid, ok := reg.ValidateAPIToken(apiToken)
	if !ok {
		t.Fatal("expected API token to be valid")
	}
	if cid != "client-1" {
		t.Fatalf("expected client-1, got %s", cid)
	}

	_, ok = reg.ValidateAPIToken("invalid-token")
	if ok {
		t.Fatal("expected invalid token to fail")
	}
}
