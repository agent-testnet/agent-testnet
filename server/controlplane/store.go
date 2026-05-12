package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/api"
)

// Store manages persistent and in-memory state for clients.
type Store struct {
	mu       sync.RWMutex
	clients  map[string]*api.Client    // clientID -> Client
	tokens   map[string]string         // apiToken hash -> clientID
	filePath string
}

// storeData is the on-disk serialization format.
type storeData struct {
	Clients []api.ClientPersist `json:"clients"`
}

func NewStore(dataDir string) (*Store, error) {
	fp := filepath.Join(dataDir, "state.json")
	s := &Store{
		clients:  make(map[string]*api.Client),
		tokens:   make(map[string]string),
		filePath: fp,
	}

	if _, err := os.Stat(fp); err == nil {
		if err := s.load(); err != nil {
			return nil, fmt.Errorf("load store: %w", err)
		}
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var sd storeData
	if err := json.Unmarshal(data, &sd); err != nil {
		return err
	}
	for _, cp := range sd.Clients {
		s.clients[cp.ID] = &api.Client{
			ID:          cp.ID,
			WGPublicKey: cp.WGPublicKey,
			TunnelCIDR:  cp.TunnelCIDR,
			CreatedAt:   cp.CreatedAt,
		}
		if cp.APITokenHash != "" {
			s.tokens[cp.APITokenHash] = cp.ID
		}
	}
	return nil
}

func (s *Store) flush() error {
	sd := storeData{}
	for _, c := range s.clients {
		cp := api.ClientPersist{
			ID:          c.ID,
			WGPublicKey: c.WGPublicKey,
			TunnelCIDR:  c.TunnelCIDR,
			CreatedAt:   c.CreatedAt,
		}
		// Find the token hash for this client
		for hash, cid := range s.tokens {
			if cid == c.ID {
				cp.APITokenHash = hash
				break
			}
		}
		sd.Clients = append(sd.Clients, cp)
	}

	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o600)
}

func (s *Store) AddClient(c *api.Client, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ID] = c
	s.tokens[tokenHash] = c.ID
	return s.flush()
}

// RemoveClient removes a client and its API token from the store.
func (s *Store) RemoveClient(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.clients[id]; !exists {
		return fmt.Errorf("client %s not found", id)
	}

	for hash, cid := range s.tokens {
		if cid == id {
			delete(s.tokens, hash)
			break
		}
	}
	delete(s.clients, id)
	return s.flush()
}

func (s *Store) GetClient(id string) *api.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clients[id]
}

func (s *Store) ListClients() []*api.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*api.Client, 0, len(s.clients))
	for _, c := range s.clients {
		result = append(result, c)
	}
	return result
}

func (s *Store) ValidateTokenHash(hash string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cid, ok := s.tokens[hash]
	return cid, ok
}

func (s *Store) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}
