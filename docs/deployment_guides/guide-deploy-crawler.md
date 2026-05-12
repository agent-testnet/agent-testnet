# Deploy a Search Engine with Sandboxed Crawler

Step-by-step guide for deploying a search engine on the agent testnet. This is an **active node** -- it both serves search results to agents and crawls other testnet services to build its index. The crawler runs inside `testnet-toolkit sandbox run` to ensure it can only reach testnet services.

For background on how nodes, DNS, and TLS work, see the [Node Development Guide](../node-development.md). For `testnet-toolkit` flag details, see the [Toolkit Reference](../toolkit-reference.md). For architecture rationale, see the [Node Toolkit Design](../design_documents/node-toolkit-design.md).

## Prerequisites

- A Linux host with a public IP, reachable from the testnet server
- The testnet server is running and your node is declared in `nodes.yaml`
- `testnet-toolkit` binary installed at `/usr/local/bin/testnet-toolkit`
- nginx installed
- WireGuard installed (`apt install wireguard-tools`)
- Root access (required for WireGuard and the sandbox)
- Your search engine binary or application (this guide uses a generic crawler; substitute your own)

## 1. Declare the node in nodes.yaml

On the **testnet server**, add the search engine node:

```yaml
nodes:
  # ... existing nodes ...
  - name: "search"
    address: "SEARCH_HOST_IP:443"
    secret: "shared-secret-for-search"
    domains:
      - "search.testnet"
```

Send `SIGHUP` to the server to reload:

```bash
sudo kill -HUP $(pidof testnet-server)
```

## 2. Set up WireGuard tunnel

The crawler needs outbound access to other testnet services via VIPs. This requires a WireGuard tunnel to the testnet server.

### Register as a client

```bash
# Generate WireGuard keys
wg genkey | tee /etc/wireguard/privatekey | wg pubkey > /etc/wireguard/publickey

# Register with the testnet server to get tunnel parameters
# (You'll need a join token from the server operator)
curl -sk -X POST https://SERVER_IP:8443/api/v1/clients/register \
  -H "Authorization: Bearer JOIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"wg_public_key\": \"$(cat /etc/wireguard/publickey)\"}"
```

Save the response -- you need `tunnel_cidr`, `server_wg_public_key`, `server_wg_addr`, `dns_ip`, and `api_token`.

### Create WireGuard config

```bash
cat > /etc/wireguard/wg0.conf << EOF
[Interface]
PrivateKey = $(cat /etc/wireguard/privatekey)
Address = FIRST_IP_IN_TUNNEL_CIDR/24

[Peer]
PublicKey = SERVER_WG_PUBLIC_KEY
Endpoint = SERVER_PUBLIC_IP:51820
AllowedIPs = 10.99.0.0/16, 83.150.0.0/16
PersistentKeepalive = 25
EOF

chmod 600 /etc/wireguard/wg0.conf
```

Replace the placeholder values with the registration response values.

### Bring up the tunnel

```bash
sudo wg-quick up wg0
```

Verify connectivity:

```bash
# Can we reach testnet DNS?
dig @83.150.0.1 search.testnet

# Can we reach a VIP? (if other nodes are running)
curl --cacert /etc/testnet/certs/ca.pem https://some-other-node.testnet/health
```

## 3. Fetch certificates

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name search \
  --secret shared-secret-for-search \
  --out-dir /etc/testnet/certs
