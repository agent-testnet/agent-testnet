#!/usr/bin/env bash
#
# Testnet universal installer
#
# Usage:
#   curl -fsSL https://.../install.sh | sudo bash -s <role>
#
#   Roles: server, client, node
#
# One-command deployment via environment variables:
#
#   Server:
#     AUTO_START=1 sudo -E bash install.sh server
#     (Place nodes.yaml at /tmp/nodes.yaml before running, or it uses the
#      default empty config. The deploy script handles this automatically.)
#
#   Node:
#     SERVER_URL=https://SERVER:8443 NODE_NAME=mynode NODE_SECRET=s3c \
#       sudo -E bash install.sh node
#
#   Client:
#     SERVER_URL=https://SERVER:8443 JOIN_TOKEN=tok \
#       sudo -E bash install.sh client
#
# Environment variables (optional, all roles):
#   AUTO_START   - Set to 1 to auto-start the service after install (server)
#   RELEASE_URL  - Base URL for release artifacts (default: GitHub releases)
#   VERSION      - Release version tag (default: latest)
#
set -euo pipefail

ROLE="${1:-}"
RELEASE_URL="${RELEASE_URL:-https://github.com/agent-testnet/agent-testnet/releases/latest/download}"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/opt/testnet/configs"
DATA_DIR="/opt/testnet/data"

# ---- helpers ----

info()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33mWARN:\033[0m %s\n" "$*"; }
err()   { printf "\033[1;31mERROR:\033[0m %s\n" "$*" >&2; exit 1; }

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) err "Unsupported architecture: $(uname -m)" ;;
    esac
}

detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        echo "$ID"
    else
        echo "unknown"
    fi
}

install_pkg() {
    local os_id
    os_id="$(detect_os)"
    case "$os_id" in
        ubuntu|debian)
            apt-get update -qq
            apt-get install -y -qq "$@" >/dev/null 2>&1
            ;;
        alpine)
            apk add --no-cache "$@" >/dev/null 2>&1
            ;;
        centos|rhel|fedora|amzn)
            yum install -y "$@" >/dev/null 2>&1
            ;;
        *)
            warn "Unknown OS '$os_id'. Install manually: $*"
            ;;
    esac
}

download_binary() {
    local name="$1"
    if [ -x "${INSTALL_DIR}/${name}" ]; then
        info "${name} already exists at ${INSTALL_DIR}/${name}, skipping download"
        return 0
    fi
    local arch
    arch="$(detect_arch)"
    local url="${RELEASE_URL}/${name}-linux-${arch}"
    info "Downloading ${name} (linux/${arch})..."
    curl -fsSL "$url" -o "${INSTALL_DIR}/${name}"
    chmod +x "${INSTALL_DIR}/${name}"
    info "Installed ${INSTALL_DIR}/${name}"
}

install_systemd_unit() {
    local service_name="$1"
    local unit_content="$2"
    echo "$unit_content" > "/etc/systemd/system/${service_name}.service"
    systemctl daemon-reload
    systemctl enable "${service_name}"
    info "Installed systemd unit: ${service_name}"
}

# ---- rootfs builder (inlined from scripts/gen-rootfs.sh) ----

