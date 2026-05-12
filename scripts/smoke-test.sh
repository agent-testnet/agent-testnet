#!/bin/bash
set -euo pipefail

# End-to-end smoke test for the testnet MVP.
# Requires: the three binaries in ./bin/, root privileges.
#
# What it tests:
#   1. Server starts and generates a join token
#   2. Control plane API responds (HTTPS)
#   3. CA cert is downloadable
#   4. Client can register with join token
#   5. Node can fetch TLS certificates
#   6. DNS resolves declared domains to VIPs
#   7. DNS returns NXDOMAIN for undeclared domains
#
# Note: Full tunnel + VM tests require /dev/kvm and root. This script
# covers the API-level smoke test that works without those.

BIN_DIR="./bin"
DATA_DIR="$(mktemp -d)/testnet-smoke"
SERVER_PID=""
NODE_PID=""

cleanup() {
    echo ""
    echo "==> Cleaning up..."
    [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
    rm -rf "$DATA_DIR"
    echo "    Done."
}
trap cleanup EXIT

PASS=0
FAIL=0

check() {
    local desc="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        FAIL=$((FAIL + 1))
    fi
}

check_output() {
    local desc="$1"
    local expected="$2"
    shift 2
    local output
    output=$("$@" 2>/dev/null) || true
    if echo "$output" | grep -q "$expected"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected '$expected', got: $output)"
        FAIL=$((FAIL + 1))
    fi
}

echo "==> Testnet MVP Smoke Test"
echo ""

# Prepare data directory
mkdir -p "$DATA_DIR"

# Write test nodes.yaml
cat > "$DATA_DIR/nodes.yaml" <<YAML
nodes:
  - name: "google-node"
    address: "198.51.100.5:443"
    secret: "gn-test-secret"
    domains:
      - "google.com"
      - "youtube.com"
  - name: "github-node"
    address: "198.51.100.20:443"
    secret: "gh-test-secret"
    domains:
      - "github.com"
YAML

# Write server config pointing to temp data dir
cat > "$DATA_DIR/server.yaml" <<YAML
controlplane:
  listen: ":18443"
  data_dir: "${DATA_DIR}/data"
  nodes_file: "${DATA_DIR}/nodes.yaml"
  tls:
    cert_file: "${DATA_DIR}/data/api-cert.pem"
    key_file: "${DATA_DIR}/data/api-key.pem"
  ca:
    key_file: "${DATA_DIR}/data/ca-key.pem"
    cert_file: "${DATA_DIR}/data/ca-cert.pem"

dns:
  listen_tunnel: ""
  listen_public: ":15353"
  refresh_interval: "5s"

wireguard:
  listen_port: 0
  tunnel_ip: "10.99.0.1/16"
  private_key_file: "${DATA_DIR}/data/wg-key"

router:
  log_file: "${DATA_DIR}/data/traffic.log"

vip:
  subnet: "83.150.0.0/16"
  dns_vip: "83.150.0.1"
YAML

echo "--- Starting server ---"
"$BIN_DIR/testnet-server" --config "$DATA_DIR/server.yaml" > "$DATA_DIR/server.log" 2>&1 &
SERVER_PID=$!
sleep 2

# Check server started
if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "  FAIL: Server failed to start"
    echo "  Server log:"
    cat "$DATA_DIR/server.log"
    exit 1
fi
echo "  Server started (PID: $SERVER_PID)"

# Extract join token from data dir (logs now redact the token)
JOIN_TOKEN_FILE="$DATA_DIR/data/join-token"
if [ ! -f "$JOIN_TOKEN_FILE" ]; then
    echo "  FAIL: Join token file not found at $JOIN_TOKEN_FILE"
    cat "$DATA_DIR/server.log"
    exit 1
fi
JOIN_TOKEN=$(cat "$JOIN_TOKEN_FILE")
echo "  Join token: ${JOIN_TOKEN:0:8}..."

SERVER_URL="https://localhost:18443"

echo ""
echo "--- API Tests ---"

