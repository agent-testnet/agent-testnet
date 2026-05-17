#!/usr/bin/env bash
#
# Integration test: launches a Firecracker VM via the testnet-client daemon
# and verifies network isolation, plus the per-VM passthrough proxy feature
# (`testnet-client agent proxy add`).
#
# Must be run as root on the client host with the testnet-client daemon
# already running (e.g. `systemctl start testnet-client`).
#
# Reads domains from nodes.yaml (passed via NODES_YAML env or first argument,
# defaults to /opt/testnet/configs/nodes.yaml on the server install path).
#
# Tests:
#   1. VM boots and is SSH-reachable
#   2. DNS resolves each declared domain to a VIP
#   3. DNS resolves auto-name ({name}.testnet) to a VIP
#   4. DNS returns NXDOMAIN for undeclared domains
#   5. HTTPS to declared domains reaches the testnet node
#   6. HTTPS to the node /health endpoint works
#   7. Connections to undeclared domains are blocked
#   8. Connections to arbitrary external IPs are blocked
#   9. Public per-VM proxy (83.150.255.x) makes a real upstream reachable
#  10. Removing the proxy returns the VM to the previous "blocked" state
#
set -euo pipefail

AGENT_ID=""
GUEST_IP=""
SSH_KEY=""
PASS=0
FAIL=0
TOTAL=0

cleanup() {
    echo ""
    echo "==> Cleaning up..."
    if [ -n "$AGENT_ID" ]; then
        testnet-client agent stop "$AGENT_ID" 2>/dev/null || true
    fi
    echo "    Done."
}
trap cleanup EXIT

check() {
    local desc="$1"
    shift
    TOTAL=$((TOTAL + 1))
    if "$@" >/dev/null 2>&1; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        FAIL=$((FAIL + 1))
    fi
}

check_fail() {
    local desc="$1"
    shift
    TOTAL=$((TOTAL + 1))
    if "$@" >/dev/null 2>&1; then
        echo "  FAIL: $desc (should have failed but succeeded)"
        FAIL=$((FAIL + 1))
    else
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    fi
}

check_output() {
    local desc="$1"
    local expected="$2"
    shift 2
    TOTAL=$((TOTAL + 1))
    local output
    output=$("$@" 2>&1) || true
    if echo "$output" | grep -q "$expected"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected '$expected', got: ${output:0:200})"
        FAIL=$((FAIL + 1))
    fi
}

vm_ssh() {
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$SSH_KEY" "root@${GUEST_IP}" "$@"
}

echo "==> Testnet VM Integration Test"
echo ""

# ---- Preflight ----

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: Must be run as root" >&2
    exit 1
fi

if [ ! -e /dev/kvm ]; then
    echo "ERROR: /dev/kvm not found — Firecracker requires KVM" >&2
    exit 1
fi

if ! command -v testnet-client >/dev/null 2>&1; then
    echo "ERROR: testnet-client not found in PATH" >&2
    exit 1
fi

if ! ip link show wg-testnet >/dev/null 2>&1; then
    echo "ERROR: WireGuard tunnel (wg-testnet) not up. Run testnet-client setup first." >&2
    exit 1
fi

if ! testnet-client agent list >/dev/null 2>&1; then
    echo "ERROR: testnet-client daemon is not reachable (agent commands need the socket)." >&2
    echo "  Start it with: systemctl start testnet-client (or 'testnet-client daemon start')" >&2
    exit 1
fi

# ---- Load domains from nodes.yaml ----

NODES_FILE="${1:-${NODES_YAML:-/opt/testnet/configs/nodes.yaml}}"
if [ ! -f "$NODES_FILE" ]; then
    echo "ERROR: nodes.yaml not found at ${NODES_FILE}" >&2
    echo "  Pass it as an argument or set NODES_YAML=/path/to/nodes.yaml" >&2
    exit 1
fi

# Parse node names and domains from the YAML.
# Node names -> auto-names like "{name}.testnet"
# Domains -> explicitly declared domains
NODE_NAMES=()
DECLARED_DOMAINS=()
in_domains=false
while IFS= read -r line; do
    if echo "$line" | grep -qE '^\s*- name:'; then
        name=$(echo "$line" | sed 's/.*name: *"\{0,1\}\([^"]*\)"\{0,1\}.*/\1/' | tr -d '[:space:]')
        [ -n "$name" ] && NODE_NAMES+=("$name")
        in_domains=false
    elif echo "$line" | grep -qE '^\s*domains:'; then
        in_domains=true
    elif $in_domains && echo "$line" | grep -qE '^\s*- "'; then
        domain=$(echo "$line" | sed 's/.*- *"\([^"]*\)".*/\1/')
        [ -n "$domain" ] && DECLARED_DOMAINS+=("$domain")
    elif $in_domains && ! echo "$line" | grep -qE '^\s*-'; then
        in_domains=false
    fi
done < "$NODES_FILE"

