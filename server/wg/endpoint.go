package wg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
	"golang.org/x/crypto/curve25519"
)

// Endpoint manages the WireGuard server interface and peer configurations.
type Endpoint struct {
	cfg       *config.ServerConfig
	cp        *controlplane.ControlPlane
	mu        sync.Mutex
	privKey   string
	pubKey    string
	ifaceName string
	peers     map[string]peerInfo // pubKey -> info
}

type peerInfo struct {
	PublicKey  string
	AllowedIP string
}

// NewEndpoint creates a new WireGuard endpoint manager.
func NewEndpoint(cfg *config.ServerConfig, cp *controlplane.ControlPlane) (*Endpoint, error) {
	e := &Endpoint{
		cfg:       cfg,
		cp:        cp,
		ifaceName: "wg0",
		peers:     make(map[string]peerInfo),
	}

	if err := e.loadOrGenerateKey(); err != nil {
		return nil, err
	}

	cp.SetWGPublicKey(e.pubKey)
	return e, nil
}

// PublicKey returns the server's WireGuard public key.
func (e *Endpoint) PublicKey() string {
	return e.pubKey
}

func (e *Endpoint) loadOrGenerateKey() error {
	keyFile := e.cfg.WireGuard.PrivateKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(e.cfg.ControlPlane.DataDir, "wg-key")
	}

	if data, err := os.ReadFile(keyFile); err == nil {
		e.privKey = strings.TrimSpace(string(data))
		pubKey, err := wgPubFromPriv(e.privKey)
		if err != nil {
			return fmt.Errorf("derive WG public key: %w", err)
		}
		e.pubKey = pubKey
		return nil
	}

	privKey, pubKey, err := generateWGKeyPair()
	if err != nil {
		return fmt.Errorf("generate WG keypair: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, []byte(privKey), 0o600); err != nil {
		return err
	}

	e.privKey = privKey
	e.pubKey = pubKey
	return nil
}

// Start sets up the WireGuard interface and waits for context cancellation.
func (e *Endpoint) Start(ctx context.Context) error {
	if err := e.setupInterface(); err != nil {
		return fmt.Errorf("setup WG interface: %w", err)
	}

	log.Printf("[wireguard] interface %s up, listening on UDP :%d, pubkey: %s",
		e.ifaceName, e.cfg.WireGuard.ListenPort, e.pubKey)

	<-ctx.Done()
	e.teardownInterface()
	return nil
}

func (e *Endpoint) setupInterface() error {
	tunnelIP := e.cfg.WireGuard.TunnelIP

	// Create the WireGuard interface using `ip link` + `wg` commands.
	// This requires the `wireguard-tools` package on the server host.
	cmds := [][]string{
		{"ip", "link", "add", "dev", e.ifaceName, "type", "wireguard"},
		{"ip", "address", "add", "dev", e.ifaceName, tunnelIP},
		{"ip", "link", "set", "mtu", "1420", "up", "dev", e.ifaceName},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("command %v failed: %s: %w", args, string(out), err)
		}
	}

	// Configure WireGuard with private key and listen port
	keyFile := e.cfg.WireGuard.PrivateKeyFile
	if keyFile == "" {
		keyFile = filepath.Join(e.cfg.ControlPlane.DataDir, "wg-key")
	}

	cmd := exec.Command("wg", "set", e.ifaceName,
		"listen-port", fmt.Sprintf("%d", e.cfg.WireGuard.ListenPort),
		"private-key", keyFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wg set failed: %s: %w", string(out), err)
	}

	// Add the DNS VIP as a local address so the DNS server can bind to it
	dnsVIP := e.cfg.VIP.DNSVIP
	if dnsVIP != "" {
		cmd = exec.Command("ip", "address", "add", "dev", e.ifaceName, dnsVIP+"/32")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("[wireguard] add VIP %s: %s", dnsVIP, string(out))
		}
	}

	// Add route for the VIP subnet through the WG interface
	vipSubnet := e.cfg.VIP.Subnet
	cmd = exec.Command("ip", "route", "add", vipSubnet, "dev", e.ifaceName)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[wireguard] route add %s: %s", vipSubnet, string(out))
	}

	return nil
}

