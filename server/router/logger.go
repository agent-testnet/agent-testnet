package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
)

// Logger observes connections via conntrack and writes structured JSON logs.
type Logger struct {
	cfg     *config.ServerConfig
	cp      *controlplane.ControlPlane
	logFile *os.File
}

// ConnLog is a single connection log entry.
type ConnLog struct {
	Timestamp  string `json:"timestamp"`
	AgentIP    string `json:"agent_ip"`
	Domain     string `json:"domain,omitempty"`
	DstVIP     string `json:"dst_vip"`
	DstReal    string `json:"dst_real"`
	Protocol   string `json:"protocol"`
	DstPort    string `json:"dst_port,omitempty"`
}

// NewLogger creates a connection logger.
func NewLogger(cfg *config.ServerConfig, cp *controlplane.ControlPlane) *Logger {
	return &Logger{
		cfg: cfg,
		cp:  cp,
	}
}

// Start begins monitoring conntrack events. Blocks until context is cancelled.
func (l *Logger) Start(ctx context.Context) {
	logPath := l.cfg.Router.LogFile
	if logPath == "" {
		logPath = "data/traffic.log"
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		log.Printf("[logger] failed to create log dir: %v", err)
		return
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("[logger] failed to open log file: %v", err)
		return
	}
	l.logFile = f
	defer f.Close()

	log.Printf("[logger] connection logging to %s", logPath)

	// Try conntrack event monitoring first
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
			l.writeEntry(entry)
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
			entries := l.readProcConntrack()
			for _, e := range entries {
				key := fmt.Sprintf("%s-%s-%s-%s", e.AgentIP, e.DstVIP, e.Protocol, e.DstPort)
				if !seen[key] {
					seen[key] = true
					l.writeEntry(e)
				}
			}
		}
	}
}

func (l *Logger) readProcConntrack() []*ConnLog {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		return nil
	}

	var entries []*ConnLog
	for _, line := range strings.Split(string(data), "\n") {
		if entry := l.parseConntrackLine(line); entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (l *Logger) parseConntrackLine(line string) *ConnLog {
	// Only log traffic from the tunnel subnet
	if !strings.Contains(line, "src=10.99.") {
		return nil
	}

	entry := &ConnLog{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	fields := strings.Fields(line)
	for _, f := range fields {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "src":
			if strings.HasPrefix(parts[1], "10.99.") {
				entry.AgentIP = parts[1]
			}
		case "dst":
			if entry.DstVIP == "" && strings.HasPrefix(parts[1], "83.150.") {
				entry.DstVIP = parts[1]
			} else if entry.DstReal == "" && !strings.HasPrefix(parts[1], "10.") && !strings.HasPrefix(parts[1], "83.150.") {
				entry.DstReal = parts[1]
			}
		case "dport":
			if entry.DstPort == "" {
				entry.DstPort = parts[1]
			}
		}
	}

	// Detect protocol
	for _, proto := range []string{"tcp", "udp", "icmp"} {
		if strings.Contains(line, proto) {
			entry.Protocol = proto
			break
		}
	}

	if entry.AgentIP == "" {
		return nil
	}

	// Enrich with domain name
	if entry.DstVIP != "" {
		vip := net.ParseIP(entry.DstVIP)
		if vip != nil {
			for _, dm := range l.cp.Nodes().AllDomainMappings() {
				if dm.VIP == vip.String() {
					entry.Domain = dm.Domain
					break
				}
			}
		}
	}

	return entry
}

func (l *Logger) writeEntry(entry *ConnLog) {
	if l.logFile == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.logFile.Write(data)
	l.logFile.Write([]byte("\n"))
}
