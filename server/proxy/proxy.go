// Package proxy is the transparent HTTP/HTTPS MITM proxy that the testnet
// server uses to terminate, log, and forward traffic from agent VMs to
// nodes.
//
// Why MITM: agent VMs trust the testnet CA (the rootfs ships with it
// installed), and every node serves a certificate signed by the same CA.
// That lets us mint per-host leaf certs on the fly, decrypt traffic at the
// server, capture HTTP-level events (method, URL, status, byte counts),
// and then re-encrypt to the real upstream verified against the same CA.
//
// Wiring: the router (server/router) rewrites iptables so traffic to
// VIP:80 and VIP:443 is REDIRECTed to the listener addresses configured
// here. Other ports keep their existing DNAT path and are not observed by
// this proxy.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
	"github.com/agent-testnet/agent-testnet/server/observability"
)

// Proxy is the transparent MITM proxy for testnet HTTP/HTTPS traffic.
type Proxy struct {
	cfg    *config.ServerConfig
	cp     *controlplane.ControlPlane
	obs    *observability.EventLogger
	caPool *x509.CertPool
}

// New constructs a Proxy. It does not open any listeners — call Start.
// The control plane must already have its CA initialised.
func New(cfg *config.ServerConfig, cp *controlplane.ControlPlane, obs *observability.EventLogger) (*Proxy, error) {
	caPEM := cp.CA().RootCertPEM()
	if len(caPEM) == 0 {
		return nil, fmt.Errorf("proxy: CA not initialised")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("proxy: failed to append testnet CA to pool")
	}
	return &Proxy{cfg: cfg, cp: cp, obs: obs, caPool: pool}, nil
}

// Start opens both listeners and serves until the context is cancelled.
// Each accepted connection is handled in its own goroutine.
func (p *Proxy) Start(ctx context.Context) error {
	httpsAddr := p.cfg.Proxy.HTTPSListen
	httpAddr := p.cfg.Proxy.HTTPListen

	httpsLn, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		return fmt.Errorf("proxy: listen https %s: %w", httpsAddr, err)
	}
	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		httpsLn.Close()
		return fmt.Errorf("proxy: listen http %s: %w", httpAddr, err)
	}

	log.Printf("[proxy] HTTPS MITM listening on %s", httpsAddr)
	log.Printf("[proxy] HTTP  proxy listening on %s", httpAddr)

	go func() {
		<-ctx.Done()
		httpsLn.Close()
		httpLn.Close()
	}()

	go p.acceptLoop(ctx, httpsLn, true)
	p.acceptLoop(ctx, httpLn, false)
	return nil
}

func (p *Proxy) acceptLoop(ctx context.Context, ln net.Listener, isTLS bool) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[proxy] accept error: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go p.handle(c, isTLS)
	}
}

func (p *Proxy) handle(c net.Conn, isTLS bool) {
	defer c.Close()

	agentIP := remoteIP(c.RemoteAddr())

	dst, err := origDst(c)
	if err != nil {
		p.logEvent(map[string]any{
			"event": "proxy_error",
			"agent": agentIP,
			"stage": "orig_dst",
			"error": err.Error(),
		})
		return
	}

	node := p.cp.Nodes().NodeForVIP(dst.IP)
	if node == nil {
		p.logEvent(map[string]any{
			"event":   "proxy_error",
			"agent":   agentIP,
			"stage":   "vip_lookup",
			"dst_vip": dst.IP.String(),
			"error":   "no node for VIP",
		})
		return
	}

	if isTLS {
		p.handleHTTPS(c, agentIP, dst, node)
		return
	}
	p.handleHTTP(c, agentIP, dst, node)
}

func (p *Proxy) handleHTTPS(rawConn net.Conn, agentIP string, dst *net.TCPAddr, node *api.Node) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := strings.ToLower(strings.TrimSpace(hello.ServerName))
			if host == "" {
				// No SNI — fall back to the VIP literal so the
				// handshake completes; the agent likely won't trust
				// it (no SAN match), which surfaces as a tls error
				// the agent reports rather than a silent hang.
				host = dst.IP.String()
			}
			return p.cp.CA().IssueLeafForHost(host)
		},
	}
	tlsConn := tls.Server(rawConn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		p.logEvent(map[string]any{
			"event":   "proxy_error",
			"agent":   agentIP,
			"stage":   "tls_handshake",
			"dst_vip": dst.IP.String(),
			"node":    node.Name,
			"error":   err.Error(),
		})
		return
	}

	sni := strings.ToLower(tlsConn.ConnectionState().ServerName)
	if sni == "" {
		// Best-effort: pick any domain owned by this VIP for upstream
		// verification.
		if doms := p.cp.Nodes().DomainsForVIP(dst.IP); len(doms) > 0 {
			sni = doms[0]
		} else {
			sni = node.Name + ".testnet"
		}
	}

	p.serveHTTP(tlsConn, agentIP, dst, node, sni, true)
}

