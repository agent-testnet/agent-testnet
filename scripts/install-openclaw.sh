#!/usr/bin/env bash
#
# Install and run OpenClaw AI agent inside a testnet Firecracker VM.
#
# This script runs on the testnet CLIENT HOST (the EC2 instance that runs
# Firecracker). It handles the full lifecycle:
#
#   1. Launches a sandboxed Firecracker agent VM
#   2. Sets up transparent TCP proxies so the VM can reach Alpine repos,
#      npm registry, and your LLM API provider (the VM normally has no
#      internet — only testnet VIPs are reachable)
#   3. Installs Node.js and OpenClaw inside the VM
#   4. Configures OpenClaw with your LLM API key
#   5. Starts the OpenClaw gateway
#
# How the proxy works:
#   Agent VMs can only forward traffic to testnet VIPs (83.150.0.0/16).
#   However, traffic to the host's TAP gateway IP is LOCAL (hits the INPUT
#   chain, not FORWARD), so it bypasses sandbox restrictions. We add extra
#   IPs to the TAP device and run socat TCP proxies on each — the VM sees
#   them via /etc/hosts and TLS passes through end-to-end.
#
# Usage:
#   sudo bash scripts/install-openclaw.sh --api-key sk-ant-...
#   sudo ANTHROPIC_API_KEY=sk-ant-... bash scripts/install-openclaw.sh
#   sudo bash scripts/install-openclaw.sh --provider openai --api-key sk-...
#   sudo bash scripts/install-openclaw.sh --provider openrouter --api-key sk-or-v1-... --model anthropic/claude-3.5-haiku
#
# Commands:
#   install    (default) Launch VM, install OpenClaw, start gateway
#   chat       SSH into the VM and open the OpenClaw terminal UI
#   status     Show VM and OpenClaw status
#   stop       Stop the gateway, kill proxies, shut down the VM
#   reconfig   Change provider/model/API key on a running VM (restarts gateway)
#
# Options:
#   --api-key KEY        LLM API key (or set OPENCLAW_API_KEY / ANTHROPIC_API_KEY)
#   --provider NAME      LLM provider: anthropic, openai, xai, openrouter (default: anthropic)
#   --model NAME         Model name (default: per-provider best)
#                        For openrouter, use provider/model format (e.g. anthropic/claude-3.5-haiku)
#   --agent-ip IP        Use an already-running agent VM instead of launching one
#   --ssh-key PATH       SSH key for the existing agent VM (required with --agent-ip)
#   --vcpu N             vCPUs for the VM (default: 2)
#   --mem N              Memory in MB for the VM (default: 4096)
#
set -euo pipefail

# Force HOME=/root so testnet-client paths are consistent regardless of how
# sudo was invoked (sudo vs sudo -E). The client setup writes its config and
# data to /root/.testnet/, and testnet-client resolves ~ via $HOME.
export HOME=/root

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
STATE_FILE="${HOME}/.testnet/openclaw-state.json"

# ---- defaults ----

API_KEY="${OPENCLAW_API_KEY:-${ANTHROPIC_API_KEY:-${OPENAI_API_KEY:-${OPENROUTER_API_KEY:-}}}}"
PROVIDER="${OPENCLAW_PROVIDER:-anthropic}"
PROVIDER_EXPLICIT=false
MODEL=""
AGENT_IP=""
SSH_KEY=""
VCPU=2
MEM_MB=4096
USE_EXISTING_VM=false
COMMAND="install"

PROXY_IP_OFFSET=10

# ---- helpers ----

info()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33mWARN:\033[0m %s\n" "$*"; }
err()   { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

vm_ssh() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
        -i "$SSH_KEY" "root@${AGENT_IP}" "$@"
}

vm_ssh_interactive() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -t \
        -i "$SSH_KEY" "root@${AGENT_IP}" "$@"
}

save_state() {
    mkdir -p "$(dirname "$STATE_FILE")"
    [ -f "$STATE_FILE" ] || echo '{}' > "$STATE_FILE"
    chmod 600 "$STATE_FILE"
    python3 -c "
import json, sys
with open('$STATE_FILE') as f:
    state = json.load(f)
state[sys.argv[1]] = sys.argv[2]
with open('$STATE_FILE', 'w') as f:
    json.dump(state, f, indent=2)
" "$1" "$2"
}

load_state() {
    [ -f "$STATE_FILE" ] || { echo ""; return; }
    python3 -c "
import json, sys
with open('$STATE_FILE') as f:
    state = json.load(f)
print(state.get(sys.argv[1], ''))
" "$1"
}

get_llm_domain() {
    case "$1" in
        anthropic)  echo "api.anthropic.com" ;;
        openai)     echo "api.openai.com" ;;
        xai)        echo "api.x.ai" ;;
        openrouter) echo "openrouter.ai" ;;
        *)          echo "" ;;
    esac
}

