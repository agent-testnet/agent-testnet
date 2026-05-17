package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

// ProxySpec is the immutable description of a per-VM passthrough proxy.
type ProxySpec struct {
	AgentID    string
	Domain     string
	Visibility string
	IP         string
	Ports      []int
	Upstream   string
	TAPDevice  string
	VMGuestIP  string
	VMSSHKey   string
}

// Proxy is a running per-VM passthrough proxy. Each Proxy owns a single
// TAP-side IP alias, one iptables INPUT ACCEPT rule per port, one TCP
// listener per port, and one /etc/hosts entry inside the VM. Stop() reverses
// everything, except the /etc/hosts entry is also re-removed via SSH.
//
// The TCP forwarder is intentionally a dumb byte pump: it never speaks TLS,
// so the cert presented by the real upstream validates against the original
// hostname (the agent VM's TLS client sends SNI for the spoofed domain, and
// because we're forwarding to the same domain at the upstream level the cert
// matches). This is the same architecture the legacy bash setup used with
// `socat`, just owned by the daemon now.
type Proxy struct {
	spec      ProxySpec
	cancel    context.CancelFunc
	listeners []net.Listener
	wg        sync.WaitGroup
}

// Spec returns the proxy's immutable description.
func (p *Proxy) Spec() ProxySpec { return p.spec }

// Info returns the api-level view of this proxy.
func (p *Proxy) Info() api.ProxyInfo {
	return api.ProxyInfo{
		AgentID:    p.spec.AgentID,
		Domain:     p.spec.Domain,
		Visibility: p.spec.Visibility,
		IP:         p.spec.IP,
		Ports:      append([]int(nil), p.spec.Ports...),
		Upstream:   p.spec.Upstream,
	}
}

// StartProxy aliases spec.IP onto spec.TAPDevice, opens the necessary INPUT
// holes, writes /etc/hosts inside the guest, and spins up TCP forwarder
// goroutines on each spec.Port. The caller is responsible for IP allocation
// (see ProxyAllocator) — this function takes the IP as input so the daemon
// can persist the assignment before any side effects happen.
//
// All side effects are best-effort idempotent: re-aliasing an existing IP,
// re-inserting an existing iptables rule, and re-appending an /etc/hosts
// entry that's already present are all no-ops.
func StartProxy(ctx context.Context, spec ProxySpec) (*Proxy, error) {
	if spec.AgentID == "" || spec.Domain == "" || spec.IP == "" || spec.TAPDevice == "" {
		return nil, fmt.Errorf("proxy spec: agent_id, domain, ip, and tap_device are required")
	}
	if len(spec.Ports) == 0 {
		spec.Ports = []int{443}
	}
	if spec.Upstream == "" {
		spec.Upstream = fmt.Sprintf("%s:%d", spec.Domain, spec.Ports[0])
	}

	if err := ensureTAPAlias(spec.IP, spec.TAPDevice); err != nil {
		return nil, fmt.Errorf("alias %s on %s: %w", spec.IP, spec.TAPDevice, err)
	}

	for _, port := range spec.Ports {
		if err := ensureINPUTAccept(spec.TAPDevice, spec.IP, port); err != nil {
			cleanupProxySideEffects(spec)
			return nil, fmt.Errorf("iptables ACCEPT %s:%d: %w", spec.IP, port, err)
		}
	}

	if err := writeVMHostsEntry(spec); err != nil {
		// Non-fatal: the VM may not be SSH-ready yet (proxy might be set up
		// before the VM finishes boot, e.g. for install-time proxies that
		// need to be in place before apk update). Log and continue; the
		// caller can re-apply later if needed.
		log.Printf("[proxy %s/%s] warning: writing /etc/hosts in VM failed (will retry on first connect): %v",
			spec.AgentID, spec.Domain, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	p := &Proxy{spec: spec, cancel: cancel}

	for _, port := range spec.Ports {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", spec.IP, port))
		if err != nil {
			p.Stop()
			return nil, fmt.Errorf("listen %s:%d: %w", spec.IP, port, err)
		}
		p.listeners = append(p.listeners, listener)
		p.wg.Add(1)
		go p.serve(ctx, listener, port)
	}

	log.Printf("[proxy] %s -> %s (vis=%s, ip=%s, ports=%v) up",
		spec.Domain, spec.Upstream, spec.Visibility, spec.IP, spec.Ports)
	return p, nil
}

// Stop tears down the listener goroutines, removes the iptables INPUT rules,
// removes the TAP alias, and best-effort removes the VM /etc/hosts entry.
// Safe to call more than once.
func (p *Proxy) Stop() {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	for _, l := range p.listeners {
		l.Close()
	}
	p.listeners = nil
	p.wg.Wait()
	cleanupProxySideEffects(p.spec)
	log.Printf("[proxy] %s -> %s down", p.spec.Domain, p.spec.Upstream)
}

func (p *Proxy) serve(ctx context.Context, listener net.Listener, port int) {
	defer p.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[proxy %s:%d] accept error: %v", p.spec.IP, port, err)
			return
		}
		go p.pump(ctx, conn, port)
	}
}

