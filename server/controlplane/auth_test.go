package controlplane

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

func testNodeManager(t *testing.T, nodes []api.Node) *NodeManager {
	t.Helper()
	dir := t.TempDir()
	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(filepath.Join(dir, "nodes.yaml"), vipAlloc)

	nm.mu.Lock()
	for i := range nodes {
		n := &nodes[i]
		vip, err := vipAlloc.AllocateVIP(n.Name)
		if err != nil {
			t.Fatal(err)
		}
		n.VIP = vip
		nm.nodes[n.Name] = n
	}
	nm.mu.Unlock()
	return nm
}

func TestNewAuth_GeneratesToken(t *testing.T) {
	dir := t.TempDir()
	nm := testNodeManager(t, nil)

	auth, err := NewAuth(dir, nm)
	if err != nil {
		t.Fatal(err)
	}

	token := auth.JoinToken()
	if token == "" {
		t.Fatal("expected non-empty join token")
	}
	if len(token) != 64 { // 32 bytes -> 64 hex chars
		t.Fatalf("expected 64-char hex token, got %d chars", len(token))
	}

	data, err := os.ReadFile(filepath.Join(dir, "join-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != token {
		t.Fatalf("token on disk %q != in-memory %q", string(data), token)
	}
}

func TestNewAuth_LoadsExistingToken(t *testing.T) {
	dir := t.TempDir()
	existing := "deadbeef1234567890abcdef12345678"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "join-token"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth.JoinToken() != existing {
		t.Fatalf("expected loaded token %q, got %q", existing, auth.JoinToken())
	}
}

func TestValidateJoinToken(t *testing.T) {
	dir := t.TempDir()
	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !auth.ValidateJoinToken(auth.JoinToken()) {
		t.Fatal("expected valid token to pass")
	}
	if auth.ValidateJoinToken("wrong-token") {
		t.Fatal("expected wrong token to fail")
	}
	if auth.ValidateJoinToken("") {
		t.Fatal("expected empty token to fail")
	}
}

func TestValidateNodeSecret(t *testing.T) {
	nodes := []api.Node{
		{Name: "test-node", Address: "1.2.3.4:443", Secret: "node-secret-123"},
	}
	nm := testNodeManager(t, nodes)
	dir := t.TempDir()
	auth, err := NewAuth(dir, nm)
	if err != nil {
		t.Fatal(err)
	}

	if !auth.ValidateNodeSecret("test-node", "node-secret-123") {
		t.Fatal("expected valid node secret to pass")
	}
	if auth.ValidateNodeSecret("test-node", "wrong-secret") {
		t.Fatal("expected wrong secret to fail")
	}
	if auth.ValidateNodeSecret("test-node", "") {
		t.Fatal("expected empty secret to fail")
	}
	if auth.ValidateNodeSecret("nonexistent", "node-secret-123") {
		t.Fatal("expected unknown node to fail")
	}
}

func TestValidateNodeSecret_NilNodes(t *testing.T) {
	dir := t.TempDir()
	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth.ValidateNodeSecret("any", "any") {
		t.Fatal("expected nil nodes to fail")
	}
}

func TestRotateJoinToken(t *testing.T) {
	dir := t.TempDir()
	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	oldToken := auth.JoinToken()

	newToken, err := auth.RotateJoinToken()
	if err != nil {
		t.Fatal(err)
	}
	if newToken == "" {
		t.Fatal("expected non-empty new token")
	}
	if newToken == oldToken {
		t.Fatal("expected new token to differ from old")
	}
	if auth.ValidateJoinToken(oldToken) {
		t.Fatal("old token should be invalid after rotation")
	}
	if !auth.ValidateJoinToken(newToken) {
		t.Fatal("new token should be valid after rotation")
	}

	data, err := os.ReadFile(filepath.Join(dir, "join-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != newToken {
		t.Fatal("rotated token not persisted to disk")
	}
}

func TestIssueAPIToken(t *testing.T) {
	dir := t.TempDir()
	auth, err := NewAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	token, hash, err := auth.IssueAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected non-empty API token")
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}
	if HashToken(token) != hash {
		t.Fatalf("hash mismatch: HashToken(%q) = %q, want %q", token, HashToken(token), hash)
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	h1 := HashToken("test-token")
	h2 := HashToken("test-token")
	if h1 != h2 {
		t.Fatalf("expected same hash, got %q and %q", h1, h2)
	}
	h3 := HashToken("different-token")
	if h1 == h3 {
		t.Fatal("expected different tokens to produce different hashes")
	}
}