get_default_model() {
    case "$1" in
        anthropic)  echo "claude-sonnet-4-6" ;;
        openai)     echo "gpt-5.4" ;;
        xai)        echo "grok-2" ;;
        openrouter) echo "google/gemini-2.5-flash-lite" ;;
    esac
}

get_api_key_env_var() {
    case "$1" in
        anthropic)  echo "ANTHROPIC_API_KEY" ;;
        openai)     echo "OPENAI_API_KEY" ;;
        xai)        echo "XAI_API_KEY" ;;
        openrouter) echo "OPENROUTER_API_KEY" ;;
    esac
}

# OpenRouter models use "openrouter/provider/model" format;
# direct providers use "provider/model".
get_model_ref() {
    local provider="$1" model="$2"
    if [ "$provider" = "openrouter" ]; then
        echo "openrouter/${model}"
    else
        echo "${provider}/${model}"
    fi
}

# Write ~/.openclaw/openclaw.json in the VM. Keeps install + reconfig in sync.
# Args: $1 = model_ref, $2 = gateway auth token
#
# Browser notes:
#   - ssrfPolicy.dangerouslyAllowPrivateNetwork=true works around OpenClaw
#     2026.4.x behaviour where the default SSRF policy silently blocks the
#     gateway's own loopback CDP pre-flight (ws://127.0.0.1:18800/...) and
#     any hostname-based navigation, causing "Chrome CDP websocket not
#     reachable" and "strict browser SSRF policy" errors inside the VM.
#   - Browser flags are chosen for a headless Firecracker guest (no GPU, no
#     /dev/shm worth using). Avoid --use-gl=swiftshader here: it crashes on
#     Alpine Chromium in a microVM.
write_openclaw_config() {
    local model_ref="$1" gw_token="$2"
    vm_ssh "cat > ~/.openclaw/openclaw.json" <<OCEOF
{
  "agents": {
    "defaults": {
      "workspace": "~/.openclaw/workspace",
      "model": {
        "primary": "${model_ref}"
      }
    }
  },
  "gateway": {
    "mode": "local",
    "port": 18789,
    "bind": "loopback",
    "auth": {
      "mode": "token",
      "token": "${gw_token}"
    }
  },
  "browser": {
    "enabled": true,
    "headless": true,
    "executablePath": "/usr/bin/chromium-browser",
    "noSandbox": true,
    "ssrfPolicy": {
      "dangerouslyAllowPrivateNetwork": true
    },
    "extraArgs": [
      "--disable-gpu",
      "--disable-software-rasterizer",
      "--use-gl=disabled",
      "--disable-dev-shm-usage",
      "--ignore-certificate-errors",
      "--ozone-platform=headless",
      "--disable-extensions",
      "--disable-background-networking",
      "--headless=new"
    ]
  },
  "skills": {
    "allowBundled": []
  },
  "logging": {
    "level": "info",
    "redactSensitive": "tools"
  }
}
OCEOF
    vm_ssh "chmod 600 ~/.openclaw/openclaw.json"
}

# Work around an OpenClaw 2026.4.x bug where the per-agent models.json is
# generated with "https://openrouter.ai/v1" as the OpenRouter baseUrl instead
# of the correct "https://openrouter.ai/api/v1". The wrong URL returns the
# OpenRouter marketing homepage (HTTP 200 HTML) instead of the API, so the
# OpenAI-compatible parser sees zero choices and the agent turn ends with
# stopReason=stop and empty content ("Agent couldn't generate a response").
# This patches every models.json under ~/.openclaw/agents/*/agent/ and must
# be run AFTER the gateway has started at least once (so the files exist).
fix_openrouter_base_url() {
    vm_ssh 'for f in /root/.openclaw/agents/*/agent/models.json; do
              [ -f "$f" ] || continue
              grep -q "openrouter.ai/v1\"" "$f" 2>/dev/null || continue
              sed -i "s|https://openrouter.ai/v1\"|https://openrouter.ai/api/v1\"|g" "$f"
              echo "fixed: $f"
            done' || true
}

start_openclaw_gateway() {
    vm_ssh "source /etc/profile.d/openclaw.sh && \
            export OPENCLAW_NO_RESPAWN=1 && \
            export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser && \
            export CHROME_PATH=/usr/bin/chromium-browser && \
            export NODE_COMPILE_CACHE=/var/tmp/openclaw-compile-cache && \
            mkdir -p /var/tmp/openclaw-compile-cache && \
            setsid nohup openclaw gateway </dev/null > ~/.openclaw/gateway.log 2>&1 &"
    sleep 5
}

# ---- parse arguments ----

