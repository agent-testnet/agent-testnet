package api

import "net"

// Node represents a testnet node from nodes.yaml.
type Node struct {
	Name    string   `json:"name" yaml:"name"`
	Address string   `json:"address" yaml:"address"`
	Secret  string   `json:"-" yaml:"secret"`
	Domains []string `json:"domains,omitempty" yaml:"domains,omitempty"`
	VIP     net.IP   `json:"vip,omitempty" yaml:"-"`
}

// NodesConfig is the top-level structure of nodes.yaml.
type NodesConfig struct {
	Nodes []Node `yaml:"nodes"`
}

// DomainMapping maps a domain or auto-name to a VIP.
type DomainMapping struct {
	Domain string `json:"domain"`
	VIP    string `json:"vip"`
	Node   string `json:"node"`
}

// Client represents a registered testnet client.
type Client struct {
	ID          string `json:"id"`
	WGPublicKey string `json:"wg_public_key"`
	TunnelCIDR  string `json:"tunnel_cidr"`
	APIToken    string `json:"-"`
	CreatedAt   string `json:"created_at"`
}

// ClientPersist is the serialization format for client state on disk.
type ClientPersist struct {
	ID           string `json:"id"`
	WGPublicKey  string `json:"wg_public_key"`
	TunnelCIDR   string `json:"tunnel_cidr"`
	APITokenHash string `json:"api_token_hash"`
	CreatedAt    string `json:"created_at"`
}

// RegisterRequest is sent by clients to register with the server.
type RegisterRequest struct {
	WGPublicKey string `json:"wg_public_key"`
}

// RegisterResponse is returned after successful client registration.
type RegisterResponse struct {
	ClientID     string `json:"client_id"`
	APIToken     string `json:"api_token"`
	TunnelCIDR   string `json:"tunnel_cidr"`
	ServerWGKey  string `json:"server_wg_public_key"`
	ServerWGAddr string `json:"server_wg_addr"`
	DNSIP        string `json:"dns_ip"`
	CACert       string `json:"ca_cert"`
}

// CertResponse is returned when a node fetches its TLS certs.
type CertResponse struct {
	CertPEM string `json:"cert_pem"`
	KeyPEM  string `json:"key_pem"`
	CAPEM   string `json:"ca_pem"`
}

// NodeInfo is the public information about a node (returned by list API).
type NodeInfo struct {
	Name    string   `json:"name"`
	VIP     string   `json:"vip"`
	Domains []string `json:"domains,omitempty"`
}

// AgentInfo describes a running agent VM.
type AgentInfo struct {
	ID         string `json:"id"`
	TunnelIP   string `json:"tunnel_ip"`
	Status     string `json:"status"`
	VCPU       int    `json:"vcpu"`
	MemMB      int    `json:"mem_mb"`
	SSHKeyPath string `json:"ssh_key_path,omitempty"`
}

// AgentConfig specifies how to launch an agent VM.
type AgentConfig struct {
	RootFS string `json:"rootfs,omitempty"`
	VCPU   int    `json:"vcpu,omitempty"`
	MemMB  int    `json:"mem_mb,omitempty"`
}

// Proxy visibility levels for client-side per-VM passthrough proxies.
//
// Public proxies are aliased on a host-side TAP device using an address
// in the testnet's reserved client passthrough subnet (default
// 83.150.255.0/24). They look like a public IP to the agent — useful for
// agents whose URL fetch SSRF guard rejects RFC1918 / "special-use" IPs
// (e.g. OpenClaw blocking https://openrouter.ai when it resolves to
// 172.16.x.y).
//
// Private proxies use the agent's per-VM /24 (172.16.<vmIndex>.0/24) and
// are sufficient for plumbing whose consumers don't care about the source
// IP (apk, npm, git, etc.).
const (
	ProxyVisibilityPublic  = "public"
	ProxyVisibilityPrivate = "private"
)

// ProxyConfig is the request payload for `agent proxy add`.
type ProxyConfig struct {
	AgentID    string `json:"agent_id"`
	Domain     string `json:"domain"`
	Visibility string `json:"visibility,omitempty"` // ProxyVisibilityPublic (default) or ProxyVisibilityPrivate
	Ports      []int  `json:"ports,omitempty"`      // defaults to [443]
	Upstream   string `json:"upstream,omitempty"`   // defaults to <domain>:<first port>
}

// ProxyInfo describes a running passthrough proxy for an agent VM.
type ProxyInfo struct {
	AgentID    string `json:"agent_id"`
	Domain     string `json:"domain"`
	Visibility string `json:"visibility"`
	IP         string `json:"ip"`
	Ports      []int  `json:"ports"`
	Upstream   string `json:"upstream"`
}

// ProxyRef identifies a proxy for remove / lookup operations.
type ProxyRef struct {
	AgentID string `json:"agent_id"`
	Domain  string `json:"domain"`
}

// DaemonRequest is used for CLI-to-daemon communication over the Unix socket.
type DaemonRequest struct {
	Command string      `json:"command"`
	Payload interface{} `json:"payload,omitempty"`
}

// DaemonResponse is the response from the daemon.
type DaemonResponse struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Payload interface{} `json:"payload,omitempty"`
}