func (p *Proxy) pump(ctx context.Context, client net.Conn, port int) {
	defer client.Close()

	upstream := p.spec.Upstream
	if upstream == "" {
		upstream = fmt.Sprintf("%s:%d", p.spec.Domain, port)
	}

	dialer := net.Dialer{}
	server, err := dialer.DialContext(ctx, "tcp", upstream)
	if err != nil {
		log.Printf("[proxy %s -> %s] dial: %v", p.spec.Domain, upstream, err)
		return
	}
	defer server.Close()

	// Bidirectional copy. Either direction closing tears the other down.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, server); done <- struct{}{} }()
	<-done
}

// cleanupProxySideEffects removes iptables rules, TAP alias and /etc/hosts
// entry, ignoring "doesn't exist" errors so partial setup is safely
// reversible.
func cleanupProxySideEffects(spec ProxySpec) {
	for _, port := range spec.Ports {
		_ = exec.Command("iptables",
			"-D", "INPUT",
			"-i", spec.TAPDevice,
			"-d", spec.IP,
			"-p", "tcp", "--dport", strconv.Itoa(port),
			"-j", "ACCEPT").Run()
	}
	_ = exec.Command("ip", "addr", "del", spec.IP+"/32", "dev", spec.TAPDevice).Run()
	_ = removeVMHostsEntry(spec)
}

func ensureTAPAlias(ip, tap string) error {
	out, err := exec.Command("ip", "addr", "add", ip+"/32", "dev", tap).CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		// "RTNETLINK answers: File exists" → already aliased, idempotent OK.
		if strings.Contains(s, "File exists") {
			return nil
		}
		return fmt.Errorf("ip addr add: %s", s)
	}
	return nil
}

func ensureINPUTAccept(tap, ip string, port int) error {
	// Check first to keep the rule list short across reloads. iptables -C
	// returns 0 if the rule exists, non-zero otherwise.
	check := exec.Command("iptables",
		"-C", "INPUT",
		"-i", tap,
		"-d", ip,
		"-p", "tcp", "--dport", strconv.Itoa(port),
		"-j", "ACCEPT")
	if check.Run() == nil {
		return nil
	}

	out, err := exec.Command("iptables",
		"-I", "INPUT",
		"-i", tap,
		"-d", ip,
		"-p", "tcp", "--dport", strconv.Itoa(port),
		"-j", "ACCEPT").CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables -I INPUT: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// writeVMHostsEntry appends `<ip> <domain>` to /etc/hosts inside the VM,
// replacing any pre-existing line for the same domain. Idempotent.
func writeVMHostsEntry(spec ProxySpec) error {
	if spec.VMSSHKey == "" || spec.VMGuestIP == "" {
		return nil
	}
	// Remove any existing line(s) for this domain, then append the fresh
	// mapping. The combined sed+tee trick keeps the operation single-shot
	// and works across BusyBox/Alpine sed.
	cmd := fmt.Sprintf(
		"sed -i.bak '/[[:space:]]%[1]s$/d' /etc/hosts && "+
			"echo '%[2]s %[1]s' >> /etc/hosts",
		shellEscape(spec.Domain), shellEscape(spec.IP))
	return runVMSSH(spec.VMSSHKey, spec.VMGuestIP, cmd)
}

// removeVMHostsEntry strips the line we added from /etc/hosts inside the VM.
func removeVMHostsEntry(spec ProxySpec) error {
	if spec.VMSSHKey == "" || spec.VMGuestIP == "" {
		return nil
	}
	cmd := fmt.Sprintf(
		"sed -i.bak '/[[:space:]]%s$/d' /etc/hosts",
		shellEscape(spec.Domain))
	return runVMSSH(spec.VMSSHKey, spec.VMGuestIP, cmd)
}

func runVMSSH(keyPath, guestIP, remoteCmd string) error {
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		"-i", keyPath,
		"root@"+guestIP,
		remoteCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh root@%s: %s: %w", guestIP, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// shellEscape produces a string safe to embed inside single-quoted shell
// args. It only handles characters we'd realistically see in domain names
// and IPs (alphanumerics, dot, hyphen) — anything else returns the string
// unchanged but quoted. We err on the side of refusing surprising input
// rather than building a bulletproof shell escaper.
func shellEscape(s string) string {
	for _, r := range s {
		if !(r == '.' || r == '-' || r == ':' ||
			('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')) {
			return ""
		}
	}
	return s
}
