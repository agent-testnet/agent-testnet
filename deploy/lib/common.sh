#!/usr/bin/env bash
#
# Shared deployment helpers used by both deploy/aws-deploy.sh and
# deploy/vultr-deploy.sh.
#
# This file is sourced (`source "$(dirname "$0")/lib/common.sh"`); it does
# not exit or trap on its own. Callers must set the following globals
# BEFORE sourcing:
#
#   SCRIPT_DIR     absolute path of the cloud-specific script's directory
#   PROJECT_DIR    repo root (typically "$(dirname "$SCRIPT_DIR")")
#   DIST_DIR       where cross-compiled binaries land ("${PROJECT_DIR}/dist")
#   STATE_FILE     JSON state file path (per-cloud, e.g. .aws-state.json)
#   NODES_YAML_SRC nodes.yaml path used by reload/test/render
#
# Anything beyond logging, state I/O, SSH wrappers, multi-client
# bookkeeping, persona/openclaw orchestration, and the cloud-agnostic
# verbs (ssh/logs/reload/restart/redeploy/test) belongs in the
# per-cloud script.

# ---- logging ----

info()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33mWARN:\033[0m %s\n" "$*"; }
err()   { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

# ---- state file (JSON, parameterized by $STATE_FILE) ----

save_state() {
    local key="$1" value="$2"
    if [ ! -f "$STATE_FILE" ]; then
        echo '{}' > "$STATE_FILE"
    fi
    local tmp="${STATE_FILE}.tmp"
    python3 -c "
import json, sys
with open('$STATE_FILE') as f:
    state = json.load(f)
state['$key'] = '$value'
with open('$tmp', 'w') as f:
    json.dump(state, f, indent=2)
"
    mv "$tmp" "$STATE_FILE"
}

load_state() {
    local key="$1"
    if [ ! -f "$STATE_FILE" ]; then
        echo ""
        return
    fi
    python3 -c "
import json
with open('$STATE_FILE') as f:
    state = json.load(f)
print(state.get('$key', ''))
"
}

delete_state() {
    local key="$1"
    [ -f "$STATE_FILE" ] || return 0
    local tmp="${STATE_FILE}.tmp"
    python3 -c "
import json
with open('$STATE_FILE') as f:
    state = json.load(f)
state.pop('$key', None)
with open('$tmp', 'w') as f:
    json.dump(state, f, indent=2)
"
    mv "$tmp" "$STATE_FILE"
}

# ---- multi-client state helpers ----
#
# Clients are stored under per-name keys:
#   client_names              space-separated list, e.g. "0 1 alice"
#   instance_client_<name>    cloud instance id
#   ip_client_<name>          public IP
#   vol_client_<name>         data volume id
#
# The legacy single-client schema (instance_client / ip_client / vol_client
# without a suffix) is migrated to client "0" on first read by
# migrate_legacy_client_state below.

client_names_list() {
    load_state "client_names"
}

is_registered_client() {
    local name="$1"
    local cur
    cur=$(client_names_list)
    case " $cur " in
        *" $name "*) return 0 ;;
        *)           return 1 ;;
    esac
}

register_client() {
    local name="$1"
    is_registered_client "$name" && return 0
    local cur
    cur=$(client_names_list)
    if [ -z "$cur" ]; then
        save_state "client_names" "$name"
    else
        save_state "client_names" "$cur $name"
    fi
}

unregister_client() {
    local name="$1"
    local cur new
    cur=$(client_names_list)
    new=""
    for n in $cur; do
        [ "$n" = "$name" ] && continue
        if [ -z "$new" ]; then new="$n"; else new="$new $n"; fi
    done
    save_state "client_names" "$new"
}

next_client_index() {
    local cur i
    cur=$(client_names_list)
    i=0
    while :; do
        case " $cur " in
            *" $i "*) i=$((i + 1)) ;;
            *)        echo "$i"; return ;;
        esac
    done
}

