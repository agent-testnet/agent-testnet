#!/usr/bin/env bash
#
# Deploy Agent Testnet to Vultr using vultr-cli (v3).
#
# Usage:
#   Full demo (server + node + one client named "0"):
#     bash deploy/vultr-deploy.sh deploy
#
#   Tier-by-tier deploy (each is idempotent and reuses shared state):
#     bash deploy/vultr-deploy.sh server-deploy
#     bash deploy/vultr-deploy.sh node-deploy
#     bash deploy/vultr-deploy.sh client-deploy [--client NAME]
#                                                [--server-url URL] [--join-token TOK]
#                                                [--plan PLAN]
#     bash deploy/vultr-deploy.sh client-remove --client NAME
#
#   Operations (subcommands targeting clients support --client NAME; when only
#   one client exists it is auto-selected):
#     bash deploy/vultr-deploy.sh status
#     bash deploy/vultr-deploy.sh ssh <role> [--client NAME]
#     bash deploy/vultr-deploy.sh restart <role> [--client NAME]
#     bash deploy/vultr-deploy.sh redeploy <role> [--client NAME]
#     bash deploy/vultr-deploy.sh logs <role> [--client NAME]
#     bash deploy/vultr-deploy.sh reload                   # server-only
#     bash deploy/vultr-deploy.sh test [--client NAME]
#     bash deploy/vultr-deploy.sh openclaw <sub> [--client NAME] [...]
#     bash deploy/vultr-deploy.sh teardown [--full]        # tears down clients + server + node
#
# OpenClaw subcommands (shared with aws-deploy.sh, see deploy/lib/common.sh):
#   bash deploy/vultr-deploy.sh openclaw install --api-key KEY [--provider anthropic|openai|xai|openrouter] [--openclaw-version 2026.5.7] [--persona NAME]
#   bash deploy/vultr-deploy.sh openclaw chat
#   bash deploy/vultr-deploy.sh openclaw status
#   bash deploy/vultr-deploy.sh openclaw stop
#   bash deploy/vultr-deploy.sh openclaw reconfig --api-key KEY --provider openrouter --model anthropic/claude-haiku-4.5
#   bash deploy/vultr-deploy.sh openclaw reconfig --client 1 --persona mrsmith --persona-confirm
#
# Joining an existing testnet (e.g. a Vultr client joining an AWS-hosted server):
#   bash deploy/vultr-deploy.sh client-deploy \
#     --server-url https://<server>:8443 --join-token <tok>
#
# Prerequisites:
#   - vultr-cli v3+ in PATH (https://github.com/vultr/vultr-cli)
#   - VULTR_API_KEY environment variable exported
#   - Go 1.25+ (binaries are cross-compiled automatically if missing)
#   - deploy/install.sh present
#
# Cloud-tier choices (see plan + research notes):
#   - server / node tiers run on Cloud Compute (vc2). Cheap, /dev/kvm not
#     required.
#   - client tier runs on Bare Metal (vbm). Required because the client
#     hosts Firecracker microVMs and must expose /dev/kvm to userspace,
#     which Vultr's Cloud Compute does not provide. Bare Metal pricing
#     starts higher than Cloud Compute (typically ~$120/mo+); pick the
#     smallest plan you can tolerate.
#
# Block storage caveat:
#   Vultr Block Storage attaches only to Cloud Compute instances, not
#   Bare Metal. The script therefore creates persistent block volumes
#   for the server and node tiers (mirroring the AWS EBS layout) but
#   stores the client's /root/.testnet on the Bare Metal box's local
#   disk. On client teardown, that data is gone — exactly the same
#   trade-off as AWS 'teardown --full' for the client tier.
#
# Resources are tagged with testnet-stack=agent-testnet for easy
# identification. State lives in deploy/.vultr-state.json.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DIST_DIR="${PROJECT_DIR}/dist"

STACK_PREFIX="${STACK_PREFIX:-testnet}"
STACK_TAG="${STACK_TAG:-testnet-stack}"
STACK_VALUE="${STACK_VALUE:-agent-testnet}"
STATE_FILE="${STATE_FILE:-${SCRIPT_DIR}/.vultr-state.json}"
KEY_NAME="${KEY_NAME:-${STACK_PREFIX}-vultr-deploy-key}"
KEY_FILE="${KEY_FILE:-${SCRIPT_DIR}/.vultr-${STACK_PREFIX}-key.pem}"

REGION="${VULTR_REGION:-fra}"

PLAN_SERVER="${PLAN_SERVER:-vc2-1c-1gb}"
PLAN_NODE="${PLAN_NODE:-vc2-1c-2gb}"
PLAN_CLIENT="${PLAN_CLIENT:-vbm-4c-32gb}"

NODES_YAML_SRC="${NODES_YAML_SRC:-${PROJECT_DIR}/configs/nodes.yaml}"
NODE_SECRET="$(head -c 16 /dev/urandom | base64 | tr -d '/+=' | head -c 24)"

# ---- shared helpers ----
#
# Logging, state file I/O, multi-client mgmt, SSH wrappers, ensure_binaries,
# nodes.yaml rendering, resolve_role_keys, mount_block_device, and the
# cloud-agnostic verbs (do_ssh / do_logs / do_reload / do_restart /
# do_redeploy / do_test / do_openclaw) all live in lib/common.sh and are
# shared with deploy/aws-deploy.sh. Anything below is Vultr-specific.

# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

# ---- Vultr CLI helpers ----

# vc <args...>
#
# Run vultr-cli with JSON output forced. Some subcommands (e.g.
# `bare-metal create`) print non-JSON status text to stderr while still
# emitting the resource on stdout; we keep stderr separate so callers can
# parse a clean JSON document. Use vc_quiet for fire-and-forget mutations
# whose output we don't need.
vc() {
    vultr-cli "$@" -o json
}

vc_quiet() {
    vultr-cli "$@" >/dev/null 2>&1
}

