package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
)

// PeerManager is the interface the control plane uses to manage WireGuard peers.
// Implemented by wg.Endpoint; decoupled to avoid import cycles.
type PeerManager interface {
	AddPeer(pubKey, allowedIPs string) error
}

// ControlPlane is the main server-side control plane.
type ControlPlane struct {
	cfg      *config.ServerConfig
	store    *Store
	auth     *Auth
	ca       *CA
	nodes    *NodeManager
	vipAlloc *VIPAllocator
	registry *Registry
	mux      *http.ServeMux
	wgPubKey string       // set by WireGuard endpoint after init
	peerMgr  PeerManager  // set after WG endpoint init
	limiter  *rateLimiter
}

// New initializes all control plane components.
func New(cfg *config.ServerConfig) (*ControlPlane, error) {
	if err := os.MkdirAll(cfg.ControlPlane.DataDir, 0o700); err != nil {
		return nil, err
	}

	vipAlloc, err := NewVIPAllocator(cfg.VIP.Subnet, cfg.VIP.DNSVIP)
	if err != nil {
		return nil, err
	}

	nodes := NewNodeManager(cfg.ControlPlane.NodesFile, vipAlloc)

	ca, err := NewCA(cfg.ControlPlane.DataDir, cfg.ControlPlane.CA.KeyFile, cfg.ControlPlane.CA.CertFile)
	if err != nil {
		return nil, err
	}

	if err := ca.GenerateAPICert(cfg.ControlPlane.TLS.CertFile, cfg.ControlPlane.TLS.KeyFile); err != nil {
		return nil, err
	}

	auth, err := NewAuth(cfg.ControlPlane.DataDir, nodes)
	if err != nil {
		return nil, err
	}

	store, err := NewStore(cfg.ControlPlane.DataDir)
	if err != nil {
		return nil, err
	}

	registry := NewRegistry(store, auth)

	if err := nodes.Load(); err != nil {
		return nil, err
	}

	cp := &ControlPlane{
		cfg:      cfg,
		store:    store,
		auth:     auth,
		ca:       ca,
		nodes:    nodes,
		vipAlloc: vipAlloc,
		registry: registry,
		limiter:  newRateLimiter(20, time.Minute),
	}
	cp.setupRoutes()

	log.Printf("[controlplane] join token available at %s/join-token", cfg.ControlPlane.DataDir)
	log.Printf("[controlplane] AGPL-3.0-or-later — source: %s (version=%s commit=%s)", sourceURL(), Version, Commit)
	return cp, nil
}

// SetWGPublicKey stores the server's WireGuard public key for registration responses.
func (cp *ControlPlane) SetWGPublicKey(key string) {
	cp.wgPubKey = key
}

// SetPeerManager sets the WireGuard peer manager used to auto-add peers on registration.
func (cp *ControlPlane) SetPeerManager(pm PeerManager) {
	cp.peerMgr = pm
}

// Nodes returns the node manager.
func (cp *ControlPlane) Nodes() *NodeManager {
	return cp.nodes
}

// CA returns the certificate authority.
func (cp *ControlPlane) CA() *CA {
	return cp.ca
}

// Registry returns the client registry.
func (cp *ControlPlane) Registry() *Registry {
	return cp.registry
}

func (cp *ControlPlane) setupRoutes() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /source", cp.handleSource)
	mux.HandleFunc("GET /api/v1/ca/root", cp.handleGetCACert)
	mux.HandleFunc("POST /api/v1/clients/register", cp.limiter.wrap(cp.handleRegister))
	mux.HandleFunc("GET /api/v1/nodes/{name}/certs", cp.limiter.wrap(cp.handleGetNodeCerts))
	mux.HandleFunc("GET /api/v1/nodes", cp.handleListNodes)
	mux.HandleFunc("GET /api/v1/domains", cp.handleListDomains)
	mux.HandleFunc("DELETE /api/v1/clients/self", cp.limiter.wrap(cp.handleDeregister))
	mux.HandleFunc("POST /api/v1/admin/rotate-join-token", cp.limiter.wrap(cp.handleRotateJoinToken))

	cp.mux = mux
}

// Handler returns the HTTP handler for the control plane (useful for testing).
func (cp *ControlPlane) Handler() http.Handler { return cp.mux }

// ListenAndServe starts the HTTPS API server.
func (cp *ControlPlane) ListenAndServe() error {
	server := &http.Server{
		Addr:              cp.cfg.ControlPlane.Listen,
		Handler:           cp.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("[controlplane] listening on %s (HTTPS)", cp.cfg.ControlPlane.Listen)
	return server.ListenAndServeTLS(cp.cfg.ControlPlane.TLS.CertFile, cp.cfg.ControlPlane.TLS.KeyFile)
}

// ReloadNodes re-reads nodes.yaml.
func (cp *ControlPlane) ReloadNodes() error {
	return cp.nodes.Load()
}

func (cp *ControlPlane) handleGetCACert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(cp.ca.RootCertPEM())
}