# Test: CA cert endpoint (no auth)
check "GET /api/v1/ca/root (no auth)" \
    curl -sk "$SERVER_URL/api/v1/ca/root"

# Test: CA cert is valid PEM
check_output "CA cert is PEM" "BEGIN CERTIFICATE" \
    curl -sk "$SERVER_URL/api/v1/ca/root"

# Test: Register client
REGISTER_RESP=$(curl -sk -X POST "$SERVER_URL/api/v1/clients/register" \
    -H "Authorization: Bearer $JOIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"wg_public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}' 2>/dev/null)

if echo "$REGISTER_RESP" | grep -q "client_id"; then
    echo "  PASS: POST /api/v1/clients/register"
    PASS=$((PASS + 1))
else
    echo "  FAIL: POST /api/v1/clients/register (response: $REGISTER_RESP)"
    FAIL=$((FAIL + 1))
fi

# Extract API token
API_TOKEN=$(echo "$REGISTER_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['api_token'])" 2>/dev/null || true)

# Test: Register with bad token fails
check_output "Register with bad token -> 401" "invalid join token" \
    curl -sk -X POST "$SERVER_URL/api/v1/clients/register" \
    -H "Authorization: Bearer bad-token" \
    -H "Content-Type: application/json" \
    -d '{"wg_public_key": "dGVzdA=="}'

# Test: List nodes (requires API token)
if [ -n "$API_TOKEN" ]; then
    check_output "GET /api/v1/nodes (with API token)" "google-node" \
        curl -sk "$SERVER_URL/api/v1/nodes" -H "Authorization: Bearer $API_TOKEN"

    check_output "GET /api/v1/domains (with API token)" "google.com" \
        curl -sk "$SERVER_URL/api/v1/domains" -H "Authorization: Bearer $API_TOKEN"
else
    echo "  SKIP: Node/domain listing (no API token)"
fi

# Test: List nodes without token fails
check_output "GET /api/v1/nodes without token -> 401" "missing API token" \
    curl -sk "$SERVER_URL/api/v1/nodes"

# Test: Fetch node certs with node secret
NODE_SECRET="gn-test-secret"
CERT_RESP=$(curl -sk "$SERVER_URL/api/v1/nodes/google-node/certs" \
    -H "Authorization: Bearer $NODE_SECRET" 2>/dev/null)

if echo "$CERT_RESP" | grep -q "cert_pem"; then
    echo "  PASS: GET /api/v1/nodes/google-node/certs (node secret)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: GET /api/v1/nodes/google-node/certs (response: $CERT_RESP)"
    FAIL=$((FAIL + 1))
fi

# Test: Fetch certs with wrong secret fails
check_output "Node certs with bad secret -> 401" "invalid node secret" \
    curl -sk "$SERVER_URL/api/v1/nodes/google-node/certs" \
    -H "Authorization: Bearer wrong-secret"

echo ""
echo "--- DNS Tests ---"

# Test: DNS resolves registered domain
check_output "DNS: google.com -> VIP" "83.150" \
    dig @127.0.0.1 -p 15353 google.com A +short +time=2 +tries=1

check_output "DNS: github.com -> VIP" "83.150" \
    dig @127.0.0.1 -p 15353 github.com A +short +time=2 +tries=1

check_output "DNS: google-node.testnet -> VIP" "83.150" \
    dig @127.0.0.1 -p 15353 google-node.testnet A +short +time=2 +tries=1

# Test: DNS returns NXDOMAIN for unregistered domain
NXDOMAIN_RESP=$(dig @127.0.0.1 -p 15353 evil.com A +time=2 +tries=1 2>/dev/null)
if echo "$NXDOMAIN_RESP" | grep -q "NXDOMAIN"; then
    echo "  PASS: DNS: evil.com -> NXDOMAIN"
    PASS=$((PASS + 1))
else
    echo "  FAIL: DNS: evil.com should return NXDOMAIN"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "==============================="
echo "Results: $PASS passed, $FAIL failed"
echo "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
echo "All smoke tests passed!"