while [[ $# -gt 0 ]]; do
    case "$1" in
        install|chat|status|stop|reconfig)
            COMMAND="$1"; shift ;;
        --api-key)
            API_KEY="$2"; shift 2 ;;
        --provider)
            PROVIDER="$2"; PROVIDER_EXPLICIT=true; shift 2 ;;
        --model)
            MODEL="$2"; shift 2 ;;
        --agent-ip)
            AGENT_IP="$2"; USE_EXISTING_VM=true; shift 2 ;;
        --ssh-key)
            SSH_KEY="$2"; USE_EXISTING_VM=true; shift 2 ;;
        --vcpu)
            VCPU="$2"; shift 2 ;;
        --mem)
            MEM_MB="$2"; shift 2 ;;
        -h|--help)
            sed -n '2,/^set -euo/{ /^set -euo/d; s/^# \?//p; }' "$0"
            exit 0 ;;
        *)
            err "Unknown option: $1 (use --help)" ;;
    esac
done

# ---- proxy management ----

start_proxy() {
    local domain="$1" proxy_ip="$2" tap_device="$3"

    ip addr add "${proxy_ip}/32" dev "$tap_device" 2>/dev/null || true

    # The INPUT chain has a blanket DROP for this TAP (set by testnet-client).
    # Insert ACCEPT rules at the top so the VM can reach this specific proxy.
    iptables -I INPUT -i "$tap_device" -d "$proxy_ip" -p tcp --dport 443 -j ACCEPT 2>/dev/null || true
    iptables -I INPUT -i "$tap_device" -d "$proxy_ip" -p tcp --dport 80 -j ACCEPT 2>/dev/null || true

    socat TCP-LISTEN:443,bind="${proxy_ip}",fork,reuseaddr "TCP:${domain}:443" </dev/null >/dev/null 2>&1 &
    local pid443=$!
    socat TCP-LISTEN:80,bind="${proxy_ip}",fork,reuseaddr "TCP:${domain}:80" </dev/null >/dev/null 2>&1 &
    local pid80=$!

    save_state "proxy_pid_${domain}_443" "$pid443"
    save_state "proxy_pid_${domain}_80" "$pid80"

    vm_ssh "grep -q '${domain}' /etc/hosts 2>/dev/null || echo '${proxy_ip} ${domain}' >> /etc/hosts"

    info "  ${domain} -> ${proxy_ip} (pids: $pid443, $pid80)"
}

