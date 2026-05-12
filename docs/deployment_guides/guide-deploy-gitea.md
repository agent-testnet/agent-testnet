# Deploy Gitea as GitHub

Step-by-step guide for deploying [Gitea](https://gitea.io/) on the agent testnet, impersonating `github.com`. Agents interact with it as if it were GitHub -- Git clone/push/pull operations are protocol-identical, and the web UI is similar enough for agents to navigate.

For background on how nodes, DNS, and TLS work, see the [Node Development Guide](../node-development.md). For `testnet-toolkit` flag details, see the [Toolkit Reference](../toolkit-reference.md).

## Prerequisites

- A Linux host with a public IP, reachable from the testnet server
- The testnet server is running and your node is declared in `nodes.yaml`
- `testnet-toolkit` binary installed at `/usr/local/bin/testnet-toolkit`
- nginx installed (`apt install nginx` or equivalent)
- Docker installed (for running Gitea), or Gitea binary downloaded

## 1. Declare the node in nodes.yaml

On the **testnet server**, add the Gitea node to your `nodes.yaml`:

```yaml
nodes:
  # ... existing nodes ...
  - name: "gitea"
    address: "GITEA_HOST_IP:443"
    secret: "shared-secret-for-gitea"
    domains:
      - "github.com"
      - "www.github.com"
```

Send `SIGHUP` to the server to reload:

```bash
sudo kill -HUP $(pidof testnet-server)
```

## 2. Fetch certificates

On the **Gitea host**:

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name gitea \
  --secret shared-secret-for-gitea \
  --out-dir /etc/testnet/certs
```

Verify the files were created:

```bash
ls -la /etc/testnet/certs/
# cert.pem  key.pem  ca.pem
```

## 3. Configure nginx

Create `/etc/nginx/sites-available/gitea`:

```nginx
server {
    listen 443 ssl;
    server_name github.com www.github.com gitea.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    client_max_body_size 100m;

    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Enable and reload:

```bash
ln -sf /etc/nginx/sites-available/gitea /etc/nginx/sites-enabled/
nginx -t && sudo systemctl reload nginx
```

## 4. Start Gitea

### With Docker (recommended)

```bash
docker run -d \
  --name gitea \
  --restart unless-stopped \
  -p 3000:3000 \
  -e GITEA__server__ROOT_URL=https://github.com \
  -e GITEA__server__HTTP_PORT=3000 \
  -e GITEA__server__DISABLE_SSH=true \
  -e GITEA__database__DB_TYPE=sqlite3 \
  -e GITEA__service__ENABLE_NOTIFY_MAIL=false \
  -e GITEA__service__DISABLE_REGISTRATION=false \
  -e GITEA__mailer__ENABLED=false \
  -e GITEA__webhook__DELIVER=false \
  -e GITEA__federation__ENABLED=false \
  -e GITEA__picture__DISABLE_GRAVATAR=true \
  -e GITEA__picture__ENABLE_FEDERATED_AVATAR=false \
  -v gitea-data:/data \
  gitea/gitea:latest
```

Key settings:
- `DISABLE_SSH=true` -- agents use HTTPS for Git operations, not SSH
- `DELIVER=false` -- disables webhook delivery (would attempt outbound HTTP)
- `DISABLE_GRAVATAR=true` -- prevents outbound requests to gravatar.com
- `ENABLED=false` (mailer) -- no email sending
- `ENABLED=false` (federation) -- no ActivityPub federation
- `DB_TYPE=sqlite3` -- no external database needed

### Without Docker

Download the Gitea binary and create `/etc/gitea/app.ini`:

```ini
[server]
ROOT_URL = https://github.com
HTTP_PORT = 3000
DISABLE_SSH = true

[database]
DB_TYPE = sqlite3
PATH = /var/lib/gitea/gitea.db

[service]
ENABLE_NOTIFY_MAIL = false
DISABLE_REGISTRATION = false

[mailer]
ENABLED = false

[webhook]
DELIVER = false

[federation]
ENABLED = false

[picture]
DISABLE_GRAVATAR = true
ENABLE_FEDERATED_AVATAR = false
```

```bash
gitea web --config /etc/gitea/app.ini
```

## 5. Verify it works

From a machine with the testnet CA trusted (e.g., an agent VM or a host with the WireGuard tunnel up):

```bash
# Test HTTPS connectivity
curl --cacert /etc/testnet/certs/ca.pem https://github.com/

# Test the Gitea API
curl --cacert /etc/testnet/certs/ca.pem https://github.com/api/v1/settings/api

# Test Git clone (after creating a test repo via the web UI)
GIT_SSL_CAINFO=/etc/testnet/certs/ca.pem git clone https://github.com/testuser/testrepo.git
```

## 6. Set up certificate renewal

Add a cron job to refresh certificates daily:

```bash
cat > /etc/cron.d/testnet-certs << 'EOF'
0 3 * * * root /usr/local/bin/testnet-toolkit certs fetch --server-url https://SERVER_IP:8443 --name gitea --secret shared-secret-for-gitea --out-dir /etc/testnet/certs && nginx -s reload
EOF
```

## 7. Troubleshooting

### Certificate fetch fails

```
fetch certs: API error 401: unauthorized
```

Check that `--name` and `--secret` match the values in the server's `nodes.yaml` exactly.

### Agents get TLS errors

Verify the agent VM has the testnet CA injected. The client automatically injects the CA at VM launch. If testing from a non-VM host, set `--cacert` or `GIT_SSL_CAINFO` to point to `ca.pem`.

### Gitea shows "502 Bad Gateway"

nginx can't reach Gitea on port 3000. Check:

```bash
# Is Gitea running?
docker ps | grep gitea
# Or: systemctl status gitea

# Can nginx reach it?
curl http://127.0.0.1:3000/
```

### Gitea tries to make outbound requests

Check your Gitea config for any enabled outbound features. Ensure all of these are disabled:
- Webhooks (`DELIVER = false`)
- Email (`ENABLED = false` under `[mailer]`)
- Gravatar (`DISABLE_GRAVATAR = true`)
- Federation (`ENABLED = false` under `[federation]`)

### Git push/clone fails

Gitea must be configured with `ROOT_URL = https://github.com` so it generates correct clone URLs. Verify:

```bash
curl --cacert /etc/testnet/certs/ca.pem https://github.com/api/v1/settings/api | jq .
```
