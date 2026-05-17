#!/usr/bin/env bash
#
# Install and run OpenClaw AI agent inside a testnet Firecracker VM.
#
# This script runs on the testnet CLIENT HOST (the EC2 instance that runs
# Firecracker). It handles the full lifecycle:
#
#   1. Launches a sandboxed Firecracker agent VM via the testnet-client daemon
#   2. Asks the daemon to expose a few real upstream domains (alpine repos,
#      npm registry, GitHub for npm git deps, and your LLM API provider) to
#      the VM via per-VM passthrough proxies. Install-only domains use a
#      "private" per-VM IP (172.16.<vmIndex>.x); the LLM proxy uses a
#      "public" IP (83.150.255.x) so OpenClaw's url-fetch SSRF guard, which
#      rejects RFC1918 / "special-use" IPs, doesn't block the gateway.
#   3. Installs Node.js and OpenClaw inside the VM
#   4. Configures OpenClaw with your LLM API key
#   5. Starts the OpenClaw gateway
#
# All proxy IP allocation, iptables rules, TCP forwarding, and /etc/hosts
# injection inside the VM are owned by the testnet-client daemon — see
# `testnet-client agent proxy --help` and docs/agent-proxies.md.
#
# Usage:
#   sudo bash scripts/install-openclaw.sh --api-key sk-ant-...
#   sudo ANTHROPIC_API_KEY=sk-ant-... bash scripts/install-openclaw.sh
#   sudo bash scripts/install-openclaw.sh --provider openai --api-key sk-...
#   sudo bash scripts/install-openclaw.sh --provider openrouter --api-key sk-or-v1-... --model anthropic/claude-haiku-4.5
#   sudo bash scripts/install-openclaw.sh --openclaw-version 2026.5.7 --api-key sk-ant-...
#
# Commands:
#   install    (default) Launch VM, install OpenClaw, start gateway
#   chat       SSH into the VM and open the OpenClaw terminal UI
#   status     Show VM and OpenClaw status
#   stop       Stop the gateway, tear down proxies, shut down the VM
#   reconfig   Change provider/model/API key on a running VM (restarts gateway)
#
# Options:
#   --api-key KEY            LLM API key (or set OPENCLAW_API_KEY / ANTHROPIC_API_KEY)
#   --provider NAME          LLM provider: anthropic, openai, xai, openrouter (default: anthropic)
#   --model NAME             Model name (default: per-provider best)
#                            For openrouter, use provider/model format (e.g. anthropic/claude-haiku-4.5)
#   --openclaw-version VER   Pin npm version of openclaw to install (default:
#                            tested CalVer release — see OPENCLAW_DEFAULT_VERSION).
#                            Pass "latest" to follow the npm latest dist-tag (NOT recommended — see below).
#   --persona NAME           Apply persona files from $PERSONA_SRC_DIR/<NAME>
#                            (default /tmp/personas/<NAME>) to the agent VM's
#                            ~/.openclaw/workspace, splice the persona's
#                            heartbeat.json into agents.defaults.heartbeat,
#                            and restart the gateway. On 'reconfig' the
#                            persona's IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md
#                            files are OVERWRITTEN; memory/, MEMORY.md, and
#                            anything else in the workspace are preserved.
#                            See configs/personas/README.md.
#   --persona-confirm        Acknowledges the workspace-file overwrite for
#                            non-interactive 'reconfig --persona NAME' runs
#                            (equivalent to OPENCLAW_PERSONA_CONFIRM=1).
#                            Ignored on 'install' (no existing files to lose).
#   --agent-ip IP            Use an already-running agent VM instead of launching one
#   --ssh-key PATH           SSH key for the existing agent VM (required with --agent-ip)
#   --vcpu N                 vCPUs for the VM (default: 2)
#   --mem N                  Memory in MB for the VM (default: 4096)
#
# Why we pin OpenClaw:
#   OpenClaw ships daily CalVer releases under the npm `latest` tag, which has
#   repeatedly shipped agentic-tool regressions that break the gateway against
#   Anthropic-compatible providers (orphan tool_use blocks, missing browser
#   tool, etc. — see openclaw/openclaw#74907, #76507, #70456). Pinning to a
#   tested version isolates this stack from those upstream churns. Override
#   only when you've verified a newer version works for your provider/model
#   combination.
set -euo pipefail

# Force HOME=/root so testnet-client paths are consistent regardless of how
# sudo was invoked (sudo vs sudo -E). The client setup writes its config and
# data to /root/.testnet/, and testnet-client resolves ~ via $HOME.
export HOME=/root

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
STATE_FILE="${HOME}/.testnet/openclaw-state.json"

