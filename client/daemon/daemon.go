package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
)

// Daemon is the testnet-client background service.
type Daemon struct {
	cfg        *config.ClientConfig
	dataDir    string
	mu         sync.Mutex
	agents     map[string]*AgentInstance
	nextAgentIP byte
	tunnelCIDR string
	dnsIP      string
	caCertPEM  []byte
	apiToken   string
	serverURL  string
	cancel     context.CancelFunc // set when Run starts
}

// StateFile is the on-disk format for daemon registration state.
type StateFile struct {
	ServerURL  string `json:"server_url"`
	APIToken   string `json:"api_token"`
	ClientID   string `json:"client_id"`
	TunnelCIDR string `json:"tunnel_cidr"`
	DNSIP      string `json:"dns_ip"`
	WGPrivKey  string `json:"wg_priv_key"`
	WGPubKey   string `json:"wg_pub_key"`
	ServerWGKey string `json:"server_wg_key"`
}

// New creates a new daemon instance.
func New(cfg *config.ClientConfig) (*Daemon, error) {
	dataDir := expandPath(cfg.Daemon.DataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}

	d := &Daemon{
		cfg:         cfg,
		dataDir:     dataDir,
		agents:      make(map[string]*AgentInstance),
		nextAgentIP: 10, // .10 is the first agent IP in the /24
	}

	if err := d.loadState(); err != nil {
		log.Printf("[daemon] no saved state (run 'setup' first): %v", err)
	}

	return d, nil
}

// Run starts the daemon, sets up WireGuard, and listens on the Unix socket.
func (d *Daemon) Run(ctx context.Context) error {
	if d.tunnelCIDR == "" {
		return fmt.Errorf("not registered — run 'testnet-client setup' first")
	}

	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel

	if err := d.setupWireGuard(); err != nil {
		cancel()
		return fmt.Errorf("setup wireguard: %w", err)
	}

	socketPath := d.cfg.Daemon.Socket
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		cancel()
		return err
	}
	os.Remove(socketPath) // clean up stale socket

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		cancel()
		return fmt.Errorf("listen on socket %s: %w", socketPath, err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	// Restrict socket to owner only
	if err := os.Chmod(socketPath, 0o600); err != nil {
		log.Printf("[daemon] warning: could not restrict socket permissions: %v", err)
	}

	log.Printf("[daemon] listening on %s", socketPath)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[daemon] accept error: %v", err)
			continue
		}
		go d.handleConnection(conn)
	}
}

func (d *Daemon) handleConnection(conn net.Conn) {
	defer conn.Close()

	var req api.DaemonRequest
	limited := io.LimitReader(conn, 1024*1024) // 1 MB max request
	if err := json.NewDecoder(limited).Decode(&req); err != nil {
		writeResponse(conn, false, "invalid request", nil)
		return
	}

	switch req.Command {
	case "agent-launch":
		d.handleAgentLaunch(conn, req)
	case "agent-stop":
		d.handleAgentStop(conn, req)
	case "agent-list":
		d.handleAgentList(conn)
	case "status":
		d.handleStatus(conn)
	case "shutdown":
		d.handleShutdown(conn)
	default:
		writeResponse(conn, false, fmt.Sprintf("unknown command: %s", req.Command), nil)
	}
}

func (d *Daemon) handleAgentLaunch(conn net.Conn, req api.DaemonRequest) {
	var cfg api.AgentConfig
	if req.Payload != nil {
		data, err := json.Marshal(req.Payload)
		if err != nil {
			writeResponse(conn, false, fmt.Sprintf("marshal payload: %v", err), nil)
			return
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			writeResponse(conn, false, fmt.Sprintf("invalid agent config: %v", err), nil)
			return
		}
	}

	agent, err := d.LaunchAgent(cfg)
	if err != nil {
		writeResponse(conn, false, err.Error(), nil)
		return
	}
	writeResponse(conn, true, "", agent.Info())
}

func (d *Daemon) handleAgentStop(conn net.Conn, req api.DaemonRequest) {
	var payload struct {
		ID string `json:"id"`
	}
	if req.Payload != nil {
		data, err := json.Marshal(req.Payload)
		if err != nil {
			writeResponse(conn, false, fmt.Sprintf("marshal payload: %v", err), nil)
			return
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			writeResponse(conn, false, fmt.Sprintf("invalid payload: %v", err), nil)
			return
		}
	}

	if err := d.StopAgent(payload.ID); err != nil {
		writeResponse(conn, false, err.Error(), nil)
		return
	}
	writeResponse(conn, true, "", nil)
}