# Pull a top-level key from a JSON document on stdin via python3.
# Usage:  echo "$json" | json_get path.to.key
json_get() {
    python3 -c "
import json, sys
data = json.load(sys.stdin)
keys = sys.argv[1].split('.')
for k in keys:
    if isinstance(data, dict):
        data = data.get(k)
    elif isinstance(data, list) and k.isdigit():
        idx = int(k)
        data = data[idx] if 0 <= idx < len(data) else None
    else:
        data = None
    if data is None:
        break
print('' if data is None else data)
" "$1"
}

# Tag every resource consistently. Vultr accepts a list of free-form tags
# on most resource types (instances, bare-metal, block storage); the
# control plane reflects them back via `vultr-cli ... list --tag=...`.
default_tags() {
    echo "${STACK_TAG}=${STACK_VALUE}"
}

# ---- preflight + binary build ----

preflight_check() {
    [ -f "${SCRIPT_DIR}/install.sh" ] || err "Missing deploy/install.sh"
    command -v vultr-cli >/dev/null 2>&1 || \
        err "vultr-cli not found in PATH. Install: https://github.com/vultr/vultr-cli"
    [ -n "${VULTR_API_KEY:-}" ] || \
        err "VULTR_API_KEY env var not set. Export your Vultr API token before running."
    vc_quiet account info || err "vultr-cli account info failed (token invalid or network down)"
}

# ---- OS resolution ----

# Resolve and cache the latest Ubuntu 24.04 LTS x64 OS id for use with
# `vultr-cli instance create --os` / `bare-metal create --os`. Sets $OS_ID
# and persists to state. Vultr OS ids are stable across regions but the
# numeric id can change when canonical reissues an image, so we resolve
# from `vultr-cli os list` rather than hard-coding.
ensure_os_id() {
    OS_ID=$(load_state "os_id")
    if [ -n "$OS_ID" ]; then
        info "Reusing OS id from state: ${OS_ID}"
        return
    fi

    info "Resolving Ubuntu 24.04 LTS x64 OS id..."
    OS_ID=$(vc os list | python3 -c "
import json, sys
doc = json.load(sys.stdin)
oss = doc.get('os') or doc.get('oses') or []
for o in oss:
    name = (o.get('name') or '').lower()
    arch = (o.get('arch') or '').lower()
    if 'ubuntu 24.04' in name and ('x64' in arch or 'x64' in name or 'amd64' in arch):
        print(o.get('id', ''))
        break
")
    [ -n "$OS_ID" ] || err "Could not find Ubuntu 24.04 LTS x64 in 'vultr-cli os list'"
    info "Using OS id: ${OS_ID}"
    save_state "os_id" "$OS_ID"
}

# ---- SSH key ----

ensure_ssh_key() {
    local key_id
    key_id=$(load_state "ssh_key_id")
    if [ -n "$key_id" ]; then
        if vc_quiet ssh-key get "$key_id"; then
            info "Reusing SSH key: ${key_id}"
            SSH_KEY_ID="$key_id"
            return
        fi
        warn "SSH key id ${key_id} from state no longer exists, recreating..."
    fi

    if [ ! -f "$KEY_FILE" ]; then
        info "Generating ed25519 SSH key at ${KEY_FILE}..."
        ssh-keygen -t ed25519 -f "$KEY_FILE" -N "" -C "${KEY_NAME}" >/dev/null
        chmod 600 "$KEY_FILE"
    fi

    local pubkey
    pubkey=$(cat "${KEY_FILE}.pub")

    info "Uploading SSH key to Vultr..."
    SSH_KEY_ID=$(vc ssh-key create --name="$KEY_NAME" --key="$pubkey" | json_get id)
    [ -n "$SSH_KEY_ID" ] || err "Failed to register SSH key with Vultr"

    save_state "ssh_key_id" "$SSH_KEY_ID"
    save_state "key_name" "$KEY_NAME"
    save_state "key_file" "$KEY_FILE"
    info "SSH key registered: ${SSH_KEY_ID}"
}

# ---- reserved IP ----

ensure_reserved_ip() {
    local rip_id
    rip_id=$(load_state "reserved_ip_id")
    if [ -n "$rip_id" ]; then
        if vc_quiet reserved-ip get "$rip_id"; then
            RIP_ID="$rip_id"
            RIP_PUBLIC=$(vc reserved-ip get "$rip_id" | json_get subnet)
            [ -z "$RIP_PUBLIC" ] && RIP_PUBLIC=$(vc reserved-ip get "$rip_id" | json_get ip_address)
            info "Reusing Reserved IP: ${RIP_PUBLIC} (${RIP_ID})"
            return
        fi
        warn "Reserved IP ${rip_id} from state no longer exists, allocating new one..."
    fi

    info "Allocating Reserved IP for server..."
    RIP_ID=$(vc reserved-ip create --type=v4 --region="$REGION" --label="${STACK_PREFIX}-server-rip" | json_get id)
    [ -n "$RIP_ID" ] || err "Failed to allocate Reserved IP"

    RIP_PUBLIC=$(vc reserved-ip get "$RIP_ID" | json_get subnet)
    [ -z "$RIP_PUBLIC" ] && RIP_PUBLIC=$(vc reserved-ip get "$RIP_ID" | json_get ip_address)

    save_state "reserved_ip_id" "$RIP_ID"
    save_state "reserved_ip_public" "$RIP_PUBLIC"
    info "Allocated Reserved IP: ${RIP_PUBLIC} (${RIP_ID})"
}

# Attach the persistent server Reserved IP to the freshly-launched server
# instance and switch the instance's primary IP to it. Vultr requires the
# instance to be reachable (active+running) before attach.
attach_reserved_ip() {
    local instance_id="$1"
    info "Attaching Reserved IP ${RIP_PUBLIC} to server instance ${instance_id}..."
    vc_quiet reserved-ip attach "$RIP_ID" --instance-id="$instance_id" || \
        err "Failed to attach Reserved IP ${RIP_ID} to ${instance_id}"
    sleep 2
}

# ---- firewall groups ----
#
# Vultr Cloud Compute supports Firewall Groups (analogous to AWS SGs).
# Bare Metal does NOT support firewall groups — for the client tier we
# rely on the box only having sshd listening on the public interface
# (the testnet-client daemon binds to wg-testnet, not the public IP).

# Idempotent helper: create a firewall group if it doesn't exist and
# install a fixed rule set on it. State key holds the group id so we
# can reuse across re-deploys. Args:
#   $1 role            "server" | "node" | "client"
#   $2 description     human-readable description
#   $@ rules           "PROTO PORT" entries (e.g. "tcp 22", "udp 51820")
ensure_firewall_group() {
    local role="$1" description="$2"
    shift 2

    local state_key="firewall_group_${role}"
    local fwg
    fwg=$(load_state "$state_key")
    if [ -n "$fwg" ]; then
        if vc_quiet firewall group get "$fwg"; then
            info "Reusing ${role} firewall group: ${fwg}"
            eval "FWG_${role^^}=\$fwg"
            return
        fi
        warn "Firewall group ${fwg} for ${role} no longer exists, recreating..."
    fi

    info "Creating ${role} firewall group..."
    fwg=$(vc firewall group create --description="$description" | json_get id)
    [ -n "$fwg" ] || err "Failed to create ${role} firewall group"

    for rule in "$@"; do
        local proto port
        read -r proto port <<< "$rule"
        info "  rule: allow ${proto}/${port} from 0.0.0.0/0"
        vc_quiet firewall rule create "$fwg" \
            --protocol="$proto" \
            --port="$port" \
            --subnet=0.0.0.0 \
            --size=0 \
            --ip-type=v4 \
            || warn "  failed to add ${proto}/${port} rule (continuing)"
    done

    save_state "$state_key" "$fwg"
    eval "FWG_${role^^}=\$fwg"
    info "Created ${role} firewall group: ${fwg}"
}

ensure_firewall_group_server() {
    ensure_firewall_group server \
        "Testnet server: API + WireGuard + DNS" \
        "tcp 22" \
        "tcp 8443" \
        "udp 51820" \
        "udp 5353" \
        "tcp 5353"
}

ensure_firewall_group_node() {
    ensure_firewall_group node \
        "Testnet node: HTTPS" \
        "tcp 22" \
        "tcp 443"
}

# ---- block storage (server/node only) ----
#
# Vultr Block Storage attaches only to Cloud Compute instances, NOT
# Bare Metal. The client tier therefore stores /root/.testnet on the
# local disk; see the script header for the trade-off.

ensure_volume() {
    local role="$1" size="$2"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    if [ -n "$vol_id" ]; then
        if vc_quiet block-storage get "$vol_id"; then
            info "Reusing data volume for ${role}: ${vol_id}" >&2
            echo "$vol_id"
            return
        fi
        warn "Volume ${vol_id} for ${role} no longer exists, creating new one..." >&2
    fi

    info "Creating ${size} GiB data volume for ${role} in ${REGION}..." >&2
    vol_id=$(vc block-storage create \
        --region="$REGION" \
        --size="$size" \
        --label="${STACK_PREFIX}-${role}-data" \
        | json_get id)
    [ -n "$vol_id" ] || err "Failed to create block storage for ${role}"
    save_state "vol_${role}" "$vol_id"
    info "Created volume ${vol_id} for ${role}" >&2
    echo "$vol_id"
}

attach_and_mount_volume() {
    local vol_id="$1" instance_id="$2" ip="$3" key="$4" mount_point="$5"

    info "Waiting for volume ${vol_id} to be available..."
    local i status
    for i in $(seq 1 30); do
        status=$(vc block-storage get "$vol_id" | json_get status)
        case "$status" in
            active|pending) break ;;
            *) sleep 2 ;;
        esac
    done

    info "Attaching ${vol_id} to ${instance_id}..."
    vc_quiet block-storage attach "$vol_id" --instance="$instance_id" --live=true \
        || err "Failed to attach volume ${vol_id} to ${instance_id}"

    info "Waiting for volume to attach..."
    for i in $(seq 1 30); do
        local attached
        attached=$(vc block-storage get "$vol_id" | json_get attached_to_instance)
        if [ "$attached" = "$instance_id" ]; then
            break
        fi
        sleep 3
    done
    sleep 3

    # Vultr's virtio block devices show up as /dev/vdb (preferred); some
    # plans expose them under /dev/sdb. Pass both candidates so the
    # generalized mount_block_device helper can pick the live one.
    mount_block_device "$ip" "$key" "$mount_point" /dev/vdb /dev/sdb
}