# Resolve a user-supplied client argument (possibly empty) to a canonical
# registered name. With no arg, auto-selects the sole client; errors if zero
# or multiple clients exist. With an arg, errors if the name isn't registered.
resolve_client() {
    local arg="${1:-}"
    local names
    names=$(client_names_list)
    if [ -z "$arg" ]; then
        local count=0 only=""
        for n in $names; do
            count=$((count + 1))
            only="$n"
        done
        case "$count" in
            0) err "No clients deployed. Run 'client-deploy' first." ;;
            1) echo "$only" ;;
            *) err "Multiple clients (${names}). Specify --client NAME." ;;
        esac
        return
    fi
    if is_registered_client "$arg"; then
        echo "$arg"
    else
        err "Unknown client '${arg}'. Known clients: ${names:-<none>}"
    fi
}

# One-time migration: rewrite legacy single-client state keys (instance_client,
# ip_client, vol_client) to the per-name form (instance_client_0, ip_client_0,
# vol_client_0) and seed client_names="0". Idempotent — does nothing once a
# client_names list exists.
migrate_legacy_client_state() {
    [ -f "$STATE_FILE" ] || return 0
    local existing
    existing=$(client_names_list)
    [ -n "$existing" ] && return 0

    local legacy_inst legacy_ip legacy_vol
    legacy_inst=$(load_state "instance_client")
    legacy_ip=$(load_state "ip_client")
    legacy_vol=$(load_state "vol_client")

    [ -z "$legacy_inst" ] && [ -z "$legacy_ip" ] && [ -z "$legacy_vol" ] && return 0

    info "Migrating legacy single-client state to client '0'..."
    [ -n "$legacy_inst" ] && save_state "instance_client_0" "$legacy_inst"
    [ -n "$legacy_ip" ]   && save_state "ip_client_0"       "$legacy_ip"
    [ -n "$legacy_vol" ]  && save_state "vol_client_0"      "$legacy_vol"
    save_state "client_names" "0"

    delete_state "instance_client"
    delete_state "ip_client"
    delete_state "vol_client"
}

# Strip --client NAME from an argument vector. Writes the resulting argv to
# the global REMAINING_ARGS array and the resolved name to CLIENT_FLAG_NAME
# (empty if --client wasn't passed). Repeated --client wins last.
parse_client_flag() {
    REMAINING_ARGS=()
    CLIENT_FLAG_NAME=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --client)
                [ $# -ge 2 ] || err "--client requires a value"
                CLIENT_FLAG_NAME="$2"
                shift 2
                ;;
            *)
                REMAINING_ARGS+=("$1")
                shift
                ;;
        esac
    done
}

# ---- SSH ----

# wait_for_ssh <ip> <key> [max_attempts]
#
# Polls SSH on the target every 5s until it answers, with a configurable
# attempt cap (default 40 ≈ 200s, suitable for AWS EC2). Vultr Bare Metal
# provisioning can take 5–10 min, so the Vultr script bumps this to ~120.
wait_for_ssh() {
    local ip="$1"
    local key="$2"
    local max_attempts="${3:-40}"
    local attempt=0
    # EIP/reserved-IP reuse means the host key changes on each new instance
    ssh-keygen -R "$ip" >/dev/null 2>&1 || true
    info "Waiting for SSH on ${ip} (up to $((max_attempts * 5))s)..."
    while [ $attempt -lt $max_attempts ]; do
        if ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o BatchMode=yes \
            -i "$key" "ubuntu@${ip}" "echo ready" >/dev/null 2>&1; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 5
    done
    err "SSH to ${ip} timed out after $((max_attempts * 5))s"
}

remote_exec() {
    local ip="$1"
    local key="$2"
    shift 2
    ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes -i "$key" "ubuntu@${ip}" "$@"
}

remote_copy() {
    local key="$1"
    local src="$2"
    local dest="$3"
    scp -o StrictHostKeyChecking=accept-new -o BatchMode=yes -i "$key" "$src" "$dest"
}

