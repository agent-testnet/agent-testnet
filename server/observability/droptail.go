package observability

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"strings"
)

// DropLogPrefix is the magic string used by router iptables LOG rules.
// Drop events are recognised by scanning kernel log lines for this prefix.
const DropLogPrefix = "TESTNET-DROP:"

// StartDropTailer reads kernel log entries via `journalctl -k -f`, picks
// out lines containing DropLogPrefix, parses their `src=`, `dst=`, `proto=`,
// `dpt=` fields, and emits one event=drop row per match. Blocks until ctx
// is cancelled or the kernel-log stream errors fatally.
//
// If journalctl isn't available (e.g. non-systemd hosts) the function logs
// a warning and returns — callers should not treat that as fatal.
func (l *EventLogger) StartDropTailer(ctx context.Context) {
	if l == nil {
		return
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		log.Printf("[observability] drop tailer disabled: journalctl not found (%v)", err)
		return
	}

	cmd := exec.CommandContext(ctx, "journalctl", "-k", "-f", "-o", "cat", "--no-pager")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[observability] drop tailer pipe: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[observability] drop tailer start: %v", err)
		return
	}
	log.Printf("[observability] drop tailer reading kernel log via journalctl")

	go l.scanDropLines(stdout)

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		log.Printf("[observability] drop tailer exited: %v", err)
	}
}

func (l *EventLogger) scanDropLines(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, DropLogPrefix) {
			continue
		}
		if evt := parseDropLine(line); evt != nil {
			l.Log(evt)
		}
	}
}

// parseDropLine extracts a {agent,dst_ip,dst_port,proto} drop event from a
// kernel log line emitted by an `iptables -j LOG --log-prefix TESTNET-DROP:`
// rule. Returns nil if the line doesn't look like a usable drop entry.
func parseDropLine(line string) map[string]any {
	evt := map[string]any{
		"event":  "drop",
		"reason": "non-vip",
	}

	for _, field := range strings.Fields(line) {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch strings.ToUpper(parts[0]) {
		case "SRC":
			evt["agent"] = parts[1]
		case "DST":
			evt["dst_ip"] = parts[1]
		case "PROTO":
			evt["proto"] = strings.ToLower(parts[1])
		case "DPT":
			evt["dst_port"] = parts[1]
		case "SPT":
			evt["src_port"] = parts[1]
		}
	}

	if _, ok := evt["agent"]; !ok {
		return nil
	}
	return evt
}