detach_volume() {
    local role="$1"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    [ -n "$vol_id" ] || return 0

    local attached
    attached=$(vc block-storage get "$vol_id" 2>/dev/null | json_get attached_to_instance || echo "")
    if [ -n "$attached" ]; then
        info "Detaching data volume for ${role}: ${vol_id}..."
        vc_quiet block-storage detach "$vol_id" --live=true || true
        sleep 5
    fi
}

delete_volume() {
    local role="$1"
    local vol_id
    vol_id=$(load_state "vol_${role}")
    [ -n "$vol_id" ] || return 0

    info "Deleting data volume for ${role}: ${vol_id}..."
    vc_quiet block-storage delete "$vol_id" || true
}

# ---- launch helpers ----

# Launch a Cloud Compute (vc2) instance. Used for server + node.
# Args: $1 state_key  $2 hostname  $3 firewall_group_id  $4 plan
# Echoes the instance id on stdout.
launch_vc2() {
    local state_key="$1" hostname="$2" fwg="$3" plan="$4"
    info "Launching ${hostname} VC2 instance (${plan}, ${REGION})..." >&2

    local args=(
        --region="$REGION"
        --plan="$plan"
        --os="$OS_ID"
        --label="${STACK_PREFIX}-${hostname}"
        --hostname="${STACK_PREFIX}-${hostname}"
        --ssh-keys="$SSH_KEY_ID"
        --firewall-group="$fwg"
        --tags="$(default_tags)"
    )

    local instance_id
    instance_id=$(vc instance create "${args[@]}" | json_get id)
    [ -n "$instance_id" ] || err "Failed to launch VC2 instance for ${hostname}"
    save_state "instance_${state_key}" "$instance_id"
    echo "$instance_id"
}

