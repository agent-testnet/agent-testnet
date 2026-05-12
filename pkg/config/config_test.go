package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadServerConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "server.yaml")

	yaml := `controlplane:
  listen: ":9443"
  data_dir: "/tmp/testnet-data"
  nodes_file: "/tmp/nodes.yaml"
  tls:
    cert_file: "/tmp/cert.pem"
    key_file: "/tmp/key.pem"

dns:
  listen_public: ":5353"

vip:
  subnet: "10.200.0.0/16"
  dns_vip: "10.200.0.1"
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServerConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ControlPlane.Listen != ":9443" {
		t.Fatalf("expected listen :9443, got %s", cfg.ControlPlane.Listen)
	}
	if cfg.ControlPlane.DataDir != "/tmp/testnet-data" {
		t.Fatalf("expected data_dir /tmp/testnet-data, got %s", cfg.ControlPlane.DataDir)
	}
	if cfg.VIP.Subnet != "10.200.0.0/16" {
		t.Fatalf("expected VIP subnet 10.200.0.0/16, got %s", cfg.VIP.Subnet)
	}

	// DNS defaults should be applied for unset fields
	if cfg.DNS.ListenTunnel != "83.150.0.1:53" {
		t.Fatalf("expected default DNS listen_tunnel, got %s", cfg.DNS.ListenTunnel)
	}
	if cfg.DNS.RefreshInterval != 10*time.Second {
		t.Fatalf("expected default refresh interval 10s, got %v", cfg.DNS.RefreshInterval)
	}

	// WireGuard defaults
	if cfg.WireGuard.ListenPort != 51820 {
		t.Fatalf("expected default WG port 51820, got %d", cfg.WireGuard.ListenPort)
	}
	if cfg.WireGuard.TunnelIP != "10.99.0.1/16" {
		t.Fatalf("expected default tunnel IP, got %s", cfg.WireGuard.TunnelIP)
	}
}

func TestLoadServerConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "server.yaml")

	// Minimal config -- all defaults should apply
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadServerConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ControlPlane.Listen != ":8443" {
		t.Fatalf("expected default listen :8443, got %s", cfg.ControlPlane.Listen)
	}
	if cfg.ControlPlane.DataDir != "./data" {
		t.Fatalf("expected default data_dir ./data, got %s", cfg.ControlPlane.DataDir)
	}
	if cfg.ControlPlane.NodesFile != "./configs/nodes.yaml" {
		t.Fatalf("expected default nodes_file, got %s", cfg.ControlPlane.NodesFile)
	}
	if cfg.VIP.Subnet != "83.150.0.0/16" {
		t.Fatalf("expected default VIP subnet, got %s", cfg.VIP.Subnet)
	}
	if cfg.VIP.DNSVIP != "83.150.0.1" {
		t.Fatalf("expected default DNS VIP, got %s", cfg.VIP.DNSVIP)
	}
}

func TestLoadServerConfig_FileNotFound(t *testing.T) {
	_, err := LoadServerConfig("/nonexistent/server.yaml")
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadServerConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(cfgFile, []byte("{{invalid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadServerConfig(cfgFile)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadClientConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "client.yaml")

	yaml := `server:
  url: "https://1.2.3.4:8443"
daemon:
  data_dir: "/tmp/testnet-client"
sandbox:
  firecracker_bin: "/usr/local/bin/firecracker"
`
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadClientConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.URL != "https://1.2.3.4:8443" {
		t.Fatalf("expected server URL, got %s", cfg.Server.URL)
	}

	// Defaults should apply
	if cfg.Daemon.Socket != "/var/run/testnet-client.sock" {
		t.Fatalf("expected default socket, got %s", cfg.Daemon.Socket)
	}
	if cfg.Sandbox.DefaultVCPU != 1 {
		t.Fatalf("expected default vcpu 1, got %d", cfg.Sandbox.DefaultVCPU)
	}
	if cfg.Sandbox.DefaultMemMB != 512 {
		t.Fatalf("expected default mem 512, got %d", cfg.Sandbox.DefaultMemMB)
	}
}

func TestLoadClientConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "client.yaml")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadClientConfig(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.Socket != "/var/run/testnet-client.sock" {
		t.Fatalf("expected default socket, got %s", cfg.Daemon.Socket)
	}
	if cfg.Sandbox.DefaultVCPU != 1 {
		t.Fatalf("expected default vcpu 1, got %d", cfg.Sandbox.DefaultVCPU)
	}
	if cfg.Sandbox.DefaultMemMB != 512 {
		t.Fatalf("expected default mem 512, got %d", cfg.Sandbox.DefaultMemMB)
	}
}