# Domains we expose to the agent VM during install. These are dumb TCP
# passthroughs — TLS is end-to-end against the real upstream, so cert
# validation works without per-tool config.
INSTALL_DOMAINS=(
    "dl-cdn.alpinelinux.org"
    "registry.npmjs.org"
    "github.com"
    "codeload.github.com"
)

# ---- defaults ----

# Pinned, tested OpenClaw release. Bump this only after verifying a newer
# version doesn't regress the gateway tool-use / browser path against
# Anthropic-compatible providers. See header for rationale.
OPENCLAW_DEFAULT_VERSION="2026.5.7"

API_KEY="${OPENCLAW_API_KEY:-${ANTHROPIC_API_KEY:-${OPENAI_API_KEY:-${OPENROUTER_API_KEY:-}}}}"
PROVIDER="${OPENCLAW_PROVIDER:-anthropic}"
PROVIDER_EXPLICIT=false
MODEL=""
OPENCLAW_VERSION="${OPENCLAW_VERSION:-$OPENCLAW_DEFAULT_VERSION}"

# Persona overlay (optional). $OPENCLAW_PERSONA is the persona subdir name
# under $PERSONA_SRC_DIR (e.g. "lobby"). $PERSONA_SRC_DIR points at where
# the persona tarball is unpacked locally on this host; deploy/aws-deploy.sh
# uploads it to /tmp/personas/<NAME> by default. The confirm flag/env var
# is required for non-interactive `reconfig --persona` runs because that
# path overwrites IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md in the agent VM.
OPENCLAW_PERSONA="${OPENCLAW_PERSONA:-}"
OPENCLAW_PERSONA_CONFIRM="${OPENCLAW_PERSONA_CONFIRM:-0}"
PERSONA_SRC_DIR="${PERSONA_SRC_DIR:-/tmp/personas}"

AGENT_ID=""
AGENT_IP=""
SSH_KEY=""
VCPU=2
MEM_MB=4096
USE_EXISTING_VM=false
COMMAND="install"

# ---- helpers ----

info()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33mWARN:\033[0m %s\n" "$*"; }
err()   { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

vm_ssh() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
        -i "$SSH_KEY" "root@${AGENT_IP}" "$@"
}

# scp wrapper sharing vm_ssh's options. Used by apply_persona to upload
# the workspace markdown files; using scp instead of `vm_ssh "cat > ..."`
# avoids quoting headaches around arbitrary persona content.
vm_scp() {
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
        -i "$SSH_KEY" "$@"
}

# Interactive SSH into the Firecracker agent VM. Adds ServerAlive* keepalives
# so the WireGuard / NAT path doesn't drop an idle OpenClaw TUI session (the
# AWS NAT idle timeout is 350s; a TUI sitting on a model prompt can easily
# exceed that). Without these the user gets "Write failed: Broken pipe" and
# their local terminal is left in raw mode by the abandoned TUI.
vm_ssh_interactive() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -t \
        -o ServerAliveInterval=30 -o ServerAliveCountMax=6 \
        -o TCPKeepAlive=yes \
        -i "$SSH_KEY" "root@${AGENT_IP}" "$@"
}