stop_proxy() {
    local domain="$1"
    for suffix in 443 80; do
        local pid
        pid=$(load_state "proxy_pid_${domain}_${suffix}")
        [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    done
}

kill_all_proxies() {
    local tap_device
    tap_device=$(load_state "tap_device")
    local gw_base
    gw_base=$(load_state "gw_base")

    pkill -f "socat TCP-LISTEN.*bind=.*fork.*reuseaddr" 2>/dev/null || true

    if [ -n "$tap_device" ] && [ -n "$gw_base" ]; then
        for i in $(seq "$PROXY_IP_OFFSET" 25); do
            local pip="${gw_base}.${i}"
            iptables -D INPUT -i "$tap_device" -d "$pip" -p tcp --dport 443 -j ACCEPT 2>/dev/null || true
            iptables -D INPUT -i "$tap_device" -d "$pip" -p tcp --dport 80 -j ACCEPT 2>/dev/null || true
            ip addr del "${pip}/32" dev "$tap_device" 2>/dev/null || true
        done
    fi
}

# ---- install command ----

do_install() {
    [ "$(id -u)" -eq 0 ] || err "Must be run as root (or via sudo)"

    if [ -z "$API_KEY" ]; then
        err "LLM API key required. Pass --api-key KEY, or export OPENCLAW_API_KEY / ANTHROPIC_API_KEY."
    fi

    local llm_domain
    llm_domain=$(get_llm_domain "$PROVIDER")
    [ -n "$llm_domain" ] || err "Unknown provider: $PROVIDER (use: anthropic, openai, xai, openrouter)"

    [ -z "$MODEL" ] && MODEL=$(get_default_model "$PROVIDER")

    command -v testnet-client >/dev/null 2>&1 || err "testnet-client not found in PATH"
    if ! ip link show wg-testnet >/dev/null 2>&1; then
        err "WireGuard tunnel (wg-testnet) not up. Run 'testnet-client setup' first."
    fi

    if ! command -v socat >/dev/null 2>&1; then
        info "Installing socat..."
        apt-get update -qq >/dev/null 2>&1
        apt-get install -y -qq socat >/dev/null 2>&1 || err "Failed to install socat"
    fi

    # ---- Launch or connect to VM ----

    if $USE_EXISTING_VM; then
        [ -n "$AGENT_IP" ] || err "--agent-ip required with --ssh-key"
        [ -n "$SSH_KEY" ] || err "--ssh-key required with --agent-ip"
        info "Using existing agent VM at ${AGENT_IP}"
    else
        info "Launching agent VM (${VCPU} vCPU, ${MEM_MB}MB RAM)..."

        # Kill ALL stale processes from previous runs. Without this, old
        # Firecracker VMs keep running on the same guest IP and old socat
        # proxies hold the bind addresses, causing silent conflicts.
        pkill -9 -f "firecracker --api-sock" 2>/dev/null || true
        pkill -9 -f "testnet-client agent" 2>/dev/null || true
        pkill -f "socat TCP-LISTEN.*bind=.*fork.*reuseaddr" 2>/dev/null || true
        sleep 1

        for tap in $(ip -o link show 2>/dev/null | grep -oP 'tap-\d+' || true); do
            # Remove INPUT rules referencing this TAP before deleting it
            while iptables -D INPUT -i "$tap" -j DROP 2>/dev/null; do :; done
            iptables -D INPUT -i "$tap" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
            for i in $(seq "$PROXY_IP_OFFSET" 25); do
                iptables -D INPUT -i "$tap" -p tcp --dport 443 -j ACCEPT 2>/dev/null || true
                iptables -D INPUT -i "$tap" -p tcp --dport 80 -j ACCEPT 2>/dev/null || true
            done
            ip link del dev "$tap" 2>/dev/null || true
        done
        iptables -F FORWARD 2>/dev/null || true
        iptables -t nat -F PREROUTING 2>/dev/null || true
        iptables -t nat -F POSTROUTING 2>/dev/null || true
        rm -rf "${HOME}/.testnet/data/agents/agent-"* 2>/dev/null || true
        rm -rf /tmp/testnet-vm-* 2>/dev/null || true

        # The default rootfs is 512MB which is too small for Node.js + OpenClaw.
        # Create a larger copy (1.5GB) and pass it via --rootfs.
        local base_rootfs="${HOME}/.testnet/bin/rootfs.ext4"
        local oc_rootfs="${HOME}/.testnet/bin/rootfs-openclaw.ext4"
        local rootfs_flag=""
        if [ -f "$base_rootfs" ]; then
            info "Creating enlarged rootfs for OpenClaw (3GB)..."
            cp "$base_rootfs" "$oc_rootfs"
            truncate -s 3G "$oc_rootfs"
            e2fsck -fy "$oc_rootfs" >/dev/null 2>&1 || true
            resize2fs "$oc_rootfs" 3G
            info "  Rootfs size: $(du -h "$oc_rootfs" | cut -f1) (filesystem: $(dumpe2fs -h "$oc_rootfs" 2>/dev/null | grep 'Block count' | awk '{print $3}') blocks)"
            rootfs_flag="--rootfs ${oc_rootfs}"
            save_state "oc_rootfs" "$oc_rootfs"
        else
            warn "Base rootfs not found at ${base_rootfs}, launching with defaults"
        fi

        local launch_log
        launch_log=$(mktemp)
        nohup testnet-client agent launch --standalone \
            --vcpu "$VCPU" --mem "$MEM_MB" $rootfs_flag </dev/null > "$launch_log" 2>&1 &
        local agent_pid=$!
        disown "$agent_pid" 2>/dev/null || true
        save_state "agent_pid" "$agent_pid"

        info "Waiting for VM to boot..."
        local booted=false
        for _ in $(seq 1 45); do
            if grep -q "SSH:" "$launch_log" 2>/dev/null; then
                booted=true
                break
            fi
            if ! kill -0 "$agent_pid" 2>/dev/null; then
                echo "--- Agent launch output ---"
                cat "$launch_log"
                rm -f "$launch_log"
                err "Agent process died during launch"
            fi
            sleep 1
        done

        if ! $booted; then
            echo "--- Agent launch output ---"
            cat "$launch_log"
            rm -f "$launch_log"
            err "VM did not launch within 45 seconds"
        fi

        AGENT_IP=$(grep "Guest IP:" "$launch_log" | awk '{print $NF}')
        SSH_KEY=$(grep "SSH:" "$launch_log" | awk '{for(i=1;i<=NF;i++) if($i=="-i") print $(i+1)}')
        local agent_id
        agent_id=$(grep "ID:" "$launch_log" | head -1 | awk '{print $NF}')
        rm -f "$launch_log"

        [ -n "$AGENT_IP" ] || err "Could not parse guest IP from launch output"
        [ -n "$SSH_KEY" ] || err "Could not parse SSH key from launch output"

        info "VM launched: ${agent_id}"
        info "  Guest IP: ${AGENT_IP}"
        info "  SSH key:  ${SSH_KEY}"
    fi

    save_state "agent_ip" "$AGENT_IP"
    save_state "ssh_key" "$SSH_KEY"
    save_state "provider" "$PROVIDER"

    # Wait for SSH
    info "Waiting for SSH access..."
    local ssh_ready=false
    for _ in $(seq 1 30); do
        if vm_ssh "echo ready" >/dev/null 2>&1; then
            ssh_ready=true
            break
        fi
        sleep 2
    done
    $ssh_ready || err "VM not reachable via SSH after 60 seconds"
    info "VM is SSH-ready."

    local disk_info
    disk_info=$(vm_ssh "df -h / | tail -1" 2>/dev/null || echo "unknown")
    info "VM disk: ${disk_info}"

    # ---- Detect network topology ----

    local gw_base
    gw_base=$(echo "$AGENT_IP" | sed 's/\.[0-9]*$//')
    local gateway_ip="${gw_base}.1"
    save_state "gw_base" "$gw_base"

    local tap_device
    tap_device=$(ip -o addr show | grep "${gateway_ip}/" | awk '{print $2}' | head -1)
    [ -n "$tap_device" ] || err "Could not find TAP device for gateway ${gateway_ip}"
    save_state "tap_device" "$tap_device"
    info "Network: ${tap_device} (gateway ${gateway_ip}, guest ${AGENT_IP})"

    # ---- Set up domain proxies ----
    #
    # Each domain gets a unique IP on the TAP interface and a socat TCP proxy.
    # The VM reaches these via /etc/hosts entries. TLS passes through end-to-end
    # because socat just forwards raw TCP bytes — the VM's TLS client talks
    # directly to the real server, and the cert matches the hostname.

    info "Setting up domain proxies..."
    local ip_offset=$PROXY_IP_OFFSET

    # OpenClaw's npm tree pulls a couple of dependencies directly from GitHub
    # (e.g. libsignal-node via `ssh://git@github.com/...`). We proxy github.com
    # on 443 and force git-over-HTTPS below so the install doesn't try port 22.
    local install_domains="dl-cdn.alpinelinux.org registry.npmjs.org github.com codeload.github.com"
    for domain in $install_domains $llm_domain; do
        local proxy_ip="${gw_base}.${ip_offset}"
        start_proxy "$domain" "$proxy_ip" "$tap_device"
        ip_offset=$((ip_offset + 1))
    done

    save_state "llm_domain" "$llm_domain"
    sleep 1

    # ---- Install Node.js + npm via Alpine packages ----

    info "Updating Alpine packages..."
    vm_ssh "apk update" >/dev/null 2>&1 || {
        warn "apk update failed — checking proxy connectivity..."
        vm_ssh "wget -q -O /dev/null https://dl-cdn.alpinelinux.org/alpine/v3.21/main/x86_64/APKINDEX.tar.gz" 2>&1 || true
        err "Cannot reach Alpine mirror through proxy. Check socat processes on host."
    }

    info "Installing Node.js, npm, and Chromium..."
    vm_ssh "apk add --no-cache nodejs npm chromium nss freetype harfbuzz ca-certificates ttf-freefont" >/dev/null 2>&1

    # Configure Chromium for headless operation inside a microVM.
    # The wrapper at /usr/bin/chromium-browser sources this file and applies
    # the flags to every launch, so they take effect regardless of how
    # OpenClaw (or the agent) invokes the browser.
    vm_ssh "cat > /etc/chromium/chromium.conf" <<'CRCONF'
CHROMIUM_FLAGS="--disable-gpu --disable-software-rasterizer --use-gl=disabled --disable-dev-shm-usage --ignore-certificate-errors --ozone-platform=headless"
CRCONF

    local node_ver
    node_ver=$(vm_ssh "node --version" 2>/dev/null || echo "FAILED")
    if [ "$node_ver" = "FAILED" ]; then
        err "Node.js installation failed"
    fi
    info "Node.js ${node_ver} installed in VM"

    # ---- Install OpenClaw ----

    # Alpine's bundled npm ships an old tar (6.x) that chokes on deeply nested
    # paths in packages like @aws-sdk. Upgrading npm first pulls in a fixed tar.
    info "Upgrading npm..."
    vm_ssh "mkdir -p /root/.npm-tmp && TMPDIR=/root/.npm-tmp npm install -g npm@latest 2>&1" | tail -2

    # Install git (needed for npm git-url deps) and rewrite ssh://git@github.com
    # URLs to https:// so git uses the github.com:443 proxy we set up above.
    vm_ssh "apk add --no-cache git openssh-client" >/dev/null 2>&1
    vm_ssh "git config --global --unset-all url.https://github.com/.insteadOf 2>/dev/null; \
            git config --global --add url.https://github.com/.insteadOf ssh://git@github.com/ && \
            git config --global --add url.https://github.com/.insteadOf git@github.com: && \
            git config --global --add url.https://github.com/.insteadOf git://github.com/"

    # npm uses /tmp for extraction, which is tmpfs (RAM-backed) on Alpine.
    # OpenClaw + @aws-sdk deps need significant temp space, so point npm's
    # tmp and cache to the rootfs instead.
    info "Installing OpenClaw via npm (this may take a few minutes)..."
    vm_ssh "mkdir -p /root/.npm-tmp && TMPDIR=/root/.npm-tmp npm install -g openclaw --cache /root/.npm-cache 2>&1" | tail -5

    if ! vm_ssh "command -v openclaw" >/dev/null 2>&1; then
        err "OpenClaw installation failed. Check: ssh -i $SSH_KEY root@$AGENT_IP 'npm install -g openclaw'"
    fi

    local oc_ver
    oc_ver=$(vm_ssh "openclaw --version" 2>/dev/null || echo "unknown")
    info "OpenClaw ${oc_ver} installed in VM"

    # ---- Remove install-only proxies (keep LLM API) ----

    info "Removing temporary installation proxies..."
    for domain in $install_domains; do
        stop_proxy "$domain"
    done
    local cleanup_offset=$PROXY_IP_OFFSET
    for domain in $install_domains; do
        ip addr del "${gw_base}.${cleanup_offset}/32" dev "$tap_device" 2>/dev/null || true
        cleanup_offset=$((cleanup_offset + 1))
    done

    # ---- Configure OpenClaw ----

    local api_key_env
    api_key_env=$(get_api_key_env_var "$PROVIDER")

    info "Configuring OpenClaw (provider: ${PROVIDER}, model: ${MODEL})..."

    vm_ssh "mkdir -p ~/.openclaw/workspace ~/.openclaw/logs"

    # Generate a random auth token for the gateway
    local gw_token
    gw_token=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)
    save_state "gw_token" "$gw_token"

    local model_ref
    model_ref=$(get_model_ref "$PROVIDER" "$MODEL")

    # OpenClaw strictly validates config — unknown keys prevent gateway from starting.
    write_openclaw_config "$model_ref" "$gw_token"

    # Persist the API key via OpenClaw's .env mechanism (auto-loaded by the gateway)
    vm_ssh "cat > ~/.openclaw/.env" <<ENV
${api_key_env}=${API_KEY}
PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 ~/.openclaw/.env"

    # Also set it in the shell environment for CLI usage (openclaw tui, etc.)
    vm_ssh "cat > /etc/profile.d/openclaw.sh" <<ENV
export ${api_key_env}="${API_KEY}"
export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
export CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 /etc/profile.d/openclaw.sh"

    # ---- Start OpenClaw gateway ----

    info "Starting OpenClaw gateway (first boot, to materialize agent state)..."
    start_openclaw_gateway

    if ! vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        warn "Gateway may not have started cleanly."
        warn "Log output:"
        vm_ssh "cat ~/.openclaw/gateway.log" 2>/dev/null || true
    fi

    # Wait for the default agent's models.json to be created, then patch the
    # OpenRouter baseUrl and restart the gateway so the fix takes effect.
    info "Applying OpenRouter baseUrl workaround..."
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        vm_ssh "test -f /root/.openclaw/agents/main/agent/models.json" >/dev/null 2>&1 && break
        sleep 1
    done
    fix_openrouter_base_url
    vm_ssh "killall -9 openclaw-gateway 2>/dev/null; pkill -9 -f 'openclaw.*gateway' 2>/dev/null; sleep 1" || true

    info "Restarting OpenClaw gateway with patched models.json..."
    start_openclaw_gateway

    if vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        info "OpenClaw gateway is running!"
    else
        warn "Gateway may not have started cleanly."
        warn "Log output:"
        vm_ssh "cat ~/.openclaw/gateway.log" 2>/dev/null || true
    fi

    # ---- Summary ----

    echo ""
    echo "============================================"
    echo "  OpenClaw installed in testnet agent VM"
    echo "============================================"
    echo ""
    echo "  Agent IP:   ${AGENT_IP}"
    echo "  Provider:   ${PROVIDER} (${MODEL})"
    echo "  OpenClaw:   ${oc_ver}"
    echo "  Node.js:    ${node_ver}"
    echo ""

    if [ "${DEPLOY_MODE:-}" = "1" ]; then
        echo "  Talk to OpenClaw:"
        echo "    bash deploy/aws-deploy.sh openclaw chat"
        echo ""
        echo "  Check status:"
        echo "    bash deploy/aws-deploy.sh openclaw status"
        echo ""
        echo "  Stop everything:"
        echo "    bash deploy/aws-deploy.sh openclaw stop"
    else
        echo "  Talk to OpenClaw:"
        echo "    sudo bash scripts/install-openclaw.sh chat"
        echo ""
        echo "  Or SSH in directly:"
        echo "    ssh -i ${SSH_KEY} root@${AGENT_IP}"
        echo '    source /etc/profile.d/openclaw.sh'
        echo '    openclaw tui'
        echo ""
        echo "  Check status:"
        echo "    sudo bash scripts/install-openclaw.sh status"
        echo ""
        echo "  Stop everything:"
        echo "    sudo bash scripts/install-openclaw.sh stop"
    fi
    echo ""
}

