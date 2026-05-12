package router

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
)

const (
	testnetChain = "TESTNET-FWD"
	natChain     = "TESTNET-NAT"
	wgIface      = "wg0"
)

// Router manages kernel IP forwarding and iptables rules for DNAT/MASQUERADE.
type Router struct {
	cfg   *config.ServerConfig
	cp    *controlplane.ControlPlane
	mu    sync.Mutex
	rules []dnatRule
}

type dnatRule struct {
	VIP     string
	RealIP  string
}

// New creates a new router.
func New(cfg *config.ServerConfig, cp *controlplane.ControlPlane) (*Router, error) {
	return &Router{
		cfg: cfg,
		cp:  cp,
	}, nil
}

// Start enables IP forwarding and sets up iptables rules.
func (r *Router) Start(ctx context.Context) error {
	if err := r.enableIPForward(); err != nil {
		return err
	}
	if err := r.setupChains(); err != nil {
		return err
	}
	if err := r.syncRules(); err != nil {
		return err
	}

	// Register for node change notifications
	r.cp.Nodes().OnChange(func() {
		if err := r.syncRules(); err != nil {
			log.Printf("[router] failed to sync rules after node change: %v", err)
		}
	})

	log.Println("[router] IP forwarding enabled, iptables rules installed")

	go NewLogger(r.cfg, r.cp).Start(ctx)

	<-ctx.Done()
	return nil
}

// Cleanup removes all iptables rules and custom chains.
func (r *Router) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	log.Println("[router] cleaning up iptables rules...")

	// Remove jump rules
	iptables("-D", "FORWARD", "-j", testnetChain)
	iptables("-t", "nat", "-D", "PREROUTING", "-j", natChain)
	iptables("-t", "nat", "-D", "POSTROUTING", "-o", getDefaultIface(), "-j", "MASQUERADE")

	// Flush and delete custom chains
	iptables("-F", testnetChain)
	iptables("-X", testnetChain)
	iptables("-t", "nat", "-F", natChain)
	iptables("-t", "nat", "-X", natChain)
}

func (r *Router) enableIPForward() error {
	cmd := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("enable ip_forward: %s: %w", string(out), err)
	}
	return nil
}

func (r *Router) setupChains() error {
	// Create custom chains (ignore errors if they already exist)
	iptables("-N", testnetChain)
	iptables("-t", "nat", "-N", natChain)

	// Set FORWARD chain default policy to DROP
	if err := iptables("-P", "FORWARD", "DROP"); err != nil {
		return fmt.Errorf("set FORWARD DROP policy: %w", err)
	}

	// Jump from FORWARD to our custom chain
	iptables("-D", "FORWARD", "-j", testnetChain) // remove if exists
	if err := iptables("-I", "FORWARD", "1", "-j", testnetChain); err != nil {
		return fmt.Errorf("insert FORWARD jump: %w", err)
	}

	// Allow established/related connections
	if err := iptables("-A", testnetChain, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("add conntrack rule: %w", err)
	}

	// Jump from nat PREROUTING to our NAT chain
	iptables("-t", "nat", "-D", "PREROUTING", "-j", natChain) // remove if exists
	if err := iptables("-t", "nat", "-I", "PREROUTING", "1", "-j", natChain); err != nil {
		return fmt.Errorf("insert nat PREROUTING jump: %w", err)
	}

	// MASQUERADE outbound traffic
	defaultIface := getDefaultIface()
	iptables("-t", "nat", "-D", "POSTROUTING", "-o", defaultIface, "-j", "MASQUERADE")
	if err := iptables("-t", "nat", "-A", "POSTROUTING", "-o", defaultIface, "-j", "MASQUERADE"); err != nil {
		return fmt.Errorf("add MASQUERADE rule: %w", err)
	}

	return nil
}

// syncRules reads node data and updates DNAT + FORWARD rules.
func (r *Router) syncRules() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Flush existing rules in our chains (keep the chain structure)
	iptables("-F", testnetChain)
	iptables("-t", "nat", "-F", natChain)

	// Re-add conntrack rule
	iptables("-A", testnetChain, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")

	nodes := r.cp.Nodes().ListNodes()
	r.rules = make([]dnatRule, 0, len(nodes))

	for _, node := range nodes {
		vip := node.VIP.String()
		addr := node.Address
		realHost := strings.Split(addr, ":")[0]

		// DNAT rules need a protocol when a port is specified
		for _, proto := range []string{"tcp", "udp"} {
			if err := iptables("-t", "nat", "-A", natChain,
				"-d", vip, "-p", proto, "-j", "DNAT", "--to-destination", addr); err != nil {
				log.Printf("[router] failed to add %s DNAT rule %s -> %s: %v", proto, vip, addr, err)
			}
		}

		// FORWARD: allow traffic from wg0 to the real host
		if err := iptables("-A", testnetChain,
			"-i", wgIface, "-d", realHost, "-j", "ACCEPT"); err != nil {
			log.Printf("[router] failed to add FORWARD rule for %s: %v", realHost, err)
		}

		r.rules = append(r.rules, dnatRule{VIP: vip, RealIP: addr})
	}

	log.Printf("[router] synced %d DNAT rules", len(r.rules))
	return nil
}

// AddRoute adds a DNAT rule for a single VIP -> real IP.
func (r *Router) AddRoute(vip net.IP, nodeAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	vipStr := vip.String()
	realHost := strings.Split(nodeAddr, ":")[0]
	for _, proto := range []string{"tcp", "udp"} {
		if err := iptables("-t", "nat", "-A", natChain,
			"-d", vipStr, "-p", proto, "-j", "DNAT", "--to-destination", nodeAddr); err != nil {
			return err
		}
	}
	if err := iptables("-A", testnetChain,
		"-i", wgIface, "-d", realHost, "-j", "ACCEPT"); err != nil {
		return err
	}
	r.rules = append(r.rules, dnatRule{VIP: vipStr, RealIP: nodeAddr})
	return nil
}

// RemoveRoute removes a DNAT rule for a VIP.
func (r *Router) RemoveRoute(vip net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	vipStr := vip.String()
	for i, rule := range r.rules {
		if rule.VIP == vipStr {
			realHost := strings.Split(rule.RealIP, ":")[0]
			for _, proto := range []string{"tcp", "udp"} {
				iptables("-t", "nat", "-D", natChain,
					"-d", vipStr, "-p", proto, "-j", "DNAT", "--to-destination", rule.RealIP)
			}
			iptables("-D", testnetChain,
				"-i", wgIface, "-d", realHost, "-j", "ACCEPT")
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no rule for VIP %s", vipStr)
}

func iptables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if outStr != "" {
			return fmt.Errorf("iptables %v: %s: %w", args, outStr, err)
		}
		return fmt.Errorf("iptables %v: %w", args, err)
	}
	return nil
}

func getDefaultIface() string {
	cmd := exec.Command("ip", "route", "show", "default")
	out, err := cmd.Output()
	if err != nil {
		return "eth0"
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "eth0"
}