if [ ${#NODE_NAMES[@]} -eq 0 ]; then
    echo "ERROR: No nodes found in ${NODES_FILE}" >&2
    exit 1
fi
if [ ${#DECLARED_DOMAINS[@]} -eq 0 ]; then
    echo "ERROR: No domains found in ${NODES_FILE}" >&2
    exit 1
fi

# The first declared domain is used for connectivity/health tests
FIRST_DOMAIN="${DECLARED_DOMAINS[0]}"
# Auto-names: {name}.testnet for each node
AUTO_NAMES=()
for n in "${NODE_NAMES[@]}"; do
    AUTO_NAMES+=("${n}.testnet")
done

echo "--- Config (from ${NODES_FILE}) ---"
echo "  Nodes:        ${NODE_NAMES[*]}"
echo "  Domains:      ${DECLARED_DOMAINS[*]}"
echo "  Auto-names:   ${AUTO_NAMES[*]}"
echo ""

# Stop any previously-launched test agents so a fresh run gets a fresh VM.
# We let the daemon do the heavy lifting (TAP cleanup, iptables, proxies)
# rather than mass-flushing iptables ourselves.
echo "--- Pre-launch cleanup ---"
EXISTING=$(testnet-client agent list --json 2>/dev/null | \
    python3 -c 'import json, sys
data = json.load(sys.stdin) or []
for a in data:
    print(a.get("id", ""))' || true)
for id in $EXISTING; do
    [ -z "$id" ] && continue
    echo "  Stopping leftover agent: $id"
    testnet-client agent stop "$id" 2>/dev/null || true
done
echo ""

echo "--- Launching agent VM ---"
# --standalone=false routes the launch through the systemd-managed daemon so
# the proxy tests below (which speak the unix socket) hit the same process
# that owns the agent.
LAUNCH_JSON=$(testnet-client agent launch --json --standalone=false 2>&1) || {
    echo "ERROR: testnet-client agent launch failed"
    echo "$LAUNCH_JSON"
    exit 1
}

AGENT_ID=$(echo "$LAUNCH_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])')
GUEST_IP=$(echo "$LAUNCH_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin)["tunnel_ip"])')
SSH_KEY=$(echo "$LAUNCH_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("ssh_key_path", ""))')

echo "  Agent ID:  $AGENT_ID"
echo "  Guest IP:  $GUEST_IP"
echo "  SSH key:   $SSH_KEY"
echo ""

# Wait for VM to be SSH-reachable (up to 60s)
echo "--- Waiting for VM SSH ---"
VM_READY=false
for i in $(seq 1 30); do
    if vm_ssh "echo ready" 2>/dev/null; then
        VM_READY=true
        break
    fi
    sleep 2
done

if ! $VM_READY; then
    echo "ERROR: VM not SSH-reachable after 60s"
    exit 1
fi
echo "  VM is ready."

# The testnet CA cert is at a known path inside the VM, injected during
# rootfs preparation. Alpine's update-ca-certificates may not bundle it
# correctly via chroot, so HTTPS tests use --cacert explicitly.
CA_CERT="/usr/local/share/ca-certificates/testnet/testnet-ca.crt"
echo ""

# ---- Tests ----

echo "--- DNS Tests (declared domains) ---"

for domain in "${DECLARED_DOMAINS[@]}"; do
    check_output "DNS: ${domain} resolves to a VIP (83.150.x.x)" "83\.150\." \
        vm_ssh "nslookup ${domain} 2>/dev/null | grep -o '83\.150\.[0-9]*\.[0-9]*'"
done

for autoname in "${AUTO_NAMES[@]}"; do
    check_output "DNS: ${autoname} (auto-name) resolves to a VIP" "83\.150\." \
        vm_ssh "nslookup ${autoname} 2>/dev/null | grep -o '83\.150\.[0-9]*\.[0-9]*'"
done

echo ""
echo "--- DNS Tests (undeclared domains — should fail) ---"

check_output "DNS: undeclared domain (twitter.com) -> NXDOMAIN" "NXDOMAIN\|can't find\|SERVFAIL\|server can" \
    vm_ssh "nslookup twitter.com 2>&1"

check_output "DNS: undeclared domain (evil.example.com) -> NXDOMAIN" "NXDOMAIN\|can't find\|SERVFAIL\|server can" \
    vm_ssh "nslookup evil.example.com 2>&1"

echo ""
echo "--- Connectivity Tests (should succeed) ---"

check "HTTPS to ${FIRST_DOMAIN} (declared domain, via node)" \
    vm_ssh "curl -sf --cacert $CA_CERT --max-time 10 https://${FIRST_DOMAIN}/"

check "HTTPS to ${FIRST_DOMAIN}/health (node health endpoint)" \
    vm_ssh "curl -sf --cacert $CA_CERT --max-time 10 https://${FIRST_DOMAIN}/health"

for domain in "${DECLARED_DOMAINS[@]:1}"; do
    check "HTTPS to ${domain} (declared domain)" \
        vm_ssh "curl -sf --cacert $CA_CERT --max-time 10 https://${domain}/"
done

for autoname in "${AUTO_NAMES[@]}"; do
    check "HTTPS to ${autoname} (auto-name domain)" \
        vm_ssh "curl -sf --cacert $CA_CERT --max-time 10 https://${autoname}/"
done

echo ""
echo "--- Isolation Tests (should be blocked) ---"

check_fail "Cannot reach undeclared domain (twitter.com)" \
    vm_ssh "curl -sf --max-time 5 https://twitter.com/ 2>/dev/null"

check_fail "Cannot reach undeclared domain (example.com)" \
    vm_ssh "curl -sf --max-time 5 http://example.com/ 2>/dev/null"

check_fail "Cannot reach arbitrary IP (8.8.8.8)" \
    vm_ssh "curl -sf --max-time 5 http://8.8.8.8/ 2>/dev/null"

check_fail "Cannot reach metadata service (169.254.169.254)" \
    vm_ssh "curl -sf --max-time 5 http://169.254.169.254/ 2>/dev/null"

check_fail "Cannot ping external host (1.1.1.1)" \
    vm_ssh "ping -c 1 -W 3 1.1.1.1 2>/dev/null"

echo ""
echo "--- Passthrough Proxy Tests ---"

# Use a real-but-stable external host as the proxy upstream. example.com is
# IANA-managed and answers HTTPS, which is enough to verify the TCP forwarder
# end-to-end.
PROXY_DOMAIN="example.com"

check_fail "(baseline) ${PROXY_DOMAIN} is unreachable from VM before proxy" \
    vm_ssh "curl -sf --max-time 5 https://${PROXY_DOMAIN}/ 2>/dev/null"

# Add a public proxy via the daemon. Capture the JSON to assert the IP came
# from the public passthrough range (83.150.255.x).
PROXY_ADD_JSON=$(testnet-client agent proxy add "$AGENT_ID" "$PROXY_DOMAIN" --visibility public --json 2>&1) || {
    echo "  FAIL: 'agent proxy add' command failed: $PROXY_ADD_JSON"
    FAIL=$((FAIL + 1))
    TOTAL=$((TOTAL + 1))
}
PROXY_IP=$(echo "$PROXY_ADD_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("ip", ""))' 2>/dev/null || echo "")

TOTAL=$((TOTAL + 1))
if [[ "$PROXY_IP" =~ ^83\.150\.255\.[0-9]+$ ]]; then
    echo "  PASS: public proxy IP is in 83.150.255.0/24 (got $PROXY_IP)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: expected public proxy IP in 83.150.255.0/24, got '$PROXY_IP'"
    FAIL=$((FAIL + 1))
fi

check_output "VM /etc/hosts has ${PROXY_DOMAIN} -> ${PROXY_IP}" "$PROXY_IP" \
    vm_ssh "grep ' ${PROXY_DOMAIN}\$' /etc/hosts"

# The TCP forwarder is dumb passthrough, so curl from inside the VM to
# https://example.com/ should reach the real upstream and validate the cert.
check "HTTPS to ${PROXY_DOMAIN} via public passthrough proxy" \
    vm_ssh "curl -sf --max-time 10 https://${PROXY_DOMAIN}/ 2>/dev/null"

# Now remove the proxy and confirm the VM is back to "isolated".
testnet-client agent proxy remove "$AGENT_ID" "$PROXY_DOMAIN" >/dev/null 2>&1 || true

check_fail "${PROXY_DOMAIN} is unreachable again after proxy removal" \
    vm_ssh "curl -sf --max-time 5 https://${PROXY_DOMAIN}/ 2>/dev/null"

# Also exercise the private (172.16.<vm>.x) flavour. Useful for verifying the
# private allocator works end-to-end even if no consumer in the test cares
# about the IP source.
PRIV_ADD_JSON=$(testnet-client agent proxy add "$AGENT_ID" "$PROXY_DOMAIN" --visibility private --json 2>&1) || {
    echo "  FAIL: private proxy add failed: $PRIV_ADD_JSON"
    FAIL=$((FAIL + 1))
    TOTAL=$((TOTAL + 1))
}
PRIV_IP=$(echo "$PRIV_ADD_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("ip", ""))' 2>/dev/null || echo "")

TOTAL=$((TOTAL + 1))
if [[ "$PRIV_IP" =~ ^172\.16\.[0-9]+\.[0-9]+$ ]]; then
    echo "  PASS: private proxy IP is in 172.16/12 (got $PRIV_IP)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: expected private proxy IP in 172.16/12, got '$PRIV_IP'"
    FAIL=$((FAIL + 1))
fi

check "HTTPS to ${PROXY_DOMAIN} via private passthrough proxy" \
    vm_ssh "curl -sf --max-time 10 https://${PROXY_DOMAIN}/ 2>/dev/null"

testnet-client agent proxy remove "$AGENT_ID" "$PROXY_DOMAIN" >/dev/null 2>&1 || true

echo ""
echo "==============================="
echo "Results: $PASS passed, $FAIL failed (of $TOTAL)"
echo "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
echo "All integration tests passed!"