build_rootfs() {
    local output="$1"
    local size="512M"
    local alpine_ver="3.21"
    local arch
    arch="$(uname -m)"

    case "$arch" in
        x86_64)  local alpine_arch="x86_64" ;;
        aarch64) local alpine_arch="aarch64" ;;
        arm64)   local alpine_arch="aarch64" ;;
        *)       err "Unsupported arch: $arch" ;;
    esac

    install_pkg e2fsprogs curl

    local work_dir mount_dir
    work_dir="$(mktemp -d)"
    mount_dir="${work_dir}/rootfs"
    trap "umount '${mount_dir}' 2>/dev/null || true; rm -rf '${work_dir}'" RETURN

    mkdir -p "$(dirname "$output")"
    dd if=/dev/zero of="${work_dir}/rootfs.ext4" bs=1 count=0 seek="$size" 2>/dev/null
    mkfs.ext4 -F "${work_dir}/rootfs.ext4" >/dev/null 2>&1

    mkdir -p "$mount_dir"
    mount -o loop "${work_dir}/rootfs.ext4" "$mount_dir"

    local mirror="https://dl-cdn.alpinelinux.org/alpine/v${alpine_ver}/releases/${alpine_arch}"
    local mini="alpine-minirootfs-${alpine_ver}.3-${alpine_arch}.tar.gz"
    curl -sL "${mirror}/${mini}" | tar -xz -C "$mount_dir"

    cp /etc/resolv.conf "${mount_dir}/etc/resolv.conf" 2>/dev/null || \
        echo "nameserver 8.8.8.8" > "${mount_dir}/etc/resolv.conf"

    chroot "$mount_dir" /bin/sh -c '
        apk update >/dev/null 2>&1
        apk add --no-cache \
            openrc ca-certificates haveged bash \
            curl wget git openssh-client openssh-server openssh-sftp-server \
            jq iproute2 >/dev/null 2>&1

        update-ca-certificates >/dev/null 2>&1 || true
        mkdir -p /usr/local/share/ca-certificates/testnet

        sed -i "s|^#ttyS0|ttyS0|" /etc/inittab 2>/dev/null || true
        grep -q "ttyS0" /etc/inittab || \
            echo "ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100" >> /etc/inittab

        rc-update add devfs sysinit 2>/dev/null || true
        rc-update add dmesg sysinit 2>/dev/null || true
        rc-update add mdev sysinit 2>/dev/null || true
        rc-update add hwclock boot 2>/dev/null || true
        rc-update add modules boot 2>/dev/null || true
        rc-update add sysctl boot 2>/dev/null || true
        rc-update add hostname boot 2>/dev/null || true
        rc-update add bootmisc boot 2>/dev/null || true
        rc-update add haveged boot 2>/dev/null || true
        rc-update add networking boot 2>/dev/null || true
        rc-update add mount-ro shutdown 2>/dev/null || true
        rc-update add killprocs shutdown 2>/dev/null || true
        rc-update add savecache shutdown 2>/dev/null || true

        rc-update add sshd default 2>/dev/null || true
        ssh-keygen -A >/dev/null 2>&1
        sed -i "s/#PermitRootLogin.*/PermitRootLogin prohibit-password/" /etc/ssh/sshd_config
        sed -i "s/#PasswordAuthentication.*/PasswordAuthentication no/" /etc/ssh/sshd_config
        sed -i "s/PasswordAuthentication yes/PasswordAuthentication no/" /etc/ssh/sshd_config
        echo "PermitRootLogin prohibit-password" >> /etc/ssh/sshd_config
        echo "PasswordAuthentication no" >> /etc/ssh/sshd_config
        echo "PubkeyAuthentication yes" >> /etc/ssh/sshd_config

        mkdir -p /root/.ssh && chmod 700 /root/.ssh
        touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys

        echo "testnet-agent" > /etc/hostname

        mkdir -p /etc/network
        cat > /etc/network/interfaces <<NETEOF
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet manual
NETEOF
    '

    local rand_pw
    rand_pw="$(head -c 32 /dev/urandom | base64)"
    echo "root:${rand_pw}" | chroot "$mount_dir" chpasswd 2>/dev/null || true

    echo "nameserver 10.99.0.1" > "${mount_dir}/etc/resolv.conf"

    umount "$mount_dir"
    cp "${work_dir}/rootfs.ext4" "$output"
    info "Rootfs built: $output ($(du -h "$output" | cut -f1))"
}

# ---- role installers ----

