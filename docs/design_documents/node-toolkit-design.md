# Testnet Node Toolkit -- Design Document

How to integrate existing open-source web applications into the agent testnet, when to build from scratch instead, and a composable toolkit that handles the testnet-specific integration for both cases.

Prerequisite reading: [Node Development Guide](node-development.md) -- explains the testnet architecture, what agents are, how DNS/VIP/TLS work, and the control plane API.

## Motivation

The testnet needs many services: a Reddit-like forum, a search engine, a GitHub-like code host, a wiki, a messaging platform, and more. Building each from scratch is one option -- [reddit-clone-design.md](reddit-clone-design.md) and [search-engine-design.md](search-engine-design.md) take that approach. But most of these services already exist as mature open-source projects (Lemmy, Gitea, SearXNG, DokuWiki, Mattermost, etc.). If we can handle the testnet-specific integration separately, we can deploy many of these unmodified.

This document analyzes the build-vs-reuse trade-off for each type of service and proposes a small toolkit of composable CLI tools that bridge the gap between off-the-shelf apps and the testnet.

## The core integration problem

Every testnet node must satisfy two requirements:

1. **TLS certificates** -- Serve HTTPS using certificates from the testnet CA, fetched at startup from the control plane via `pkg/api`.
2. **Domain routing** -- Traffic arrives via DNS + VIP + DNAT, which is handled entirely by the testnet server. The node just needs to terminate TLS with the right cert (which already contains SANs for all claimed domains).

Additionally, **active nodes** (like a search engine) that make outbound requests must:

3. **Resolve DNS** via testnet DNS only (typically `83.150.0.1:53`).
4. **Trust the testnet CA** for outbound HTTPS, not the system CA store.
5. **Discover testnet domains** to know what exists and seed outbound work (crawling, etc.).

An off-the-shelf open-source app knows nothing about testnet certs, DNS, or domains. The toolkit bridges this gap with three small, single-purpose tools.

## The toolkit

Rather than building a monolithic adapter binary that reimplements reverse proxying (something nginx and Caddy already do well), the toolkit is a single `testnet-toolkit` binary with three subcommand groups that handle only the testnet-specific parts. Operators compose them with standard infrastructure tools they already know.

```
testnet-toolkit certs fetch      Fetch TLS certs from the control plane, write to disk
testnet-toolkit sandbox run      Run a process in a network-confined testnet environment
testnet-toolkit seed urls        Query the control plane for domains, output seed URLs
testnet-toolkit seed domains     Output raw domain names
testnet-toolkit seed json        Output full domain mappings as JSON
```

The single-binary approach matches how `testnet-client` is structured: one binary, cobra subcommands, shared root-level flags. One download, one build target, one `install.sh` entry.

### `testnet-toolkit certs` -- certificate fetching

Fetches TLS certificates from the testnet control plane and writes them to disk. This is the one thing standard tools (nginx, Caddy) cannot do on their own -- it requires speaking the testnet's control plane API.

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name forum \
  --secret shared-secret-for-forum \
  --out-dir /etc/testnet/certs
```

This writes three files:

```
/etc/testnet/certs/
  cert.pem     Node TLS certificate (signed by testnet CA, SANs include all claimed domains)
  key.pem      Node TLS private key
  ca.pem       Testnet root CA certificate
