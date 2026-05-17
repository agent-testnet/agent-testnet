package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/agent-testnet/agent-testnet/client/sandbox"
	"github.com/agent-testnet/agent-testnet/pkg/api"
)

// proxyStateFileName is the per-agent record of currently-running proxies,
// written under <dataDir>/agents/<agentID>/. Persistence is for inspection
// only; the daemon does NOT restore proxies on startup because agent VMs are
// child processes and don't survive the daemon dying.
const proxyStateFileName = "proxies.json"

// proxyState is what we write to disk under the per-agent dir.
type proxyState struct {
	Proxies []api.ProxyInfo `json:"proxies"`
}

// AddProxy creates a new per-VM passthrough proxy for the given agent. If a
// proxy already exists for the same (agentID, domain) the existing one is
// returned unchanged (idempotent re-add).
func (d *Daemon) AddProxy(req api.ProxyConfig) (*api.ProxyInfo, error) {
	d.mu.Lock()
	agent, ok := d.agents[req.AgentID]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("agent %s not found (is it running?)", req.AgentID)
	}

	visibility := req.Visibility
	if visibility == "" {
		visibility = api.ProxyVisibilityPublic
	}
	if visibility != api.ProxyVisibilityPublic && visibility != api.ProxyVisibilityPrivate {
		return nil, fmt.Errorf("invalid visibility %q (use %q or %q)",
			visibility, api.ProxyVisibilityPublic, api.ProxyVisibilityPrivate)
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = []int{443}
	}
	upstream := req.Upstream
	if upstream == "" {
		upstream = fmt.Sprintf("%s:%d", req.Domain, ports[0])
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()

	if existing, ok := agent.proxies[req.Domain]; ok {
		info := existing.Info()
		return &info, nil
	}

	ip, err := d.proxyAlloc.Allocate(visibility, agent.vmIndex)
	if err != nil {
		return nil, fmt.Errorf("allocate %s IP: %w", visibility, err)
	}

	spec := sandbox.ProxySpec{
		AgentID:    req.AgentID,
		Domain:     req.Domain,
		Visibility: visibility,
		IP:         ip,
		Ports:      ports,
		Upstream:   upstream,
		TAPDevice:  agent.network.TAPDevice,
		VMGuestIP:  agent.network.GuestIP,
		VMSSHKey:   agent.sshKeyPath,
	}

	p, err := sandbox.StartProxy(context.Background(), spec)
	if err != nil {
		d.proxyAlloc.Release(ip)
		return nil, fmt.Errorf("start proxy: %w", err)
	}

	agent.proxies[req.Domain] = p
	if err := d.persistProxiesLocked(req.AgentID, agent); err != nil {
		log.Printf("[daemon] warning: persisting proxies for %s: %v", req.AgentID, err)
	}

	info := p.Info()
	return &info, nil
}

// RemoveProxy stops and removes a previously-added passthrough proxy. Removing
// a non-existent proxy is a no-op (idempotent).
func (d *Daemon) RemoveProxy(ref api.ProxyRef) error {
	d.mu.Lock()
	agent, ok := d.agents[ref.AgentID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent %s not found", ref.AgentID)
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()

	p, ok := agent.proxies[ref.Domain]
	if !ok {
		return nil
	}
	p.Stop()
	d.proxyAlloc.Release(p.Spec().IP)
	delete(agent.proxies, ref.Domain)

	if err := d.persistProxiesLocked(ref.AgentID, agent); err != nil {
		log.Printf("[daemon] warning: persisting proxies for %s: %v", ref.AgentID, err)
	}
	return nil
}

// ListProxies returns the running proxies for an agent.
func (d *Daemon) ListProxies(agentID string) ([]api.ProxyInfo, error) {
	d.mu.Lock()
	agent, ok := d.agents[agentID]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentID)
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()

	out := make([]api.ProxyInfo, 0, len(agent.proxies))
	for _, p := range agent.proxies {
		out = append(out, p.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out, nil
}

// persistProxiesLocked writes the current proxy set for an agent to disk.
// Caller must hold agent.mu.
func (d *Daemon) persistProxiesLocked(agentID string, agent *AgentInstance) error {
	dir := filepath.Join(d.dataDir, "agents", agentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	infos := make([]api.ProxyInfo, 0, len(agent.proxies))
	for _, p := range agent.proxies {
		infos = append(infos, p.Info())
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Domain < infos[j].Domain })
	data, err := json.MarshalIndent(proxyState{Proxies: infos}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, proxyStateFileName), data, 0o600)
}