install_server() {
    info "Installing testnet-server..."

    install_pkg wireguard-tools iptables conntrack

    download_binary "testnet-server"

    mkdir -p "$CONFIG_DIR" "$DATA_DIR"

    if [ ! -f "${CONFIG_DIR}/server.yaml" ]; then
        cat > "${CONFIG_DIR}/server.yaml" <<'YAML'
controlplane:
  listen: ":8443"
  data_dir: "/opt/testnet/data"
  nodes_file: "/opt/testnet/configs/nodes.yaml"
  tls:
    cert_file: "/opt/testnet/data/api-cert.pem"
    key_file: "/opt/testnet/data/api-key.pem"
  ca:
    key_file: "/opt/testnet/data/ca-key.pem"
    cert_file: "/opt/testnet/data/ca-cert.pem"

dns:
  listen_tunnel: "83.150.0.1:53"
  listen_public: ":5353"
  refresh_interval: "10s"

wireguard:
  listen_port: 51820
  tunnel_ip: "10.99.0.1/16"
  private_key_file: "/opt/testnet/data/wg-key"

router:
  log_file: "/opt/testnet/data/traffic.log"

vip:
  subnet: "83.150.0.0/16"
  dns_vip: "83.150.0.1"
YAML
        info "Created ${CONFIG_DIR}/server.yaml"
    fi

    # Write nodes.yaml: prefer /tmp/nodes.yaml (placed by deploy script),
    # fall back to default placeholder.
    if [ -f /tmp/nodes.yaml ]; then
        cp /tmp/nodes.yaml "${CONFIG_DIR}/nodes.yaml"
        info "Created ${CONFIG_DIR}/nodes.yaml (from /tmp/nodes.yaml)"
    elif [ ! -f "${CONFIG_DIR}/nodes.yaml" ]; then
        cat > "${CONFIG_DIR}/nodes.yaml" <<'YAML'
nodes: []
#  - name: example
#    address: "1.2.3.4:443"
#    secret: "change-me"
#    domains:
#      - "example.com"
YAML
        info "Created ${CONFIG_DIR}/nodes.yaml (edit this!)"
    fi

    install_systemd_unit "testnet-server" "$(cat <<'UNIT'
[Unit]
Description=Testnet Server (control plane, DNS, WireGuard, router)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/bin/sh -c 'command -v wg >/dev/null && command -v iptables >/dev/null'
ExecStart=/usr/local/bin/testnet-server --config /opt/testnet/configs/server.yaml
ExecReload=/bin/kill -HUP $MAINPID
WorkingDirectory=/opt/testnet
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=testnet-server

[Install]
WantedBy=multi-user.target
UNIT
)"

    sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true
    if ! grep -q '^net.ipv4.ip_forward=1' /etc/sysctl.conf 2>/dev/null; then
        echo "net.ipv4.ip_forward=1" >> /etc/sysctl.conf
    fi

    # Auto-start if requested (one-command mode via deploy script)
    if [ "${AUTO_START:-}" = "1" ]; then
        info "Starting testnet-server..."
        systemctl start testnet-server
        sleep 3
        if systemctl is-active --quiet testnet-server; then
            info "testnet-server is running"
            info "Join token:"
            cat "${DATA_DIR}/join-token" 2>/dev/null || warn "Token not found yet — check: cat ${DATA_DIR}/join-token"
        else
            warn "testnet-server may not have started cleanly. Check: journalctl -u testnet-server -f"
        fi
    else
        echo ""
        info "Server installed!"
        echo ""
        echo "  Next steps:"
        echo "    1. Edit /opt/testnet/configs/nodes.yaml with your node definitions"
        echo "    2. sudo systemctl start testnet-server"
        echo "    3. Check logs: sudo journalctl -u testnet-server -f"
        echo "    4. The join token is printed in the server logs on first start"
    fi
}