# ---- chat command ----

do_chat() {
    load_vm_state

    if ! vm_ssh "echo ok" >/dev/null 2>&1; then
        err "Agent VM at ${AGENT_IP} is not reachable. Run 'install' first."
    fi

    if ! vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        warn "OpenClaw gateway is not running — starting it..."
        fix_openrouter_base_url
        start_openclaw_gateway
    fi

    ensure_llm_proxy

    info "Connecting to OpenClaw in agent VM..."
    echo "  Use Ctrl+C to exit."
    echo ""

    vm_ssh_interactive 'source /etc/profile.d/openclaw.sh && export OPENCLAW_NO_RESPAWN=1 && openclaw tui'
}

# ---- status command ----

do_status() {
    load_vm_state

    echo ""
    echo "Agent Testnet — OpenClaw Status"
    echo "==============================="
    echo ""
    echo "  Agent IP:  ${AGENT_IP}"
    echo "  SSH key:   ${SSH_KEY}"

    if ! vm_ssh "echo ok" >/dev/null 2>&1; then
        echo "  VM:        UNREACHABLE"
        echo ""
        return
    fi
    echo "  VM:        reachable"

    local oc_ver
    oc_ver=$(vm_ssh "openclaw --version 2>/dev/null" || echo "not installed")
    echo "  OpenClaw:  ${oc_ver}"

    if vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        echo "  Gateway:   running"
    else
        echo "  Gateway:   stopped"
    fi

    local provider
    provider=$(load_state "provider")
    local llm_domain
    llm_domain=$(load_state "llm_domain")
    echo "  Provider:  ${provider:-unknown}"

    local proxy_running=false
    if [ -n "$llm_domain" ]; then
        local pid
        pid=$(load_state "proxy_pid_${llm_domain}_443")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            proxy_running=true
        fi
    fi
    echo "  LLM proxy: $(${proxy_running} && echo 'running' || echo 'stopped')"
    echo ""
}

