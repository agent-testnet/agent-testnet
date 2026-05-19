package router

import (
	"context"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
	"github.com/agent-testnet/agent-testnet/server/observability"
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
	obs   *observability.EventLogger
	mu    sync.Mutex
	rules []dnatRule
}

type dnatRule struct {
	VIP    string
	RealIP string
}

// New creates a new router.
func New(cfg *config.ServerConfig, cp *controlplane.ControlPlane, obs *observability.EventLogger) (*Router, error) {
	return &Router{
		cfg: cfg,
		cp:  cp,
		obs: obs,
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

	go NewLogger(r.cfg, r.cp, r.obs).Start(ctx)

	if r.obs != nil && r.cfg.Observability.LogDropsEnabled() {
		go r.obs.StartDropTailer(ctx)
	}

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
//
// Per-VIP NAT layout (in order, since iptables processes rules top-to-bottom):
//  1. tcp/443 -> REDIRECT to the HTTPS MITM proxy on the local host so
//     the proxy can terminate TLS and log HTTP request metadata.
//  2. tcp/80  -> REDIRECT to the plain HTTP proxy for the same reason.
//  3. tcp/any -> DNAT to the node's real address (preserves the previous
//     "everything goes to the node's HTTPS port" behaviour for non-web
//     protocols like SMTP, gRPC on custom ports, etc.).
//  4. udp/any -> DNAT to the node's real address.
//
// FORWARD layout:
//   - Allow ESTABLISHED/RELATED.
//   - Allow per-node wg0 -> real_host for the DNAT'd flows.
//   - Rate-limited LOG with prefix observability.DropLogPrefix so the
//     drop tailer can emit event=drop rows.
//   - Explicit DROP (default policy already drops, but the explicit DROP
//     makes the LOG-then-DROP intent obvious in `iptables -L`).
func (r *Router) syncRules() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Flush existing rules in our chains (keep the chain structure)
	iptables("-F", testnetChain)
	iptables("-t", "nat", "-F", natChain)

	// Re-add conntrack rule
	iptables("-A", testnetChain, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")

	httpsPort := proxyPort(r.cfg.Proxy.HTTPSListen)
	httpPort := proxyPort(r.cfg.Proxy.HTTPListen)

	nodes := r.cp.Nodes().ListNodes()
	r.rules = make([]dnatRule, 0, len(nodes))

	for _, node := range nodes {
		installNodeRules(node.VIP.String(), node.Address, httpsPort, httpPort)
		r.rules = append(r.rules, dnatRule{VIP: node.VIP.String(), RealIP: node.Address})
	}

	// Append the LOG-then-DROP terminator so anything from wg0 that
	// didn't match an ACCEPT rule above is recorded (rate-limited to
	// protect dmesg) and then explicitly dropped.
	if r.cfg.Observability.LogDropsEnabled() {
		if err := iptables("-A", testnetChain,
			"-i", wgIface,
			"-m", "limit", "--limit", "30/min", "--limit-burst", "10",
			"-j", "LOG", "--log-prefix", observability.DropLogPrefix+" ", "--log-level", "4"); err != nil {
			log.Printf("[router] failed to add LOG drop rule: %v", err)
		}
	}
	if err := iptables("-A", testnetChain, "-i", wgIface, "-j", "DROP"); err != nil {
		log.Printf("[router] failed to add explicit DROP rule: %v", err)
	}

	log.Printf("[router] synced %d NAT rule sets (HTTPS proxy :%s, HTTP proxy :%s)", len(r.rules), httpsPort, httpPort)
	return nil
}

// proxyPort returns the numeric port string from a listen address like
// ":18443" or "127.0.0.1:18443". Returns "" if the address can't be
// parsed; the router then skips the corresponding REDIRECT (the proxy is
// effectively disabled for that protocol).
func proxyPort(listen string) string {
	if listen == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if _, err := strconv.Atoi(port); err != nil {
		return ""
	}
	return port
}

// AddRoute installs the NAT + FORWARD rules for a single VIP -> real
// node address. Mirrors what syncRules does per node so the two stay in
// sync for any callers that mutate the topology incrementally.
func (r *Router) AddRoute(vip net.IP, nodeAddr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	vipStr := vip.String()
	installNodeRules(vipStr,
		nodeAddr,
		proxyPort(r.cfg.Proxy.HTTPSListen),
		proxyPort(r.cfg.Proxy.HTTPListen),
	)
	r.rules = append(r.rules, dnatRule{VIP: vipStr, RealIP: nodeAddr})
	return nil
}

// RemoveRoute removes the rules previously installed by AddRoute / syncRules.
func (r *Router) RemoveRoute(vip net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	vipStr := vip.String()
	httpsPort := proxyPort(r.cfg.Proxy.HTTPSListen)
	httpPort := proxyPort(r.cfg.Proxy.HTTPListen)
	for i, rule := range r.rules {
		if rule.VIP == vipStr {
			removeNodeRules(vipStr, rule.RealIP, httpsPort, httpPort)
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no rule for VIP %s", vipStr)
}

// installNodeRules appends the per-VIP NAT and FORWARD rules described
// in syncRules' comment block. Errors are logged but not returned; one
// bad rule shouldn't abort the whole sync.
func installNodeRules(vip, addr, httpsPort, httpPort string) {
	realHost := strings.Split(addr, ":")[0]

	if httpsPort != "" {
		if err := iptables("-t", "nat", "-A", natChain,
			"-d", vip, "-p", "tcp", "--dport", "443",
			"-j", "REDIRECT", "--to-ports", httpsPort); err != nil {
			log.Printf("[router] failed to add HTTPS REDIRECT rule %s:443 -> :%s: %v", vip, httpsPort, err)
		}
	}
	if httpPort != "" {
		if err := iptables("-t", "nat", "-A", natChain,
			"-d", vip, "-p", "tcp", "--dport", "80",
			"-j", "REDIRECT", "--to-ports", httpPort); err != nil {
			log.Printf("[router] failed to add HTTP REDIRECT rule %s:80 -> :%s: %v", vip, httpPort, err)
		}
	}

	for _, proto := range []string{"tcp", "udp"} {
		if err := iptables("-t", "nat", "-A", natChain,
			"-d", vip, "-p", proto, "-j", "DNAT", "--to-destination", addr); err != nil {
			log.Printf("[router] failed to add %s DNAT rule %s -> %s: %v", proto, vip, addr, err)
		}
	}

	if err := iptables("-A", testnetChain,
		"-i", wgIface, "-d", realHost, "-j", "ACCEPT"); err != nil {
		log.Printf("[router] failed to add FORWARD rule for %s: %v", realHost, err)
	}
}

// removeNodeRules is the inverse of installNodeRules.
func removeNodeRules(vip, addr, httpsPort, httpPort string) {
	realHost := strings.Split(addr, ":")[0]

	if httpsPort != "" {
		iptables("-t", "nat", "-D", natChain,
			"-d", vip, "-p", "tcp", "--dport", "443",
			"-j", "REDIRECT", "--to-ports", httpsPort)
	}
	if httpPort != "" {
		iptables("-t", "nat", "-D", natChain,
			"-d", vip, "-p", "tcp", "--dport", "80",
			"-j", "REDIRECT", "--to-ports", httpPort)
	}
	for _, proto := range []string{"tcp", "udp"} {
		iptables("-t", "nat", "-D", natChain,
			"-d", vip, "-p", proto, "-j", "DNAT", "--to-destination", addr)
	}
	iptables("-D", testnetChain,
		"-i", wgIface, "-d", realHost, "-j", "ACCEPT")
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
