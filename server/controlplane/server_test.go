package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
)

func setupTestServer(t *testing.T) (*ControlPlane, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")

	nodesYAML := `nodes:
  - name: "test-node"
    address: "1.2.3.4:443"
    secret: "test-node-secret"
    domains:
      - "example.com"
      - "www.example.com"
`
	nodesFile := filepath.Join(dir, "nodes.yaml")
	if err := os.WriteFile(nodesFile, []byte(nodesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ServerConfig{
		ControlPlane: config.ControlPlaneConfig{
			Listen:    ":0",
			DataDir:   dataDir,
			NodesFile: nodesFile,
			TLS: config.TLSConfig{
				CertFile: filepath.Join(dataDir, "api-cert.pem"),
				KeyFile:  filepath.Join(dataDir, "api-key.pem"),
			},
			CA: config.CAConfig{
				KeyFile:  filepath.Join(dataDir, "ca-key.pem"),
				CertFile: filepath.Join(dataDir, "ca-cert.pem"),
			},
		},
		WireGuard: config.WireGuardConfig{
			TunnelIP: "10.99.0.1/16",
		},
		VIP: config.VIPConfig{
			Subnet: "83.150.0.0/16",
			DNSVIP: "83.150.0.1",
		},
	}

	cp, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cp.SetWGPublicKey("dGVzdC13Zy1wdWJsaWMta2V5LWJhc2U2NC1lbmM=")

	ts := httptest.NewServer(cp.Handler())
	t.Cleanup(ts.Close)
	return cp, ts
}

// validWGKey returns a valid 32-byte base64-encoded WireGuard key.
func validWGKey() string {
	return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
}

func TestAPI_GetCACert(t *testing.T) {
	_, ts := setupTestServer(t)

	resp, err := http.Get(ts.URL + "/api/v1/ca/root")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body []byte
	body, err = readAll(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "BEGIN CERTIFICATE") {
		t.Fatal("expected PEM certificate in response")
	}
}

func TestAPI_Register(t *testing.T) {
	cp, ts := setupTestServer(t)

	body := `{"wg_public_key": "` + validWGKey() + `"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/clients/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cp.auth.JoinToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readAll(resp)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var reg api.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	if reg.ClientID == "" {
		t.Fatal("expected non-empty client_id")
	}
	if reg.APIToken == "" {
		t.Fatal("expected non-empty api_token")
	}
	if reg.TunnelCIDR == "" {
		t.Fatal("expected non-empty tunnel_cidr")
	}
	if reg.CACert == "" {
		t.Fatal("expected non-empty ca_cert")
	}
}

func TestAPI_RegisterBadToken(t *testing.T) {
	_, ts := setupTestServer(t)

	body := `{"wg_public_key": "` + validWGKey() + `"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/clients/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAPI_RegisterMissingKey(t *testing.T) {
	cp, ts := setupTestServer(t)

	body := `{}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/clients/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cp.auth.JoinToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestAPI_RegisterNoAuth(t *testing.T) {
	_, ts := setupTestServer(t)

	body := `{"wg_public_key": "` + validWGKey() + `"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/clients/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func registerTestClient(t *testing.T, cp *ControlPlane, tsURL string) api.RegisterResponse {
	t.Helper()
	body := `{"wg_public_key": "` + validWGKey() + `"}`
	req, _ := http.NewRequest("POST", tsURL+"/api/v1/clients/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cp.auth.JoinToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readAll(resp)
		t.Fatalf("registration failed: %d %s", resp.StatusCode, string(b))
	}

	var reg api.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestAPI_ListNodes(t *testing.T) {
	cp, ts := setupTestServer(t)
	reg := registerTestClient(t, cp, ts.URL)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+reg.APIToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var nodes []api.NodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Name != "test-node" {
		t.Fatalf("expected test-node, got %s", nodes[0].Name)
	}
}

func TestAPI_ListNodesUnauthed(t *testing.T) {
	_, ts := setupTestServer(t)

	resp, err := http.Get(ts.URL + "/api/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAPI_ListDomains(t *testing.T) {
	cp, ts := setupTestServer(t)
	reg := registerTestClient(t, cp, ts.URL)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/domains", nil)
	req.Header.Set("Authorization", "Bearer "+reg.APIToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var mappings []api.DomainMapping
	if err := json.NewDecoder(resp.Body).Decode(&mappings); err != nil {
		t.Fatal(err)
	}
	// test-node has 2 domains + auto-name = 3 mappings
	if len(mappings) != 3 {
		t.Fatalf("expected 3 domain mappings, got %d", len(mappings))
	}
}

func TestAPI_GetNodeCerts(t *testing.T) {
	_, ts := setupTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/nodes/test-node/certs", nil)
	req.Header.Set("Authorization", "Bearer test-node-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readAll(resp)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var certResp api.CertResponse
	if err := json.NewDecoder(resp.Body).Decode(&certResp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(certResp.CertPEM, "BEGIN CERTIFICATE") {
		t.Fatal("expected PEM cert")
	}
	if !strings.Contains(certResp.KeyPEM, "BEGIN EC PRIVATE KEY") {
		t.Fatal("expected PEM key")
	}
}

func TestAPI_GetNodeCertsBadSecret(t *testing.T) {
	_, ts := setupTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/nodes/test-node/certs", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAPI_Deregister(t *testing.T) {
	cp, ts := setupTestServer(t)
	reg := registerTestClient(t, cp, ts.URL)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/clients/self", nil)
	req.Header.Set("Authorization", "Bearer "+reg.APIToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readAll(resp)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	// Verify the token is now invalid
	req2, _ := http.NewRequest("GET", ts.URL+"/api/v1/nodes", nil)
	req2.Header.Set("Authorization", "Bearer "+reg.APIToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after deregister, got %d", resp2.StatusCode)
	}
}

func TestAPI_RotateJoinToken(t *testing.T) {
	cp, ts := setupTestServer(t)
	oldToken := cp.auth.JoinToken()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/admin/rotate-join-token", nil)
	req.Header.Set("Authorization", "Bearer "+oldToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readAll(resp)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	newToken := result["join_token"]
	if newToken == "" {
		t.Fatal("expected non-empty new join token")
	}
	if newToken == oldToken {
		t.Fatal("expected rotated token to differ")
	}

	// Old token should no longer work for registration
	body := `{"wg_public_key": "` + validWGKey() + `"}`
	req2, _ := http.NewRequest("POST", ts.URL+"/api/v1/clients/register", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+oldToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with old token, got %d", resp2.StatusCode)
	}
}

func readAll(resp *http.Response) ([]byte, error) {
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