func (e *Endpoint) teardownInterface() {
	cmd := exec.Command("ip", "link", "del", "dev", e.ifaceName)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[wireguard] teardown error: %s: %v", string(out), err)
	}
}

// AddPeer adds a WireGuard peer (client) with the given public key and allowed IPs.
func (e *Endpoint) AddPeer(pubKey, allowedIPs string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cmd := exec.Command("wg", "set", e.ifaceName,
		"peer", pubKey,
		"allowed-ips", allowedIPs)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wg set peer failed: %s: %w", string(out), err)
	}

	e.peers[pubKey] = peerInfo{
		PublicKey:  pubKey,
		AllowedIP: allowedIPs,
	}
	log.Printf("[wireguard] added peer %s (allowed: %s)", pubKey[:16]+"...", allowedIPs)

	go e.checkPeerHandshake(pubKey, 30*time.Second)
	return nil
}

// checkPeerHandshake warns if a peer never completes a WireGuard handshake,
// which typically indicates that UDP is blocked by the provider's firewall.
func (e *Endpoint) checkPeerHandshake(pubKey string, timeout time.Duration) {
	time.Sleep(timeout)

	cmd := exec.Command("wg", "show", e.ifaceName, "latest-handshakes")
	out, err := cmd.Output()
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == pubKey {
			if fields[1] == "0" {
				log.Printf("[wireguard] WARNING: peer %s... has not completed a handshake after %s",
					pubKey[:16], timeout)
				log.Printf("[wireguard] This usually means UDP port %d is blocked by the hosting provider's firewall",
					e.cfg.WireGuard.ListenPort)
				log.Printf("[wireguard] Check your provider's dashboard for a platform-level firewall and open UDP %d",
					e.cfg.WireGuard.ListenPort)
			}
			return
		}
	}
}

// RemovePeer removes a WireGuard peer.
func (e *Endpoint) RemovePeer(pubKey string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cmd := exec.Command("wg", "set", e.ifaceName,
		"peer", pubKey, "remove")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wg remove peer failed: %s: %w", string(out), err)
	}

	delete(e.peers, pubKey)
	return nil
}

// generateWGKeyPair produces a WireGuard keypair (base64-encoded).
func generateWGKeyPair() (privKey, pubKey string, err error) {
	var privBytes [32]byte
	if _, err := rand.Read(privBytes[:]); err != nil {
		return "", "", err
	}
	// Clamp the private key per Curve25519
	privBytes[0] &= 248
	privBytes[31] &= 127
	privBytes[31] |= 64

	privKey = base64.StdEncoding.EncodeToString(privBytes[:])

	// Derive public key — use the `wg` tool if available, otherwise use
	// a pure Go implementation
	pubKey, err = wgPubFromPriv(privKey)
	if err != nil {
		return "", "", err
	}
	return privKey, pubKey, nil
}

// wgPubFromPriv derives a WireGuard public key from a private key.
// Uses golang.org/x/crypto/curve25519 directly — no external tool needed.
func wgPubFromPriv(privKeyBase64 string) (string, error) {
	privBytes, err := base64.StdEncoding.DecodeString(privKeyBase64)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	if len(privBytes) != 32 {
		return "", fmt.Errorf("invalid private key length: %d", len(privBytes))
	}

	pubBytes, err := curve25519.X25519(privBytes, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("curve25519 scalar mult: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pubBytes), nil
}

// GenerateClientKeyPair is a helper to generate a WireGuard keypair for clients.
func GenerateClientKeyPair() (privKey, pubKey string, err error) {
	return generateWGKeyPair()
}

// ParseCIDR extracts the base IP and netmask from a CIDR string.
func ParseCIDR(cidr string) (net.IP, *net.IPNet, error) {
	return net.ParseCIDR(cidr)
}

// PrivateKeyToHex converts a base64 WireGuard key to hex.
func PrivateKeyToHex(b64key string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64key)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