# Restore the local TTY state. Call this before exiting any function that
# spawned a curses/raw-mode TUI through SSH: if the remote TUI dies abnormally
# (broken pipe, killed gateway, network blip), the local terminal stays in
# raw mode and prints typed bytes as escape sequences instead of executing
# commands. `stty sane` re-enables canonical/echo modes, and the escape
# sequences leave the alternate screen buffer and re-show the cursor.
restore_tty() {
    stty sane 2>/dev/null || true
    # rmcup (leave alt screen) + cnorm (show cursor) + sgr0 (reset attrs).
    # Use raw escapes rather than `tput` so we don't depend on TERM being
    # set correctly after an abnormal exit.
    printf '\033[?1049l\033[?25h\033[0m' 2>/dev/null || true
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

reset_state() {
    mkdir -p "$(dirname "$STATE_FILE")"
    echo '{}' > "$STATE_FILE"
    chmod 600 "$STATE_FILE"
}

# ensure_daemon errors out unless `testnet-client daemon` is reachable.
# All proxy + agent-lifecycle ops in this script go through the daemon, so
# without it nothing useful can happen.
ensure_daemon() {
    if ! testnet-client agent list >/dev/null 2>&1; then
        err "testnet-client daemon is not reachable. Start it (e.g. 'systemctl start testnet-client') and try again."
    fi
}

agent_proxy_add() {
    local domain="$1" visibility="$2"
    if ! testnet-client agent proxy add "$AGENT_ID" "$domain" --visibility "$visibility" --json >/dev/null; then
        err "failed to add ${visibility} proxy for ${domain} on agent ${AGENT_ID}"
    fi
    info "  proxy: ${domain} (${visibility})"
}

agent_proxy_remove() {
    local domain="$1"
    testnet-client agent proxy remove "$AGENT_ID" "$domain" >/dev/null 2>&1 || true
}

# get_llm_proxy_ip returns the IP the daemon assigned to the LLM proxy (so
# /etc/hosts entries / status checks can reference it). Empty if absent.
get_llm_proxy_ip() {
    local domain="$1"
    testnet-client agent proxy list "$AGENT_ID" --json 2>/dev/null | \
        python3 -c "
import json, sys
data = json.load(sys.stdin) or []
for p in data:
    if p.get('domain') == sys.argv[1]:
        print(p.get('ip', ''))
        break
" "$domain"
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

# load_persona_heartbeat NAME
#
# Reads $PERSONA_SRC_DIR/<NAME>/heartbeat.json and prints it as a single
# minified JSON object so it can be spliced verbatim into openclaw.json
# (see write_openclaw_config). Empty output if the persona doesn't ship
# a heartbeat.json — callers treat that as "no heartbeat block".
load_persona_heartbeat() {
    local name="$1"
    local hb_path="${PERSONA_SRC_DIR}/${name}/heartbeat.json"
    [ -f "$hb_path" ] || { echo ""; return; }
    python3 -c "
import json, sys
with open(sys.argv[1]) as f:
    print(json.dumps(json.load(f)))
" "$hb_path"
}

# apply_persona NAME MODE
#
# Apply the persona at $PERSONA_SRC_DIR/<NAME> to the agent VM's OpenClaw
# workspace.
#
# MODE is either "install" or "reconfig":
#   install  — workspace is fresh, no confirmation needed.
#   reconfig — the workspace probably already has a different persona
#              installed; warn the operator and require confirmation
#              before overwriting IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md.
#
# Confirmation policy:
#   - Interactive shell (both stdin and stdout are TTYs): prompt y/N.
#   - Otherwise (deploy-script path, CI): require OPENCLAW_PERSONA_CONFIRM=1
#     in the environment. The deploy wrapper sets this when the user
#     passes `--persona-confirm`.
#
# Only the five known persona files are touched. memory/, MEMORY.md, and
# anything else the agent has written are preserved. BOOTSTRAP.md is
# removed so OpenClaw's identity-ritual code doesn't re-fire and
# overwrite the new IDENTITY.md on next agent boot. The workspace git
# repo (managed by OpenClaw, not us) is committed if present so the
# swap shows up in its history.
#
# Side effects: saves `persona` and `heartbeat_json` to the local state
# file so subsequent `reconfig` runs without --persona keep the same
# heartbeat schedule.
apply_persona() {
    local name="$1" mode="${2:-install}"
    [ -n "$name" ]              || err "apply_persona: persona name required"
    [ "$mode" = "install" ] || [ "$mode" = "reconfig" ] \
        || err "apply_persona: mode must be 'install' or 'reconfig' (got '$mode')"

    local persona_dir="${PERSONA_SRC_DIR}/${name}"
    [ -d "$persona_dir" ] || err "Persona directory not found: ${persona_dir}"

    local required
    for required in IDENTITY.md SOUL.md AGENTS.md; do
        [ -f "${persona_dir}/${required}" ] \
            || err "Persona '${name}' is missing required file: ${required}"
    done

    # Files this helper owns. Anything outside this list (memory/,
    # MEMORY.md, plus whatever else OpenClaw or the agent itself writes)
    # is left untouched on purpose.
    local persona_files=(IDENTITY.md SOUL.md AGENTS.md USER.md HEARTBEAT.md)

    if [ "$mode" = "reconfig" ]; then
        # List which of the persona files actually exist on the VM so the
        # warning is precise.
        local existing="" f=""
        for f in "${persona_files[@]}"; do
            if vm_ssh "test -f ~/.openclaw/workspace/${f}" >/dev/null 2>&1; then
                existing="${existing} ${f}"
            fi
        done

        if [ -n "$existing" ]; then
            warn "About to overwrite workspace files on persona swap to '${name}':${existing}"
            warn "memory/, MEMORY.md, and other workspace files are preserved."

            if [ -t 0 ] && [ -t 1 ]; then
                printf "Continue? [y/N] " >&2
                local reply=""
                read -r reply
                case "$reply" in
                    y|Y|yes|YES) ;;
                    *) err "Aborted by user." ;;
                esac
            else
                if [ "${OPENCLAW_PERSONA_CONFIRM:-0}" != "1" ]; then
                    err "Refusing to overwrite persona files in non-interactive mode. Re-run with --persona-confirm (or set OPENCLAW_PERSONA_CONFIRM=1)."
                fi
            fi
        fi
    fi

    info "Uploading persona '${name}' to ~/.openclaw/workspace/ ..."
    vm_ssh "mkdir -p /root/.openclaw/workspace /root/.openclaw/workspace/memory"

    local f
    for f in "${persona_files[@]}"; do
        if [ -f "${persona_dir}/${f}" ]; then
            vm_scp "${persona_dir}/${f}" "root@${AGENT_IP}:/root/.openclaw/workspace/${f}" >/dev/null
        fi
    done

    # BOOTSTRAP.md triggers OpenClaw's identity ritual. We already wrote
    # IDENTITY.md explicitly, so the ritual would just overwrite it on
    # next agent boot.
    vm_ssh "rm -f /root/.openclaw/workspace/BOOTSTRAP.md"

    # OpenClaw initializes a git repo inside the workspace to track its
    # own edits; if it's there, commit the swap so the workspace history
    # captures who set the persona and when.
    vm_ssh "cd /root/.openclaw/workspace && \
            if [ -d .git ]; then \
                git add -A >/dev/null 2>&1 || true; \
                git -c user.name=installer -c user.email=installer@testnet commit -m 'persona: ${name}' --allow-empty >/dev/null 2>&1 || true; \
            fi" >/dev/null 2>&1 || true

    save_state "persona" "$name"

    # Persist the minified heartbeat block so a later `reconfig` run
    # without --persona can keep the same schedule (the persona tarball
    # won't be uploaded again in that case).
    local hb_json
    hb_json=$(load_persona_heartbeat "$name")
    save_state "heartbeat_json" "$hb_json"
}