# ---- block device mount ----
#
# mount_block_device <ip> <key> <mount_point> <candidate_dev> [<candidate_dev>...]
#
# Probes the target for the first block device that exists, formats it
# ext4 if it has no filesystem yet, and mounts it at <mount_point>. The
# candidate list is tried in order — AWS passes /dev/xvdf and /dev/nvme1n1
# (Nitro instances expose the volume as the latter); Vultr passes
# /dev/vdb. The helper also accepts /dev/nvme1n1 as a special-case fallback
# when /dev/xvdf is the primary candidate, preserving the existing AWS
# auto-detect behaviour.
mount_block_device() {
    local ip="$1" key="$2" mount_point="$3"
    shift 3
    [ "$#" -gt 0 ] || err "mount_block_device: at least one candidate device required"

    # Build a shell list of candidates the remote shell can iterate.
    local candidates=""
    for c in "$@"; do
        candidates="${candidates} ${c}"
    done

    info "Mounting at ${mount_point} (candidates:${candidates})..."
    remote_exec "$ip" "$key" "
        sudo bash -c '
            DEV=\"\"
            for i in \$(seq 1 10); do
                for cand in${candidates}; do
                    if [ -b \"\$cand\" ]; then
                        DEV=\"\$cand\"
                        break 2
                    fi
                done
                sleep 2
            done

            if [ -z \"\$DEV\" ]; then
                echo \"no candidate block device found:${candidates}\" >&2
                exit 1
            fi

            if ! blkid \$DEV >/dev/null 2>&1; then
                mkfs.ext4 -q \$DEV
            fi
            mkdir -p ${mount_point}
            mount \$DEV ${mount_point}
        '
    "
}

# ---- preflight + binary build ----

require_nodes_yaml() {
    if [ ! -f "$NODES_YAML_SRC" ]; then
        err "Missing ${NODES_YAML_SRC}. Copy the example and edit it:
  cp configs/nodes.yaml.example configs/nodes.yaml"
    fi
}

# Cross-compile the listed Linux/amd64 binaries into DIST_DIR if they are
# missing. Skips the build entirely when all are already present.
ensure_binaries() {
    local need_build=false
    for bin in "$@"; do
        [ -f "${DIST_DIR}/${bin}-linux-amd64" ] || need_build=true
    done
    if $need_build; then
        command -v go >/dev/null 2>&1 || err "Go not found and binaries not pre-built. Install Go 1.25+ or run: make release"
        info "Cross-compiling Linux amd64 binaries..."
        mkdir -p "$DIST_DIR"
        for bin in "$@"; do
            CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
                go build -ldflags="-s -w" -o "${DIST_DIR}/${bin}-linux-amd64" "${PROJECT_DIR}/cmd/${bin}"
            info "  Built ${bin}-linux-amd64"
        done
    else
        info "Using existing binaries in dist/ ($*)"
    fi
}

# ---- nodes.yaml rendering ----

# Read the first node name from the configured nodes.yaml.
extract_node_name() {
    local n
    n=$(grep -m1 'name:' "$NODES_YAML_SRC" | sed 's/.*name: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/' | tr -d '[:space:]')
    [ -n "$n" ] || err "Could not extract node name from ${NODES_YAML_SRC}"
    echo "$n"
}

# Render nodes.yaml with the deploy-time node address + secret substituted in.
# Args: $1 ip_node $2 node_secret $3 destination path
render_nodes_yaml() {
    local ip_node="$1" secret="$2" dest="$3"
    [ -f "$NODES_YAML_SRC" ] || err "Missing ${NODES_YAML_SRC}"
    sed -e "s|DEPLOY_NODE_ADDRESS|${ip_node}:443|g" \
        -e "s|DEPLOY_NODE_SECRET|${secret}|g" \
        "$NODES_YAML_SRC" > "$dest"
}

# ---- shared role/client resolution ----

# Resolve a role plus optional client name to the state-key suffixes used to
# look up its instance id / IP. For client, the name resolves via
# resolve_client (errors if zero/multi clients with no name supplied).
#
# Sets globals: ROLE_INSTANCE_KEY, ROLE_IP_KEY, ROLE_LABEL.
resolve_role_keys() {
    local role="$1" name="${2:-}"
    case "$role" in
        server)
            ROLE_INSTANCE_KEY="instance_server"
            ROLE_IP_KEY="ip_server"
            ROLE_LABEL="server"
            ;;
        node)
            ROLE_INSTANCE_KEY="instance_node"
            ROLE_IP_KEY="ip_node"
            ROLE_LABEL="node"
            ;;
        client)
            local cname
            cname=$(resolve_client "$name")
            ROLE_INSTANCE_KEY="instance_client_${cname}"
            ROLE_IP_KEY="ip_client_${cname}"
            ROLE_LABEL="client-${cname}"
            ;;
        *)
            err "Unknown role: ${role}. Use: server, node, client"
            ;;
    esac
}

