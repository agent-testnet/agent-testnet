package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// IP-range conventions shared across the testnet.
//
// The full VIP space is a /16 (default 83.150.0.0/16). The testnet operator's
// VIPAllocator (used by `nodes.yaml`-driven testnet services) only hands out
// addresses in the first /17-ish portion of that space; the last /24 is
// permanently reserved for **client-side per-VM passthrough proxies** (see
// client/sandbox/proxy.go). That separation lets every client host
// independently allocate IPs in the reserved tail without coordinating with
// the server, while still keeping public-looking IPs from a single coherent
// /16.
//
// Why the split: agent VMs run with a strict SSRF guard in some agents (e.g.
// OpenClaw) that rejects URL fetches resolving to RFC1918 / "special-use" IPs.
// Aliasing the LLM proxy to a 83.150.x.y address makes it look "public" to
// the agent while still being intercepted locally by the host's TAP, all
// without colliding with operator-allocated VIPs.
const (
	// VIPSubnetDefault is the default VIP subnet — both the umbrella for
	// server-side VIP allocation and the home of the client-side passthrough
	// reservation.
	VIPSubnetDefault = "83.150.0.0/16"

	// DNSVIPDefault is the reserved DNS virtual IP.
	DNSVIPDefault = "83.150.0.1"

	// VIPAllocatorMaxOctet is the largest third-octet value the server-side
	// VIPAllocator is allowed to return (inclusive). The walk stops before
	// crossing into 83.150.255.0/24, which is reserved for client-side
	// passthrough proxies.
	VIPAllocatorMaxOctet = 254

	// ClientPassthroughSubnet is the slice of the VIP space reserved for
	// client-side per-VM passthrough proxies. Each client host independently
	// allocates IPs in this /24, aliases them onto the relevant TAP device,
	// and runs a TCP forwarder bound to them.
	ClientPassthroughSubnet = "83.150.255.0/24"

	// ClientPrivateProxyOffset is the first per-VM IP the client-side proxy
	// allocator hands out for "private" (172.16.<vmIndex>.x) passthroughs.
	// Lower addresses (.1 gateway, .2 guest) are reserved for the VM itself.
	ClientPrivateProxyOffset = 10
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
		cfg.DNS.ListenTunnel = DNSVIPDefault + ":53"
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
		cfg.VIP.Subnet = VIPSubnetDefault
	}
	if cfg.VIP.DNSVIP == "" {
		cfg.VIP.DNSVIP = DNSVIPDefault
	}
}

// SetClientDefaults applies defaults to a ClientConfig.
//
// These defaults must keep an entirely empty ClientConfig usable. The
// testnet-client root command falls back to &ClientConfig{} when the on-disk
// config cannot be read (e.g. systemd unit started without $HOME → config
// path resolves to a non-existent /.testnet/config.yaml). Without sane
// Daemon.{DataDir,WGConfig} defaults the daemon then dies with the cryptic
// "Error: mkdir : no such file or directory" because daemon.New calls
// os.MkdirAll("") on a blank data_dir.
func SetClientDefaults(cfg *ClientConfig) {
	if cfg.Daemon.Socket == "" {
		cfg.Daemon.Socket = "/var/run/testnet-client.sock"
	}
	if cfg.Daemon.DataDir == "" {
		cfg.Daemon.DataDir = "~/.testnet/data"
	}
	if cfg.Daemon.WGConfig == "" {
		cfg.Daemon.WGConfig = "~/.testnet/wg.conf"
	}
	if cfg.Sandbox.DefaultVCPU == 0 {
		cfg.Sandbox.DefaultVCPU = 1
	}
	if cfg.Sandbox.DefaultMemMB == 0 {
		cfg.Sandbox.DefaultMemMB = 512
	}
}