# ---- stop command ----

do_stop() {
    [ "$(id -u)" -eq 0 ] || err "Must be run as root (or via sudo)"

    info "Stopping OpenClaw..."

    AGENT_IP=$(load_state "agent_ip")
    SSH_KEY=$(load_state "ssh_key")

    if [ -n "$AGENT_IP" ] && [ -n "$SSH_KEY" ]; then
        if vm_ssh "echo ok" >/dev/null 2>&1; then
            vm_ssh "kill -9 \$(pgrep -f '[o]penclaw') 2>/dev/null || true"
            info "Gateway stopped."
        fi
    fi

    info "Stopping proxies..."
    kill_all_proxies

    local agent_pid
    agent_pid=$(load_state "agent_pid")
    if [ -n "$agent_pid" ] && kill -0 "$agent_pid" 2>/dev/null; then
        info "Stopping agent VM (pid ${agent_pid})..."
        kill "$agent_pid" 2>/dev/null || true
        sleep 2
        kill -9 "$agent_pid" 2>/dev/null || true
    fi

    local oc_rootfs
    oc_rootfs=$(load_state "oc_rootfs")
    [ -n "$oc_rootfs" ] && rm -f "$oc_rootfs"

    rm -f "$STATE_FILE"
    info "All stopped."
}