func (cp *ControlPlane) handleRegister(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "missing join token", http.StatusUnauthorized)
		return
	}

	var req api.RegisterRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.WGPublicKey == "" {
		http.Error(w, "missing wg_public_key", http.StatusBadRequest)
		return
	}
	if !isValidWGKey(req.WGPublicKey) {
		http.Error(w, "invalid wg_public_key: must be 44-char base64 (32 bytes)", http.StatusBadRequest)
		return
	}

	client, apiToken, err := cp.registry.RegisterClient(token, req.WGPublicKey)
	if err != nil {
		if strings.Contains(err.Error(), "invalid join token") {
			http.Error(w, "invalid join token", http.StatusUnauthorized)
		} else {
			log.Printf("[controlplane] registration error: %v", err)
			http.Error(w, "registration failed", http.StatusInternalServerError)
		}
		return
	}

	resp := api.RegisterResponse{
		ClientID:     client.ID,
		APIToken:     apiToken,
		TunnelCIDR:   client.TunnelCIDR,
		ServerWGKey:  cp.wgPubKey,
		ServerWGAddr: cp.cfg.WireGuard.TunnelIP,
		DNSIP:        cp.cfg.VIP.DNSVIP,
		CACert:       string(cp.ca.RootCertPEM()),
	}

	// Auto-add WireGuard peer so the client can connect immediately
	if cp.peerMgr != nil {
		if err := cp.peerMgr.AddPeer(req.WGPublicKey, client.TunnelCIDR); err != nil {
			log.Printf("[controlplane] WARNING: failed to add WG peer for %s: %v", client.ID, err)
		}
	}

	log.Printf("[controlplane] registered client %s (tunnel: %s)", client.ID, client.TunnelCIDR)
	writeJSON(w, http.StatusOK, resp)
}

func (cp *ControlPlane) handleGetNodeCerts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	secret := extractBearerToken(r)

	if !cp.auth.ValidateNodeSecret(name, secret) {
		http.Error(w, "invalid node secret", http.StatusUnauthorized)
		return
	}

	node := cp.nodes.GetNode(name)
	if node == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	certPEM, keyPEM, err := cp.ca.IssueCert(name, node.Domains)
	if err != nil {
		log.Printf("[controlplane] cert generation failed for node %s: %v", name, err)
		http.Error(w, "cert generation failed", http.StatusInternalServerError)
		return
	}

	resp := api.CertResponse{
		CertPEM: string(certPEM),
		KeyPEM:  string(keyPEM),
		CAPEM:   string(cp.ca.RootCertPEM()),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (cp *ControlPlane) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if !cp.validateAPIToken(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, cp.nodes.AllNodes())
}

func (cp *ControlPlane) handleListDomains(w http.ResponseWriter, r *http.Request) {
	if !cp.validateAPIToken(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, cp.nodes.AllDomainMappings())
}

func (cp *ControlPlane) validateAPIToken(w http.ResponseWriter, r *http.Request) bool {
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "missing API token", http.StatusUnauthorized)
		return false
	}
	if _, ok := cp.registry.ValidateAPIToken(token); !ok {
		http.Error(w, "invalid API token", http.StatusUnauthorized)
		return false
	}
	return true
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func (cp *ControlPlane) handleDeregister(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "missing API token", http.StatusUnauthorized)
		return
	}
	clientID, ok := cp.registry.ValidateAPIToken(token)
	if !ok {
		http.Error(w, "invalid API token", http.StatusUnauthorized)
		return
	}

	if err := cp.registry.DeregisterClient(clientID); err != nil {
		log.Printf("[controlplane] deregistration error for %s: %v", clientID, err)
		http.Error(w, "deregistration failed", http.StatusInternalServerError)
		return
	}

	log.Printf("[controlplane] deregistered client %s", clientID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deregistered"})
}

func (cp *ControlPlane) handleRotateJoinToken(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "missing join token", http.StatusUnauthorized)
		return
	}
	if !cp.auth.ValidateJoinToken(token) {
		http.Error(w, "invalid join token", http.StatusUnauthorized)
		return
	}

	newToken, err := cp.auth.RotateJoinToken()
	if err != nil {
		log.Printf("[controlplane] join token rotation error: %v", err)
		http.Error(w, "token rotation failed", http.StatusInternalServerError)
		return
	}

	log.Printf("[controlplane] join token rotated")
	writeJSON(w, http.StatusOK, map[string]string{"join_token": newToken})
}

func isValidWGKey(key string) bool {
	b, err := base64.StdEncoding.DecodeString(key)
	return err == nil && len(b) == 32
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
