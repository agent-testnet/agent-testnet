package controlplane

import (
	"fmt"
	"sync"
	"time"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

// Registry manages client registration — the only dynamic entity.
type Registry struct {
	mu        sync.Mutex
	store     *Store
	auth      *Auth
	nextCIDR  int // next client /24 index (1-254)
}

func NewRegistry(store *Store, auth *Auth) *Registry {
	r := &Registry{
		store:    store,
		auth:     auth,
		nextCIDR: 1,
	}
	count := store.ClientCount()
	if count > 0 && count < 255 {
		r.nextCIDR = count + 1
	}
	return r
}

// RegisterClient validates the join token, allocates a tunnel /24, and returns config.
func (r *Registry) RegisterClient(joinToken, wgPubKey string) (*api.Client, string, error) {
	if !r.auth.ValidateJoinToken(joinToken) {
		return nil, "", fmt.Errorf("invalid join token")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.nextCIDR > 254 {
		return nil, "", fmt.Errorf("maximum clients reached (254)")
	}

	tunnelCIDR := fmt.Sprintf("10.99.%d.0/24", r.nextCIDR)

	apiToken, tokenHash, err := r.auth.IssueAPIToken()
	if err != nil {
		return nil, "", fmt.Errorf("issue API token: %w", err)
	}

	clientID := fmt.Sprintf("client-%d", r.nextCIDR)
	client := &api.Client{
		ID:          clientID,
		WGPublicKey: wgPubKey,
		TunnelCIDR:  tunnelCIDR,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if err := r.store.AddClient(client, tokenHash); err != nil {
		return nil, "", fmt.Errorf("store client: %w", err)
	}

	r.nextCIDR++
	return client, apiToken, nil
}

// DeregisterClient removes a client from the store, freeing its slot.
func (r *Registry) DeregisterClient(clientID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store.RemoveClient(clientID)
}

// ValidateAPIToken checks an API token against the store.
func (r *Registry) ValidateAPIToken(token string) (string, bool) {
	hash := HashToken(token)
	return r.store.ValidateTokenHash(hash)
}

// ListClients returns every client currently in the persistent store.
// Used at server startup to restore WireGuard peers (which live only in
// the in-kernel wg device and are lost across restarts).
func (r *Registry) ListClients() []*api.Client {
	return r.store.ListClients()
}