func (p *Proxy) handleHTTP(c net.Conn, agentIP string, dst *net.TCPAddr, node *api.Node) {
	// For plain HTTP, the Host header from the first request supplies the
	// upstream SNI. serveHTTP reads it from req.Host on each request.
	p.serveHTTP(c, agentIP, dst, node, "", false)
}

// serveHTTP runs an http.Server on a single already-accepted (and possibly
// TLS-terminated) connection, routing every request through the per-node
// reverse proxy with structured logging.
func (p *Proxy) serveHTTP(conn net.Conn, agentIP string, dst *net.TCPAddr, node *api.Node, sni string, isTLS bool) {
	handler := p.buildHandler(agentIP, dst, node, sni, isTLS)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_ = srv.Serve(newSingleConnListener(conn))
}

// buildHandler returns the per-connection HTTP handler. The reverse proxy
// is closed over the upstream node and the SNI hostname used for upstream
// TLS verification.
func (p *Proxy) buildHandler(agentIP string, dst *net.TCPAddr, node *api.Node, sni string, isTLS bool) http.Handler {
	upstreamURL := &url.URL{
		Scheme: "https",
		Host:   node.Address,
	}

	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs:    p.caPool,
			MinVersion: tls.VersionTLS12,
		},
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Send bytes to the real node, but preserve the agent's
			// Host so virtual-host nodes (multiple SAN'd domains per
			// node) route correctly. Per-request upstream SNI is
			// pinned to req.Host in perRequestSNITransport so the
			// node's CA-signed cert verifies.
			req.URL.Scheme = upstreamURL.Scheme
			req.URL.Host = upstreamURL.Host
			if req.Host == "" {
				req.Host = sni
			}
			req.Header.Set("X-Testnet-Agent", agentIP)
		},
		Transport: &perRequestSNITransport{
			base: transport,
			pool: p.caPool,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.logEvent(map[string]any{
				"event":    "http_error",
				"agent":    agentIP,
				"method":   r.Method,
				"host":     hostFromRequest(r, sni),
				"path":     r.URL.RequestURI(),
				"upstream": upstreamURL.Host,
				"node":     node.Name,
				"tls":      isTLS,
				"error":    err.Error(),
			})
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &respRecorder{ResponseWriter: w, status: http.StatusOK}
		rp.ServeHTTP(rec, r)
		p.logEvent(map[string]any{
			"event":       "http",
			"agent":       agentIP,
			"method":      r.Method,
			"host":        hostFromRequest(r, sni),
			"path":        r.URL.RequestURI(),
			"status":      rec.status,
			"resp_bytes":  rec.written,
			"duration_ms": time.Since(start).Milliseconds(),
			"upstream":    upstreamURL.Host,
			"node":        node.Name,
			"dst_vip":     dst.IP.String(),
			"tls":         isTLS,
		})
	})
}

func (p *Proxy) logEvent(evt map[string]any) {
	if p.obs == nil {
		return
	}
	p.obs.Log(evt)
}

// ---- helpers ----

// singleConnListener wraps a single already-accepted net.Conn so it can
// be served by an http.Server. The first Accept returns the conn; later
// Accept calls block until Close, which is required for http.Server to
// shut down cleanly (otherwise it would spin on a permanent EOF).
type singleConnListener struct {
	conn   net.Conn
	served atomic.Bool
	done   chan struct{}
	once   atomic.Bool
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	return &singleConnListener{conn: c, done: make(chan struct{})}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.served.Swap(true) {
		<-l.done
		return nil, net.ErrClosed
	}
	return l.conn, nil
}

func (l *singleConnListener) Close() error {
	if !l.once.Swap(true) {
		close(l.done)
	}
	return l.conn.Close()
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// respRecorder captures the status code and response body byte count for
// logging without otherwise altering the response stream.
type respRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *respRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *respRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Hijack allows WebSocket / HTTP-upgrade proxying (ReverseProxy calls
// Hijack on connection upgrades).
func (r *respRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support Hijack")
	}
	return h.Hijack()
}

// Flush allows streaming responses (SSE) to propagate through the
// recorder without buffering.
func (r *respRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// perRequestSNITransport wraps an http.Transport so each request gets a
// TLS ClientConfig with ServerName set to the request's Host. This is
// necessary because the upstream node may serve multiple SAN'd domains
// from a single cert, and verification must match the host the agent
// targeted.
type perRequestSNITransport struct {
	base *http.Transport
	pool *x509.CertPool
}

func (t *perRequestSNITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	clone := t.base.Clone()
	clone.TLSClientConfig = &tls.Config{
		RootCAs:    t.pool,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
	return clone.RoundTrip(req)
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func hostFromRequest(r *http.Request, fallback string) string {
	if r == nil {
		return fallback
	}
	if r.Host != "" {
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			return h
		}
		return r.Host
	}
	return fallback
}