# Launch a Bare Metal instance. Used only for the client tier (KVM access
# is the only reason we pay for Bare Metal). Note: bare-metal create has
# no --firewall-group flag — the host has no managed firewall, so the
# script relies on sshd being the only public-facing service.
# Args: $1 state_key  $2 hostname  $3 plan
# Echoes the bare-metal id on stdout.
launch_bm() {
    local state_key="$1" hostname="$2" plan="$3"
    info "Launching ${hostname} Bare Metal instance (${plan}, ${REGION})..." >&2

    local args=(
        --region="$REGION"
        --plan="$plan"
        --os="$OS_ID"
        --label="${STACK_PREFIX}-${hostname}"
        --host="${STACK_PREFIX}-${hostname}"
        --ssh-keys="$SSH_KEY_ID"
        --tags="$(default_tags)"
    )

    local instance_id
    instance_id=$(vc bare-metal create "${args[@]}" | json_get id)
    [ -n "$instance_id" ] || err "Failed to launch Bare Metal instance for ${hostname}"
    save_state "instance_${state_key}" "$instance_id"
    echo "$instance_id"
}

# Wait until a Vultr instance reaches active+running+ok. VC2 typically
# takes ~30-60s; Bare Metal can take 5-10 min.
# Args: $1 instance_id  $2 kind ("instance" | "bare-metal")  $3 max_attempts
wait_for_instance() {
    local id="$1" kind="$2" max_attempts="${3:-60}"
    local attempt=0
    info "Waiting for ${kind} ${id} to become active+running..."
    while [ $attempt -lt $max_attempts ]; do
        local status power server_status
        local doc
        doc=$(vc "$kind" get "$id" 2>/dev/null || echo '{}')
        status=$(echo "$doc" | json_get status)
        power=$(echo "$doc" | json_get power_status)
        server_status=$(echo "$doc" | json_get server_status)
        if [ "$status" = "active" ] && [ "$power" = "running" ]; then
            # bare-metal omits server_status; treat ok|none|"" as ready
            case "$server_status" in
                ok|none|installingbooting|"") return 0 ;;
            esac
        fi
        attempt=$((attempt + 1))
        sleep 10
    done
    err "${kind} ${id} did not become active within $((max_attempts * 10))s (last status=${status:-?}, power=${power:-?})"
}

# Get the public IP of a Vultr resource (instance or bare-metal).
# Args: $1 id  $2 kind ("instance" | "bare-metal")
get_ip() {
    local id="$1" kind="$2"
    vc "$kind" get "$id" | json_get main_ip
}

# ---- role deploys ----

# Deploy the server tier: launch a VC2 instance, attach the persistent
# reserved IP + data volume, install testnet-server, retrieve the join
# token. Idempotent on already-running deployments — refuses to clobber.
do_server_deploy() {
    info "Deploying server to Vultr (${REGION})"
    preflight_check
    require_nodes_yaml

    local existing
    existing=$(load_state "instance_server")
    if [ -n "$existing" ] && \
       [ "$(vc instance get "$existing" 2>/dev/null | json_get status)" = "active" ]; then
        err "Server already deployed (instance ${existing}). Run 'restart server' or 'redeploy server'."
    fi

    ensure_binaries testnet-server
    ensure_os_id
    ensure_ssh_key
    ensure_firewall_group_server
    ensure_reserved_ip

    local NODE_NAME
    NODE_NAME=$(extract_node_name)
    save_state "node_name" "$NODE_NAME"
    info "Node name from config: ${NODE_NAME}"

    # Reuse a previously generated node secret so it stays stable across
    # server/node redeploys (the node side authenticates with this value).
    local NODE_SECRET_VAL
    NODE_SECRET_VAL=$(load_state "node_secret")
    if [ -z "$NODE_SECRET_VAL" ]; then
        NODE_SECRET_VAL="$NODE_SECRET"
        save_state "node_secret" "$NODE_SECRET_VAL"
    fi

    local VOL_SERVER
    VOL_SERVER=$(ensure_volume "server" 10)

    local INST_SERVER IP_SERVER
    INST_SERVER=$(launch_vc2 "server" "server" "$FWG_SERVER" "$PLAN_SERVER")
    wait_for_instance "$INST_SERVER" instance 60

    attach_reserved_ip "$INST_SERVER"
    IP_SERVER="$RIP_PUBLIC"
    save_state "ip_server" "$IP_SERVER"
    save_state "server_url" "https://${IP_SERVER}:8443"
    info "Server IP: ${IP_SERVER}"

    wait_for_ssh "$IP_SERVER" "$KEY_FILE" 60
    attach_and_mount_volume "$VOL_SERVER" "$INST_SERVER" "$IP_SERVER" "$KEY_FILE" "/opt/testnet/data"

    # Render nodes.yaml: at this point IP_NODE may be unknown (server-only
    # deploy) — render with whatever's in state, falling back to the
    # placeholder. The do_node_deploy reload step will overwrite it later.
    local IP_NODE_KNOWN
    IP_NODE_KNOWN=$(load_state "ip_node")
    [ -z "$IP_NODE_KNOWN" ] && IP_NODE_KNOWN="DEPLOY_NODE_ADDRESS"
    local nodes_tmp="${SCRIPT_DIR}/.nodes-deploy.yaml"
    render_nodes_yaml "$IP_NODE_KNOWN" "$NODE_SECRET_VAL" "$nodes_tmp"

    info "Installing server software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-server-linux-amd64" "ubuntu@${IP_SERVER}:/tmp/testnet-server"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_SERVER}:/tmp/install.sh"
    remote_copy "$KEY_FILE" "$nodes_tmp" "ubuntu@${IP_SERVER}:/tmp/nodes.yaml"
    rm -f "$nodes_tmp"

    remote_exec "$IP_SERVER" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-server /usr/local/bin/testnet-server
        sudo chmod +x /usr/local/bin/testnet-server
        export AUTO_START=1
        sudo -E bash /tmp/install.sh server
    "

    info "Retrieving join token from server..."
    local JOIN_TOKEN=""
    for _ in $(seq 1 10); do
        JOIN_TOKEN=$(remote_exec "$IP_SERVER" "$KEY_FILE" "sudo cat /opt/testnet/data/join-token 2>/dev/null" || echo "")
        [ -n "$JOIN_TOKEN" ] && break
        sleep 3
    done
    if [ -z "$JOIN_TOKEN" ]; then
        err "Could not retrieve join token after 30s. Check: ssh -i ${KEY_FILE} ubuntu@${IP_SERVER} 'sudo journalctl -u testnet-server -f'"
    fi
    info "Join token: ${JOIN_TOKEN}"
    save_state "join_token" "$JOIN_TOKEN"

    if remote_exec "$IP_SERVER" "$KEY_FILE" "sudo systemctl is-active --quiet testnet-server" 2>/dev/null; then
        info "Server: healthy"
    else
        warn "Server: testnet-server service not active. Check: bash deploy/vultr-deploy.sh logs server"
    fi
}

