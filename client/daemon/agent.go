package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/agent-testnet/agent-testnet/client/sandbox"
	"github.com/agent-testnet/agent-testnet/pkg/api"
)

// AgentInstance represents a running agent VM.
type AgentInstance struct {
	mu         sync.Mutex
	id         string
	tunnelIP   net.IP
	vm         *sandbox.VM
	network    *sandbox.NetworkConfig
	status     string
	sshKeyPath string
}

// Info returns the agent's public info.
func (a *AgentInstance) Info() api.AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return api.AgentInfo{
		ID:       a.id,
		TunnelIP: a.tunnelIP.String(),
		Status:   a.status,
		VCPU:     a.vm.VCPU(),
		MemMB:    a.vm.MemMB(),
	}
}

// LogPath returns the path to the VM's console log.
func (a *AgentInstance) LogPath() string {
	return a.vm.LogPath()
}

// SSHKeyPath returns the path to the VM's ephemeral SSH private key.
func (a *AgentInstance) SSHKeyPath() string {
	return a.sshKeyPath
}

// LaunchAgent creates and starts a new agent VM.
func (d *Daemon) LaunchAgent(cfg api.AgentConfig) (*AgentInstance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.tunnelCIDR == "" {
		return nil, fmt.Errorf("not registered")
	}

	vmIndex := int(d.nextAgentIP)
	d.nextAgentIP++
	agentID := fmt.Sprintf("agent-%d", vmIndex)

	vcpu := cfg.VCPU
	if vcpu == 0 {
		vcpu = d.cfg.Sandbox.DefaultVCPU
	}
	memMB := cfg.MemMB
	if memMB == 0 {
		memMB = d.cfg.Sandbox.DefaultMemMB
	}
	rootfs := cfg.RootFS
	if rootfs == "" {
		rootfs = expandPath(d.cfg.Sandbox.DefaultRootFS)
	}

	serverTunnelIP := "10.99.0.1"
	wgInterface := "wg-testnet"

	// Generate ephemeral SSH keypair for this VM
	agentDir := filepath.Join(d.dataDir, "agents", agentID)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return nil, fmt.Errorf("create agent dir: %w", err)
	}

	sshKeyPath := filepath.Join(agentDir, "ssh_key")
	sshPubKey, err := generateSSHKeypair(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("generate SSH keypair: %w", err)
	}
	log.Printf("[daemon] generated SSH keypair for %s at %s", agentID, sshKeyPath)

	netCfg, err := sandbox.SetupNetwork(agentID, vmIndex, serverTunnelIP, d.dnsIP, wgInterface)
	if err != nil {
		return nil, fmt.Errorf("setup network: %w", err)
	}

	vm, err := sandbox.NewVM(sandbox.VMConfig{
		ID:             agentID,
		VCPU:           vcpu,
		MemMB:          memMB,
		RootFS:         rootfs,
		KernelPath:     expandPath(d.cfg.Sandbox.KernelPath),
		FirecrackerBin: expandPath(d.cfg.Sandbox.FirecrackerBin),
		TAPDevice:      netCfg.TAPDevice,
		GuestIP:        netCfg.GuestIP,
		GatewayIP:      netCfg.GatewayIP,
		DNSIP:          serverTunnelIP,
		CACertPEM:      d.caCertPEM,
		SSHPubKey:      sshPubKey,
	})
	if err != nil {
		sandbox.TeardownNetwork(netCfg)
		return nil, fmt.Errorf("create VM: %w", err)
	}

	if err := vm.Start(); err != nil {
		sandbox.TeardownNetwork(netCfg)
		return nil, fmt.Errorf("start VM: %w", err)
	}

	tunnelIP := net.ParseIP(netCfg.GuestIP)
	agent := &AgentInstance{
		id:         agentID,
		tunnelIP:   tunnelIP,
		vm:         vm,
		network:    netCfg,
		status:     "running",
		sshKeyPath: sshKeyPath,
	}
	d.agents[agentID] = agent

	log.Printf("[daemon] launched agent %s (guest IP: %s, %d vCPU, %dMB RAM)",
		agentID, netCfg.GuestIP, vcpu, memMB)
	return agent, nil
}

// StopAgent stops a running agent VM and cleans up.
func (d *Daemon) StopAgent(id string) error {
	d.mu.Lock()
	agent, ok := d.agents[id]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("agent %s not found", id)
	}
	delete(d.agents, id)
	d.mu.Unlock()

	agent.mu.Lock()
	defer agent.mu.Unlock()

	agent.status = "stopping"

	if err := agent.vm.Stop(); err != nil {
		log.Printf("[daemon] error stopping VM %s: %v", id, err)
	}

	sandbox.TeardownNetwork(agent.network)
	agent.status = "stopped"

	log.Printf("[daemon] stopped agent %s", id)
	return nil
}

// ListAgents returns info about all running agents.
func (d *Daemon) ListAgents() []api.AgentInfo {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := make([]api.AgentInfo, 0, len(d.agents))
	for _, agent := range d.agents {
		result = append(result, agent.Info())
	}
	return result
}

// generateSSHKeypair creates an ed25519 keypair, writes the private key to
// keyPath in OpenSSH PEM format, and returns the public key in authorized_keys
// format.
func generateSSHKeypair(keyPath string) (string, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	privBytes, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}

	privPEM := pem.EncodeToMemory(privBytes)
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return "", fmt.Errorf("convert public key: %w", err)
	}

	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), nil
}