install_client() {
    info "Installing testnet-client..."

    install_pkg wireguard-tools iptables

    download_binary "testnet-client"

    # Note: Environment=HOME=/root is required. Without it, systemd starts the
    # daemon with an empty $HOME and os.UserHomeDir() in client/cli/root.go
    # returns "" — the config load then fails (path becomes "/.testnet/...")
    # and SetClientDefaults falls back to an empty Daemon.DataDir, which
    # surfaces as the cryptic "Error: mkdir : no such file or directory" on
    # startup. WorkingDirectory=/root alone does NOT set $HOME.
    install_systemd_unit "testnet-client" "$(cat <<'UNIT'
[Unit]
Description=Testnet Client Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=HOME=/root
ExecStartPre=/bin/sh -c 'test -e /dev/kvm || echo "WARNING: /dev/kvm not found"'
ExecStartPre=/bin/sh -c 'command -v wg >/dev/null && command -v iptables >/dev/null'
ExecStart=/usr/local/bin/testnet-client daemon start
WorkingDirectory=/root
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=testnet-client

[Install]
WantedBy=multi-user.target
UNIT
)"

    # Force HOME=/root so all testnet-client paths are consistent regardless
    # of how sudo was invoked (sudo vs sudo -E)
    export HOME=/root

    # Download VM dependencies (Firecracker, kernel)
    info "Downloading VM dependencies (Firecracker, kernel)..."
    testnet-client install || warn "testnet-client install had warnings (see above)"

    # Auto-setup if SERVER_URL and JOIN_TOKEN are provided (one-command mode)
    if [ -n "${SERVER_URL:-}" ] && [ -n "${JOIN_TOKEN:-}" ]; then
        info "Running testnet-client setup..."
        testnet-client setup \
            --server-url "${SERVER_URL}" \
            --join-token "${JOIN_TOKEN}"

        # Build rootfs if missing
        ROOTFS_PATH="$HOME/.testnet/bin/rootfs.ext4"
        if [ ! -f "$ROOTFS_PATH" ]; then
            if [ -n "${ROOTFS_URL:-}" ]; then
                info "Downloading rootfs from ${ROOTFS_URL}..."
                testnet-client install --rootfs-url "${ROOTFS_URL}"
            else
                info "Building agent rootfs (Alpine Linux, ~512MB)..."
                build_rootfs "$ROOTFS_PATH"
            fi
        fi

        info "Client setup complete!"
        echo ""
        echo "  To launch an agent VM:"
        echo "    sudo testnet-client agent launch --standalone"
    else
        echo ""
        info "Client installed!"
        echo ""
        echo "  Next steps:"
        echo "    1. sudo testnet-client setup --server-url https://SERVER:8443 --join-token TOKEN"
        echo "    2. sudo testnet-client agent launch"
    fi
}

install_node() {
    info "Installing testnet-node..."

    download_binary "testnet-node"
    download_binary "testnet-toolkit"

    mkdir -p /etc/testnet /opt/testnet

    # Write node.env: from individual env vars or default placeholder
    if [ -n "${SERVER_URL:-}" ] && [ -n "${NODE_NAME:-}" ] && [ -n "${NODE_SECRET:-}" ]; then
        cat > /etc/testnet/node.env <<ENV
SERVER_URL=${SERVER_URL}
NODE_NAME=${NODE_NAME}
NODE_SECRET=${NODE_SECRET}
LISTEN_ADDR=${LISTEN_ADDR:-:443}
ENV
        info "Created /etc/testnet/node.env (from env vars)"
    elif [ ! -f /etc/testnet/node.env ]; then
        cat > /etc/testnet/node.env <<'ENV'
SERVER_URL=https://SERVER_IP:8443
NODE_NAME=mynode
NODE_SECRET=change-me
LISTEN_ADDR=:443
ENV
        info "Created /etc/testnet/node.env (edit this!)"
    fi

    install_systemd_unit "testnet-node" "$(cat <<'UNIT'
[Unit]
Description=Testnet Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/testnet/node.env
ExecStart=/usr/local/bin/testnet-node \
    --server-url ${SERVER_URL} \
    --name ${NODE_NAME} \
    --secret ${NODE_SECRET} \
    --listen ${LISTEN_ADDR}
WorkingDirectory=/opt/testnet
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
StandardOutput=journal
StandardError=journal
SyslogIdentifier=testnet-node

[Install]
WantedBy=multi-user.target
UNIT
)"

    # Auto-start if config env vars were provided (one-command mode)
    if [ -n "${SERVER_URL:-}" ] && [ -n "${NODE_NAME:-}" ] && [ -n "${NODE_SECRET:-}" ]; then
        info "Starting testnet-node..."
        systemctl start testnet-node
        sleep 2
        if systemctl is-active --quiet testnet-node; then
            info "testnet-node is running"
        else
            warn "testnet-node may not have started cleanly. Check: journalctl -u testnet-node -f"
        fi
    else
        echo ""
        info "Node installed!"
        echo ""
        echo "  Next steps:"
        echo "    1. Edit /etc/testnet/node.env with your server URL, node name, and secret"
        echo "    2. sudo systemctl start testnet-node"
        echo "    3. Check logs: sudo journalctl -u testnet-node -f"
    fi
}

# ---- main ----

if [ "$(id -u)" -ne 0 ]; then
    err "This script must be run as root (or via sudo)"
fi

case "$ROLE" in
    server) install_server ;;
    client) install_client ;;
    node)   install_node ;;
    "")     err "Usage: $0 <server|client|node>" ;;
    *)      err "Unknown role: $ROLE. Use: server, client, or node" ;;
esac