# Deploy the node tier: launch a VC2 instance, install testnet-node,
# then push the updated nodes.yaml (now including this node's address)
# to the server.
do_node_deploy() {
    info "Deploying node to Vultr (${REGION})"
    preflight_check
    require_nodes_yaml

    local existing
    existing=$(load_state "instance_node")
    if [ -n "$existing" ] && \
       [ "$(vc instance get "$existing" 2>/dev/null | json_get status)" = "active" ]; then
        err "Node already deployed (instance ${existing}). Run 'restart node' or 'redeploy node'."
    fi

    local server_url node_secret
    server_url=$(load_state "server_url")
    node_secret=$(load_state "node_secret")
    [ -n "$server_url" ]   || err "No server_url in state. Deploy the server first or set it manually."
    [ -n "$node_secret" ]  || err "No node_secret in state. Deploy the server first."

    ensure_binaries testnet-node testnet-toolkit
    ensure_os_id
    ensure_ssh_key
    ensure_firewall_group_node

    local NODE_NAME
    NODE_NAME=$(extract_node_name)
    save_state "node_name" "$NODE_NAME"

    local VOL_NODE
    VOL_NODE=$(ensure_volume "node" 10)

    local INST_NODE IP_NODE
    INST_NODE=$(launch_vc2 "node" "node" "$FWG_NODE" "$PLAN_NODE")
    wait_for_instance "$INST_NODE" instance 60

    IP_NODE=$(get_ip "$INST_NODE" instance)
    [ -n "$IP_NODE" ] || err "Could not determine node public IP"
    save_state "ip_node" "$IP_NODE"
    info "Node IP: ${IP_NODE}"

    wait_for_ssh "$IP_NODE" "$KEY_FILE" 60
    attach_and_mount_volume "$VOL_NODE" "$INST_NODE" "$IP_NODE" "$KEY_FILE" "/opt/testnet"

    info "Installing node software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-node-linux-amd64" "ubuntu@${IP_NODE}:/tmp/testnet-node"
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-toolkit-linux-amd64" "ubuntu@${IP_NODE}:/tmp/testnet-toolkit"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_NODE}:/tmp/install.sh"

    remote_exec "$IP_NODE" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-node /usr/local/bin/testnet-node
        sudo chmod +x /usr/local/bin/testnet-node
        sudo mv /tmp/testnet-toolkit /usr/local/bin/testnet-toolkit
        sudo chmod +x /usr/local/bin/testnet-toolkit
        export SERVER_URL='${server_url}'
        export NODE_NAME='${NODE_NAME}'
        export NODE_SECRET='${node_secret}'
        sudo -E bash /tmp/install.sh node
    "

    # Push updated nodes.yaml (with this node's real address) to the server
    # and reload it so the new node entry is routable.
    local ip_server
    ip_server=$(load_state "ip_server")
    if [ -n "$ip_server" ]; then
        info "Updating server nodes.yaml with node address..."
        local nodes_tmp="${SCRIPT_DIR}/.nodes-deploy.yaml"
        render_nodes_yaml "$IP_NODE" "$node_secret" "$nodes_tmp"
        remote_copy "$KEY_FILE" "$nodes_tmp" "ubuntu@${ip_server}:/tmp/nodes.yaml"
        rm -f "$nodes_tmp"
        remote_exec "$ip_server" "$KEY_FILE" "
            sudo cp /tmp/nodes.yaml /opt/testnet/configs/nodes.yaml
            sudo bash -c 'rm -rf /opt/testnet/data/certs/*'
            sudo systemctl reload testnet-server
        "
    else
        warn "Server not deployed locally — skip nodes.yaml reload. Apply on the remote server manually."
    fi

    local node_ok=false
    for _ in $(seq 1 12); do
        if remote_exec "$IP_NODE" "$KEY_FILE" "sudo systemctl is-active --quiet testnet-node" 2>/dev/null; then
            node_ok=true
            break
        fi
        sleep 5
    done
    if $node_ok; then
        info "Node: healthy"
    else
        warn "Node: testnet-node service not active. Check: bash deploy/vultr-deploy.sh logs node"
    fi
}