func (d *Daemon) handleAgentList(conn net.Conn) {
	agents := d.ListAgents()
	writeResponse(conn, true, "", agents)
}

func (d *Daemon) handleShutdown(conn net.Conn) {
	log.Printf("[daemon] shutdown requested via socket")
	writeResponse(conn, true, "", nil)
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *Daemon) handleStatus(conn net.Conn) {
	status := map[string]interface{}{
		"registered":  d.tunnelCIDR != "",
		"tunnel_cidr": d.tunnelCIDR,
		"agents":      len(d.agents),
	}
	writeResponse(conn, true, "", status)
}

func (d *Daemon) setupWireGuard() error {
	state, err := d.loadStateFile()
	if err != nil {
		return err
	}

	wgConfPath := expandPath(d.cfg.Daemon.WGConfig)
	if err := os.MkdirAll(filepath.Dir(wgConfPath), 0o700); err != nil {
		return err
	}

	// Parse the tunnel CIDR to get the client's host IP (.1 in the /24)
	_, ipNet, err := net.ParseCIDR(state.TunnelCIDR)
	if err != nil {
		return fmt.Errorf("parse tunnel CIDR: %w", err)
	}
	clientIP := make(net.IP, len(ipNet.IP))
	copy(clientIP, ipNet.IP)
	clientIP[3] = 1 // .1 is the client host

	// Write WireGuard config
	wgConf := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/24

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 10.99.0.0/16, 83.150.0.0/16
PersistentKeepalive = 25
`, state.WGPrivKey, clientIP.String(), state.ServerWGKey, d.serverEndpoint())

	if err := os.WriteFile(wgConfPath, []byte(wgConf), 0o600); err != nil {
		return err
	}

	// Bring up WireGuard using wg-quick
	cmd := exec.Command("wg-quick", "up", wgConfPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wg-quick up: %s: %w", string(out), err)
	}

	log.Printf("[daemon] WireGuard tunnel up (client IP: %s)", clientIP)
	return nil
}

func (d *Daemon) serverEndpoint() string {
	// Extract host from server URL and use WG port
	url := d.serverURL
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	host := strings.Split(url, ":")[0]
	return host + ":51820"
}

func (d *Daemon) loadState() error {
	state, err := d.loadStateFile()
	if err != nil {
		return err
	}

	d.serverURL = state.ServerURL
	d.apiToken = state.APIToken
	d.tunnelCIDR = state.TunnelCIDR
	d.dnsIP = state.DNSIP

	caCertPath := filepath.Join(d.dataDir, "ca-cert.pem")
	d.caCertPEM, _ = os.ReadFile(caCertPath)

	return nil
}

func (d *Daemon) loadStateFile() (*StateFile, error) {
	data, err := os.ReadFile(filepath.Join(d.dataDir, "state.json"))
	if err != nil {
		return nil, err
	}
	var state StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveRegistration persists registration data from setup.
func (d *Daemon) SaveRegistration(state *StateFile, caCertPEM []byte) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	stateDir := d.dataDir
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0o600); err != nil {
		return err
	}

	if caCertPEM != nil {
		if err := os.WriteFile(filepath.Join(stateDir, "ca-cert.pem"), caCertPEM, 0o644); err != nil {
			return err
		}
	}

	d.serverURL = state.ServerURL
	d.apiToken = state.APIToken
	d.tunnelCIDR = state.TunnelCIDR
	d.dnsIP = state.DNSIP
	d.caCertPEM = caCertPEM

	return nil
}

// CACertPEM returns the CA cert.
func (d *Daemon) CACertPEM() []byte {
	return d.caCertPEM
}

// DNSIP returns the testnet DNS IP.
func (d *Daemon) DNSIP() string {
	return d.dnsIP
}

// TunnelCIDR returns the client's tunnel CIDR.
func (d *Daemon) TunnelCIDR() string {
	return d.tunnelCIDR
}

func writeResponse(conn net.Conn, ok bool, errMsg string, payload interface{}) {
	resp := api.DaemonResponse{
		OK:      ok,
		Error:   errMsg,
		Payload: payload,
	}
	json.NewEncoder(conn).Encode(resp)
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
