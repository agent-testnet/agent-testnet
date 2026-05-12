package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} — Agent Testnet</title>
<style>
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f1117; color: #e4e4e7;
    display: flex; justify-content: center; padding: 3rem 1.5rem;
    line-height: 1.6;
  }
  main { max-width: 42rem; width: 100%; }
  h1 { font-size: 1.75rem; color: #fff; margin-bottom: .25rem; }
  .subtitle { color: #a1a1aa; margin-bottom: 2rem; font-size: .95rem; }
  .card {
    background: #1a1b23; border: 1px solid #27272a; border-radius: .75rem;
    padding: 1.5rem; margin-bottom: 1.25rem;
  }
  .card h2 { font-size: 1.1rem; color: #fff; margin-bottom: .5rem; }
  .card p, .card li { color: #d4d4d8; font-size: .9rem; }
  .card ul { padding-left: 1.25rem; }
  .card li + li { margin-top: .25rem; }
  .badge {
    display: inline-block; background: #22c55e22; color: #4ade80;
    font-size: .75rem; font-weight: 600; padding: .15rem .5rem;
    border-radius: 9999px; margin-left: .5rem; vertical-align: middle;
  }
  .meta { display: grid; grid-template-columns: 1fr 1fr; gap: .75rem; }
  .meta-item { background: #1a1b23; border: 1px solid #27272a; border-radius: .5rem; padding: 1rem; }
  .meta-label { font-size: .75rem; color: #71717a; text-transform: uppercase; letter-spacing: .05em; }
  .meta-value { font-size: 1rem; color: #fff; margin-top: .15rem; font-family: "SF Mono", Menlo, monospace; }
  footer { margin-top: 2rem; text-align: center; color: #52525b; font-size: .8rem; }
  a { color: #60a5fa; text-decoration: none; }
  a:hover { text-decoration: underline; }
  code { background: #27272a; padding: .1rem .35rem; border-radius: .25rem; font-size: .85em; }
</style>
</head>
<body>
<main>
  <h1>Agent Testnet<span class="badge">online</span></h1>
  <p class="subtitle">Node <code>{{.Name}}</code> serving <code>{{.Host}}</code></p>

  <div class="card">
    <h2>What is the Agent Testnet?</h2>
    <p>A sandboxed internet environment for AI agents. Agents run inside
    Firecracker microVMs with full network isolation, interacting only with
    operator-declared testnet nodes like this one. Every connection is
    encrypted with certificates from the testnet's own CA&mdash;no real
    internet traffic ever leaves the sandbox.</p>
  </div>

  <div class="card">
    <h2>How it works</h2>
    <ul>
      <li><strong>Control plane</strong> &mdash; manages client registration, DNS, WireGuard tunnels, and NAT routing.</li>
      <li><strong>Clients</strong> &mdash; launch Firecracker microVMs with per-VM SSH keys and iptables isolation.</li>
      <li><strong>Nodes</strong> &mdash; any service (like this page) exposed to agents, using TLS certs issued by the testnet CA.</li>
    </ul>
  </div>

  <div class="card">
    <h2>Network model</h2>
    <p>Each declared domain gets a Virtual IP in <code>83.150.0.0/16</code>.
    Testnet DNS resolves only declared domains; everything else returns NXDOMAIN.
    A WireGuard tunnel is the sole egress path from agent VMs, and server-side
    iptables DNAT maps VIPs to real node addresses.</p>
  </div>

  <div class="meta">
    <div class="meta-item">
      <div class="meta-label">Node</div>
      <div class="meta-value">{{.Name}}</div>
    </div>
    <div class="meta-item">
      <div class="meta-label">Host</div>
      <div class="meta-value">{{.Host}}</div>
    </div>
  </div>

  <footer>
    <a href="/api/status">JSON status</a> · <a href="/health">Health check</a>
  </footer>
</main>
</body>
</html>
`))

func main() {
	serverURL := flag.String("server-url", "", "testnet server URL (https://...)")
	name := flag.String("name", "", "node name (as declared in nodes.yaml)")
	secret := flag.String("secret", "", "per-node secret from nodes.yaml")
	listenAddr := flag.String("listen", ":443", "address to listen on")
	caFingerprint := flag.String("ca-fingerprint", "", "SHA-256 fingerprint of the server's TLS cert (hex) for bootstrap verification")
	flag.Parse()

	if *serverURL == "" || *name == "" || *secret == "" {
		log.Fatal("--server-url, --name, and --secret are required")
	}

	log.Printf("Fetching TLS certificates from %s for node %s...", *serverURL, *name)
	var client *api.ServerClient
	if *caFingerprint != "" {
		client = api.NewServerClientWithFingerprint(*serverURL, *caFingerprint)
	} else {
		client = api.NewServerClient(*serverURL, nil)
	}

	var certs *api.CertResponse
	maxRetries := 30
	for attempt := 1; attempt <= maxRetries; attempt++ {
		var fetchErr error
		certs, fetchErr = client.FetchNodeCerts(*name, *secret)
		if fetchErr == nil {
			break
		}
		backoff := time.Duration(attempt) * 2 * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		log.Printf("Cert fetch attempt %d/%d failed: %v (retrying in %s)", attempt, maxRetries, fetchErr, backoff)
		time.Sleep(backoff)
	}
	if certs == nil {
		log.Fatalf("Failed to fetch certs after %d attempts", maxRetries)
	}
	log.Println("Certificates received.")

	tlsCert, err := tls.X509KeyPair([]byte(certs.CertPEM), []byte(certs.KeyPEM))
	if err != nil {
		log.Fatalf("Failed to parse TLS cert: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		indexTmpl.Execute(w, map[string]string{
			"Name": *name,
			"Host": r.Host,
		})
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"node":   *name,
			"status": "ok",
			"host":   r.Host,
			"path":   r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})

	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("Testnet node %s listening on %s (HTTPS)", *name, *listenAddr)
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