# ---- ssh ----

do_ssh() {
    parse_client_flag "$@"
    set -- "${REMAINING_ARGS[@]+"${REMAINING_ARGS[@]}"}"
    local role="${1:-}"
    [ -n "$role" ] || err "Usage: ssh <server|node|client> [--client NAME]"
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."

    resolve_role_keys "$role" "$CLIENT_FLAG_NAME"

    local ip key_file
    ip=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")

    [ -n "$ip" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    # ServerAlive* keeps idle shells alive across cloud NAT (350s timeout).
    # Restore TTY on exit in case the user ran a TUI on the far side that
    # was killed by a network drop instead of exiting cleanly.
    trap 'stty sane 2>/dev/null || true; printf "\033[?1049l\033[?25h\033[0m" 2>/dev/null || true' EXIT INT TERM
    ssh -o StrictHostKeyChecking=accept-new \
        -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
        -o TCPKeepAlive=yes \
        -i "$key_file" "ubuntu@${ip}"
}

# ---- reload ----

do_reload() {
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."
    require_nodes_yaml

    local ip_server ip_node key_file node_secret
    ip_server=$(load_state "ip_server")
    ip_node=$(load_state "ip_node")
    key_file=$(load_state "key_file")
    node_secret=$(load_state "node_secret")
    [ -n "$ip_server" ] || err "No server IP in state file."
    [ -n "$ip_node" ] || err "No node IP in state file."
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"
    [ -n "$node_secret" ] || err "No node secret in state file."

    local nodes_tmp="${SCRIPT_DIR}/.nodes-deploy.yaml"
    sed -e "s|DEPLOY_NODE_ADDRESS|${ip_node}:443|g" \
        -e "s|DEPLOY_NODE_SECRET|${node_secret}|g" \
        "$NODES_YAML_SRC" > "$nodes_tmp"

    info "Uploading updated nodes.yaml to server (${ip_server})..."
    remote_copy "$key_file" "$nodes_tmp" "ubuntu@${ip_server}:/tmp/nodes.yaml"
    rm -f "$nodes_tmp"

    # Invalidate the CA's cached certs so new certs are issued with updated SANs.
    # The certs dir is root-owned (0700), so the glob must run inside sudo bash.
    remote_exec "$ip_server" "$key_file" "
        sudo cp /tmp/nodes.yaml /opt/testnet/configs/nodes.yaml
        sudo bash -c 'rm -rf /opt/testnet/data/certs/*'
        sudo systemctl reload testnet-server
    "
    info "Server reloaded (cert cache cleared)."
    info "If domain assignments changed, restart affected nodes: <deploy-script> restart node"
}

# ---- restart ----

# Internal: restart the service for an already-resolved role + ip.
_restart_role_at() {
    local role="$1" ip="$2" key_file="$3"
    local service_name="testnet-${role}"

    # For node restarts, also sync the node name from config
    if [ "$role" = "node" ] && [ -f "$NODES_YAML_SRC" ]; then
        local NODE_NAME
        NODE_NAME=$(extract_node_name)
        if [ -n "$NODE_NAME" ]; then
            info "Syncing node name to '${NODE_NAME}'..."
            remote_exec "$ip" "$key_file" "
                sudo sed -i \"s/^NODE_NAME=.*/NODE_NAME=${NODE_NAME}/\" /etc/testnet/node.env
            "
        fi
    fi

    info "Restarting ${service_name} on ${ip}..."
    remote_exec "$ip" "$key_file" "sudo systemctl restart ${service_name}"
    sleep 3
    if remote_exec "$ip" "$key_file" "sudo systemctl is-active --quiet ${service_name}" 2>/dev/null; then
        info "${service_name} is running."
    else
        warn "${service_name} may not have started cleanly. Check: ssh -i ${key_file} ubuntu@${ip} 'sudo journalctl -u ${service_name} -f'"
    fi
}

do_restart() {
    parse_client_flag "$@"
    set -- "${REMAINING_ARGS[@]+"${REMAINING_ARGS[@]}"}"
    local role="${1:-}"
    [ -n "$role" ] || err "Usage: restart <server|node|client> [--client NAME]"
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."

    resolve_role_keys "$role" "$CLIENT_FLAG_NAME"

    local ip key_file
    ip=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")
    [ -n "$ip" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    _restart_role_at "$role" "$ip" "$key_file"
}

# ---- redeploy ----

do_redeploy() {
    parse_client_flag "$@"
    set -- "${REMAINING_ARGS[@]+"${REMAINING_ARGS[@]}"}"
    local role="${1:-}"
    [ -n "$role" ] || err "Usage: redeploy <server|node|client> [--client NAME]"
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."

    resolve_role_keys "$role" "$CLIENT_FLAG_NAME"

    local ip key_file
    ip=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")
    [ -n "$ip" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    local bin_name="testnet-${role}"
    local bin_path="${DIST_DIR}/${bin_name}-linux-amd64"

    command -v go >/dev/null 2>&1 || err "Go not found. Install Go 1.25+ or pre-build: make release"
    info "Building ${bin_name} for linux/amd64..."
    mkdir -p "$DIST_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-s -w" -o "$bin_path" "${PROJECT_DIR}/cmd/${bin_name}"

    info "Uploading ${bin_name} to ${ROLE_LABEL} (${ip})..."
    remote_copy "$key_file" "$bin_path" "ubuntu@${ip}:/tmp/${bin_name}"
    remote_exec "$ip" "$key_file" "
        sudo mv /tmp/${bin_name} /usr/local/bin/${bin_name}
        sudo chmod +x /usr/local/bin/${bin_name}
    "

    # Also upload toolkit when redeploying the node (matches initial deploy)
    if [ "$role" = "node" ]; then
        local toolkit_path="${DIST_DIR}/testnet-toolkit-linux-amd64"
        if [ -f "$toolkit_path" ]; then
            info "Uploading testnet-toolkit to node..."
            remote_copy "$key_file" "$toolkit_path" "ubuntu@${ip}:/tmp/testnet-toolkit"
            remote_exec "$ip" "$key_file" "
                sudo mv /tmp/testnet-toolkit /usr/local/bin/testnet-toolkit
                sudo chmod +x /usr/local/bin/testnet-toolkit
            "
        fi
    fi

    info "Restarting ${bin_name}..."
    _restart_role_at "$role" "$ip" "$key_file"
}

# ---- test ----

do_test() {
    parse_client_flag "$@"
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."

    resolve_role_keys "client" "$CLIENT_FLAG_NAME"

    local ip_client key_file
    ip_client=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")
    [ -n "$ip_client" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    local test_script="${PROJECT_DIR}/scripts/vm-integration-test.sh"
    [ -f "$test_script" ] || err "Test script not found: ${test_script}"
    require_nodes_yaml

    info "Uploading integration test to ${ROLE_LABEL} (${ip_client})..."
    remote_copy "$key_file" "$test_script" "ubuntu@${ip_client}:/tmp/vm-integration-test.sh"
    remote_copy "$key_file" "$NODES_YAML_SRC" "ubuntu@${ip_client}:/tmp/nodes.yaml"

    info "Running integration test (this launches a Firecracker VM)..."
    remote_exec "$ip_client" "$key_file" "sudo bash /tmp/vm-integration-test.sh /tmp/nodes.yaml"
}

# ---- logs ----

do_logs() {
    parse_client_flag "$@"
    set -- "${REMAINING_ARGS[@]+"${REMAINING_ARGS[@]}"}"
    local role="${1:-}"
    [ -n "$role" ] || err "Usage: logs <server|node|client> [--client NAME]"
    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' first."

    resolve_role_keys "$role" "$CLIENT_FLAG_NAME"

    local ip key_file
    ip=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")
    [ -n "$ip" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    local svc_name="testnet-${role}"
    info "Tailing ${svc_name} logs on ${ROLE_LABEL} (${ip}) (Ctrl+C to stop)..."
    remote_exec "$ip" "$key_file" "sudo journalctl -u ${svc_name} -f --no-pager -n 100"
}

# ---- openclaw ----

do_openclaw() {
    local subcmd="${1:-install}"
    shift || true

    [ -f "$STATE_FILE" ] || err "No state file. Run 'deploy' or 'client-deploy' first."

    parse_client_flag "$@"
    set -- "${REMAINING_ARGS[@]+"${REMAINING_ARGS[@]}"}"

    resolve_role_keys "client" "$CLIENT_FLAG_NAME"

    local ip_client key_file
    ip_client=$(load_state "$ROLE_IP_KEY")
    key_file=$(load_state "key_file")
    [ -n "$ip_client" ] || err "No IP found for ${ROLE_LABEL}"
    [ -f "$key_file" ] || err "SSH key not found: ${key_file}"

    local oc_script="${PROJECT_DIR}/scripts/install-openclaw.sh"
    [ -f "$oc_script" ] || err "Missing ${oc_script}"

    # Upload the script (idempotent)
    remote_copy "$key_file" "$oc_script" "ubuntu@${ip_client}:/tmp/install-openclaw.sh"

    # Shared arg parser for install and reconfig. --openclaw-version is only
    # meaningful for install (reconfig keeps the already-installed version);
    # passing it on reconfig is rejected to avoid silently misleading callers.
    # --persona is allowed on both: install applies the persona to a fresh
    # workspace; reconfig swaps it (and requires --persona-confirm because
    # it overwrites IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md).
    parse_openclaw_args() {
        OC_API_KEY="${OPENCLAW_API_KEY:-${ANTHROPIC_API_KEY:-${OPENAI_API_KEY:-${OPENROUTER_API_KEY:-}}}}"
        OC_PROVIDER=""
        OC_MODEL=""
        OC_VERSION=""
        OC_PERSONA=""
        OC_PERSONA_CONFIRM=0
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --api-key)          OC_API_KEY="$2"; shift 2 ;;
                --provider)         OC_PROVIDER="$2"; shift 2 ;;
                --model)            OC_MODEL="$2"; shift 2 ;;
                --openclaw-version) OC_VERSION="$2"; shift 2 ;;
                --persona)          OC_PERSONA="$2"; shift 2 ;;
                --persona-confirm)  OC_PERSONA_CONFIRM=1; shift ;;
                *)                  err "Unknown openclaw option: $1" ;;
            esac
        done
    }

    # Tarball configs/personas/<NAME>/ and unpack it to /tmp/personas/<NAME>
    # on the remote client so the install-openclaw.sh script (running on
    # the cloud host) can find it under its default PERSONA_SRC_DIR. No-op
    # when no persona was requested.
    upload_persona() {
        local name="$1" ip="$2" key="$3"
        [ -n "$name" ] || return 0

        local src="${PROJECT_DIR}/configs/personas/${name}"
        if [ ! -d "$src" ]; then
            local available
            available=$(ls "${PROJECT_DIR}/configs/personas" 2>/dev/null | tr '\n' ' ')
            err "Persona '${name}' not found under configs/personas/. Available: ${available:-<none>}"
        fi

        local tar_local="${TMPDIR:-/tmp}/persona-${name}-$$.tar.gz"
        tar -czf "$tar_local" -C "${PROJECT_DIR}/configs/personas" "${name}"
        remote_copy "$key" "$tar_local" "ubuntu@${ip}:/tmp/persona-${name}.tar.gz"
        rm -f "$tar_local"

        # Drop the previous extraction so a stale tarball can't survive a
        # mid-deploy interruption. install-openclaw.sh reads from /tmp/personas
        # (its PERSONA_SRC_DIR default).
        remote_exec "$ip" "$key" \
            "sudo rm -rf /tmp/personas/${name} && sudo mkdir -p /tmp/personas && sudo tar -xzf /tmp/persona-${name}.tar.gz -C /tmp/personas && sudo rm -f /tmp/persona-${name}.tar.gz"
    }

    # Forward the wrapper script's basename so install-openclaw.sh's
    # post-install hint points at whichever deploy script the user ran
    # (aws-deploy.sh vs vultr-deploy.sh).
    local deploy_script_name
    deploy_script_name=$(basename "$0")

    case "$subcmd" in
        install)
            parse_openclaw_args "$@"
            [ -n "${OC_PROVIDER}" ] || OC_PROVIDER="anthropic"

            [ -n "$OC_API_KEY" ] || err "API key required. Pass --api-key KEY or export OPENCLAW_API_KEY."

            local extra_args="--provider ${OC_PROVIDER}"
            [ -n "$OC_MODEL" ]   && extra_args="${extra_args} --model ${OC_MODEL}"
            [ -n "$OC_VERSION" ] && extra_args="${extra_args} --openclaw-version ${OC_VERSION}"
            [ -n "$OC_PERSONA" ] && extra_args="${extra_args} --persona ${OC_PERSONA}"

            info "Installing OpenClaw on ${ROLE_LABEL} VM (${ip_client})..."
            info "This will launch a Firecracker agent VM, install Node.js + OpenClaw,"
            info "set up LLM API proxies, and start the gateway. This takes a few minutes."
            [ -n "$OC_PERSONA" ] && info "Applying persona: ${OC_PERSONA}"
            echo ""

            upload_persona "$OC_PERSONA" "$ip_client" "$key_file"

            remote_exec "$ip_client" "$key_file" \
                "OPENCLAW_API_KEY='${OC_API_KEY}' DEPLOY_MODE=1 DEPLOY_SCRIPT='${deploy_script_name}' sudo -E bash /tmp/install-openclaw.sh install ${extra_args}"
            ;;

        reconfig)
            parse_openclaw_args "$@"

            [ -n "$OC_API_KEY" ] || err "API key required. Pass --api-key KEY or export OPENCLAW_API_KEY / OPENROUTER_API_KEY."
            [ -z "$OC_VERSION" ] || err "--openclaw-version is install-only. Re-run 'openclaw install' to change the pinned version."

            local extra_args=""
            [ -n "$OC_PROVIDER" ] && extra_args="--provider ${OC_PROVIDER}"
            [ -n "$OC_MODEL" ]    && extra_args="${extra_args} --model ${OC_MODEL}"
            [ -n "$OC_PERSONA" ]  && extra_args="${extra_args} --persona ${OC_PERSONA}"
            # --persona-confirm propagates via env (see remote_exec below) so
            # install-openclaw.sh's non-interactive overwrite guard accepts it.
            [ "${OC_PERSONA_CONFIRM:-0}" = "1" ] && extra_args="${extra_args} --persona-confirm"

            info "Reconfiguring OpenClaw on ${ROLE_LABEL} VM (${ip_client})..."
            [ -n "$OC_PERSONA" ] && info "Swapping persona to: ${OC_PERSONA}"

            upload_persona "$OC_PERSONA" "$ip_client" "$key_file"

            remote_exec "$ip_client" "$key_file" \
                "OPENCLAW_API_KEY='${OC_API_KEY}' OPENCLAW_PERSONA_CONFIRM='${OC_PERSONA_CONFIRM:-0}' sudo -E bash /tmp/install-openclaw.sh reconfig ${extra_args}"
            ;;

        chat)
            info "Connecting to OpenClaw on ${ROLE_LABEL} VM (${ip_client})..."
            info "Press Ctrl+C to exit."
            echo ""

            # Interactive: needs -t for TTY pass-through across both SSH hops
            # (local -> cloud client -> install-openclaw.sh chat -> Firecracker VM).
            #
            # ServerAlive* keepalives prevent cloud NAT / WireGuard from
            # silently dropping the connection while the TUI sits idle
            # waiting for the user. Without them the TUI eventually receives
            # "Write failed: Broken pipe" and is killed mid-raw-mode,
            # leaving the local terminal printing escape sequences for
            # every keystroke. We restore the TTY on exit regardless.
            trap 'stty sane 2>/dev/null || true; printf "\033[?1049l\033[?25h\033[0m" 2>/dev/null || true' EXIT INT TERM
            ssh -o StrictHostKeyChecking=accept-new -t \
                -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
                -o TCPKeepAlive=yes \
                -i "$key_file" "ubuntu@${ip_client}" \
                "sudo bash /tmp/install-openclaw.sh chat"
            ;;

        status)
            remote_exec "$ip_client" "$key_file" \
                "sudo bash /tmp/install-openclaw.sh status"
            ;;

        stop)
            info "Stopping OpenClaw on ${ROLE_LABEL} VM (${ip_client})..."
            remote_exec "$ip_client" "$key_file" \
                "sudo bash /tmp/install-openclaw.sh stop"
            ;;

        *)
            err "Unknown openclaw subcommand: ${subcmd}. Use: install, reconfig, chat, status, stop"
            ;;
    esac
}
