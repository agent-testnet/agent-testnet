#!/bin/bash
set -euo pipefail

# Builds a lean ext4 rootfs from Alpine Linux for agent VMs.
# Includes OpenRC init, SSH, and common CLI tools (bash, curl, git, jq).
# Agents can install their own runtimes (node, python, etc.) via apk.
#
# Requires: root (or sudo), curl, mkfs.ext4, mount
# Usage: sudo bash scripts/gen-rootfs.sh [output-path]

OUTPUT="${1:-$HOME/.testnet/bin/rootfs.ext4}"
ROOTFS_SIZE="${ROOTFS_SIZE:-512M}"
ALPINE_VERSION="${ALPINE_VERSION:-3.21}"
ARCH="$(uname -m)"

case "$ARCH" in
    x86_64)  ALPINE_ARCH="x86_64" ;;
    aarch64) ALPINE_ARCH="aarch64" ;;
    arm64)   ALPINE_ARCH="aarch64" ;;
    *)       echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_VERSION}/releases/${ALPINE_ARCH}"
ALPINE_MINI="alpine-minirootfs-${ALPINE_VERSION}.3-${ALPINE_ARCH}.tar.gz"

WORK_DIR="$(mktemp -d)"
MOUNT_DIR="${WORK_DIR}/rootfs"
trap "sudo umount '${MOUNT_DIR}' 2>/dev/null || true; rm -rf '${WORK_DIR}'" EXIT

echo "==> Building agent testnet rootfs"
echo "    Alpine: v${ALPINE_VERSION} (${ALPINE_ARCH})"
echo "    Output: ${OUTPUT}"
echo "    Size:   ${ROOTFS_SIZE}"

mkdir -p "$(dirname "$OUTPUT")"

dd if=/dev/zero of="${WORK_DIR}/rootfs.ext4" bs=1 count=0 seek="${ROOTFS_SIZE}" 2>/dev/null
mkfs.ext4 -F "${WORK_DIR}/rootfs.ext4" >/dev/null 2>&1

mkdir -p "${MOUNT_DIR}"
sudo mount -o loop "${WORK_DIR}/rootfs.ext4" "${MOUNT_DIR}"

echo "    Downloading Alpine minirootfs..."
curl -sL "${ALPINE_MIRROR}/${ALPINE_MINI}" | sudo tar -xz -C "${MOUNT_DIR}"

sudo cp /etc/resolv.conf "${MOUNT_DIR}/etc/resolv.conf" 2>/dev/null || \
    echo "nameserver 8.8.8.8" | sudo tee "${MOUNT_DIR}/etc/resolv.conf" >/dev/null

echo "    Installing packages..."
sudo chroot "${MOUNT_DIR}" /bin/sh -c '
    apk update >/dev/null 2>&1

    apk add --no-cache \
        openrc \
        ca-certificates \
        haveged \
        bash \
        curl wget \
        git openssh-client openssh-server openssh-sftp-server \
        jq \
        iproute2 \
        >/dev/null 2>&1

    update-ca-certificates >/dev/null 2>&1 || true
    mkdir -p /usr/local/share/ca-certificates/testnet

    # OpenRC: enable serial console on ttyS0
    sed -i "s|^#ttyS0|ttyS0|" /etc/inittab 2>/dev/null || true
    grep -q "ttyS0" /etc/inittab || \
        echo "ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100" >> /etc/inittab

    # Enable essential services
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

    # SSH server: key-only auth, no passwords
    rc-update add sshd default 2>/dev/null || true
    ssh-keygen -A >/dev/null 2>&1
    sed -i "s/#PermitRootLogin.*/PermitRootLogin prohibit-password/" /etc/ssh/sshd_config
    sed -i "s/#PasswordAuthentication.*/PasswordAuthentication no/" /etc/ssh/sshd_config
    sed -i "s/PasswordAuthentication yes/PasswordAuthentication no/" /etc/ssh/sshd_config
    echo "PermitRootLogin prohibit-password" >> /etc/ssh/sshd_config
    echo "PasswordAuthentication no" >> /etc/ssh/sshd_config
    echo "PubkeyAuthentication yes" >> /etc/ssh/sshd_config

    # Pre-create /root/.ssh for key injection at VM launch
    mkdir -p /root/.ssh
    chmod 700 /root/.ssh
    touch /root/.ssh/authorized_keys
    chmod 600 /root/.ssh/authorized_keys

    # Hostname
    echo "testnet-agent" > /etc/hostname

    # Networking: auto-configure from kernel ip= boot param (already done by kernel)
    mkdir -p /etc/network
    cat > /etc/network/interfaces << NETEOF
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet manual
NETEOF
'

# Set random root password from HOST (where /dev/urandom exists).
# PasswordAuthentication is off so this is unreachable; it just prevents the
# account from being "locked" (which causes some sshd builds to reject pubkey).
RAND_PW="$(head -c 32 /dev/urandom | base64)"
echo "root:${RAND_PW}" | sudo chroot "${MOUNT_DIR}" chpasswd 2>/dev/null || true

# DNS: always point to the testnet DNS server (via tunnel)
echo "nameserver 10.99.0.1" | sudo tee "${MOUNT_DIR}/etc/resolv.conf" >/dev/null

sudo umount "${MOUNT_DIR}"
cp "${WORK_DIR}/rootfs.ext4" "${OUTPUT}"

echo "==> Rootfs built successfully: ${OUTPUT}"
ls -lh "${OUTPUT}"
echo "    To inject testnet CA at launch, mount the image and place cert at:"
echo "    /usr/local/share/ca-certificates/testnet/testnet-ca.crt"
echo "    Then run: update-ca-certificates"