```

## 4. Configure nginx (for serving search results)

Create `/etc/nginx/sites-available/search`:

```nginx
server {
    listen 443 ssl;
    server_name search.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Enable and reload:

```bash
ln -sf /etc/nginx/sites-available/search /etc/nginx/sites-enabled/
nginx -t && sudo systemctl reload nginx
```

## 5. Get seed URLs

Query the control plane for all testnet domains, excluding the search engine itself:

```bash
testnet-toolkit seed urls \
  --server-url https://SERVER_IP:8443 \
  --api-token API_TOKEN \
  --exclude-node search \
  > /var/lib/search/seeds.txt
```

Check the output:

```bash
cat /var/lib/search/seeds.txt
# https://reddit.com/
# https://github.com/
# https://en.wikipedia.org/
# ...
```

## 6. Run the crawler in the sandbox

The sandbox confines the crawler to testnet-only networking. It can only resolve testnet domains and reach testnet VIPs.

```bash
sudo testnet-toolkit sandbox run \
  --dns-ip 83.150.0.1 \
  --ca-cert /etc/testnet/certs/ca.pem \
  --wg-interface wg0 \
  -- /usr/local/bin/my-crawler \
    --seeds /var/lib/search/seeds.txt \
    --index /var/lib/search/index.db
```

What happens inside the sandbox:
- DNS queries go to testnet DNS at 83.150.0.1
- HTTPS requests use the testnet CA for verification
- Only traffic to `83.150.0.0/16` (VIPs) and `10.99.0.0/16` (tunnel) is allowed
- All other outbound traffic is dropped by iptables
- If the crawler encounters a link to `external-site.com`, DNS returns NXDOMAIN

### Using wget as a simple crawler

If you don't have a custom crawler binary, `wget` can mirror testnet sites:

```bash
# Crawl each seed URL
while read url; do
  sudo testnet-toolkit sandbox run \
    --dns-ip 83.150.0.1 \
    --ca-cert /etc/testnet/certs/ca.pem \
    -- wget --mirror --no-parent --convert-links \
      --ca-certificate=/etc/testnet/certs/ca.pem \
      "$url" -P /var/lib/search/mirror/
done < /var/lib/search/seeds.txt
```

## 7. Start the search frontend

Start your search application, serving results on port 8080:

```bash
/usr/local/bin/my-search-frontend \
  --index /var/lib/search/index.db \
  --listen :8080
```

Or with Docker:

```bash
docker run -d \
  --name search-frontend \
  --restart unless-stopped \
  -p 8080:8080 \
  -v /var/lib/search:/data \
  my-search-frontend:latest
```

## 8. Verify it works

From an agent VM or a host with the WireGuard tunnel:

```bash
# Test the search frontend
curl --cacert /etc/testnet/certs/ca.pem https://search.testnet/

# Test a search query (API depends on your search frontend)
curl --cacert /etc/testnet/certs/ca.pem 'https://search.testnet/search?q=test'
```

## 9. Set up periodic re-crawl

Create a cron job to refresh seeds and re-crawl:

```bash
cat > /etc/cron.d/testnet-crawl << 'EOF'
# Refresh seed URLs every hour
0 * * * * root /usr/local/bin/testnet-toolkit seed urls --server-url https://SERVER_IP:8443 --api-token API_TOKEN --exclude-node search > /var/lib/search/seeds.txt

# Re-crawl every 2 hours
0 */2 * * * root /usr/local/bin/testnet-toolkit sandbox run --dns-ip 83.150.0.1 --ca-cert /etc/testnet/certs/ca.pem --wg-interface wg0 -- /usr/local/bin/my-crawler --seeds /var/lib/search/seeds.txt --index /var/lib/search/index.db
EOF
```

Also set up certificate renewal:

```bash
cat > /etc/cron.d/testnet-certs << 'EOF'
0 3 * * * root /usr/local/bin/testnet-toolkit certs fetch --server-url https://SERVER_IP:8443 --name search --secret shared-secret-for-search --out-dir /etc/testnet/certs && nginx -s reload
EOF
```

## 10. Troubleshooting

### Sandbox fails with "requires root privileges"

The sandbox creates network namespaces and iptables rules. Run with `sudo`:

```bash
sudo testnet-toolkit sandbox run ...
```

### Crawler can't resolve domains

Verify the WireGuard tunnel is up and DNS is reachable:

```bash
# Check WireGuard status
sudo wg show wg0

# Test DNS through the tunnel
dig @83.150.0.1 reddit.com

# If DNS fails, check the tunnel has traffic flowing
sudo wg show wg0 | grep "latest handshake"
```

### Crawler gets TLS errors

The testnet CA must be trusted. Inside the sandbox, the CA cert is installed best-effort. If `update-ca-certificates` isn't available in your environment, pass the CA cert explicitly to your crawler:

```bash
# For wget
wget --ca-certificate=/etc/testnet/certs/ca.pem ...

# For curl
curl --cacert /etc/testnet/certs/ca.pem ...
```

### Crawler reaches the internet (shouldn't happen)

The sandbox iptables rules should prevent this. Verify the sandbox is being used correctly:

```bash
# List FORWARD rules -- should see the sandbox's veth rules
sudo iptables -L FORWARD -v -n
```

If you see traffic bypassing the sandbox, ensure the crawler is being invoked via `testnet-toolkit sandbox run` and not directly.

### Seed URLs are empty

Check that other nodes are registered and running:

```bash
testnet-toolkit seed json --server-url https://SERVER_IP:8443 --api-token API_TOKEN | jq .
```

If the response is empty, no other nodes are declared in `nodes.yaml` on the server.

### Search frontend returns no results

The crawler must complete before the frontend has anything to serve. Check the index:

```bash
ls -la /var/lib/search/index.db
# Should exist and have non-zero size after a crawl completes
```