```

The operator then points nginx, Caddy, or any other TLS-terminating reverse proxy at these files using standard configuration. The app runs on localhost behind the proxy, unmodified.

#### Cert renewal

Certificates expire after 1 year (see `server/controlplane/ca.go`). Re-run `testnet-toolkit certs fetch` to get fresh certs. For automated renewal, use a cron job or systemd timer:

```bash
# /etc/cron.d/testnet-certs
0 3 * * * root testnet-toolkit certs fetch --server-url ... --name ... --secret ... --out-dir /etc/testnet/certs && nginx -s reload
```

Or as a systemd timer:

```ini
# /etc/systemd/system/testnet-certs-renew.timer
[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

```ini
# /etc/systemd/system/testnet-certs-renew.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/testnet-toolkit certs fetch --server-url ... --name ... --secret ... --out-dir /etc/testnet/certs
ExecStartPost=/bin/systemctl reload nginx
```

#### Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--server-url` | `SERVER_URL` | (required) | Control plane URL (e.g. `https://203.0.113.10:8443`) |
| `--name` | `NODE_NAME` | (required) | Node name from nodes.yaml |
| `--secret` | `NODE_SECRET` | (required) | Node secret from nodes.yaml |
| `--out-dir` | `CERT_OUT_DIR` | `/etc/testnet/certs` | Directory to write cert.pem, key.pem, ca.pem |

#### Implementation

Roughly 50-80 lines of Go. The core logic is the same bootstrap code from `cmd/testnet-node/main.go` (lines 27-31), but instead of loading certs into a `tls.Config`, it writes them to disk:

```go
client := api.NewServerClient(*serverURL, nil)
certs, err := client.FetchNodeCerts(*name, *secret)
// ...
os.WriteFile(filepath.Join(*outDir, "cert.pem"), []byte(certs.CertPEM), 0o600)
os.WriteFile(filepath.Join(*outDir, "key.pem"), []byte(certs.KeyPEM), 0o600)
os.WriteFile(filepath.Join(*outDir, "ca.pem"), []byte(certs.CAPEM), 0o644)
```

### `testnet-toolkit sandbox` -- network confinement

Runs a process inside a network environment where it can only reach the testnet. Used for active nodes -- any app that makes outbound HTTP requests (crawlers, federation, webhooks) where those requests must be confined to testnet services.

```bash
testnet-toolkit sandbox run \
  --dns-ip 83.150.0.1 \
  --ca-cert /etc/testnet/certs/ca.pem \
  --wg-interface wg0 \
  -- yacy /opt/yacy
```

The sandbox:

1. Creates a Linux network namespace for the child process.
2. Sets up a `veth` pair connecting the namespace to the host.
3. Configures routing inside the namespace so only VIP traffic (`83.150.0.0/16`) and DNS traffic reach the WireGuard tunnel.
4. Writes `/etc/resolv.conf` inside the namespace pointing to testnet DNS.
5. Installs the testnet CA cert into the namespace's system trust store (`/usr/local/share/ca-certificates/`).
6. Drops all other outbound traffic via iptables.
7. Executes the given command inside the namespace.

This mirrors the agent VM sandboxing in `client/sandbox/network.go` -- the same iptables pattern (ACCEPT to VIPs, ACCEPT to server tunnel IP, DROP everything else) but applied to a network namespace instead of a TAP device.

#### Docker alternative

For apps that ship as Docker images, the sandbox can be achieved with Docker flags instead of a dedicated tool:

```bash
# Fetch certs first
testnet-toolkit certs fetch --server-url ... --name ... --secret ... --out-dir /etc/testnet/certs

# Run the app in a testnet-confined container
docker run \
  --dns 83.150.0.1 \
  --dns-search testnet \
  -v /etc/testnet/certs/ca.pem:/usr/local/share/ca-certificates/testnet.crt \
  --add-host=host.docker.internal:host-gateway \
  --network=testnet-only \
  yacy/yacy
```

Where `testnet-only` is a Docker network routed through the WireGuard interface. This achieves the same confinement without `testnet-toolkit sandbox` -- at the cost of requiring Docker and manual network setup.

#### Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--dns-ip` | `DNS_IP` | `83.150.0.1` | Testnet DNS address |
| `--ca-cert` | `CA_CERT_PATH` | `/etc/testnet/certs/ca.pem` | Testnet CA cert to install in the namespace |
| `--wg-interface` | `WG_INTERFACE` | `wg0` | WireGuard interface to route through |
| `--allowed-cidrs` | `ALLOWED_CIDRS` | `83.150.0.0/16,10.99.0.0/16` | CIDRs reachable from the sandbox |

#### Implementation

Approximately 200-300 lines of Go. Uses `syscall.CLONE_NEWNET` + `syscall.CLONE_NEWNS` for namespace creation, `ip link` / `ip addr` / `ip route` for veth setup, and the same `iptables` pattern from `client/sandbox/network.go`. Requires root (or `CAP_NET_ADMIN` + `CAP_SYS_ADMIN`).

### `testnet-toolkit seed` -- domain discovery

Queries the control plane for all registered domains and outputs a list of seed URLs. Used to feed crawlers, link checkers, or any tool that needs to know what exists on the testnet.

```bash
testnet-toolkit seed urls \
  --server-url https://SERVER_IP:8443 \
  --api-token <token> \
  --exclude-node search
```

Output (one URL per line, suitable for piping):

```
https://reddit.com/
https://www.reddit.com/
https://github.com/
https://www.github.com/
https://en.wikipedia.org/
https://forum.testnet/
https://gitea.testnet/
```

The `--exclude-node` flag filters out domains belonging to the specified node (a search engine shouldn't crawl itself).

#### Subcommands

| Command | Description |
|---------|-------------|
| `testnet-toolkit seed urls` | Output `https://{domain}/` for each testnet domain, one per line |
| `testnet-toolkit seed domains` | Output raw domain names, one per line |
| `testnet-toolkit seed json` | Output the full domain list as JSON (same format as `GET /api/v1/domains`) |

#### Periodic re-seeding

Run via cron to pick up new nodes:

```bash
# Every 5 minutes, update the crawler's seed list
*/5 * * * * root testnet-toolkit seed urls --server-url ... --api-token ... --exclude-node search > /var/lib/crawler/seeds.txt
```

Or integrate directly into a crawl loop:

```bash
# Crawl all testnet sites using wget, confined to the sandbox
testnet-toolkit seed urls --server-url ... --api-token ... --exclude-node search | while read url; do
  testnet-toolkit sandbox run --dns-ip 83.150.0.1 --ca-cert /etc/testnet/certs/ca.pem \
    -- wget --mirror --no-parent "$url" -P /var/lib/search-index/
done
```

#### Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--server-url` | `SERVER_URL` | (required) | Control plane URL |
| `--api-token` | `API_TOKEN` | (required) | API token for authenticated control plane calls |
| `--exclude-node` | `EXCLUDE_NODE` | (none) | Node name to exclude from output (typically self) |

#### Implementation

Roughly 40-60 lines of Go. Calls `client.ListDomains()`, filters, and prints. Trivial.

## How the tools compose

### Passive node (e.g. Gitea for GitHub)

```
testnet-toolkit certs fetch  -->  writes cert.pem, key.pem to disk
                                      |
                                  nginx/Caddy reads certs, terminates TLS on :443
                                      |
                                  reverse-proxies to http://localhost:3000
                                      |
                                  Gitea serves HTTP (unmodified, stock Docker image)
```

The operator runs `testnet-toolkit certs fetch` once (plus renewal cron), configures nginx with a standard reverse proxy block, and starts Gitea. Two standard processes (nginx + Gitea), no custom binaries in the request path.

### Active node (e.g. search engine with crawler)

```
testnet-toolkit certs fetch    -->  writes cert.pem, key.pem, ca.pem to disk
                                        |
                                    nginx reads certs, terminates TLS on :443
                                        |
                                    reverse-proxies to http://localhost:8080
                                        |
                                    search frontend serves HTTP (results page, JSON API)

testnet-toolkit seed urls      -->  outputs seed URLs for the crawler

testnet-toolkit sandbox run    -->  runs the crawler in a confined network namespace
                                    (DNS = testnet, CA = testnet, routing = WireGuard only)
                                        |
                                    crawler fetches pages, builds index
```

The serving side uses the same `certs fetch` + nginx pattern. The crawling side uses `seed urls` to get URLs and `sandbox run` to confine the crawler. The crawler can be any software -- a custom Go binary, wget, YaCy, Scrapy -- because the sandbox handles network confinement at the OS level.

## The fidelity problem

The biggest factor in deciding build-vs-reuse is **fidelity** -- how closely the service matches what agents expect.

When an agent visits `reddit.com`, it draws on training data about Reddit's:
- URL structure (`/r/subreddit`, `/r/subreddit/comments/id/slug`, `/u/username`)
- API endpoints and response shapes
- HTML structure (specific elements, CSS classes, form actions)
- Behavior (how voting works, how comment threading renders, what a subreddit page looks like)

An agent that tries `reddit.com/r/technology` on a Lemmy instance will get a 404. Lemmy uses `/c/community`, not `/r/subreddit`. Its API, HTML, and interaction patterns are completely different.

This matters most for services agents are heavily trained on -- Reddit, GitHub, Stack Overflow, Wikipedia. For these, agents have strong priors about the exact interface and will fail or behave unnaturally if the service deviates.

This matters less for infrastructure-style services (search engines, static hosting, messaging) where agents don't have deep expectations about the exact interface and interact more generically.

**Possible mitigation**: Add a compatibility shim that rewrites URLs and API responses to match the expected service. In practice, this quickly becomes as much work as building from scratch -- you're essentially reimplementing the original API surface on top of a different backend.

## Analysis by node type

### Reddit clone -- Lemmy, Kbin, or similar

**Integration difficulty: Low (TLS only)**

- `testnet-toolkit certs fetch` + nginx handles TLS; app runs on localhost
- Must disable: ActivityPub federation (outbound HTTP), email (SMTP), external image proxying
- Lemmy requires Postgres + PictRS -- multi-process deployment, ~500MB+ container footprint
- A custom Go binary with SQLite is ~10MB and a single process

**Fidelity: Poor.** Lemmy's URL structure (`/c/`, `/post/`), API (`/api/v3/`), and HTML bear no resemblance to Reddit. An agent trained on Reddit will not recognize or navigate the interface.

**Verdict: Build from scratch.** Fidelity is critical for a service agents are heavily trained on. The custom design ([reddit-clone-design.md](reddit-clone-design.md)) matches Reddit's URL structure, API shape, and interaction patterns. It's also lighter (single binary, SQLite, no external dependencies). Estimated implementation: ~1 week.

### Search engine -- SearXNG, MeiliSearch, YaCy

**Integration difficulty: Medium (with toolkit) / High (without)**

Without the toolkit, integrating any off-the-shelf search engine is deep surgery: you must configure it to use testnet DNS, trust the testnet CA, and discover testnet domains. With the toolkit:

- `testnet-toolkit certs fetch` + nginx handles TLS for the search results frontend
- `testnet-toolkit sandbox run` confines the crawler to testnet-only networking
- `testnet-toolkit seed urls` feeds the crawler with testnet domain URLs

However, the search frontend still needs to read from whatever index the crawler produces. SearXNG is a meta-engine (queries upstream engines that don't exist on the testnet -- wrong model). MeiliSearch is an index server but not a crawler. YaCy is a full crawler + index but is heavyweight Java and hard to configure.

**Verdict: Build the crawler from scratch; consider MeiliSearch for the index.** The toolkit makes it feasible to use an off-the-shelf index server (like MeiliSearch) for storage and search, paired with a custom crawler that runs inside `testnet-sandbox` and feeds it. But writing a simple crawler + SQLite FTS5 index as a single binary ([search-engine-design.md](search-engine-design.md)) may still be simpler than orchestrating MeiliSearch + a custom crawler + the toolkit.

### GitHub clone -- Gitea, Forgejo

**Integration difficulty: Low**

- `testnet-toolkit certs fetch` + nginx handles TLS; Gitea serves HTTP on a configurable port
- Must disable: webhook delivery (outbound HTTP), external OAuth providers, federation, Gravatar
- SSH access for `git push`/`git pull` needs separate handling (port 22 passthrough or Gitea's built-in SSH on a different port)
- Requires a database (SQLite is supported by Gitea -- no Postgres needed)
- Gitea is a single binary with embedded assets, relatively lightweight

**Fidelity: Acceptable.** Gitea's UI is visually different from GitHub, but the workflow is similar: repositories, issues, pull requests, file browsing. URL structure differs (`/{owner}/{repo}` is the same, but issue/PR paths vary). Agents familiar with GitHub will likely navigate Gitea with some friction but not total failure. The Git protocol (clone, push, pull) is identical.

**Verdict: Good candidate for the toolkit approach.** Gitea with SQLite is lightweight, the UI is close enough, and Git operations are protocol-compatible. `testnet-toolkit certs fetch` + nginx handles TLS; a Gitea config file disables outbound features.

### Messaging -- Mattermost, Matrix/Element

**Integration difficulty: Medium**

- `testnet-toolkit certs fetch` + nginx handles TLS (nginx natively supports WebSocket proxying)
- Must disable: email notifications, push notifications, external integrations, outbound webhooks
- Mattermost requires Postgres; Matrix/Synapse requires Postgres
- Agents will likely use the REST API rather than real-time features

**Fidelity: Moderate.** Agents don't have strong expectations about the exact URL structure of Telegram or Slack. A generic messaging API with channels, messages, and users is recognizable regardless of the specific platform.

**Verdict: Possible with the toolkit.** The database dependency (Postgres) adds operational weight. A custom lightweight messaging service might be preferable if simplicity is prioritized.

### Wiki -- DokuWiki, Wiki.js

**Integration difficulty: Low**

- `testnet-toolkit certs fetch` + nginx handles TLS; DokuWiki runs behind PHP-FPM or a built-in server
- DokuWiki is a flat-file PHP wiki (no database at all)
- No outbound dependencies to disable
- Pre-seeded content can be added as flat files

**Fidelity: Acceptable.** Agents visiting `wikipedia.org` don't need pixel-perfect Wikipedia. A wiki with searchable, well-structured content in standard HTML is sufficient. The interaction pattern (read pages, follow links, search) is generic.

**Verdict: Good candidate for the toolkit approach.** Especially DokuWiki, which has no database dependency.

## Recommendation: hybrid approach

Not all nodes are equal. Use the right tool for each:

### Build from scratch

Services where agents have strong API/URL expectations from training data. The testnet version must match those expectations or agents will fail.

- **Reddit clone** (`testnet-forum`) -- agents expect `/r/`, `/u/`, Reddit's API shape. Design doc: [reddit-clone-design.md](reddit-clone-design.md).
- **Search engine** (`testnet-search`) -- the crawler is inherently custom. Design doc: [search-engine-design.md](search-engine-design.md).

### Use open-source + toolkit

Services where agents interact more generically, or where the OSS project's interface is close enough to the target.

- **Gitea/Forgejo** for GitHub -- Git protocol is identical, UI is similar enough.
- **DokuWiki** for Wikipedia -- wiki browsing is generic.
- **Mattermost or similar** for messaging -- REST API patterns are generic.

Both approaches benefit from the toolkit. From-scratch nodes use `testnet-toolkit certs fetch` to write certs to disk instead of embedding bootstrap logic in every binary. Active from-scratch nodes can use `testnet-toolkit sandbox run` and `testnet-toolkit seed urls` instead of reimplementing network confinement and domain discovery.

## Deployment examples

### Passive node: Gitea behind nginx

#### 1. nodes.yaml

```yaml
- name: "gitea"
  address: "GITEA_HOST_IP:443"
  secret: "shared-secret-for-gitea"
  domains:
    - "github.com"
    - "www.github.com"
```

#### 2. Fetch certs

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name gitea \
  --secret shared-secret-for-gitea \
  --out-dir /etc/testnet/certs
```

#### 3. Configure nginx

```nginx
server {
    listen 443 ssl;
    server_name github.com www.github.com gitea.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

#### 4. Start Gitea

```bash
# With Docker
docker run -d \
  -p 3000:3000 \
  -e GITEA__server__ROOT_URL=https://github.com \
  -e GITEA__server__HTTP_PORT=3000 \
  -e GITEA__server__DISABLE_SSH=true \
  -e GITEA__service__ENABLE_NOTIFY_MAIL=false \
  -e GITEA__mailer__ENABLED=false \
  -e GITEA__webhook__DELIVER=false \
  -e GITEA__federation__ENABLED=false \
  -e GITEA__picture__DISABLE_GRAVATAR=true \
  -v gitea-data:/data \
  gitea/gitea:latest

# Or without Docker
gitea web --port 3000 --config /etc/gitea/app.ini
```

#### 5. Set up cert renewal

```bash
# Add to crontab
echo '0 3 * * * root /usr/local/bin/testnet-toolkit certs fetch --server-url https://SERVER_IP:8443 --name gitea --secret shared-secret-for-gitea --out-dir /etc/testnet/certs && nginx -s reload' > /etc/cron.d/testnet-certs
```

### Active node: search engine with sandboxed crawler

#### 1. nodes.yaml

```yaml
- name: "search"
  address: "SEARCH_HOST_IP:443"
  secret: "shared-secret-for-search"
  domains:
    - "search.testnet"
```

#### 2. Set up WireGuard tunnel (for crawler outbound)

Follow the WireGuard setup from the [Node Development Guide](node-development.md#wireguard-tunnel-setup) to establish tunnel access. This gives the host access to VIPs and testnet DNS.

#### 3. Fetch certs

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name search \
  --secret shared-secret-for-search \
  --out-dir /etc/testnet/certs
```

#### 4. Configure nginx (for serving search results)

```nginx
server {
    listen 443 ssl;
    server_name search.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
}
```

#### 5. Get seed URLs and run the crawler

```bash
# Discover what to crawl
testnet-toolkit seed urls \
  --server-url https://SERVER_IP:8443 \
  --api-token <token> \
  --exclude-node search \
  > /var/lib/search/seeds.txt

# Run the crawler confined to the testnet
testnet-toolkit sandbox run \
  --dns-ip 83.150.0.1 \
  --ca-cert /etc/testnet/certs/ca.pem \
  --wg-interface wg0 \
  -- /usr/local/bin/my-crawler --seeds /var/lib/search/seeds.txt --index /var/lib/search/index.db
```

The crawler -- whatever it is -- runs inside the sandbox. It can only resolve testnet domains and can only reach testnet VIPs. If it encounters a link to `external-site.com`, DNS returns NXDOMAIN (testnet DNS doesn't know it) and iptables drops any traffic that somehow bypasses DNS.

#### 6. Periodic re-crawl with fresh seeds

```bash
# Cron: refresh seeds and re-crawl every hour
0 * * * * root testnet-toolkit seed urls --server-url ... --api-token ... --exclude-node search > /var/lib/search/seeds.txt && testnet-toolkit sandbox run --dns-ip 83.150.0.1 --ca-cert /etc/testnet/certs/ca.pem -- /usr/local/bin/my-crawler --seeds /var/lib/search/seeds.txt --index /var/lib/search/index.db
```

## Documentation

The toolkit is only useful if operators can deploy nodes without reading source code. Each tool needs clear usage instructions, and the overall workflow needs end-to-end guides for common use cases. The documentation is as much a deliverable as the binaries.

### Toolkit reference

Since `testnet-toolkit` is a single binary, it gets a single reference page covering all subcommand groups:

- **What it does** -- one sentence, no jargon
- **Installation** -- how to download or build the binary
- **Global flags** -- `--server-url` and environment variable fallbacks
- **Per-subcommand sections** -- full command synopsis with all flags and environment variables
- **Examples** -- at least two per subcommand: a minimal invocation and a realistic production invocation
- **Output** -- what files are written or what is printed to stdout
- **Error handling** -- what happens when the control plane is unreachable, certs are invalid, etc.
- **Permissions** -- whether root is required and what capabilities are needed

| File | Covers |
|------|--------|
| `docs/toolkit-reference.md` | Full `testnet-toolkit` reference: all subcommands, flags, examples |

### Step-by-step deployment guides

End-to-end guides that walk an operator through deploying a specific service from zero to running. These are the primary entry point for someone deploying a node -- they link to the reference pages for details but stand alone as complete walkthroughs.

| Guide | Covers |
|-------|--------|
| `docs/guide-deploy-gitea.md` | Gitea as GitHub: nodes.yaml, `testnet-toolkit certs fetch`, nginx config, Gitea Docker/binary config, cert renewal, verification |
| `docs/guide-deploy-dokuwiki.md` | DokuWiki as Wikipedia: nodes.yaml, `testnet-toolkit certs fetch`, nginx config, DokuWiki setup, content seeding |
| `docs/guide-deploy-crawler.md` | Search engine with sandboxed crawler: nodes.yaml, WireGuard setup, `testnet-toolkit certs/seed/sandbox`, nginx, cron scheduling |

Each guide follows the same structure:

1. **Prerequisites** -- what you need before starting (server running, node entry in nodes.yaml, WireGuard for active nodes)
2. **Fetch certificates** -- `testnet-certs fetch` command with concrete values
3. **Configure the reverse proxy** -- complete nginx config block, copy-pasteable
4. **Configure and start the application** -- Docker command or binary flags, with every outbound feature disabled
5. **Verify it works** -- curl commands to confirm the node is reachable and serving correctly
6. **Set up renewal / re-seeding** -- cron or systemd timer for ongoing maintenance
7. **Troubleshooting** -- common failure modes and how to diagnose them (cert fetch fails, app can't start, TLS errors from agents)

### Updates to existing docs

The [Node Development Guide](node-development.md) should be updated to mention the toolkit as an alternative to embedding bootstrap logic in every binary. Specifically:

- In the "Minimal node example" section, add a note: "For wrapping existing applications instead of writing a custom binary, see the [Node Toolkit](node-toolkit-design.md) which provides `testnet-toolkit certs fetch` to fetch certificates to disk for use with nginx or Caddy."
- In the "Active node" section, add a note: "For confining an existing application's outbound traffic to the testnet without custom Go code, see `testnet-toolkit sandbox run` in the [Node Toolkit](node-toolkit-design.md)."

### README for the toolkit

The `testnet-toolkit` binary should be listed in the main project `README.md` alongside the existing `testnet-server`, `testnet-client`, and `testnet-node` entries, with one-line descriptions of each subcommand and a link to the detailed docs.

## Project layout

The toolkit lives in the agent-testnet repo alongside the existing binaries:

```
agent-testnet/
  cmd/testnet-server/          # existing: control plane
  cmd/testnet-client/          # existing: agent VM management
  cmd/testnet-node/            # existing: minimal stub node
  cmd/testnet-toolkit/         # new: single binary, cobra subcommands
    main.go
  toolkit/                     # new: toolkit packages
    cli/                       # cobra command definitions
      root.go                  # root command, --server-url flag
      certs.go                 # certs fetch subcommand
      seed.go                  # seed urls/domains/json subcommands
      sandbox.go               # sandbox run subcommand
    sandbox/                   # network namespace logic
      namespace.go
  pkg/api/                     # existing: shared types + HTTP client
  docs/
    node-toolkit-design.md     # this document
    toolkit-reference.md       # testnet-toolkit reference (all subcommands)
    guide-deploy-gitea.md      # end-to-end: Gitea as GitHub
    guide-deploy-dokuwiki.md   # end-to-end: DokuWiki as Wikipedia
    guide-deploy-crawler.md    # end-to-end: search engine with sandboxed crawler
```

The toolkit imports `pkg/api`. The `certs` and `seed` subcommands have no dependencies beyond the standard library and `pkg/api`. The `sandbox` subcommand shells out to `ip` and `iptables` (same approach as `client/sandbox/network.go`).

## Summary of trade-offs

| Factor | Build from scratch | Open-source + toolkit |
|--------|-------------------|----------------------|
| Agent fidelity | Exact URL/API match | Different interface |
| Implementation time | ~1 week per node | ~1 day per node (after toolkit exists) |
| Operational weight | Single binary, SQLite | Multiple processes, possibly Postgres |
| Feature richness | MVP only | Full-featured app |
| Maintenance burden | You own all the code | Upstream project handles bugs/features |
| Testnet-specific behavior | Full control | Limited to app config flags |
| Deployment complexity | One process, one binary | nginx + app + cron, but all standard tools |

## Suggested node strategy

| Service | Approach | Project | Domain claim |
|---------|----------|---------|-------------|
| Forum (Reddit) | From scratch | `testnet-forum` | `reddit.com`, `www.reddit.com` |
| Search engine | From scratch | `testnet-search` | `google.com`, `www.google.com` |
| Code hosting (GitHub) | Gitea + toolkit | `gitea` + nginx | `github.com`, `www.github.com` |
| Wiki (Wikipedia) | DokuWiki + toolkit | `dokuwiki` + nginx | `en.wikipedia.org` |
| Static hosting | From scratch | `testnet-hosting` | operator-defined |

Build `certs fetch` first (smallest subcommand, ~50 lines, unblocks all passive nodes). Then `seed` (also small, ~40 lines, unblocks crawler workflows). Then `sandbox run` (most complex, ~250 lines, unblocks active node confinement).

## Future extensions

- **install.sh integration**: Add a `toolkit` role to `deploy/install.sh` that downloads the `testnet-toolkit` binary, similar to the existing `node` role
- **Caddy integration**: Document Caddy config alongside nginx (Caddy's config is simpler and supports automatic cert reloading via `watch`)
- **sandbox Docker mode**: A `testnet-toolkit sandbox docker` subcommand that creates and manages a Docker network with the right routing and DNS -- simpler for operators who prefer Docker
- **certs daemon mode**: A `testnet-toolkit certs watch` subcommand that monitors cert expiry and re-fetches automatically, sending SIGHUP to a configured process (nginx, Caddy) on renewal
- **Ansible/Terraform modules**: Automation for deploying toolkit-based nodes at scale
