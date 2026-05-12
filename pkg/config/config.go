package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig is the top-level server configuration.
type ServerConfig struct {
	ControlPlane ControlPlaneConfig `yaml:"controlplane"`
	DNS          DNSConfig          `yaml:"dns"`
	WireGuard    WireGuardConfig    `yaml:"wireguard"`
	Router       RouterConfig       `yaml:"router"`
	VIP          VIPConfig          `yaml:"vip"`
}

type ControlPlaneConfig struct {
	Listen    string    `yaml:"listen"`
	DataDir   string    `yaml:"data_dir"`
	NodesFile string    `yaml:"nodes_file"`
	TLS       TLSConfig `yaml:"tls"`
	CA        CAConfig  `yaml:"ca"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type CAConfig struct {
	KeyFile  string `yaml:"key_file"`
	CertFile string `yaml:"cert_file"`
}

type DNSConfig struct {
	ListenTunnel    string        `yaml:"listen_tunnel"`
	ListenPublic    string        `yaml:"listen_public"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
}

type WireGuardConfig struct {
	ListenPort     int    `yaml:"listen_port"`
	TunnelIP       string `yaml:"tunnel_ip"`
	PrivateKeyFile string `yaml:"private_key_file"`
}

type RouterConfig struct {
	LogFile string `yaml:"log_file"`
}

type VIPConfig struct {
	Subnet string `yaml:"subnet"`
	DNSVIP string `yaml:"dns_vip"`
}

// ClientConfig is the top-level client configuration.
type ClientConfig struct {
	Server  ClientServerConfig  `yaml:"server"`
	Daemon  ClientDaemonConfig  `yaml:"daemon"`
	Sandbox ClientSandboxConfig `yaml:"sandbox"`
}

type ClientServerConfig struct {
	URL string `yaml:"url"`
}

type ClientDaemonConfig struct {
	Socket   string `yaml:"socket"`
	DataDir  string `yaml:"data_dir"`
	WGConfig string `yaml:"wg_config"`
}

type ClientSandboxConfig struct {
	FirecrackerBin string `yaml:"firecracker_bin"`
	KernelPath     string `yaml:"kernel_path"`
	DefaultRootFS  string `yaml:"default_rootfs"`
	DefaultVCPU    int    `yaml:"default_vcpu"`
	DefaultMemMB   int    `yaml:"default_mem_mb"`
	VMSubnet       string `yaml:"vm_subnet"`
}

// LoadServerConfig reads and parses a server YAML config file.
func LoadServerConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	setServerDefaults(&cfg)
	return &cfg, nil
}

// LoadClientConfig reads and parses a client YAML config file.
func LoadClientConfig(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	SetClientDefaults(&cfg)
	return &cfg, nil
}

func setServerDefaults(cfg *ServerConfig) {
	if cfg.ControlPlane.Listen == "" {
		cfg.ControlPlane.Listen = ":8443"
	}
	if cfg.ControlPlane.DataDir == "" {
		cfg.ControlPlane.DataDir = "./data"
	}
	if cfg.ControlPlane.NodesFile == "" {
		cfg.ControlPlane.NodesFile = "./configs/nodes.yaml"
	}
	if cfg.DNS.ListenTunnel == "" {
		cfg.DNS.ListenTunnel = "83.150.0.1:53"
	}
	if cfg.DNS.RefreshInterval == 0 {
		cfg.DNS.RefreshInterval = 10 * time.Second
	}
	if cfg.WireGuard.ListenPort == 0 {
		cfg.WireGuard.ListenPort = 51820
	}
	if cfg.WireGuard.TunnelIP == "" {
		cfg.WireGuard.TunnelIP = "10.99.0.1/16"
	}
	if cfg.VIP.Subnet == "" {
		cfg.VIP.Subnet = "83.150.0.0/16"
	}
	if cfg.VIP.DNSVIP == "" {
		cfg.VIP.DNSVIP = "83.150.0.1"
	}
}

// SetClientDefaults applies defaults to a ClientConfig.
func SetClientDefaults(cfg *ClientConfig) {
	if cfg.Daemon.Socket == "" {
		cfg.Daemon.Socket = "/var/run/testnet-client.sock"
	}
	if cfg.Sandbox.DefaultVCPU == 0 {
		cfg.Sandbox.DefaultVCPU = 1
	}
	if cfg.Sandbox.DefaultMemMB == 0 {
		cfg.Sandbox.DefaultMemMB = 512
	}
}
