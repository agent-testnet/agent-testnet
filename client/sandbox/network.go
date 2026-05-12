package sandbox

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// NetworkConfig holds the network setup state for a single agent VM.
type NetworkConfig struct {
	AgentID      string
	TAPDevice    string
	GuestIP      string
	GatewayIP    string
	DNSIP        string
	ServerTunIP  string
	WGInterface  string
}

// SetupNetwork creates a TAP device and iptables rules for an agent VM.
//
// Each VM gets its own /24 subnet on a private link (172.16.X.0/24) to avoid
// colliding with the WireGuard tunnel subnet. Traffic is MASQUERADE'd into the
// WG interface so return packets route correctly. DNS queries to port 53 are
// DNAT'd to the server's tunnel-side DNS on port 5353.
func SetupNetwork(agentID string, vmIndex int, serverTunnelIP, dnsIP, wgInterface string) (*NetworkConfig, error) {
	tapName := tapDeviceName(agentID)

	// Each VM gets 172.16.<vmIndex>.0/24; guest at .2, host gateway at .1
	if vmIndex < 1 || vmIndex > 254 {
		return nil, fmt.Errorf("vmIndex %d out of range [1,254]", vmIndex)
	}
	guestIP := fmt.Sprintf("172.16.%d.2", vmIndex)
	gatewayIP := fmt.Sprintf("172.16.%d.1", vmIndex)
	subnet := fmt.Sprintf("172.16.%d.0/24", vmIndex)

	// Create TAP device on the per-VM subnet
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tapName, "mode", "tap"},
		{"ip", "addr", "add", gatewayIP + "/24", "dev", tapName},
		{"ip", "link", "set", tapName, "up"},
	}
	for _, args := range cmds {
		if err := runCmd(args...); err != nil {
			cleanupTAP(tapName)
			return nil, fmt.Errorf("TAP setup (%v): %w", args, err)
		}
	}

	// Enable IP forwarding
	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		log.Printf("[net] warning: failed to enable ip_forward: %v", err)
	}

	rules := [][]string{
		// Allow VM -> VIP subnet (testnet services)
		{"-A", "FORWARD", "-i", tapName, "-s", guestIP + "/32",
			"-d", "83.150.0.0/16", "-j", "ACCEPT"},
		// Allow VM -> server tunnel IP (for DNS)
		{"-A", "FORWARD", "-i", tapName, "-s", guestIP + "/32",
			"-d", serverTunnelIP + "/32", "-j", "ACCEPT"},
		// Allow established/related return traffic
		{"-A", "FORWARD", "-o", tapName, "-m", "conntrack",
			"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		// DROP everything else from/to this TAP
		{"-A", "FORWARD", "-i", tapName, "-j", "DROP"},
		{"-A", "FORWARD", "-o", tapName, "-j", "DROP"},

		// Block VM from reaching host services (SSH, etc.) via the gateway IP.
		// FORWARD rules only cover routed traffic; packets addressed directly
		// to a TAP-interface IP hit the INPUT chain instead. Without this,
		// the VM can probe any port the host listens on.
		// Allow return traffic from host-initiated connections (e.g. SSH to VM).
		{"-A", "INPUT", "-i", tapName, "-m", "conntrack",
			"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-A", "INPUT", "-i", tapName, "-j", "DROP"},
	}
	for _, args := range rules {
		if err := runIPTables(args...); err != nil {
			cleanupNetwork(tapName, guestIP, subnet, serverTunnelIP, wgInterface)
			return nil, fmt.Errorf("iptables rule %v: %w", args, err)
		}
	}

	// MASQUERADE: rewrite source to the host's WG interface IP so return
	// packets route back through conntrack reverse-NAT to the VM.
	if err := runIPTables("-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", wgInterface, "-j", "MASQUERADE"); err != nil {
		cleanupNetwork(tapName, guestIP, subnet, serverTunnelIP, wgInterface)
		return nil, fmt.Errorf("MASQUERADE rule: %w", err)
	}

	// DNS DNAT: the VM's resolv.conf points to the server tunnel IP on port 53,
	// but the server DNS listens on port 5353. Redirect transparently.
	for _, proto := range []string{"udp", "tcp"} {
		if err := runIPTables("-t", "nat", "-A", "PREROUTING",
			"-i", tapName, "-p", proto, "--dport", "53",
			"-d", serverTunnelIP,
			"-j", "DNAT", "--to-destination", serverTunnelIP+":5353"); err != nil {
			cleanupNetwork(tapName, guestIP, subnet, serverTunnelIP, wgInterface)
			return nil, fmt.Errorf("DNS DNAT rule (%s): %w", proto, err)
		}
	}

	return &NetworkConfig{
		AgentID:     agentID,
		TAPDevice:   tapName,
		GuestIP:     guestIP,
		GatewayIP:   gatewayIP,
		DNSIP:       dnsIP,
		ServerTunIP: serverTunnelIP,
		WGInterface: wgInterface,
	}, nil
}

// TeardownNetwork removes the TAP device and iptables rules for an agent.
func TeardownNetwork(cfg *NetworkConfig) {
	if cfg == nil {
		return
	}
	subnet := cfg.GuestIP[:strings.LastIndex(cfg.GuestIP, ".")] + ".0/24"
	cleanupNetwork(cfg.TAPDevice, cfg.GuestIP, subnet, cfg.ServerTunIP, cfg.WGInterface)
}

func cleanupNetwork(tapName, guestIP, subnet, serverTunnelIP, wgInterface string) {
	runIPTables("-D", "INPUT", "-i", tapName, "-j", "DROP")
	runIPTables("-D", "INPUT", "-i", tapName, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	runIPTables("-D", "FORWARD", "-i", tapName, "-s", guestIP+"/32",
		"-d", "83.150.0.0/16", "-j", "ACCEPT")
	runIPTables("-D", "FORWARD", "-i", tapName, "-s", guestIP+"/32",
		"-d", serverTunnelIP+"/32", "-j", "ACCEPT")
	runIPTables("-D", "FORWARD", "-o", tapName, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	runIPTables("-D", "FORWARD", "-i", tapName, "-j", "DROP")
	runIPTables("-D", "FORWARD", "-o", tapName, "-j", "DROP")
	runIPTables("-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "-o", wgInterface, "-j", "MASQUERADE")
	for _, proto := range []string{"udp", "tcp"} {
		runIPTables("-t", "nat", "-D", "PREROUTING",
			"-i", tapName, "-p", proto, "--dport", "53",
			"-d", serverTunnelIP,
			"-j", "DNAT", "--to-destination", serverTunnelIP+":5353")
	}

	cleanupTAP(tapName)
}

func cleanupTAP(tapName string) {
	runCmd("ip", "link", "del", "dev", tapName)
}

func tapDeviceName(agentID string) string {
	name := "tap-" + agentID
	name = strings.ReplaceAll(name, "agent-", "")
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

func runCmd(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func runIPTables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}
