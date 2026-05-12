package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testNodesYAML = `nodes:
  - name: "alpha"
    address: "1.2.3.4:443"
    secret: "alpha-secret"
    domains:
      - "alpha.example.com"
      - "www.alpha.com"
  - name: "beta"
    address: "5.6.7.8:443"
    secret: "beta-secret"
    domains:
      - "beta.example.com"
`

func writeNodesFile(t *testing.T, dir, content string) string {
	t.Helper()
	fp := filepath.Join(dir, "nodes.yaml")
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestNodeManager_Load(t *testing.T) {
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, testNodesYAML)

	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(fp, vipAlloc)
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	// Verify GetNode
	alpha := nm.GetNode("alpha")
	if alpha == nil {
		t.Fatal("expected alpha node")
	}
	if alpha.Address != "1.2.3.4:443" {
		t.Fatalf("expected address 1.2.3.4:443, got %s", alpha.Address)
	}
	if alpha.VIP == nil {
		t.Fatal("expected VIP to be assigned")
	}

	beta := nm.GetNode("beta")
	if beta == nil {
		t.Fatal("expected beta node")
	}

	// Verify ResolveDomain
	if vip := nm.ResolveDomain("alpha.example.com"); vip == nil {
		t.Fatal("expected VIP for alpha.example.com")
	}
	if vip := nm.ResolveDomain("beta.example.com"); vip == nil {
		t.Fatal("expected VIP for beta.example.com")
	}
	if vip := nm.ResolveDomain("nonexistent.com"); vip != nil {
		t.Fatal("expected nil VIP for unknown domain")
	}

	// Verify AllNodes
	nodes := nm.AllNodes()
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
}

func TestNodeManager_AutoName(t *testing.T) {
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, testNodesYAML)

	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(fp, vipAlloc)
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	// Auto-name should resolve to same VIP as the node
	alphaVIP := nm.ResolveDomain("alpha.testnet")
	if alphaVIP == nil {
		t.Fatal("expected VIP for alpha.testnet auto-name")
	}
	alpha := nm.GetNode("alpha")
	if !alphaVIP.Equal(alpha.VIP) {
		t.Fatalf("auto-name VIP %s != node VIP %s", alphaVIP, alpha.VIP)
	}

	betaVIP := nm.ResolveDomain("beta.testnet")
	if betaVIP == nil {
		t.Fatal("expected VIP for beta.testnet auto-name")
	}
}

func TestNodeManager_Validation_DuplicateName(t *testing.T) {
	yaml := `nodes:
  - name: "dup"
    address: "1.2.3.4:443"
    secret: "s1"
  - name: "dup"
    address: "5.6.7.8:443"
    secret: "s2"
`
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, yaml)
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager(fp, vipAlloc)
	err := nm.Load()
	if err == nil {
		t.Fatal("expected error for duplicate node names")
	}
	if !strings.Contains(err.Error(), "duplicate node name") {
		t.Fatalf("expected duplicate error, got: %v", err)
	}
}

func TestNodeManager_Validation_MissingAddress(t *testing.T) {
	yaml := `nodes:
  - name: "noaddr"
    secret: "s1"
`
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, yaml)
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager(fp, vipAlloc)
	err := nm.Load()
	if err == nil {
		t.Fatal("expected error for missing address")
	}
	if !strings.Contains(err.Error(), "missing address") {
		t.Fatalf("expected missing address error, got: %v", err)
	}
}

func TestNodeManager_Validation_InvalidAddress(t *testing.T) {
	yaml := `nodes:
  - name: "badaddr"
    address: "not-host-port"
    secret: "s1"
`
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, yaml)
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager(fp, vipAlloc)
	err := nm.Load()
	if err == nil {
		t.Fatal("expected error for invalid address format")
	}
	if !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("expected host:port error, got: %v", err)
	}
}

func TestNodeManager_Validation_MissingName(t *testing.T) {
	yaml := `nodes:
  - address: "1.2.3.4:443"
    secret: "s1"
`
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, yaml)
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager(fp, vipAlloc)
	err := nm.Load()
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "missing name") {
		t.Fatalf("expected missing name error, got: %v", err)
	}
}

func TestNodeManager_Validation_DuplicateDomain(t *testing.T) {
	yaml := `nodes:
  - name: "n1"
    address: "1.2.3.4:443"
    secret: "s1"
    domains:
      - "shared.com"
  - name: "n2"
    address: "5.6.7.8:443"
    secret: "s2"
    domains:
      - "shared.com"
`
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, yaml)
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager(fp, vipAlloc)
	err := nm.Load()
	if err == nil {
		t.Fatal("expected error for duplicate domain")
	}
	if !strings.Contains(err.Error(), "duplicate domain") {
		t.Fatalf("expected duplicate domain error, got: %v", err)
	}
}

func TestNodeManager_Reload(t *testing.T) {
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, testNodesYAML)

	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(fp, vipAlloc)
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	if nm.GetNode("alpha") == nil {
		t.Fatal("expected alpha before reload")
	}

	// Rewrite with different content
	newYAML := `nodes:
  - name: "gamma"
    address: "9.8.7.6:443"
    secret: "gamma-secret"
    domains:
      - "gamma.example.com"
`
	if err := os.WriteFile(fp, []byte(newYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	if nm.GetNode("alpha") != nil {
		t.Fatal("alpha should be gone after reload")
	}
	if nm.GetNode("gamma") == nil {
		t.Fatal("expected gamma after reload")
	}
}

func TestNodeManager_AllDomainMappings(t *testing.T) {
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, testNodesYAML)

	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(fp, vipAlloc)
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	mappings := nm.AllDomainMappings()
	// 2 nodes: alpha has 2 domains + auto-name, beta has 1 domain + auto-name = 5 total
	if len(mappings) != 5 {
		t.Fatalf("expected 5 domain mappings, got %d", len(mappings))
	}
}

func TestNodeManager_VIPToAddress(t *testing.T) {
	dir := t.TempDir()
	fp := writeNodesFile(t, dir, testNodesYAML)

	vipAlloc, err := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	if err != nil {
		t.Fatal(err)
	}
	nm := NewNodeManager(fp, vipAlloc)
	if err := nm.Load(); err != nil {
		t.Fatal(err)
	}

	alpha := nm.GetNode("alpha")
	addr, ok := nm.VIPToAddress(alpha.VIP)
	if !ok || addr != "1.2.3.4:443" {
		t.Fatalf("expected (1.2.3.4:443, true), got (%s, %v)", addr, ok)
	}
}

func TestNodeManager_FileNotFound(t *testing.T) {
	vipAlloc, _ := NewVIPAllocator("83.150.0.0/16", "83.150.0.1")
	nm := NewNodeManager("/nonexistent/nodes.yaml", vipAlloc)
	if err := nm.Load(); err == nil {
		t.Fatal("expected error for missing file")
	}
}
