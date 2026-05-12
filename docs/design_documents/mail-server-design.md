# Testnet Mail Server via Roundcube + docker-mailserver -- Design Document

Deploy [Roundcube](https://roundcube.net/) webmail backed by [docker-mailserver](https://github.com/docker-mailserver/docker-mailserver) as the email node on the agent testnet, serving `mail.testnet` and `gmail.com`. Roundcube is a mature, open-source webmail client (PHP, 20+ years) with a clean three-pane interface. docker-mailserver bundles Postfix (SMTP) and Dovecot (IMAP) in a single container with file-based configuration and no external database.

Background reading (in the [agent-testnet](https://github.com/agent-testnet/agent-testnet) repo):
- [Node Development Guide](https://github.com/agent-testnet/agent-testnet/blob/main/docs/node-development.md) -- testnet architecture, what nodes are, how DNS/VIP/TLS work
- [Node Toolkit](https://github.com/agent-testnet/agent-testnet/blob/main/docs/node-toolkit-design.md) -- the `testnet-toolkit` CLI and build-vs-reuse analysis
- [Toolkit Reference](https://github.com/agent-testnet/agent-testnet/blob/main/docs/toolkit-reference.md) -- how to download and use `testnet-toolkit`

## Testnet overview

The [agent testnet](https://github.com/agent-testnet/agent-testnet) is a sandboxed internet for AI agents. Agents run in isolated Firecracker microVMs with no access to the real internet. All their traffic routes through a controlled network where operator-declared services impersonate real websites.

Three roles:

- **Server** -- Central control plane. Runs DNS, a private certificate authority (CA), a WireGuard VPN hub, and iptables NAT routing. All traffic flows through it.
- **Client** -- Runs agent VMs. Each VM is network-isolated and can only reach testnet services.
- **Node** -- Any HTTPS service that agents can interact with. Registered in the server's `nodes.yaml` with a name, address, shared secret, and list of domain names to impersonate.

When an agent visits `gmail.com`: testnet DNS resolves it to a Virtual IP (VIP) in `83.150.0.0/16`, traffic travels through the WireGuard tunnel to the server, and the server uses DNAT to forward it to the mail node's real public IP. The agent never knows it's on a testnet.

Every node must serve HTTPS using certificates issued by the testnet's private CA (fetched via [`testnet-toolkit certs fetch`](https://github.com/agent-testnet/agent-testnet/blob/main/docs/toolkit-reference.md)). The CA cert is injected into agent VMs so they trust these connections. Public CAs are not trusted inside agent VMs.

## Motivation

The testnet needs an email service so agents can:

1. **Create email accounts** -- An agent visiting `gmail.com` should be able to register an address like `agent42@gmail.com` and use it across the testnet.

2. **Sign up for other services** -- Many websites require email for registration. Currently, the forum (Lemmy) skips email verification by design. As more services are added (code hosting, static hosting, messaging), some will require email addresses during signup. Agents need a working inbox to receive verification emails and complete registration flows.

3. **Communicate via email** -- Agents may send emails to each other or to service-specific addresses, mimicking how email works on the real internet.

### Why Roundcube + docker-mailserver

1. **Agent interaction model** -- [OpenClaw](https://github.com/agent-testnet/agent-testnet/blob/main/scripts/install-openclaw.sh) agents interact with the testnet through headless Chromium via Puppeteer. They navigate HTML pages, click links, and fill forms -- the same way they use the forum (Lemmy) and search engine. Roundcube's web interface is the primary interaction surface, not a protocol-level API. Roundcube's DOM is simple and predictable (standard HTML forms, straightforward inbox table, compose window), making it easy for an LLM driving a browser to navigate.

2. **Proven with automation** -- Roundcube has been deployed on millions of servers over 20+ years. Its HTML rendering is stable and well-understood. The `elastic` skin provides a clean, modern three-pane layout (folders, message list, reading pane) that works well in headless browsers.

3. **Lightweight backend** -- docker-mailserver bundles Postfix and Dovecot in a single container with file-based configuration. No external database, no complex dependency graph. Accounts are managed via a CLI tool (`setup email add`), and configuration lives in flat files.

4. **Same trade-off as Lemmy** -- The [forum design](https://github.com/agent-testnet/agent-testnet/blob/main/docs/lemmy-forum-design.md) accepted that agents see Lemmy's UI rather than Reddit's. The same principle applies here: agents land on `gmail.com` but see Roundcube's interface. Once on the page, they adapt to what they see.

### Alternatives considered

| | Roundcube + docker-mailserver | Mailu | Stalwart + Bulwark |
|--|--|--|--|
| **Maturity** | 20+ years (Roundcube), well-tested | Mature, integrated stack | Stalwart mature, Bulwark brand new (March 2026) |
| **Resource usage** | ~300 MB idle (2 containers + nginx) | ~500-800 MB idle (5-7 containers) | ~120 MB idle (2 containers + nginx) |
| **Web UI** | Clean, stable DOM, easy for browser automation | Roundcube (same UI) or SOGo | Next.js, untested with headless browsers |
| **Configuration** | File-based, no database for mail | Docker env vars, more moving parts | TOML config, RocksDB storage |
| **Agent API** | IMAP (via webmail) | IMAP (via webmail) | JMAP (HTTP-based, nice for curl agents) |
| **Operational complexity** | Low (2 containers, flat files) | Medium (5+ containers, admin panel) | Low (2 containers) |

Stalwart + Bulwark would be the lightest option and offers JMAP (an HTTP-based email API useful for stock agents using curl). However, Bulwark Webmail launched in March 2026 and is untested with headless browser automation. For a testnet that needs to reliably work when agents hit it, Roundcube's 20-year track record wins. Stalwart remains a strong future candidate once Bulwark matures.

## Deliverables

This repo should contain everything needed to deploy the mail node on a fresh Linux host. The developer produces:

```
testnet-mail/
  docker-compose.yml        docker-mailserver + Roundcube containers
  mailserver.env            docker-mailserver environment configuration
  roundcube/config.php      Roundcube configuration (IMAP/SMTP endpoints, branding)
  nginx/mail.conf           nginx site config (TLS termination, URL rewrites)
  scripts/seed-mail.sh      Seed script to create starter email accounts
  scripts/deploy.sh         One-command deploy: fetch certs, start containers, configure nginx
  README.md                 Operator guide: prerequisites, deploy, verify, troubleshoot
```

The deploy script should accept the following environment variables (all required):

| Variable | Example | Description |
|----------|---------|-------------|
| `SERVER_URL` | `https://203.0.113.10:8443` | Testnet control plane URL |
| `NODE_NAME` | `mail` | Node name as declared in the server's `nodes.yaml` |
| `NODE_SECRET` | `shared-secret-for-mail` | Shared secret from `nodes.yaml` |
| `MAIL_DOMAIN` | `gmail.com` | Primary domain for email addresses |

The script should:
1. Run `testnet-toolkit certs fetch` to write TLS certs to disk
2. Install the nginx config and reload nginx
3. Start Docker Compose
4. Wait for docker-mailserver to be healthy, then run the seed script
5. Set up a daily cron job for certificate renewal

### Prerequisites on the host

- Linux (Ubuntu 22.04+ or Debian 12+ recommended)
- Docker and Docker Compose v2
- nginx
- `testnet-toolkit` binary at `/usr/local/bin/testnet-toolkit` (download from [agent-testnet releases](https://github.com/agent-testnet/agent-testnet/releases) or build with `make build-toolkit` from the [agent-testnet repo](https://github.com/agent-testnet/agent-testnet))
- `curl` and `jq` (for the seed script)

## Architecture

The mail server is a **passive node**: it serves webmail to agents over HTTPS and handles SMTP locally. It does not need to reach other testnet services. No WireGuard tunnel, no client registration.

```
                        +------------------+
                        |  Testnet Server  |
                        |  DNS + VIP + CA  |
                        +--------+---------+
                                 |
                          WireGuard tunnel
                                 |
          +----------------------+----------------------------+
          |                                                   |
   +------+------+                                +-----------+-----------+
   |   Agent VM  |                                |     Mail Node Host   |
   |             |   GET /mail (webmail UI)       |                      |
   |             +--(via VIP + DNAT)----------->  |  nginx (:443, TLS)   |
   |             |                                |    |                  |
   |             |                                |    | URL rewrites     |
   |             |                                |    | Gmail -> Roundcube|
   |             |                                |    v                  |
   |             |                                |  Roundcube (:8080)   |
   |             |                                |    | IMAP/SMTP        |
   |             |                                |    v                  |
   |             |                                |  docker-mailserver   |
   |             |                                |  (:143 IMAP, :25 SMTP)|
   |             |<-- HTML response --------------|                      |
   +-------------+                                +----------------------+
```

### Components

| Component | Role | Runs on |
|-----------|------|---------|
| nginx | TLS termination (testnet CA certs), URL rewriting, reverse proxy | Host, port 443 |
| Roundcube | PHP webmail client, serves the inbox/compose UI | Container, port 8080 |
| docker-mailserver | Postfix (SMTP) + Dovecot (IMAP), mailbox storage | Container, ports 25/143/587 |

### How agents interact with email

OpenClaw agents use headless Chromium to browse web pages. The interaction flow:

1. Agent navigates to `gmail.com` in the browser
2. nginx rewrites the URL and proxies to Roundcube's login page
3. Agent sees a webmail login form (username + password fields)
4. Agent logs in (or registers via the sign-up page, if enabled) and sees the three-pane inbox
5. Agent reads emails, composes messages, clicks links in emails (e.g. verification URLs)

This is the same browsing model agents use for Lemmy (forum) and the search engine. The webmail HTML is the primary interface, not IMAP or SMTP directly.

Stock agents (curl-only VMs without Chromium) can still interact via IMAP if needed, since docker-mailserver exposes standard protocols. However, this is a secondary path.

## Deployment

### 1. Declare the node in nodes.yaml

On the **testnet server**, add:

```yaml
nodes:
  # ... existing nodes ...
  - name: "mail"
    address: "MAIL_HOST_IP:443"
    secret: "shared-secret-for-mail"
    domains:
      - "gmail.com"
      - "mail.google.com"
```

Reload:

```bash
sudo kill -HUP $(pidof testnet-server)
```

Agents visiting `gmail.com` will be routed to this node. The auto-name `mail.testnet` is also available without explicit declaration.

### 2. Fetch certificates

On the **mail host**:

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name mail \
  --secret shared-secret-for-mail \
  --out-dir /etc/testnet/certs
```

Verify:

```bash
ls -la /etc/testnet/certs/
# cert.pem  key.pem  ca.pem
```

### 3. Configure nginx

Create `/etc/nginx/sites-available/mail`:

```nginx
server {
    listen 443 ssl;
    server_name gmail.com mail.google.com mail.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    client_max_body_size 25m;

    # --- Gmail -> Roundcube URL rewrites ---

    # Gmail's inbox URL -> Roundcube inbox
    rewrite ^/mail/?$                         / permanent;
    rewrite ^/mail/u/\d+/?$                   / permanent;

    # Gmail compose
    rewrite ^/mail/u/\d+/\#inbox\?compose=.*$ /?_task=mail&_action=compose permanent;

    # Gmail sign-in redirects to Roundcube login
    rewrite ^/accounts/ServiceLogin           / permanent;
    rewrite ^/ServiceLogin                    / permanent;

    # Gmail settings
    rewrite ^/mail/u/\d+/\#settings/?         /?_task=settings permanent;

    # Gmail contacts
    rewrite ^/contacts/?                      /?_task=addressbook permanent;

    # --- Roundcube proxy ---

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;

        proxy_buffering off;
        proxy_request_buffering off;
    }

    # --- Health check ---

    location /health {
        proxy_pass http://127.0.0.1:8080/?_task=login;
        proxy_set_header Host $host;
    }
}
```

Enable and reload:

```bash
ln -sf /etc/nginx/sites-available/mail /etc/nginx/sites-enabled/
nginx -t && sudo systemctl reload nginx
```

### 4. Start the mail stack with Docker Compose

Create `/opt/testnet-mail/docker-compose.yml`:

```yaml
version: "3.8"

x-logging: &default-logging
  driver: "json-file"
  options:
    max-size: "50m"
    max-file: "4"

services:
  mailserver:
    image: ghcr.io/docker-mailserver/docker-mailserver:15
    container_name: mailserver
    hostname: gmail.com
    env_file: mailserver.env
    ports:
      - "127.0.0.1:25:25"
      - "127.0.0.1:143:143"
      - "127.0.0.1:587:587"
    volumes:
      - mail-data:/var/mail/
      - mail-state:/var/mail-state/
      - mail-logs:/var/log/mail/
      - ./config/:/tmp/docker-mailserver/
      - /etc/localtime:/etc/localtime:ro
    restart: unless-stopped
    stop_grace_period: 1m
    healthcheck:
      test: "ss --listening --ipv4 --tcp | grep --silent ':smtp' || exit 1"
      timeout: 3s
      retries: 0
    logging: *default-logging

  roundcube:
    image: roundcube/roundcubemail:latest-apache
    container_name: roundcube
    environment:
      - ROUNDCUBEMAIL_DEFAULT_HOST=mailserver
      - ROUNDCUBEMAIL_DEFAULT_PORT=143
      - ROUNDCUBEMAIL_SMTP_SERVER=mailserver
      - ROUNDCUBEMAIL_SMTP_PORT=25
      - ROUNDCUBEMAIL_SKIN=elastic
      - ROUNDCUBEMAIL_PLUGINS=archive,zipdownload,managesieve
      - ROUNDCUBEMAIL_UPLOAD_MAX_FILESIZE=25M
    volumes:
      - roundcube-db:/var/roundcube/db
      - ./roundcube/custom-config.php:/var/roundcube/config/custom-config.php:ro
    ports:
      - "127.0.0.1:8080:80"
    depends_on:
      mailserver:
        condition: service_healthy
    restart: unless-stopped
    logging: *default-logging

volumes:
  mail-data:
  mail-state:
  mail-logs:
  roundcube-db:
```

Create `/opt/testnet-mail/mailserver.env`:

```bash
# Hostname and domain
OVERRIDE_HOSTNAME=gmail.com

# Disable features unnecessary for a closed testnet
ENABLE_CLAMAV=0
ENABLE_RSPAMD=0
ENABLE_AMAVIS=0
ENABLE_SPAMASSASSIN=0
ENABLE_FAIL2BAN=0
ENABLE_POSTGREY=0
ENABLE_QUOTAS=0
ENABLE_MANAGESIEVE=0
ENABLE_UPDATE_CHECK=0

# Disable SRS (Sender Rewriting Scheme) -- no external relay
ENABLE_SRS=0

# Disable DKIM/DMARC/SPF -- not needed on a private testnet
ENABLE_OPENDKIM=0
ENABLE_OPENDMARC=0
ENABLE_POLICYD_SPF=0

# Allow plaintext auth on localhost (Roundcube connects locally)
# TLS is terminated by nginx for external traffic.
PERMIT_DOCKER=connected-networks

# Disable outbound relay -- all mail stays local
RELAY_HOST=
DEFAULT_RELAY_HOST=

# Use Maildir format for easy backup/inspection
MAILBOX_FORMAT=maildir

# Logging
LOG_LEVEL=warn

# Postfix tuning for agent traffic
POSTFIX_MESSAGE_SIZE_LIMIT=26214400

# Accept mail for our domain without requiring the sender to exist locally,
# since other testnet services may send from arbitrary addresses.
SPOOF_PROTECTION=0
```

Create `/opt/testnet-mail/roundcube/custom-config.php`:

```php
<?php

// Branding -- make it look like "Gmail" to agents
$config['product_name'] = 'Gmail';
$config['support_url'] = '';

// Default view: show inbox in widescreen layout
$config['layout'] = 'widescreen';

// Session and security settings for testnet use
$config['session_lifetime'] = 1440;  // 24 hours
$config['auto_create_user'] = true;

// Disable IP check (agents may come from various VIPs)
$config['ip_check'] = false;

// Compose defaults
$config['default_charset'] = 'UTF-8';
$config['htmleditor'] = 0;  // plain text by default (easier for agents)
$config['draft_autosave'] = 0;  // no auto-save drafts

// Disable spell checking (not useful for agents)
$config['enable_spellcheck'] = false;

// Identity -- set the default sender format
$config['identities_level'] = 0;

// Disable unnecessary UI elements that would confuse agents
$config['disabled_actions'] = [];
```

Start:

```bash
cd /opt/testnet-mail
docker compose up -d
```

### 5. Seed accounts

After first startup, create email accounts so agents can log in immediately. docker-mailserver uses a CLI tool for account management:

```bash
#!/usr/bin/env bash
# seed-mail.sh — Create starter email accounts.
# Run once after initial deployment.

CONTAINER="mailserver"

create_account() {
  local email="$1" password="$2"
  docker exec "$CONTAINER" setup email add "$email" "$password"
  echo "  Created: $email"
}

echo "Creating email accounts..."

# Admin account
create_account "admin@gmail.com" "testnet-admin-password"

# General-purpose agent accounts (agents can use these or create new ones)
create_account "agent@gmail.com" "agent-password"
create_account "user@gmail.com" "user-password"

# Service-specific accounts (for receiving notifications from other testnet services)
create_account "noreply@gmail.com" "noreply-password"

echo ""
echo "Seeding complete. Accounts created:"
docker exec "$CONTAINER" setup email list
```

### 6. Verify

From a machine with the testnet CA trusted:

```bash
# Webmail UI — does the login page load?
curl --cacert /etc/testnet/certs/ca.pem https://gmail.com/

# Does the login page contain Roundcube?
curl -s --cacert /etc/testnet/certs/ca.pem https://gmail.com/ | grep -i "roundcube\|rcmloginuser"

# Gmail-style URL rewrite — does /mail redirect?
curl -I --cacert /etc/testnet/certs/ca.pem https://gmail.com/mail
# Expect: 301 -> /

# mail.testnet auto-name
curl --cacert /etc/testnet/certs/ca.pem https://mail.testnet/

# Health check
curl --cacert /etc/testnet/certs/ca.pem https://gmail.com/health

# SMTP — can we connect locally?
docker exec mailserver bash -c "echo 'EHLO test' | nc -w 3 localhost 25"

# IMAP — can we authenticate?
docker exec mailserver bash -c "echo -e 'a1 LOGIN admin@gmail.com testnet-admin-password\na2 LIST \"\" \"*\"\na3 LOGOUT' | nc -w 3 localhost 143"
```

### 7. Certificate renewal

```bash
cat > /etc/cron.d/testnet-mail-certs << 'EOF'
0 3 * * * root /usr/local/bin/testnet-toolkit certs fetch --server-url https://SERVER_IP:8443 --name mail --secret shared-secret-for-mail --out-dir /etc/testnet/certs && nginx -s reload
EOF
```

## URL compatibility layer

The nginx rewrite rules map common Gmail URL patterns to Roundcube equivalents. Gmail's URLs are heavily JavaScript-driven (hash-based routing like `#inbox`, `#compose`), so only the top-level entry points can be rewritten. Once the agent is on the Roundcube page, Roundcube's own navigation takes over.

### Covered rewrites

| Gmail URL | Roundcube URL | Notes |
|-----------|---------------|-------|
| `/mail` | `/` | Inbox (main entry point) |
| `/mail/u/0/` | `/` | Multi-account inbox (Gmail uses account indices) |
| `/accounts/ServiceLogin` | `/` | Login redirect |
| `/ServiceLogin` | `/` | Login redirect (alternate path) |
| `/contacts` | `/?_task=addressbook` | Contacts/address book |

### Not covered (agents will encounter Roundcube-native UI)

| Gmail pattern | Why not rewritten |
|---------------|-------------------|
| `#inbox`, `#compose`, `#sent` | Hash-based routing, invisible to nginx (handled client-side) |
| `/mail/u/0/#inbox?compose=new` | Hash fragments don't reach the server |
| Gmail API (`/gmail/v1/`) | Entirely different API; would require a full translation proxy |
| `/accounts/signup` | Gmail account creation is a multi-step OAuth flow; Roundcube uses IMAP auth |
| `mail.google.com/mail/` | Domain resolves but shows Roundcube's interface |

### Agent behavior expectation

When an agent visits `gmail.com`:

1. It will likely try `/mail`, `/mail/u/0/`, or just `/`. The nginx rewrites redirect these to Roundcube's login page.
2. The agent sees a login form with username and password fields. Roundcube's login form is standard HTML — `<input name="_user">` and `<input name="_pass">` with a submit button.
3. After login, the agent sees the three-pane layout: folder list (Inbox, Sent, Drafts, Trash), message list, and reading pane. All navigation uses standard HTML links and forms.
4. To compose, the agent clicks "Compose" (a visible button). The compose form has To, Cc, Subject, and Body fields -- standard HTML inputs.
5. To read a verification email, the agent clicks the message in the message list and reads the content in the reading pane. Links in emails are standard `<a href>` tags.

This is similar to how agents handle Lemmy-as-Reddit: the initial URL patterns get them in the door, and from there they adapt to the actual interface. Gmail's heavy reliance on JavaScript/hash routing actually means agents are _more_ likely to adapt to whatever UI they see, since Gmail's URL structure is less memorable than Reddit's `/r/` pattern.

## Account management

### Creating accounts

docker-mailserver manages accounts through its `setup` CLI:

```bash
# Create a new account
docker exec mailserver setup email add user@gmail.com password123

# List all accounts
docker exec mailserver setup email list

# Delete an account
docker exec mailserver setup email del user@gmail.com

# Change password
docker exec mailserver setup email update user@gmail.com newpassword
```

Accounts are stored in `/tmp/docker-mailserver/postfix-accounts.cf` inside the container (backed by the `config/` volume mount). This is a plain text file with one `email|password_hash` per line.

### Agent self-registration

In the default configuration, agents cannot create their own accounts through the Roundcube UI -- they can only log in to pre-created accounts. This is intentional: it mirrors how Gmail works (you can't create an account from the login page; you go through a separate signup flow).

Two options for agent self-registration:

1. **Pre-create accounts** (recommended for MVP): The seed script creates a pool of accounts. Agents are given credentials as part of their task prompt or discover them on `testnet.info`.

2. **HTTP registration API** (future): Add a lightweight endpoint (e.g. `/api/register`) backed by a small script that calls `docker exec mailserver setup email add`. This would let agents create accounts programmatically by submitting a form. This can be added as a simple PHP script or a standalone Go handler behind nginx.

## SMTP routing for inter-service email

### The routing challenge

The testnet's DNAT system routes all VIP traffic to a single `host:port` per node. For the mail node declared with `address: "MAIL_HOST_IP:443"`, agent traffic to the VIP on port 443 reaches nginx on the mail host. But SMTP (port 25) traffic from other testnet services to the same VIP would also be rewritten to port 443, which doesn't speak SMTP.

### Solution: direct SMTP between nodes

Testnet nodes run on hosts with real public IPs. The VIP/DNAT system is only for routing agent VM traffic through the WireGuard tunnel. Node-to-node communication can happen directly over the public network, bypassing VIPs entirely.

When a testnet service (e.g. Lemmy) needs to send email:

1. Configure the service's SMTP setting to point at the mail node's **real public IP** (not `gmail.com` or a VIP), port 25
2. Open port 25 on the mail node host's firewall for traffic from other node IPs
3. The mail flows directly between hosts without touching the VIP/DNAT system

Example: if Lemmy enables email verification, its `lemmy.hjson` would use:

```hjson
email: {
  smtp_server: "MAIL_HOST_IP"
  smtp_port: 25
  smtp_from_address: "noreply@gmail.com"
  tls_type: "none"
}
```

This keeps the DNAT system unchanged and avoids the need for multi-port VIP routing.

### Alternative: SMTP over the VIP network (future)

If multi-port DNAT is added to the testnet router, nodes could address the mail server as `gmail.com:25` through the VIP. This would require changes to `server/router/router.go` to support per-port DNAT rules (currently all traffic to a VIP maps to a single address). This is out of scope for the initial deployment.

### Network requirements

| Source | Port | Destination | Purpose |
|--------|------|-------------|---------|
| Agent VMs (via server DNAT) | TCP 443 | nginx on mail host | Webmail HTTPS |
| Other testnet nodes | TCP 25 | docker-mailserver on mail host | Inter-service SMTP |
| Mail host | TCP 8443 | Testnet server | Certificate fetch |

Open port 25 on the mail host's security group for the IPs of other testnet node hosts.

## Configuration rationale

### Anti-spam/anti-virus disabled

```bash
ENABLE_CLAMAV=0
ENABLE_RSPAMD=0
ENABLE_SPAMASSASSIN=0
ENABLE_FAIL2BAN=0
```

All email on the testnet is between agents and testnet services. There is no external spam source. Disabling these saves ~200 MB of RAM and avoids false positives on automated agent traffic.

### No DKIM/DMARC/SPF

```bash
ENABLE_OPENDKIM=0
ENABLE_OPENDMARC=0
ENABLE_POLICYD_SPF=0
```

DNS-based email authentication is meaningless on a private testnet with a custom DNS server. These features require specific DNS TXT records that the testnet DNS doesn't serve.

### No outbound relay

```bash
RELAY_HOST=
DEFAULT_RELAY_HOST=
```

All mail stays on the testnet. docker-mailserver should never attempt to relay mail to external SMTP servers (which would fail anyway since the host's outbound port 25 is typically blocked by cloud providers).

### Spoof protection disabled

```bash
SPOOF_PROTECTION=0
```

Other testnet services may send email from arbitrary sender addresses (e.g. `noreply@reddit.com`). Spoof protection would reject these because the sender domain doesn't match the mail server's domain.

### PERMIT_DOCKER=connected-networks

Allows Roundcube (running in a sibling container on the Docker network) to authenticate and send mail via SMTP without TLS. TLS is terminated at nginx for agent-facing traffic; the internal Docker network doesn't need encryption.

### Roundcube product name "Gmail"

```php
$config['product_name'] = 'Gmail';
```

The HTML title and branding show "Gmail" so agents see the expected name when visiting `gmail.com`. This reinforces the illusion, same as the forum's `site_name: "reddit"`.

### Plain text editor default

```php
$config['htmleditor'] = 0;
```

Agents composing email via the web UI will produce plain text by default. This is simpler for agents to work with and avoids HTML rendering issues in composed messages.

## Resource requirements

### Container footprint

| Component | Idle RAM | CPU | Storage |
|-----------|----------|-----|---------|
| docker-mailserver (Postfix + Dovecot) | ~100 MB | Minimal | ~20 MB base + mailboxes |
| Roundcube (Apache + PHP) | ~150 MB | Minimal (renders on request) | ~50 MB (SQLite for session/cache) |
| nginx | ~5 MB | Minimal | — |
| **Total** | **~255 MB** | | |

Note: with anti-spam and anti-virus disabled, docker-mailserver is significantly lighter than its full-featured configuration (~100 MB vs ~500 MB).

### Host requirements

- **Minimum**: 1 vCPU, 1 GB RAM (workable for testnet traffic)
- **Recommended**: 2 vCPU, 2 GB RAM
- **Disk**: 10 GB (OS + containers + mailbox headroom)
- **Network**: TCP 443 inbound (from server DNAT), TCP 25 inbound (from other nodes), TCP 8443 outbound (cert fetch to server)

This fits on `t3a.small` or equivalent, same as the forum node.

## Operational concerns

### Backups

Mailbox data is in the `mail-data` Docker volume. Back up with:

```bash
docker run --rm -v testnet-mail_mail-data:/data -v /opt/testnet-mail/backups:/backup \
  alpine tar czf /backup/mail-$(date +%F).tar.gz -C /data .
```

Or schedule:

```bash
echo '0 4 * * * root docker run --rm -v testnet-mail_mail-data:/data -v /opt/testnet-mail/backups:/backup alpine tar czf /backup/mail-$(date +\%F).tar.gz -C /data .' > /etc/cron.d/testnet-mail-backup
```

### Adding accounts at runtime

```bash
docker exec mailserver setup email add newuser@gmail.com password
```

No restart required. Postfix and Dovecot pick up new accounts within a few seconds.

### Updating docker-mailserver

Pin the major version tag in `docker-compose.yml` (e.g. `:15`). To update:

```bash
cd /opt/testnet-mail
docker compose pull
docker compose up -d
```

### Updating Roundcube

```bash
cd /opt/testnet-mail
docker compose pull roundcube
docker compose up -d roundcube
```

Roundcube's SQLite database handles schema migrations automatically on startup.

### Logs

```bash
# All containers
docker compose -f /opt/testnet-mail/docker-compose.yml logs -f

# Mail server only (Postfix + Dovecot)
docker compose -f /opt/testnet-mail/docker-compose.yml logs -f mailserver

# Roundcube only
docker compose -f /opt/testnet-mail/docker-compose.yml logs -f roundcube

# nginx
journalctl -u nginx -f

# Postfix mail log (inside container)
docker exec mailserver cat /var/log/mail/mail.log
```

### Restarting

```bash
cd /opt/testnet-mail
docker compose restart
```

## Troubleshooting

### Certificate fetch fails

```
fetch certs: API error 401: unauthorized
```

Check that `--name mail` and `--secret` match the `nodes.yaml` entry on the server exactly.

### Roundcube shows "Connection to storage server failed"

Roundcube can't reach docker-mailserver's IMAP port. Check:

```bash
docker compose ps                                    # Are containers running?
docker exec roundcube bash -c "nc -zv mailserver 143" # IMAP reachable?
docker compose logs mailserver | grep -i "dovecot"    # Dovecot errors?
```

### Agents get TLS errors

Verify the agent VM has the testnet CA injected (automatic via `testnet-client`). For manual testing:

```bash
curl --cacert /etc/testnet/certs/ca.pem https://gmail.com/
```

### Emails from other testnet services not arriving

1. Check that port 25 is open on the mail host's firewall for the sending node's IP
2. Verify the sending service is configured with the mail host's **real IP** (not the VIP)
3. Check Postfix logs:

```bash
docker exec mailserver cat /var/log/mail/mail.log | tail -50
```

4. Test SMTP connectivity from the sending host:

```bash
echo -e "EHLO test\nMAIL FROM:<test@reddit.com>\nRCPT TO:<admin@gmail.com>\nDATA\nSubject: Test\n\nTest email.\n.\nQUIT" | nc MAIL_HOST_IP 25
```

### Login fails with correct credentials

docker-mailserver may not have picked up the account yet. Check:

```bash
docker exec mailserver setup email list              # Is the account listed?
docker exec mailserver doveadm auth test user@gmail.com password  # Auth test
```

### 502 Bad Gateway

nginx can't reach Roundcube. Check:

```bash
docker compose ps                                    # Is Roundcube running?
curl http://127.0.0.1:8080/                          # Can you reach it directly?
```

### High memory usage

If docker-mailserver uses more RAM than expected, verify anti-spam/anti-virus is disabled:

```bash
docker exec mailserver bash -c "ps aux | head -20"
```

ClamAV alone uses ~300 MB. If it's running despite `ENABLE_CLAMAV=0`, rebuild the container:

```bash
docker compose down mailserver && docker compose up -d mailserver
```

## Future extensions

- **Agent self-registration page**: A lightweight web form at `/signup` that creates email accounts by calling `docker exec mailserver setup email add`. This would let agents create their own accounts without pre-provisioning. Could be a simple PHP script added to the Roundcube container or a standalone microservice.

- **Gmail API compatibility proxy**: A reverse proxy translating Gmail's REST API (`/gmail/v1/users/{id}/messages`) to IMAP commands. This would let agents use Gmail-trained API patterns. Scope: significant, essentially a protocol translator between REST and IMAP.

- **Gmail-like Roundcube skin**: A custom Roundcube skin mimicking Gmail's visual layout (Material Design, Google-style header). Roundcube supports custom skins via the plugin system. This would increase visual fidelity for agents that interpret page layout.

- **Multi-domain support**: Serve multiple email domains (e.g. `outlook.com`, `yahoo.com`) from the same mail node. docker-mailserver supports multiple domains natively; each domain would need its own entry in `nodes.yaml`.

- **JMAP/HTTP API via Stalwart**: Add Stalwart Mail Server as an alternative backend alongside docker-mailserver, exposing JMAP endpoints for stock agents that prefer HTTP-based email access over the web UI. Useful if the testnet expands to agents without browser capabilities.

- **Webmail-based account creation flow**: Extend Roundcube with a plugin that adds a "Create Account" link on the login page, backed by docker-mailserver's account management. This would mimic Gmail's signup flow without requiring a separate registration endpoint.
