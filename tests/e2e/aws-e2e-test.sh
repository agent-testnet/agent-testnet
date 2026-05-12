#!/usr/bin/env bash
#
# End-to-end AWS test for Agent Testnet.
#
# Deploys a complete testnet stack to temporary AWS instances using isolated
# resource names (unique prefix, separate state file, separate key pair) so
# it never interferes with production deployments.
#
# Usage:
#   bash tests/e2e/aws-e2e-test.sh            # Full cycle: deploy, test, teardown
#   SKIP_TEARDOWN=1 bash tests/e2e/aws-e2e-test.sh  # Keep stack for debugging
#
# Prerequisites:
#   - AWS CLI configured (aws sts get-caller-identity)
#   - Go 1.25+ for cross-compilation
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
DEPLOY_SCRIPT="${PROJECT_DIR}/deploy/aws-deploy.sh"

TIMESTAMP="$(date +%s)"
RUN_ID="e2e-${TIMESTAMP}"

export STACK_PREFIX="tn-${RUN_ID}"
export STACK_VALUE="agent-testnet-e2e"
export STATE_FILE="${SCRIPT_DIR}/.aws-state-${RUN_ID}.json"
export KEY_FILE="${SCRIPT_DIR}/.aws-${RUN_ID}-key.pem"
export KEY_NAME="${STACK_PREFIX}-deploy-key"
export NODES_YAML_SRC="${SCRIPT_DIR}/nodes-test.yaml"

PASS=0
FAIL=0
SKIP_TEARDOWN="${SKIP_TEARDOWN:-}"

info()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33mWARN:\033[0m %s\n" "$*"; }
err()   { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; }

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

cleanup() {
    echo ""
    if [ -n "$SKIP_TEARDOWN" ]; then
        warn "SKIP_TEARDOWN is set -- leaving stack running."
        info "Stack prefix: ${STACK_PREFIX}"
        info "State file:   ${STATE_FILE}"
        info "To tear down: STATE_FILE=${STATE_FILE} STACK_PREFIX=${STACK_PREFIX} KEY_NAME=${KEY_NAME} KEY_FILE=${KEY_FILE} bash ${DEPLOY_SCRIPT} teardown"
        return
    fi
    info "Tearing down test stack (${STACK_PREFIX})..."
    bash "$DEPLOY_SCRIPT" teardown || warn "Teardown had errors (resources may need manual cleanup)"
    rm -f "$STATE_FILE" "$KEY_FILE"
    info "Cleanup complete."
}
trap cleanup EXIT

# ---- Preflight ----

info "E2E Test Run: ${RUN_ID}"
info "Stack prefix: ${STACK_PREFIX}"
info "State file:   ${STATE_FILE}"
info "Nodes config: ${NODES_YAML_SRC}"
echo ""

if [ ! -f "$DEPLOY_SCRIPT" ]; then
    err "Deploy script not found: ${DEPLOY_SCRIPT}"
    exit 1
fi
if [ ! -f "$NODES_YAML_SRC" ]; then
    err "Test nodes config not found: ${NODES_YAML_SRC}"
    exit 1
fi
if ! aws sts get-caller-identity >/dev/null 2>&1; then
    err "AWS CLI not configured. Run: aws configure"
    exit 1
fi

# ---- Deploy ----

info "Phase 1: Deploy"
echo ""
if ! bash "$DEPLOY_SCRIPT" deploy; then
    err "Deploy failed"
    exit 1
fi
echo ""

# ---- Status / Health Check ----

info "Phase 2: Health check"
echo ""
check "Status command succeeds" bash "$DEPLOY_SCRIPT" status
echo ""

# ---- Integration Test ----

info "Phase 3: Integration test (Firecracker VM)"
echo ""
if bash "$DEPLOY_SCRIPT" test; then
    echo "  PASS: VM integration test"
    PASS=$((PASS + 1))
else
    echo "  FAIL: VM integration test"
    FAIL=$((FAIL + 1))
fi

# ---- Summary ----

echo ""
echo "==============================="
echo "E2E Results: $PASS passed, $FAIL failed"
echo "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
echo "All E2E tests passed!"
