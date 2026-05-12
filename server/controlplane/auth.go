package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Auth manages join tokens and API tokens.
type Auth struct {
	joinToken string
	dataDir   string
	nodes     *NodeManager
}

func NewAuth(dataDir string, nodes *NodeManager) (*Auth, error) {
	a := &Auth{
		dataDir: dataDir,
		nodes:   nodes,
	}
	if err := a.loadOrGenerateJoinToken(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Auth) loadOrGenerateJoinToken() error {
	tokenPath := filepath.Join(a.dataDir, "join-token")
	data, err := os.ReadFile(tokenPath)
	if err == nil {
		a.joinToken = strings.TrimSpace(string(data))
		return nil
	}

	token, err := generateToken(32)
	if err != nil {
		return fmt.Errorf("generate join token: %w", err)
	}
	a.joinToken = token

	if err := os.MkdirAll(a.dataDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return fmt.Errorf("persist join token: %w", err)
	}
	return nil
}

func (a *Auth) JoinToken() string {
	return a.joinToken
}

// ValidateJoinToken checks the client join token using constant-time comparison
// to prevent timing side-channel attacks.
func (a *Auth) ValidateJoinToken(token string) bool {
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.joinToken)) == 1
}

// ValidateNodeSecret checks a node's per-node secret using constant-time
// comparison to prevent timing side-channel attacks.
func (a *Auth) ValidateNodeSecret(name, secret string) bool {
	if a.nodes == nil || secret == "" {
		return false
	}
	node := a.nodes.GetNode(name)
	if node == nil || node.Secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(node.Secret)) == 1
}

// RotateJoinToken generates a new join token and persists it, invalidating
// the previous token. Returns the new token.
func (a *Auth) RotateJoinToken() (string, error) {
	token, err := generateToken(32)
	if err != nil {
		return "", fmt.Errorf("generate join token: %w", err)
	}
	tokenPath := filepath.Join(a.dataDir, "join-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("persist join token: %w", err)
	}
	a.joinToken = token
	return token, nil
}

// IssueAPIToken generates a new API token and returns (token, sha256Hash).
func (a *Auth) IssueAPIToken() (string, string, error) {
	token, err := generateToken(32)
	if err != nil {
		return "", "", err
	}
	return token, HashToken(token), nil
}

// HashToken returns the SHA-256 hex digest of a token.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func generateToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
