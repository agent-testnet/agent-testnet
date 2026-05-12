# Agent Testnet

A sandboxed internet environment for AI agents. Agents run inside Firecracker microVMs with network isolation enforced via WireGuard tunnels, interacting only with operator-declared testnet nodes.

## Architecture

```
                    +-----------------+
                    | testnet-server  |
                    |  Control Plane  |
                    |  DNS + WG + NAT |
                    +--------+--------+
                             |
                     WireGuard tunnel
                             |
          +------------------+------------------+
          |                                     |
+---------+----------+              +-----------+--------+
| testnet-client     |              | testnet-node       |
| Firecracker VMs    |              | (any service)      |
| iptables isolation |              | TLS via testnet CA |
+--------------------+              +--------------------+
```

- **testnet-server** -- Control plane: client registration, DNS (VIP-based), WireGuard hub, iptables DNAT routing
- **testnet-client** -- Agent sandbox: Firecracker VM management, per-VM ephemeral SSH keys, network isolation via iptables
- **testnet-node** -- Any service exposed to agents; fetches TLS certs from the server's CA
- **testnet-toolkit** -- Composable CLI tools for integrating existing applications with the testnet (see [Toolkit](#toolkit) below)

## Deployment

### AWS (automated)

```bash
# 1. Create your node config from the example
cp configs/nodes.yaml.example configs/nodes.yaml
# 2. Edit configs/nodes.yaml — fill in node addresses and secrets
# 3. Deploy everything (builds binaries, provisions AWS, installs services)
bash deploy/aws-deploy.sh deploy
```

Other commands:

```bash
bash deploy/aws-deploy.sh status     # Show instance IPs
bash deploy/aws-deploy.sh ssh node   # SSH into a role
bash deploy/aws-deploy.sh reload     # Push updated nodes.yaml + reload server
bash deploy/aws-deploy.sh test       # Run integration tests on the client
bash deploy/aws-deploy.sh teardown   # Destroy all resources
```

### Manual (per-host)

Each role deploys to a fresh Linux host with a single command. Binaries are pre-placed at `/usr/local/bin/` or downloaded from a release URL.

**Server:**

```bash
# Place your nodes.yaml at /tmp/nodes.yaml, then:
AUTO_START=1 sudo -E bash install.sh server
```

**Node:**

```bash
SERVER_URL=https://SERVER_IP:8443 NODE_NAME=mynode NODE_SECRET=s3cret \
  sudo -E bash install.sh node
```

**Client:**

```bash
SERVER_URL=https://SERVER_IP:8443 JOIN_TOKEN=<token> \
  sudo -E bash install.sh client
```

Then launch an agent VM:

```bash
sudo testnet-client agent launch --standalone
```

The rootfs ships with bash, curl, git, jq, and SSH. Agents can install additional runtimes inside the VM via `apk add` (e.g. `apk add nodejs npm python3`).

### Adding a node to a running deployment

1. Deploy the node host and run `install.sh node` (or provision it however you like)
2. Add the node entry to `configs/nodes.yaml`:

```yaml
  - name: "search"
    address: "NEW_NODE_IP:443"
    secret: "the-shared-secret"
    domains:
      - "google.com"
```

3. Reload the server:

```bash
bash deploy/aws-deploy.sh reload
```

The server re-reads `nodes.yaml` on reload (SIGHUP), allocates VIPs for new nodes, and updates DNS + routing — no restart needed. The new node is immediately reachable by agents at its declared domains and its auto-name (`search.testnet`).

## Toolkit

`testnet-toolkit` is a single binary with subcommands for integrating existing open-source applications (Gitea, DokuWiki, etc.) with the testnet. Instead of writing a custom Go binary for every service, operators use the toolkit alongside standard reverse proxies (nginx, Caddy).

| Subcommand | Purpose |
|------------|---------|
| `testnet-toolkit certs fetch` | Fetch TLS certificates from the control plane and write to disk |
| `testnet-toolkit seed urls\|domains\|json` | Discover testnet domains and output seed URLs for crawlers |
| `testnet-toolkit sandbox run` | Run a process confined to testnet-only networking |

See the [Toolkit Reference](docs/toolkit-reference.md) for full usage and the [Node Toolkit Design](docs/design_documents/node-toolkit-design.md) for architecture rationale.

**Deployment guides:**

- [Deploy Gitea as GitHub](docs/deployment_guides/guide-deploy-gitea.md)
- [Deploy DokuWiki as Wikipedia](docs/deployment_guides/guide-deploy-dokuwiki.md)
- [Deploy a search engine with sandboxed crawler](docs/deployment_guides/guide-deploy-crawler.md)

## Local Development

```bash
# Build all binaries
make build

# Run the server locally (requires root for WireGuard + iptables)
make run-server

# Cross-compile Linux amd64 binaries via Docker
make build-linux

# Build the agent rootfs (requires root + Linux)
make rootfs

# Run API-level smoke tests
make smoke
```

## Network Model

- Each declared domain gets a Virtual IP (VIP) in `83.150.0.0/16`
- Testnet DNS resolves declared domains to VIPs; returns NXDOMAIN for undeclared domains
- WireGuard tunnel (`10.99.0.0/16`) is the sole egress path from agent VMs
- Server-side iptables DNAT maps VIPs to real node IPs
- Agent VMs are isolated: only VIP traffic is forwarded, all other egress is dropped

## Security

- **No hardcoded passwords**: VM rootfs uses key-only SSH auth (`PasswordAuthentication no`)
- **Ephemeral SSH keys**: Each VM gets a unique ed25519 keypair generated at launch; the private key is stored at `~/.testnet/data/agents/<id>/ssh_key`
- **Per-VM rootfs**: The base rootfs image is copied per-VM to prevent cross-VM interference
- **CA cert injection**: The testnet CA certificate is injected into each VM at launch via rootfs mount
- **Network isolation**: iptables rules on the client host drop all traffic from VMs except to testnet VIPs

## Project Structure

```
cmd/                    Entry points for the binaries
  testnet-server/
  testnet-client/
  testnet-node/
  testnet-toolkit/
toolkit/                Toolkit packages (CLI commands + sandbox logic)
  cli/
  sandbox/
server/                 Server-side packages
  controlplane/         Registration, CA, VIP allocation
  dns/                  Testnet DNS server
  router/               iptables DNAT + traffic logging
  wg/                   WireGuard endpoint management
client/                 Client-side packages
  cli/                  CLI commands (setup, install, agent, daemon)
  daemon/               Background daemon + agent VM lifecycle
  sandbox/              Firecracker VM + network isolation
pkg/                    Shared packages
  api/                  API types + HTTP client
  config/               Config structs + loading
configs/                Config files (nodes.yaml.example is the template; copy to nodes.yaml)
deploy/
  install.sh            Universal installer (one-command deployment)
scripts/
  gen-rootfs.sh         Builds the Alpine-based agent rootfs (lean: bash, curl, git, jq)
  build-release.sh      Cross-compiles release artifacts
  smoke-test.sh         API-level smoke test suite
docs/
  node-development.md              Guide for building testnet nodes
  toolkit-reference.md             testnet-toolkit CLI reference
  deploy-script-conventions.md     AWS deploy script standard for all repos
  design_documents/                Architecture and design docs
  deployment_guides/               Step-by-step deployment walkthroughs
```

## Network Requirements

The following ports must be open on the **server** host:

| Port | Protocol | Purpose |
|------|----------|---------|
| 8443 | TCP | Control plane API (HTTPS) |
| 51820 | **UDP** | WireGuard tunnel |
| 5353 | UDP/TCP | DNS (public listener) |

**Important:** Many hosting providers (IONOS, OVH, etc.) have a platform-level firewall that blocks UDP by default, separate from `iptables` on the server. If clients register successfully but the WireGuard tunnel never establishes (0 bytes received), open UDP 51820 in your provider's firewall dashboard.

The **node** needs TCP 443 (or whichever port it listens on) open for inbound HTTPS.

The **client** needs outbound UDP 51820 to the server (typically open by default).

If your hosting provider cannot open UDP ports at all, you can wrap WireGuard in a TCP tunnel using [udp2raw](https://github.com/wangyu-/udp2raw) or [wstunnel](https://github.com/erebe/wstunnel). See their documentation for setup.

## Requirements

- Go 1.25+ (for building from source)
- Linux with `/dev/kvm` (for Firecracker on the client)
- Root access (for iptables, WireGuard, Firecracker)
- Docker (for cross-compilation from macOS)

## License

Agent Testnet is licensed under the **GNU Affero General Public License v3.0 or later** (AGPL-3.0-or-later). See [LICENSE](LICENSE) for the full text.

Copyright (C) 2026 Agent Testnet contributors.

In short: you can use, study, modify, and redistribute Agent Testnet, including for commercial purposes. If you run a modified version as a network service for others, AGPL § 13 requires you to make the corresponding source available to those users — the `testnet-server` exposes this via the unauthenticated `/source` endpoint, which downstream operators MUST keep pointing at the source of the actually running build. Contributions are accepted under the same license; see [CONTRIBUTING.md](CONTRIBUTING.md).

### A note on the name

"Agent Testnet" and the project's logo are the identity of this community. The license covers the *code*, not the *name* — a fork is welcome to use the code, but should not present itself as "Agent Testnet" or imply endorsement. A formal trademark policy will follow.