# ---- reconfig command ----

do_reconfig() {
    [ "$(id -u)" -eq 0 ] || err "Must be run as root (or via sudo)"

    load_vm_state

    if ! vm_ssh "echo ok" >/dev/null 2>&1; then
        err "Agent VM at ${AGENT_IP} is not reachable. Run 'install' first."
    fi

    if ! vm_ssh "command -v openclaw" >/dev/null 2>&1; then
        err "OpenClaw is not installed in the VM. Run 'install' first."
    fi

    # Fall back to the provider from the last install if not explicitly passed
    if ! $PROVIDER_EXPLICIT; then
        local old_provider
        old_provider=$(load_state "provider")
        [ -n "$old_provider" ] && PROVIDER="$old_provider"
    fi

    [ -z "$MODEL" ] && MODEL=$(get_default_model "$PROVIDER")

    local llm_domain
    llm_domain=$(get_llm_domain "$PROVIDER")
    [ -n "$llm_domain" ] || err "Unknown provider: $PROVIDER (use: anthropic, openai, xai, openrouter)"

    if [ -z "$API_KEY" ]; then
        err "API key required for reconfig. Pass --api-key KEY or export OPENCLAW_API_KEY."
    fi

    local api_key_env
    api_key_env=$(get_api_key_env_var "$PROVIDER")
    local model_ref
    model_ref=$(get_model_ref "$PROVIDER" "$MODEL")

    info "Reconfiguring OpenClaw (provider: ${PROVIDER}, model: ${MODEL})..."

    # Stop running gateway
    vm_ssh "kill \$(pgrep -f '[o]penclaw.*gateway') 2>/dev/null || true"
    sleep 2

    # Read existing config and update the model
    local gw_token
    gw_token=$(load_state "gw_token")
    [ -n "$gw_token" ] || gw_token=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)

    write_openclaw_config "$model_ref" "$gw_token"

    # Update API key
    vm_ssh "cat > ~/.openclaw/.env" <<ENV
