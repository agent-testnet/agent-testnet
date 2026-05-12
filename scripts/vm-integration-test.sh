#!/usr/bin/env bash
#
# Integration test: launches a Firecracker VM inside the testnet client and
# verifies network isolation from the agent's perspective.
#
# Must be run as root on the client host (where testnet-client is installed
# and setup has completed).
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
#
set -euo pipefail

AGENT_PID=""
GUEST_IP=""
SSH_KEY=""
LAUNCH_LOG=""
PASS=0
FAIL=0
TOTAL=0

cleanup() {
    echo ""
    echo "==> Cleaning up..."
    if [ -n "$AGENT_PID" ]; then
        kill "$AGENT_PID" 2>/dev/null || true
        wait "$AGENT_PID" 2>/dev/null || true
    fi
    # Remove TAP device and iptables rules left by the VM in case the agent
    # process didn't shut down cleanly (the issue that caused "Device or
    # resource busy" on re-runs).
    for tap in $(ip -o link show 2>/dev/null | grep -oP 'tap-\d+' || true); do
        ip link del dev "$tap" 2>/dev/null || true
    done
    iptables -F FORWARD 2>/dev/null || true
    iptables -t nat -F PREROUTING 2>/dev/null || true
    iptables -t nat -F POSTROUTING 2>/dev/null || true
    [ -n "$LAUNCH_LOG" ] && rm -f "$LAUNCH_LOG"
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

# Clean up stale resources from previous runs (TAP devices, iptables rules,
# agent directories) so the launch doesn't fail with "Device or resource busy".
echo "--- Pre-launch cleanup ---"
for tap in $(ip -o link show 2>/dev/null | grep -oP 'tap-\d+' || true); do
    echo "  Removing stale TAP: $tap"
    ip link del dev "$tap" 2>/dev/null || true
done
iptables -F FORWARD 2>/dev/null || true
iptables -t nat -F PREROUTING 2>/dev/null || true
iptables -t nat -F POSTROUTING 2>/dev/null || true
rm -rf /root/.testnet/data/agents/agent-* 2>/dev/null || true
echo ""

echo "--- Launching agent VM ---"
LAUNCH_LOG=$(mktemp)
testnet-client agent launch --standalone > "$LAUNCH_LOG" 2>&1 &
AGENT_PID=$!

# Wait for the launch output to contain SSH info (up to 30s)
for i in $(seq 1 30); do
    if grep -q "SSH:" "$LAUNCH_LOG" 2>/dev/null; then
        break
    fi
    if ! kill -0 "$AGENT_PID" 2>/dev/null; then
        echo "ERROR: Agent process died during launch"
        cat "$LAUNCH_LOG"
        exit 1
    fi
    sleep 1
done

if ! grep -q "SSH:" "$LAUNCH_LOG"; then
    echo "ERROR: VM did not launch within 30s"
    cat "$LAUNCH_LOG"
    exit 1
fi

GUEST_IP=$(grep "Guest IP:" "$LAUNCH_LOG" | awk '{print $NF}')
SSH_KEY=$(grep "SSH:" "$LAUNCH_LOG" | awk '{for(i=1;i<=NF;i++) if($i=="-i") print $(i+1)}')
AGENT_ID=$(grep "ID:" "$LAUNCH_LOG" | head -1 | awk '{print $NF}')

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
    echo "Agent log:"
    cat "$LAUNCH_LOG"
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
echo "==============================="
echo "Results: $PASS passed, $FAIL failed (of $TOTAL)"
echo "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
echo "All integration tests passed!"
