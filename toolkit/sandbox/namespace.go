package sandbox

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// Config holds parameters for creating a testnet-confined sandbox.
type Config struct {
	DNSIP        string
	CACertPath   string
	WGInterface  string
	AllowedCIDRs []string
	Command      string
	Args         []string
}

// Run creates a network namespace, configures it for testnet-only access,
// and executes the given command inside it.
func Run(cfg *Config) error {
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("generate sandbox id: %w", err)
	}
	nsName := "tns-" + id

	vethHost := "veth-" + id
	vethNS := "vns-" + id
	// Truncate to 15 chars (Linux IFNAMSIZ limit)
	if len(vethHost) > 15 {
		vethHost = vethHost[:15]
	}
	if len(vethNS) > 15 {
		vethNS = vethNS[:15]
	}

	hostIP := "172.30.0.1"
	nsIP := "172.30.0.2"

	cleanup := func() {
		log.Printf("[sandbox] Cleaning up namespace %s...", nsName)
		cleanupIPTables(vethHost, nsIP, cfg.AllowedCIDRs, cfg.WGInterface)
		runCmd("ip", "netns", "del", nsName)
	}

	// Trap signals for cleanup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(1)
	}()

	// 1. Create the network namespace
	if err := runCmd("ip", "netns", "add", nsName); err != nil {
		return fmt.Errorf("create netns: %w", err)
	}

	// 2. Create veth pair
	if err := runCmd("ip", "link", "add", vethHost, "type", "veth", "peer", "name", vethNS); err != nil {
		cleanup()
		return fmt.Errorf("create veth pair: %w", err)
	}

	// 3. Move one end into the namespace
	if err := runCmd("ip", "link", "set", vethNS, "netns", nsName); err != nil {
		cleanup()
		return fmt.Errorf("move veth to netns: %w", err)
	}

	// 4. Configure the host side
	hostCmds := [][]string{
		{"ip", "addr", "add", hostIP + "/30", "dev", vethHost},
		{"ip", "link", "set", vethHost, "up"},
	}
	for _, args := range hostCmds {
		if err := runCmd(args...); err != nil {
			cleanup()
			return fmt.Errorf("host veth setup (%v): %w", args, err)
		}
	}

	// 5. Configure the namespace side
	nsCmds := [][]string{
		{"ip", "netns", "exec", nsName, "ip", "addr", "add", nsIP + "/30", "dev", vethNS},
		{"ip", "netns", "exec", nsName, "ip", "link", "set", vethNS, "up"},
		{"ip", "netns", "exec", nsName, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", nsName, "ip", "route", "add", "default", "via", hostIP},
	}
	for _, args := range nsCmds {
		if err := runCmd(args...); err != nil {
			cleanup()
			return fmt.Errorf("namespace veth setup (%v): %w", args, err)
		}
	}

	// 6. Enable forwarding
	if err := runCmd("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		log.Printf("[sandbox] warning: failed to enable ip_forward: %v", err)
	}

	// 7. iptables: allow traffic from the namespace only to permitted CIDRs
	if err := setupIPTables(vethHost, nsIP, cfg.AllowedCIDRs, cfg.WGInterface); err != nil {
		cleanup()
		return fmt.Errorf("iptables setup: %w", err)
	}

	// 8. Write resolv.conf inside the namespace
	resolvConf := fmt.Sprintf("nameserver %s\n", cfg.DNSIP)
	nsResolvPath := fmt.Sprintf("/etc/netns/%s/resolv.conf", nsName)
	nsResolvDir := fmt.Sprintf("/etc/netns/%s", nsName)
	if err := os.MkdirAll(nsResolvDir, 0o755); err != nil {
		cleanup()
		return fmt.Errorf("create netns config dir: %w", err)
	}
	if err := os.WriteFile(nsResolvPath, []byte(resolvConf), 0o644); err != nil {
		cleanup()
		return fmt.Errorf("write resolv.conf: %w", err)
	}

	// 9. Install the testnet CA cert inside the namespace (if the cert file exists)
	if cfg.CACertPath != "" {
		if _, err := os.Stat(cfg.CACertPath); err == nil {
			destDir := fmt.Sprintf("/etc/netns/%s", nsName)
			// Copy the CA cert so it's available inside the namespace. Applications
			// can reference it explicitly, or it can be injected into the system
			// trust store via update-ca-certificates when the namespace has one.
		destPath := destDir + "/ca.pem"
		if data, err := os.ReadFile(cfg.CACertPath); err == nil {
			if err := os.WriteFile(destPath, data, 0o644); err != nil {
				cleanup()
				return fmt.Errorf("write CA cert to namespace: %w", err)
			}
		}

			// Try to install into the system trust store inside the namespace.
			// This is best-effort: some base systems don't have update-ca-certificates.
			sysCADir := "/usr/local/share/ca-certificates"
			runCmd("ip", "netns", "exec", nsName, "mkdir", "-p", sysCADir)
			runCmd("ip", "netns", "exec", nsName, "cp", cfg.CACertPath, sysCADir+"/testnet-ca.crt")
			runCmd("ip", "netns", "exec", nsName, "update-ca-certificates")
		} else {
			log.Printf("[sandbox] warning: CA cert %s not found, skipping trust store setup", cfg.CACertPath)
		}
	}

	// 10. Execute the command inside the namespace
	log.Printf("[sandbox] Running in namespace %s: %s %s", nsName, cfg.Command, strings.Join(cfg.Args, " "))
	execArgs := append([]string{"ip", "netns", "exec", nsName, cfg.Command}, cfg.Args...)
	child := exec.Command(execArgs[0], execArgs[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	err = child.Run()

	// 11. Cleanup
	cleanup()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

func setupIPTables(vethHost, nsIP string, allowedCIDRs []string, wgInterface string) error {
	// Allow traffic from namespace to each allowed CIDR
	for _, cidr := range allowedCIDRs {
		if err := runIPTables("-A", "FORWARD", "-i", vethHost, "-s", nsIP+"/32",
			"-d", cidr, "-j", "ACCEPT"); err != nil {
			return err
		}
	}

	// Allow established/related return traffic
	if err := runIPTables("-A", "FORWARD", "-o", vethHost, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}

	// Drop everything else from/to this veth
	if err := runIPTables("-A", "FORWARD", "-i", vethHost, "-j", "DROP"); err != nil {
		return err
	}
	if err := runIPTables("-A", "FORWARD", "-o", vethHost, "-j", "DROP"); err != nil {
		return err
	}

	// MASQUERADE so return traffic from the WG interface routes back
	subnet := nsIP + "/30"
	if err := runIPTables("-t", "nat", "-A", "POSTROUTING",
		"-s", subnet, "-o", wgInterface, "-j", "MASQUERADE"); err != nil {
		return err
	}

	return nil
}

func cleanupIPTables(vethHost, nsIP string, allowedCIDRs []string, wgInterface string) {
	for _, cidr := range allowedCIDRs {
		runIPTables("-D", "FORWARD", "-i", vethHost, "-s", nsIP+"/32",
			"-d", cidr, "-j", "ACCEPT")
	}
	runIPTables("-D", "FORWARD", "-o", vethHost, "-m", "conntrack",
		"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	runIPTables("-D", "FORWARD", "-i", vethHost, "-j", "DROP")
	runIPTables("-D", "FORWARD", "-o", vethHost, "-j", "DROP")

	subnet := nsIP + "/30"
	runIPTables("-t", "nat", "-D", "POSTROUTING",
		"-s", subnet, "-o", wgInterface, "-j", "MASQUERADE")
}

func randomID() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		b[i] = chars[n.Int64()]
	}
	return string(b), nil
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