${api_key_env}=${API_KEY}
PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 ~/.openclaw/.env"

    vm_ssh "cat > /etc/profile.d/openclaw.sh" <<ENV
export ${api_key_env}="${API_KEY}"
export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
export CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 /etc/profile.d/openclaw.sh"

    # Switch LLM proxy if the provider changed
    local old_llm_domain
    old_llm_domain=$(load_state "llm_domain")
    if [ "$llm_domain" != "$old_llm_domain" ]; then
        info "Switching LLM proxy: ${old_llm_domain:-none} -> ${llm_domain}"

        # Stop old proxy by PID
        [ -n "$old_llm_domain" ] && stop_proxy "$old_llm_domain"

        local tap_device gw_base
        tap_device=$(load_state "tap_device")
        gw_base=$(load_state "gw_base")

        if [ -n "$tap_device" ] && [ -n "$gw_base" ]; then
            local install_domain_count=2
            local llm_offset=$((PROXY_IP_OFFSET + install_domain_count))
            local proxy_ip="${gw_base}.${llm_offset}"

            # Kill ANY socat still bound to this IP (catches stale processes
            # whose PIDs are no longer tracked in state)
            local stale_pids
            stale_pids=$(ss -tlnp 2>/dev/null | grep "${proxy_ip}:" | grep -oP 'pid=\K[0-9]+' || true)
            for pid in $stale_pids; do
                kill "$pid" 2>/dev/null || true
            done
            [ -n "$stale_pids" ] && sleep 1

            # Remove old /etc/hosts entry and add new one
            vm_ssh "sed -i '/${old_llm_domain:-NOOP}/d' /etc/hosts 2>/dev/null || true"
            start_proxy "$llm_domain" "$proxy_ip" "$tap_device"
        fi

        save_state "llm_domain" "$llm_domain"
    fi

    save_state "provider" "$PROVIDER"

    # Re-apply the OpenRouter baseUrl workaround in case a newer OpenClaw
    # version regenerated models.json since last run, then restart gateway.
    fix_openrouter_base_url

    info "Starting OpenClaw gateway..."
    start_openclaw_gateway

    if vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        info "OpenClaw gateway is running!"
    else
        warn "Gateway may not have started cleanly."
        vm_ssh "cat ~/.openclaw/gateway.log" 2>/dev/null || true
    fi

    echo ""
    echo "  Reconfigured: ${PROVIDER} / ${MODEL}"
    echo "  Model ref:    ${model_ref}"
    echo ""
}

# ---- shared helpers ----

load_vm_state() {
    [ -z "$AGENT_IP" ] && AGENT_IP=$(load_state "agent_ip")
    [ -z "$SSH_KEY" ] && SSH_KEY=$(load_state "ssh_key")
    [ -n "$AGENT_IP" ] || err "No agent VM found. Run 'install' first."
    [ -n "$SSH_KEY" ] || err "No SSH key found. Run 'install' first."
}

ensure_llm_proxy() {
    local llm_domain
    llm_domain=$(load_state "llm_domain")
    [ -n "$llm_domain" ] || return 0

    local pid
    pid=$(load_state "proxy_pid_${llm_domain}_443")
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        return 0
    fi

    local tap_device gw_base
    tap_device=$(load_state "tap_device")
    gw_base=$(load_state "gw_base")
    [ -n "$tap_device" ] && [ -n "$gw_base" ] || return 0

    local install_domain_count=2
    local llm_offset=$((PROXY_IP_OFFSET + install_domain_count))
    local proxy_ip="${gw_base}.${llm_offset}"

    info "Restarting LLM API proxy for ${llm_domain}..."
    start_proxy "$llm_domain" "$proxy_ip" "$tap_device"
}

# ---- main ----

case "$COMMAND" in
    install)  do_install ;;
    chat)     do_chat ;;
    status)   do_status ;;
    stop)     do_stop ;;
    reconfig) do_reconfig ;;
    *)        err "Unknown command: $COMMAND" ;;
esac