# Deploy a client tier instance under the supplied name (default: next free
# numeric index). The client runs on Bare Metal (vbm) so /dev/kvm is
# exposed for Firecracker. Block storage is NOT used (Vultr Block Storage
# can't attach to Bare Metal); /root/.testnet lives on the local disk.
#
# Args:
#   $1 name           client name (empty = auto-assign next index)
#   $2 server_url     optional override of state's server_url
#   $3 join_token     optional override of state's join_token
#   $4 plan           optional override of $PLAN_CLIENT
do_client_deploy() {
    local name="${1:-}" override_server_url="${2:-}" override_join_token="${3:-}" override_plan="${4:-}"

    info "Deploying client to Vultr (${REGION})"
    preflight_check

    [ -z "$name" ] && name=$(next_client_index)

    if is_registered_client "$name"; then
        local existing
        existing=$(load_state "instance_client_${name}")
        if [ -n "$existing" ] && \
           [ "$(vc bare-metal get "$existing" 2>/dev/null | json_get status)" = "active" ]; then
            err "Client '${name}' already deployed (instance ${existing}). Pick a different --client name or 'client-remove' first."
        fi
    fi

    # Resolve server URL + join token: command-line flags win, then state.
    local server_url join_token
    if [ -n "$override_server_url" ]; then
        server_url="$override_server_url"
        save_state "server_url" "$server_url"
    else
        server_url=$(load_state "server_url")
    fi
    if [ -n "$override_join_token" ]; then
        join_token="$override_join_token"
        save_state "join_token" "$join_token"
    else
        join_token=$(load_state "join_token")
    fi
    [ -n "$server_url" ] || err "No server_url. Pass --server-url URL or deploy a server first."
    [ -n "$join_token" ] || err "No join_token. Pass --join-token TOK or deploy a server first."

    local plan="${override_plan:-$PLAN_CLIENT}"
    case "$plan" in
        vbm-*) ;;
        *)
            err "Client plan must be a Vultr Bare Metal plan (vbm-*); got '${plan}'. Cloud Compute does not expose /dev/kvm — see script header."
            ;;
    esac

    ensure_binaries testnet-client
    ensure_os_id
    ensure_ssh_key

    local INST_CLIENT IP_CLIENT
    INST_CLIENT=$(launch_bm "client_${name}" "client-${name}" "$plan")
    # Bare Metal provisioning is much slower than VC2 (~5-10 min)
    wait_for_instance "$INST_CLIENT" bare-metal 120

    IP_CLIENT=$(get_ip "$INST_CLIENT" bare-metal)
    [ -n "$IP_CLIENT" ] || err "Could not determine client public IP"
    save_state "ip_client_${name}" "$IP_CLIENT"
    info "Client '${name}' IP: ${IP_CLIENT}"

    # Bare Metal can take longer to come up over SSH after the API reports
    # active — give it ~10 min.
    wait_for_ssh "$IP_CLIENT" "$KEY_FILE" 120

    info "Installing client software..."
    remote_copy "$KEY_FILE" "${DIST_DIR}/testnet-client-linux-amd64" "ubuntu@${IP_CLIENT}:/tmp/testnet-client"
    remote_copy "$KEY_FILE" "${SCRIPT_DIR}/install.sh" "ubuntu@${IP_CLIENT}:/tmp/install.sh"

    remote_exec "$IP_CLIENT" "$KEY_FILE" "
        sudo mkdir -p /usr/local/bin
        sudo mv /tmp/testnet-client /usr/local/bin/testnet-client
        sudo chmod +x /usr/local/bin/testnet-client
        export SERVER_URL='${server_url}'
        export JOIN_TOKEN='${join_token}'
        sudo -E bash /tmp/install.sh client
    "

    register_client "$name"

    local wg_status
    wg_status=$(remote_exec "$IP_CLIENT" "$KEY_FILE" "sudo wg show wg-testnet 2>/dev/null | grep 'latest handshake'" 2>/dev/null || echo "")
    if [ -n "$wg_status" ]; then
        info "Client '${name}': healthy (WireGuard tunnel established)"
    else
        warn "Client '${name}': WireGuard tunnel not established yet"
    fi

    if remote_exec "$IP_CLIENT" "$KEY_FILE" "test -e /dev/kvm" 2>/dev/null; then
        info "Client '${name}': /dev/kvm available (Firecracker ready)"
    else
        # On Bare Metal this should be impossible, but surface loudly if
        # Vultr ever ships a configuration where it's missing.
        warn "Client '${name}': /dev/kvm NOT FOUND on Bare Metal host."
        warn "  Firecracker VMs will not work. This indicates a Vultr-side"
        warn "  misconfiguration — file a support ticket referencing this"
        warn "  instance id: ${INST_CLIENT}"
    fi

    echo ""
    echo "  Client '${name}' deployed:"
    echo "    IP:  ${IP_CLIENT}"
    echo "    SSH: ssh -i ${KEY_FILE} ubuntu@${IP_CLIENT}"
    echo ""
    echo "  Run OpenClaw on this client:"
    echo "    bash deploy/vultr-deploy.sh openclaw install --client ${name} --api-key ..."
    echo ""
}

# Tear down a single client (terminate its Bare Metal instance) without
# touching shared infra or other clients. Bare Metal has no block storage
# attached, so there is nothing to preserve — the client's /root/.testnet
# was on the local disk and goes with the host.
do_client_remove() {
    local name="${1:-}"
    [ -n "$name" ] || err "Usage: client-remove --client NAME"
    is_registered_client "$name" || err "Unknown client '${name}'. Known: $(client_names_list)"

    local inst_id
    inst_id=$(load_state "instance_client_${name}")
    if [ -n "$inst_id" ]; then
        info "Terminating client '${name}' bare-metal (${inst_id})..."
        vc_quiet bare-metal delete "$inst_id" || true
    fi

    delete_state "instance_client_${name}"
    delete_state "ip_client_${name}"
    unregister_client "$name"

    info "Client '${name}' removed."
}

# Full deploy: server + node + a single client named "0".
do_deploy() {
    info "Deploying full Agent Testnet stack to Vultr (${REGION})"

    preflight_check
    require_nodes_yaml

    if [ -f "$STATE_FILE" ]; then
        local existing_server
        existing_server=$(load_state "instance_server")
        if [ -n "$existing_server" ] && \
           [ "$(vc instance get "$existing_server" 2>/dev/null | json_get status)" = "active" ]; then
            err "Active deployment found (server: ${existing_server}). Run 'teardown' first or use server-deploy/node-deploy/client-deploy for incremental updates."
        fi
    fi

    # Build everything up-front so all role deploys reuse the binaries.
    ensure_binaries testnet-server testnet-client testnet-node testnet-toolkit

    do_server_deploy
    do_node_deploy
    do_client_deploy "0"

    # ---- Summary ----
    local IP_SERVER IP_NODE IP_CLIENT0 NODE_NAME JOIN_TOKEN
    IP_SERVER=$(load_state "ip_server")
    IP_NODE=$(load_state "ip_node")
    IP_CLIENT0=$(load_state "ip_client_0")
    NODE_NAME=$(load_state "node_name")
    JOIN_TOKEN=$(load_state "join_token")

    echo ""
    echo "============================================"
    echo "  Agent Testnet deployed on Vultr"
    echo "============================================"
    echo ""
    echo "  Region:  ${REGION}"
    echo ""
    echo "  Server:    ${IP_SERVER} (Reserved IP)"
    echo "  Node:      ${IP_NODE}"
    echo "  Client 0:  ${IP_CLIENT0} (Bare Metal)"
    echo ""
    echo "  SSH key:    ${KEY_FILE}"
    echo "  Node name:  ${NODE_NAME}"
    echo "  Join token: ${JOIN_TOKEN}"
    echo ""
    echo "  Add another client (in this Vultr account):"
    echo "    bash deploy/vultr-deploy.sh client-deploy --client alice"
    echo ""
    echo "  Join from a different cloud / account:"
    echo "    bash deploy/vultr-deploy.sh client-deploy --server-url https://${IP_SERVER}:8443 --join-token ${JOIN_TOKEN}"
    echo ""
    echo "  Teardown (preserves Reserved IP + data volumes):"
    echo "    bash deploy/vultr-deploy.sh teardown"
    echo "  Full teardown (destroys everything):"
    echo "    bash deploy/vultr-deploy.sh teardown --full"
    echo ""
    echo "  Plans: server=${PLAN_SERVER}, node=${PLAN_NODE}, client=${PLAN_CLIENT}"
    echo ""
}