# Write ~/.openclaw/openclaw.json in the VM. Keeps install + reconfig in sync.
# Args: $1 = model_ref, $2 = gateway auth token, $3 = optional heartbeat
#       JSON object body to splice into agents.defaults.heartbeat
#
# Browser notes:
#   - ssrfPolicy.dangerouslyAllowPrivateNetwork=true works around OpenClaw
#     2026.4.x behaviour where the default browser SSRF policy silently blocks
#     the gateway's own loopback CDP pre-flight (ws://127.0.0.1:18800/...)
#     and any hostname-based navigation, causing "Chrome CDP websocket not
#     reachable" and "strict browser SSRF policy" errors inside the VM.
#   - The url-fetch SSRF guard for the LLM provider transport is a separate
#     check that we cannot relax via config. We sidestep it by giving the LLM
#     proxy a public-looking IP (83.150.255.x) — see agent_proxy_add for
#     llm_domain below. Browser flags are chosen for a headless Firecracker
#     guest (no GPU, no /dev/shm worth using). Avoid --use-gl=swiftshader
#     here: it crashes on Alpine Chromium in a microVM.
write_openclaw_config() {
    local model_ref="$1" gw_token="$2" heartbeat_json="${3:-}"

    # When a persona ships a heartbeat.json, splice it into
    # agents.defaults.heartbeat. Otherwise we leave the block out and let
    # OpenClaw fall back to its built-in default (30m / 1h depending on
    # auth mode). The trailing comma + newline live inside the variable
    # so the resulting JSON stays valid in both branches.
    local heartbeat_field=""
    if [ -n "$heartbeat_json" ]; then
        heartbeat_field=",
      \"heartbeat\": ${heartbeat_json}"
    fi

    vm_ssh "cat > ~/.openclaw/openclaw.json" <<OCEOF
{
  "agents": {
    "defaults": {
      "workspace": "~/.openclaw/workspace",
      "model": {
        "primary": "${model_ref}"
      }${heartbeat_field}
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

# Write OpenClaw's environment in the VM: both the gateway's .env (auto-loaded
# by `openclaw gateway`) and /etc/profile.d/openclaw.sh (sourced for `openclaw
# tui` and any other interactive use). Keeps install + reconfig in sync.
#
# Note: SSL trust env vars (NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, ...) are
# NOT set here. They are injected globally into every agent VM by
# client/sandbox/firecracker.go (alongside the testnet CA cert itself), via
# /etc/profile.d/testnet-ssl.sh and /etc/environment — so any HTTPS client
# inside the VM trusts testnet certs out-of-the-box without per-tool config.
# See pkg/sslenv for the rationale.
#
# Args: $1 = api key env var name (e.g. ANTHROPIC_API_KEY), $2 = api key value
write_openclaw_env() {
    local api_key_env="$1" api_key="$2"

    vm_ssh "cat > ~/.openclaw/.env" <<ENV
${api_key_env}=${api_key}
PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 ~/.openclaw/.env"

    vm_ssh "cat > /etc/profile.d/openclaw.sh" <<ENV
export ${api_key_env}="${api_key}"
export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium-browser
export CHROME_PATH=/usr/bin/chromium-browser
ENV
    vm_ssh "chmod 600 /etc/profile.d/openclaw.sh"
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
        --openclaw-version)
            OPENCLAW_VERSION="$2"; shift 2 ;;
        --persona)
            OPENCLAW_PERSONA="$2"; shift 2 ;;
        --persona-confirm)
            OPENCLAW_PERSONA_CONFIRM=1; shift ;;
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

    ensure_daemon

    # ---- Pre-install cleanup ----
    #
    # Three sources of stale per-VM state we have to handle here:
    #   (a) Daemon-managed agent from a previous run of THIS script — recorded
    #       as "agent_id" in our state. Stop it cleanly via the daemon.
    #   (b) LEGACY standalone agent from the pre-refactor install script —
    #       recorded as "agent_pid". The daemon doesn't know about it; we
    #       have to SIGKILL it ourselves.
    #   (c) Anything else: orphan Firecracker processes / TAP devices /
    #       per-agent dirs that survived a previous script crash. The daemon
    #       owns the modern equivalent, so anything in the kernel that ISN'T
    #       owned by the daemon is by definition stale and gets removed.

    local prev_agent_id prev_agent_pid prev_oc_rootfs
    prev_agent_id=$(load_state "agent_id")
    prev_agent_pid=$(load_state "agent_pid")
    prev_oc_rootfs=$(load_state "oc_rootfs")

    if [ -n "$prev_agent_id" ]; then
        info "Stopping previously-installed agent ($prev_agent_id)..."
        testnet-client agent stop "$prev_agent_id" 2>/dev/null || true
    fi

    if [ -n "$prev_agent_pid" ]; then
        info "Migrating from legacy install (killing standalone agent pid $prev_agent_pid)..."
        kill -9 "$prev_agent_pid" 2>/dev/null || true
        # Catch sibling firecracker / agent processes the legacy script may
        # have spawned but didn't record explicitly.
        pkill -9 -f "firecracker --api-sock" 2>/dev/null || true
        pkill -9 -f "testnet-client agent launch" 2>/dev/null || true
        sleep 1
    fi

    # Drop any TAP devices that aren't owned by a current daemon agent. Cheap
    # to run; harmless on a clean host. This is what unblocks reinstalls on
    # boxes that ran the legacy script before this refactor.
    local owned_taps
    owned_taps=$(testnet-client agent list --json 2>/dev/null | python3 -c '
import json, sys
agents = json.load(sys.stdin) or []
for a in agents:
    suffix = a.get("id", "").rsplit("-", 1)[-1]
    if suffix:
        print(f"tap-{suffix}")
' 2>/dev/null || true)
    for tap in $(ip -o link show 2>/dev/null | grep -oP 'tap-\d+' || true); do
        if [ -n "$owned_taps" ] && echo "$owned_taps" | grep -qx "$tap"; then
            continue
        fi
        info "Removing orphan TAP: $tap (not owned by daemon)"
        ip link del dev "$tap" 2>/dev/null || true
    done

    # Stale agent dirs left by the legacy script — the daemon will recreate
    # whichever ones it needs.
    rm -rf "${HOME}/.testnet/data/agents/agent-"* 2>/dev/null || true

    [ -n "$prev_oc_rootfs" ] && rm -f "$prev_oc_rootfs" 2>/dev/null || true
    reset_state

    # ---- Launch or connect to VM ----

    if $USE_EXISTING_VM; then
        [ -n "$AGENT_IP" ] || err "--agent-ip required with --ssh-key"
        [ -n "$SSH_KEY" ] || err "--ssh-key required with --agent-ip"
        info "Using existing agent VM at ${AGENT_IP}"
        # Find the matching agent ID from the daemon so proxy add/remove can
        # target it.
        AGENT_ID=$(testnet-client agent list --json 2>/dev/null | \
            python3 -c "
import json, sys
agents = json.load(sys.stdin) or []
for a in agents:
    if a.get('tunnel_ip') == sys.argv[1]:
        print(a.get('id', ''))
        break
" "$AGENT_IP")
        [ -n "$AGENT_ID" ] || err "no agent VM with tunnel_ip=${AGENT_IP} registered with the daemon"
    else
        info "Launching agent VM (${VCPU} vCPU, ${MEM_MB}MB RAM)..."

        # The default rootfs is 512MB which is too small for Node.js + OpenClaw.
        # Create a larger copy (3GB) and pass it via --rootfs.
        local base_rootfs="${HOME}/.testnet/bin/rootfs.ext4"
        local oc_rootfs="${HOME}/.testnet/bin/rootfs-openclaw.ext4"
        local rootfs_flag=""
        if [ -f "$base_rootfs" ]; then
            info "Creating enlarged rootfs for OpenClaw (3GB)..."
            cp "$base_rootfs" "$oc_rootfs"
            truncate -s 3G "$oc_rootfs"
            e2fsck -fy "$oc_rootfs" >/dev/null 2>&1 || true
            resize2fs "$oc_rootfs" 3G
            info "  Rootfs size: $(du -h "$oc_rootfs" | cut -f1)"
            rootfs_flag="--rootfs ${oc_rootfs}"
            save_state "oc_rootfs" "$oc_rootfs"
        else
            warn "Base rootfs not found at ${base_rootfs}, launching with defaults"
        fi

        # --standalone=false routes through the systemd-managed daemon so
        # subsequent `testnet-client agent proxy add` calls (which use the
        # unix socket) reach the same process that owns the agent. The CLI
        # default is still --standalone=true for dev workflows that don't
        # have a daemon running.
        local launch_json
        if ! launch_json=$(testnet-client agent launch --json --standalone=false --vcpu "$VCPU" --mem "$MEM_MB" $rootfs_flag 2>&1); then
            echo "--- agent launch output ---" >&2
            echo "$launch_json" >&2
            err "testnet-client agent launch failed"
        fi

        AGENT_ID=$(echo "$launch_json" | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])')
        AGENT_IP=$(echo "$launch_json" | python3 -c 'import json, sys; print(json.load(sys.stdin)["tunnel_ip"])')
        SSH_KEY=$(echo "$launch_json" | python3 -c 'import json, sys; print(json.load(sys.stdin).get("ssh_key_path", ""))')

        [ -n "$AGENT_ID" ] || err "could not parse agent id from daemon response"
        [ -n "$AGENT_IP" ] || err "could not parse guest IP from daemon response"
        [ -n "$SSH_KEY" ]  || err "could not parse SSH key path from daemon response"

        info "VM launched: ${AGENT_ID}"
        info "  Guest IP: ${AGENT_IP}"
        info "  SSH key:  ${SSH_KEY}"
    fi

    save_state "agent_id" "$AGENT_ID"
    save_state "agent_ip" "$AGENT_IP"
    save_state "ssh_key"  "$SSH_KEY"
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

    # ---- Set up domain proxies via the daemon ----

    info "Setting up passthrough proxies..."
    for domain in "${INSTALL_DOMAINS[@]}"; do
        agent_proxy_add "$domain" private
    done
    agent_proxy_add "$llm_domain" public
    save_state "llm_domain" "$llm_domain"
    sleep 1

    # ---- Install Node.js + npm via Alpine packages ----

    info "Updating Alpine packages..."
    vm_ssh "apk update" >/dev/null 2>&1 || {
        warn "apk update failed — checking proxy connectivity..."
        vm_ssh "wget -q -O /dev/null https://dl-cdn.alpinelinux.org/alpine/v3.21/main/x86_64/APKINDEX.tar.gz" 2>&1 || true
        err "Cannot reach Alpine mirror through proxy. Check 'testnet-client agent proxy list ${AGENT_ID}'."
    }

    info "Installing Node.js, npm, and Chromium..."
    vm_ssh "apk add --no-cache nodejs npm chromium nss freetype harfbuzz ca-certificates ttf-freefont" >/dev/null 2>&1

    # Configure Chromium for headless operation inside a microVM.
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
    #
    # OPENCLAW_VERSION pins the published npm version we install. "latest"
    # follows the npm dist-tag and is intentionally NOT the default — daily
    # OpenClaw releases regularly regress the agentic tool path. See header.
    local oc_pkg_spec="openclaw@${OPENCLAW_VERSION}"
    info "Installing ${oc_pkg_spec} via npm (this may take a few minutes)..."
    vm_ssh "mkdir -p /root/.npm-tmp && TMPDIR=/root/.npm-tmp npm install -g ${oc_pkg_spec} --cache /root/.npm-cache 2>&1" | tail -5

    if ! vm_ssh "command -v openclaw" >/dev/null 2>&1; then
        err "OpenClaw installation failed. Check: ssh -i $SSH_KEY root@$AGENT_IP 'npm install -g ${oc_pkg_spec}'"
    fi

    local oc_ver
    oc_ver=$(vm_ssh "openclaw --version" 2>/dev/null || echo "unknown")
    info "OpenClaw ${oc_ver} installed in VM (pinned: ${OPENCLAW_VERSION})"
    save_state "openclaw_version" "$OPENCLAW_VERSION"

    # ---- Remove install-only proxies (keep LLM API) ----

    info "Removing temporary installation proxies..."
    for domain in "${INSTALL_DOMAINS[@]}"; do
        agent_proxy_remove "$domain"
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
    #
    # If a persona was requested, splice its heartbeat block into the config
    # up front. The persona's workspace markdown is uploaded later, after the
    # gateway has materialized the per-agent state directories.
    local heartbeat_json=""
    if [ -n "$OPENCLAW_PERSONA" ]; then
        heartbeat_json=$(load_persona_heartbeat "$OPENCLAW_PERSONA")
    fi
    write_openclaw_config "$model_ref" "$gw_token" "$heartbeat_json"
    write_openclaw_env "$api_key_env" "$API_KEY"

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

    # ---- Apply persona (optional) ----
    #
    # Done after the gateway has booted at least twice so OpenClaw has
    # materialized ~/.openclaw/agents/main/. We restart the gateway once
    # more so the new workspace files are picked up cleanly by the next
    # agent session.
    if [ -n "$OPENCLAW_PERSONA" ]; then
        info "Applying persona: ${OPENCLAW_PERSONA}"
        apply_persona "$OPENCLAW_PERSONA" install

        info "Restarting gateway to load persona workspace..."
        vm_ssh "kill -9 \$(pgrep -f '[o]penclaw.*gateway') 2>/dev/null; pkill -9 -f 'openclaw.*gateway' 2>/dev/null; sleep 1" || true
        start_openclaw_gateway
    fi

    if vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        info "OpenClaw gateway is running!"
    else
        warn "Gateway may not have started cleanly."
        warn "Log output:"
        vm_ssh "cat ~/.openclaw/gateway.log" 2>/dev/null || true
    fi

    # ---- Summary ----

    local llm_proxy_ip
    llm_proxy_ip=$(get_llm_proxy_ip "$llm_domain")

    echo ""
    echo "============================================"
    echo "  OpenClaw installed in testnet agent VM"
    echo "============================================"
    echo ""
    echo "  Agent ID:   ${AGENT_ID}"
    echo "  Agent IP:   ${AGENT_IP}"
    echo "  Provider:   ${PROVIDER} (${MODEL})"
    echo "  OpenClaw:   ${oc_ver} (pinned: ${OPENCLAW_VERSION})"
    echo "  Node.js:    ${node_ver}"
    echo "  LLM proxy:  ${llm_domain} -> ${llm_proxy_ip:-?} (public)"
    if [ -n "$OPENCLAW_PERSONA" ]; then
        echo "  Persona:    ${OPENCLAW_PERSONA}"
    fi
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
    ensure_daemon

    if ! vm_ssh "echo ok" >/dev/null 2>&1; then
        err "Agent VM at ${AGENT_IP} is not reachable. Run 'install' first."
    fi

    if ! vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        warn "OpenClaw gateway is not running — starting it..."
        fix_openrouter_base_url
        start_openclaw_gateway
    fi

    # Defensive: ensure the LLM proxy is still registered with the daemon
    # (e.g. if it was manually removed). The daemon supervises the underlying
    # listener, so restoration is just one re-add.
    local llm_domain
    llm_domain=$(load_state "llm_domain")
    if [ -n "$llm_domain" ] && [ -z "$(get_llm_proxy_ip "$llm_domain")" ]; then
        warn "LLM proxy missing — re-adding..."
        agent_proxy_add "$llm_domain" public
    fi

    info "Connecting to OpenClaw in agent VM..."
    echo "  Use Ctrl+C to exit."
    echo ""

    # Always restore the local TTY when the TUI exits — including the
    # broken-pipe / network-drop cases that would otherwise leave the user's
    # terminal in raw mode echoing escape sequences for every keypress.
    trap restore_tty EXIT INT TERM
    vm_ssh_interactive 'source /etc/profile.d/openclaw.sh && export OPENCLAW_NO_RESPAWN=1 && openclaw tui'
    local rc=$?
    trap - EXIT INT TERM
    restore_tty
    return "$rc"
}

# ---- status command ----

do_status() {
    load_vm_state

    echo ""
    echo "Agent Testnet — OpenClaw Status"
    echo "==============================="
    echo ""
    echo "  Agent ID:  ${AGENT_ID:-unknown}"
    echo "  Agent IP:  ${AGENT_IP}"
    echo "  SSH key:   ${SSH_KEY}"

    if ! vm_ssh "echo ok" >/dev/null 2>&1; then
        echo "  VM:        UNREACHABLE"
        echo ""
        return
    fi
    echo "  VM:        reachable"

    local oc_ver pinned_ver
    oc_ver=$(vm_ssh "openclaw --version 2>/dev/null" || echo "not installed")
    pinned_ver=$(load_state "openclaw_version")
    if [ -n "$pinned_ver" ]; then
        echo "  OpenClaw:  ${oc_ver} (pinned: ${pinned_ver})"
    else
        echo "  OpenClaw:  ${oc_ver}"
    fi

    if vm_ssh "pgrep -f '[o]penclaw.*gateway'" >/dev/null 2>&1; then
        echo "  Gateway:   running"
    else
        echo "  Gateway:   stopped"
    fi

    local provider llm_domain
    provider=$(load_state "provider")
    llm_domain=$(load_state "llm_domain")
    echo "  Provider:  ${provider:-unknown}"

    local persona
    persona=$(load_state "persona")
    [ -n "$persona" ] && echo "  Persona:   ${persona}"

    local llm_proxy_ip="" proxy_status="stopped"
    if [ -n "$AGENT_ID" ] && [ -n "$llm_domain" ]; then
        llm_proxy_ip=$(get_llm_proxy_ip "$llm_domain" 2>/dev/null || echo "")
        [ -n "$llm_proxy_ip" ] && proxy_status="running (${llm_proxy_ip})"
    fi
    echo "  LLM proxy: ${proxy_status}"
    echo ""
}

# ---- stop command ----

do_stop() {
    [ "$(id -u)" -eq 0 ] || err "Must be run as root (or via sudo)"

    info "Stopping OpenClaw..."

    AGENT_ID=$(load_state "agent_id")
    AGENT_IP=$(load_state "agent_ip")
    SSH_KEY=$(load_state "ssh_key")

    if [ -n "$AGENT_ID" ] && [ -n "$AGENT_IP" ] && [ -n "$SSH_KEY" ]; then
        if vm_ssh "echo ok" >/dev/null 2>&1; then
            vm_ssh "kill -9 \$(pgrep -f '[o]penclaw') 2>/dev/null || true"
            info "Gateway stopped."
        fi
    fi

    if [ -n "$AGENT_ID" ]; then
        info "Stopping agent VM ($AGENT_ID) via daemon..."
        testnet-client agent stop "$AGENT_ID" 2>/dev/null || true
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
    ensure_daemon

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

    # Resolve which heartbeat block to write into the new openclaw.json:
    #   - --persona NAME  -> read fresh from $PERSONA_SRC_DIR/<NAME>/heartbeat.json
    #   - otherwise       -> reuse the block we persisted at last install/reconfig
    # Reading from state lets `reconfig` change only model/provider/key while
    # leaving the persona's schedule intact, without needing to re-upload the
    # persona tarball on every call.
    local heartbeat_json=""
    if [ -n "$OPENCLAW_PERSONA" ]; then
        heartbeat_json=$(load_persona_heartbeat "$OPENCLAW_PERSONA")
    else
        heartbeat_json=$(load_state "heartbeat_json")
    fi

    info "Reconfiguring OpenClaw (provider: ${PROVIDER}, model: ${MODEL})..."
    if [ -n "$OPENCLAW_PERSONA" ]; then
        info "Persona swap requested: $(load_state persona || echo "<none>") -> ${OPENCLAW_PERSONA}"
    fi

    # Stop running gateway
    vm_ssh "kill \$(pgrep -f '[o]penclaw.*gateway') 2>/dev/null || true"
    sleep 2

    local gw_token
    gw_token=$(load_state "gw_token")
    [ -n "$gw_token" ] || gw_token=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)

    write_openclaw_config "$model_ref" "$gw_token" "$heartbeat_json"
    write_openclaw_env "$api_key_env" "$API_KEY"

    # Persona workspace swap happens before we relight the gateway so the
    # next agent session reads the new IDENTITY/SOUL/AGENTS/USER/HEARTBEAT.md
    # immediately. apply_persona is responsible for prompting / requiring
    # the confirm flag because it's the step that overwrites files the
    # operator might want to preserve.
    if [ -n "$OPENCLAW_PERSONA" ]; then
        apply_persona "$OPENCLAW_PERSONA" reconfig
    fi

    # Switch LLM proxy via the daemon if the provider changed
    local old_llm_domain
    old_llm_domain=$(load_state "llm_domain")
    if [ "$llm_domain" != "$old_llm_domain" ]; then
        info "Switching LLM proxy: ${old_llm_domain:-none} -> ${llm_domain}"
        if [ -n "$old_llm_domain" ]; then
            agent_proxy_remove "$old_llm_domain"
        fi
        agent_proxy_add "$llm_domain" public
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
    local final_persona
    final_persona=$(load_state "persona")
    [ -n "$final_persona" ] && echo "  Persona:      ${final_persona}"
    echo ""
}

# ---- shared helpers ----

load_vm_state() {
    [ -z "$AGENT_ID" ] && AGENT_ID=$(load_state "agent_id")
    [ -z "$AGENT_IP" ] && AGENT_IP=$(load_state "agent_ip")
    [ -z "$SSH_KEY" ]  && SSH_KEY=$(load_state "ssh_key")
    [ -n "$AGENT_IP" ] || err "No agent VM found. Run 'install' first."
    [ -n "$SSH_KEY" ]  || err "No SSH key found. Run 'install' first."
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
