// Package observability provides a unified event log for testnet server
// components (DNS, MITM proxy, conntrack logger, iptables drop tailer).
//
// Every event is one JSON object per line written to a single append-only
// file (default: data/requests.log). The schema is loosely typed: each
// caller passes any struct or map that JSON-marshals; the logger always
// stamps a UTC RFC3339Nano timestamp at the top level.
//
// Designed to be tailed live:
//
//	tail -f data/requests.log | jq -c '{ts,event,agent,host,path,status}'
package observability

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/config"
)

// EventLogger is a process-wide JSON-lines event sink. Safe for concurrent
// use from multiple goroutines.
type EventLogger struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	dropped uint64 // events dropped because the file could not be opened
}

// New opens (or creates) the configured log file and returns a ready-to-use
// EventLogger. If the file can't be opened, a logger is still returned: it
// silently drops events (and counts them) so observability never blocks the
// data path.
func New(cfg *config.ServerConfig) *EventLogger {
	path := resolveLogPath(cfg)
	l := &EventLogger{path: path}
	if err := l.open(); err != nil {
		log.Printf("[observability] failed to open %s: %v (events will be dropped)", path, err)
	} else {
		log.Printf("[observability] writing events to %s", path)
	}
	return l
}

// Path returns the on-disk path of the event log.
func (l *EventLogger) Path() string { return l.path }

// Log marshals event as JSON and appends one line to the log file. A "ts"
// field is added (and overwrites any existing ts) so callers don't have to
// remember to set it. Errors are logged to stderr but never returned —
// observability must not break the data path.
func (l *EventLogger) Log(event map[string]any) {
	if l == nil || event == nil {
		return
	}
	event["ts"] = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("[observability] marshal event: %v", err)
		return
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		// Best-effort re-open in case the directory appeared later.
		if err := l.openLocked(); err != nil {
			l.dropped++
			return
		}
	}
	if _, err := l.file.Write(data); err != nil {
		log.Printf("[observability] write event: %v", err)
	}
}

// Close releases the underlying file handle.
func (l *EventLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *EventLogger) open() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.openLocked()
}

func (l *EventLogger) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	return nil
}

// resolveLogPath picks the event-log path with a sensible fallback chain:
//  1. cfg.Observability.LogFile (new)
//  2. cfg.Router.LogFile (legacy traffic.log path) so existing deployments
//     keep writing to a single known location until reconfigured.
//  3. ./data/requests.log
func resolveLogPath(cfg *config.ServerConfig) string {
	if cfg != nil {
		if p := cfg.Observability.LogFile; p != "" {
			return p
		}
		if p := cfg.Router.LogFile; p != "" {
			return p
		}
	}
	return "data/requests.log"
}