# ---- status ----

do_status() {
    if [ ! -f "$STATE_FILE" ]; then
        err "No state file found. Run 'deploy' first."
    fi

    local key_file ip_server
    key_file=$(load_state "key_file")
    ip_server=$(load_state "ip_server")

    echo ""
    echo "Agent Testnet Status (Vultr ${REGION})"
    echo "================================"

    local rip_id rip_ip
    rip_id=$(load_state "reserved_ip_id")
    rip_ip=$(load_state "reserved_ip_public")
    if [ -n "$rip_id" ]; then
        echo ""
        echo "Reserved IP:  ${rip_ip} (${rip_id})"
    fi
    for role in server node; do
        local vid
        vid=$(load_state "vol_${role}")
        [ -n "$vid" ] && echo "Volume (${role}): ${vid}"
    done

    local cnames
    cnames=$(client_names_list)

    local inst_server
    inst_server=$(load_state "instance_server")
    if [ -z "$inst_server" ] && [ -z "$cnames" ]; then
        local inst_node
        inst_node=$(load_state "instance_node")
        if [ -z "$inst_node" ]; then
            echo ""
            echo "No active instances. Run 'deploy' or 'client-deploy' to launch."
            echo ""
            return
        fi
    fi
    echo ""

    # -- Instances --
    echo "Instances:"
    printf "  %-12s  %-38s  %-16s  %-10s  %s\n" "ROLE" "INSTANCE" "IP" "STATUS" "SERVICE"
    printf "  %-12s  %-38s  %-16s  %-10s  %s\n" "------------" "--------------------------------------" "----------------" "----------" "-------"

    print_status_row() {
        local label="$1" inst_id="$2" ip="$3" svc_name="$4" kind="$5"
        local status svc_state
        status=$(vc "$kind" get "$inst_id" 2>/dev/null | json_get status || echo "unknown")
        if [ "$status" = "active" ] && [ -f "$key_file" ]; then
            svc_state=$(remote_exec "$ip" "$key_file" \
                "systemctl is-active $svc_name 2>/dev/null || true" 2>/dev/null) || svc_state="ssh-err"
        else
            svc_state="-"
        fi
        printf "  %-12s  %-38s  %-16s  %-10s  %s\n" "$label" "$inst_id" "$ip" "$status" "$svc_state"
    }

    for role in server node; do
        local inst_id ip
        inst_id=$(load_state "instance_${role}")
        ip=$(load_state "ip_${role}")
        [ -z "$inst_id" ] && continue
        print_status_row "$role" "$inst_id" "$ip" "testnet-${role}" instance
    done
    for cname in $cnames; do
        local inst_id ip
        inst_id=$(load_state "instance_client_${cname}")
        ip=$(load_state "ip_client_${cname}")
        [ -z "$inst_id" ] && continue
        print_status_row "client/${cname}" "$inst_id" "$ip" "testnet-client" bare-metal
    done

    # -- Registered nodes & domains (from server) --
    if [ -n "$ip_server" ] && [ -f "$key_file" ]; then
        local nodes_yaml
        nodes_yaml=$(remote_exec "$ip_server" "$key_file" \
            "sudo cat /opt/testnet/configs/nodes.yaml 2>/dev/null" 2>/dev/null) || true

        if [ -n "$nodes_yaml" ]; then
            echo ""
            echo "Registered nodes (from server nodes.yaml):"
            printf "  %-12s  %-24s  %s\n" "NAME" "ADDRESS" "DOMAINS"
            printf "  %-12s  %-24s  %s\n" "------------" "------------------------" "-------"
            echo "$nodes_yaml" | awk '
                /- name:/    { if (name) printf "  %-12s  %-24s  %s\n", name, addr, domains;
                               gsub(/.*name: *"?/, ""); gsub(/".*/, ""); name=$0; addr=""; domains="" }
                /address:/   { gsub(/.*address: *"?/, ""); gsub(/".*/, ""); addr=$0 }
                /^ *- "/ || /^ *- '\''/ { gsub(/.*- *"?/, ""); gsub(/".*/, ""); gsub(/'\''.*/, "");
                               if (domains) domains = domains ", " $0; else domains=$0 }
                END          { if (name) printf "  %-12s  %-24s  %s\n", name, addr, domains }
            '
        fi

        # -- WireGuard peers (connected clients) --
        echo ""
        echo "WireGuard peers:"
        remote_exec "$ip_server" "$key_file" \
            "sudo wg show wg0 2>/dev/null | grep -E '(peer|endpoint|latest handshake|transfer)' | sed 's/^/  /'" 2>/dev/null || echo "  (no tunnel or wg not running)"

        # -- Recent server log --
        echo ""
        echo "Server log (last 5 lines):"
        remote_exec "$ip_server" "$key_file" \
            "sudo journalctl -u testnet-server --no-pager -n 5 --output short-iso 2>/dev/null | sed 's/^/  /'" 2>/dev/null || echo "  (unavailable)"
    fi

    if [ -n "$ip_server" ]; then
        echo ""
        echo "Server URL: https://${ip_server}:8443"
    fi

    local join_token
    join_token=$(load_state "join_token")
    if [ -n "$join_token" ]; then
        echo ""
        echo "Join token: ${join_token}"
    fi

    echo ""
    echo "SSH: ssh -i ${key_file} ubuntu@<IP>"
    echo ""
}

