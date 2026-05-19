package router

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
	"github.com/agent-testnet/agent-testnet/server/observability"
)

// Logger observes connections via conntrack and emits event=conn entries
// into the shared observability log. The L7 MITM proxy in server/proxy
// covers HTTP/HTTPS, but conntrack still catches non-HTTP flows (mail,
// custom ports) and acts as a sanity check against the proxy view.
type Logger struct {
	cfg *config.ServerConfig
	cp  *controlplane.ControlPlane
	obs *observability.EventLogger
}

// NewLogger creates a connection logger. obs may be nil — the conntrack
// loop then becomes a no-op (events are dropped on the floor) which is
// cheap and lets the router start before observability is wired up.
func NewLogger(cfg *config.ServerConfig, cp *controlplane.ControlPlane, obs *observability.EventLogger) *Logger {
	return &Logger{cfg: cfg, cp: cp, obs: obs}
}

// Start begins monitoring conntrack events. Blocks until ctx is cancelled.
func (l *Logger) Start(ctx context.Context) {
	if l.obs == nil {
		log.Printf("[logger] no event logger configured; conntrack monitoring disabled")
		return
	}
	if err := l.monitorConntrack(ctx); err != nil {
		log.Printf("[logger] conntrack events unavailable: %v, falling back to polling", err)
		l.pollConntrack(ctx)
	}
}

func (l *Logger) monitorConntrack(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "conntrack", "-E", "-e", "NEW", "-o", "timestamp")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if entry := l.parseConntrackLine(line); entry != nil {
			l.obs.Log(entry)
		}
	}

	return cmd.Wait()
}

func (l *Logger) pollConntrack(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	seen := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, e := range l.readProcConntrack() {
				key := fmt.Sprintf("%v-%v-%v-%v", e["agent"], e["dst_vip"], e["proto"], e["dst_port"])
				if seen[key] {
					continue
				}
				seen[key] = true
				l.obs.Log(e)
			}
		}
	}
}

func (l *Logger) readProcConntrack() []map[string]any {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		return nil
	}

	var entries []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if entry := l.parseConntrackLine(line); entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

// parseConntrackLine extracts a connection event from a single conntrack
// output line (whether from `conntrack -E` or /proc/net/nf_conntrack).
// Only lines originating from the WireGuard tunnel subnet (10.99.*) are
// emitted; everything else is unrelated traffic.
func (l *Logger) parseConntrackLine(line string) map[string]any {
	if !strings.Contains(line, "src=10.99.") {
		return nil
	}

	entry := map[string]any{"event": "conn"}

	var dstVIP, dstReal string

	for _, f := range strings.Fields(line) {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "src":
			if _, ok := entry["agent"]; !ok && strings.HasPrefix(parts[1], "10.99.") {
				entry["agent"] = parts[1]
			}
		case "dst":
			if dstVIP == "" && strings.HasPrefix(parts[1], "83.150.") {
				dstVIP = parts[1]
			} else if dstReal == "" && !strings.HasPrefix(parts[1], "10.") && !strings.HasPrefix(parts[1], "83.150.") {
				dstReal = parts[1]
			}
		case "dport":
			if _, ok := entry["dst_port"]; !ok {
				entry["dst_port"] = parts[1]
			}
		}
	}

	for _, proto := range []string{"tcp", "udp", "icmp"} {
		if strings.Contains(line, proto) {
			entry["proto"] = proto
			break
		}
	}

	if _, ok := entry["agent"]; !ok {
		return nil
	}

	if dstVIP != "" {
		entry["dst_vip"] = dstVIP
		if vip := net.ParseIP(dstVIP); vip != nil {
			if doms := l.cp.Nodes().DomainsForVIP(vip); len(doms) > 0 {
				entry["domain"] = doms[0]
			}
			if node := l.cp.Nodes().NodeForVIP(vip); node != nil {
				entry["node"] = node.Name
			}
		}
	}
	if dstReal != "" {
		entry["dst_real"] = dstReal
	}

	return entry
}