# ---- teardown ----

do_teardown() {
    local full_teardown=false
    if [ "${1:-}" = "--full" ]; then
        full_teardown=true
        info "Full teardown requested -- Reserved IP and data volumes will be destroyed."
    fi

    if [ ! -f "$STATE_FILE" ]; then
        err "No state file found. Nothing to tear down."
    fi

    info "Tearing down Agent Testnet on Vultr (${REGION})..."

    local cnames
    cnames=$(client_names_list)

    # Detach persistent data volumes (server/node only — client tier is
    # bare metal with no block storage attached).
    for role in server node; do
        detach_volume "$role"
    done

    # Delete instances (VC2 for server/node, Bare Metal for clients)
    for role in server node; do
        local inst_id
        inst_id=$(load_state "instance_${role}")
        if [ -n "$inst_id" ]; then
            info "Terminating ${role} instance: ${inst_id}..."
            vc_quiet instance delete "$inst_id" || true
        fi
    done
    for cname in $cnames; do
        local inst_id
        inst_id=$(load_state "instance_client_${cname}")
        if [ -n "$inst_id" ]; then
            info "Terminating client/${cname} bare-metal: ${inst_id}..."
            vc_quiet bare-metal delete "$inst_id" || true
        fi
    done

    # Wait briefly for instances to release firewall group / SSH key refs
    sleep 8

    # Delete firewall groups (after instances are gone so deletion isn't blocked)
    for fwg_key in firewall_group_server firewall_group_node; do
        local fwg
        fwg=$(load_state "$fwg_key")
        if [ -n "$fwg" ]; then
            info "Deleting firewall group: ${fwg}..."
            vc_quiet firewall group delete "$fwg" || warn "Failed to delete firewall group ${fwg}"
        fi
    done

    # Delete SSH key
    local key_id
    key_id=$(load_state "ssh_key_id")
    if [ -n "$key_id" ]; then
        info "Deleting SSH key registration: ${key_id}..."
        vc_quiet ssh-key delete "$key_id" || true
    fi

    # Full teardown: release Reserved IP and delete data volumes
    if $full_teardown; then
        local rip_id
        rip_id=$(load_state "reserved_ip_id")
        if [ -n "$rip_id" ]; then
            info "Releasing Reserved IP: ${rip_id}..."
            vc_quiet reserved-ip delete "$rip_id" || true
        fi

        for role in server node; do
            delete_volume "$role"
        done
    fi

    # Clean up local state
    local key_file
    key_file=$(load_state "key_file")
    if [ -n "$key_file" ] && [ -f "$key_file" ]; then
        rm -f "$key_file" "${key_file}.pub"
        info "Removed SSH key: ${key_file}"
    fi

    if $full_teardown; then
        rm -f "$STATE_FILE"
        info "Full teardown complete. All resources destroyed."
    else
        # Preserve persistent resource keys, remove everything else.
        local preserved_keys="reserved_ip_id reserved_ip_public vol_server vol_node os_id client_names"
        for cname in $cnames; do
            preserved_keys="${preserved_keys} vol_client_${cname}"
        done
        local tmp="${STATE_FILE}.tmp"
        python3 -c "
import json
with open('$STATE_FILE') as f:
    state = json.load(f)
keep = {k: state[k] for k in '$preserved_keys'.split() if k in state}
with open('$tmp', 'w') as f:
    json.dump(keep, f, indent=2)
"
        mv "$tmp" "$STATE_FILE"
        info "Teardown complete. Persistent resources preserved (Reserved IP + data volumes)."
        info "  Re-deploy with: bash deploy/vultr-deploy.sh deploy"
        info "  Full cleanup:   bash deploy/vultr-deploy.sh teardown --full"
    fi
}

# ---- argv wrappers for client-deploy / client-remove ----

# Parse the argv supplied to the `client-deploy` subcommand and call
# do_client_deploy with the resolved positional args.
do_client_deploy_cmd() {
    local name="" server_url="" join_token="" plan=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --client)     name="$2"; shift 2 ;;
            --server-url) server_url="$2"; shift 2 ;;
            --join-token) join_token="$2"; shift 2 ;;
            --plan)       plan="$2"; shift 2 ;;
            *)            err "Unknown client-deploy option: $1" ;;
        esac
    done
    do_client_deploy "$name" "$server_url" "$join_token" "$plan"
}

do_client_remove_cmd() {
    parse_client_flag "$@"
    [ -n "$CLIENT_FLAG_NAME" ] || err "Usage: client-remove --client NAME"
    do_client_remove "$CLIENT_FLAG_NAME"
}

# ---- main ----

# Migrate any legacy single-client state keys before any subcommand reads them.
migrate_legacy_client_state

ACTION="${1:-}"
case "$ACTION" in
    deploy)        do_deploy ;;
    server-deploy) do_server_deploy ;;
    node-deploy)   do_node_deploy ;;
    client-deploy) shift; do_client_deploy_cmd "$@" ;;
    client-remove) shift; do_client_remove_cmd "$@" ;;
    teardown)      do_teardown "${2:-}" ;;
    status)        do_status ;;
    ssh)           shift; do_ssh "$@" ;;
    restart)       shift; do_restart "$@" ;;
    redeploy)      shift; do_redeploy "$@" ;;
    logs)          shift; do_logs "$@" ;;
    reload)        do_reload ;;
    test)          shift; do_test "$@" ;;
    openclaw)      shift; do_openclaw "$@" ;;
    "")            err "Usage: $0 <deploy|server-deploy|node-deploy|client-deploy|client-remove|teardown [--full]|status|ssh|redeploy|restart|logs|reload|test|openclaw>" ;;
    *)             err "Unknown action: $ACTION. Use: deploy, server-deploy, node-deploy, client-deploy, client-remove, teardown [--full], status, ssh, redeploy, restart, logs, reload, test, openclaw" ;;
esac
