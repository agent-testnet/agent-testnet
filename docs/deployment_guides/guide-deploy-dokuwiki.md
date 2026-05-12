# Deploy DokuWiki as Wikipedia

Step-by-step guide for deploying [DokuWiki](https://www.dokuwiki.org/) on the agent testnet, impersonating `en.wikipedia.org`. DokuWiki is a flat-file PHP wiki with no database dependency -- one of the lightest services to deploy.

For background on how nodes, DNS, and TLS work, see the [Node Development Guide](../node-development.md). For `testnet-toolkit` flag details, see the [Toolkit Reference](../toolkit-reference.md).

## Prerequisites

- A Linux host with a public IP, reachable from the testnet server
- The testnet server is running and your node is declared in `nodes.yaml`
- `testnet-toolkit` binary installed at `/usr/local/bin/testnet-toolkit`
- nginx installed (`apt install nginx` or equivalent)
- Docker installed (for running DokuWiki), or PHP 7.4+ with PHP-FPM

## 1. Declare the node in nodes.yaml

On the **testnet server**, add the DokuWiki node:

```yaml
nodes:
  # ... existing nodes ...
  - name: "dokuwiki"
    address: "WIKI_HOST_IP:443"
    secret: "shared-secret-for-wiki"
    domains:
      - "en.wikipedia.org"
      - "wikipedia.org"
```

Send `SIGHUP` to the server to reload:

```bash
sudo kill -HUP $(pidof testnet-server)
```

## 2. Fetch certificates

On the **DokuWiki host**:

```bash
testnet-toolkit certs fetch \
  --server-url https://SERVER_IP:8443 \
  --name dokuwiki \
  --secret shared-secret-for-wiki \
  --out-dir /etc/testnet/certs
```

Verify:

```bash
ls -la /etc/testnet/certs/
# cert.pem  key.pem  ca.pem
```

## 3. Configure nginx

Create `/etc/nginx/sites-available/dokuwiki`:

```nginx
server {
    listen 443 ssl;
    server_name en.wikipedia.org wikipedia.org dokuwiki.testnet;

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
ln -sf /etc/nginx/sites-available/dokuwiki /etc/nginx/sites-enabled/
nginx -t && sudo systemctl reload nginx
```

## 4. Start DokuWiki

### With Docker (recommended)

The [linuxserver/dokuwiki](https://hub.docker.com/r/linuxserver/dokuwiki) image is a well-maintained option:

```bash
docker run -d \
  --name dokuwiki \
  --restart unless-stopped \
  -p 8080:80 \
  -e PUID=1000 \
  -e PGID=1000 \
  -v dokuwiki-data:/config \
  lscr.io/linuxserver/dokuwiki:latest
```

After startup, visit `http://localhost:8080/install.php` to complete the installation wizard (set the wiki name, admin credentials, etc.). DokuWiki has no outbound dependencies to disable -- it's purely a content server.

### Without Docker

Install PHP and DokuWiki:

```bash
apt install php-fpm php-xml php-mbstring php-gd
wget https://download.dokuwiki.org/src/dokuwiki/dokuwiki-stable.tgz
tar xzf dokuwiki-stable.tgz -C /var/www/dokuwiki --strip-components=1
chown -R www-data:www-data /var/www/dokuwiki
```

Update the nginx config to use PHP-FPM directly instead of proxying:

```nginx
server {
    listen 443 ssl;
    server_name en.wikipedia.org wikipedia.org dokuwiki.testnet;

    ssl_certificate     /etc/testnet/certs/cert.pem;
    ssl_certificate_key /etc/testnet/certs/key.pem;

    root /var/www/dokuwiki;
    index doku.php;

    location / {
        try_files $uri $uri/ @dokuwiki;
    }

    location @dokuwiki {
        rewrite ^/_media/(.*) /lib/exe/fetch.php?media=$1 last;
        rewrite ^/_detail/(.*) /lib/exe/detail.php?media=$1 last;
        rewrite ^/_export/([^/]+)/(.*) /doku.php?do=export_$1&id=$2 last;
        rewrite ^/(.*) /doku.php?id=$1&$args last;
    }

    location ~ \.php$ {
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_pass unix:/run/php/php-fpm.sock;
    }

    location ~ /(data|conf|bin|inc)/ {
        deny all;
    }
}
```

## 5. Seed content

DokuWiki stores pages as flat text files, making it easy to pre-seed content. Pages live in the `data/pages/` directory (or `/config/dokuwiki/data/pages/` in the Docker volume).

To add a page:

```bash
# Find the DokuWiki data directory
PAGES_DIR=$(docker exec dokuwiki find /config -path '*/data/pages' -type d | head -1)

# Create a page (DokuWiki uses its own markup syntax)
docker exec dokuwiki bash -c "cat > $PAGES_DIR/start.txt << 'WIKI'
====== Welcome to the Testnet Wiki ======

This wiki serves as the encyclopedia for the agent testnet.

===== Featured Articles =====

  * [[artificial_intelligence|Artificial Intelligence]]
  * [[machine_learning|Machine Learning]]
  * [[programming|Programming Languages]]
WIKI"
```

For bulk content, mount a directory of `.txt` files into the DokuWiki data volume:

```bash
# Copy pre-written wiki pages into the container
docker cp ./wiki-content/. dokuwiki:/config/dokuwiki/data/pages/
docker exec dokuwiki chown -R abc:abc /config/dokuwiki/data/pages/
```

## 6. Verify it works

From a machine with the testnet CA trusted:

```bash
# Test HTTPS connectivity
curl --cacert /etc/testnet/certs/ca.pem https://en.wikipedia.org/

# Check that a specific page loads
curl --cacert /etc/testnet/certs/ca.pem https://en.wikipedia.org/doku.php?id=start

# Search for content
curl --cacert /etc/testnet/certs/ca.pem 'https://en.wikipedia.org/doku.php?do=search&id=artificial+intelligence'
```

## 7. Set up certificate renewal

```bash
cat > /etc/cron.d/testnet-certs << 'EOF'
0 3 * * * root /usr/local/bin/testnet-toolkit certs fetch --server-url https://SERVER_IP:8443 --name dokuwiki --secret shared-secret-for-wiki --out-dir /etc/testnet/certs && nginx -s reload
EOF
```

## 8. Troubleshooting

### DokuWiki shows a blank page

Check PHP is running and nginx can reach it:

```bash
# Docker mode
docker logs dokuwiki

# PHP-FPM mode
systemctl status php*-fpm
curl http://127.0.0.1:8080/
```

### Agent gets 404 for wiki pages

DokuWiki uses its own URL structure (`/doku.php?id=page_name`), not Wikipedia's (`/wiki/Page_Name`). Agents trained on Wikipedia may try the wrong URLs. If this causes problems, add nginx rewrite rules:

```nginx
# Rewrite Wikipedia-style URLs to DokuWiki format
rewrite ^/wiki/(.*)$ /doku.php?id=$1 last;
```

### Content not showing after seeding

DokuWiki caches pages aggressively. Clear the cache:

```bash
docker exec dokuwiki rm -rf /config/dokuwiki/data/cache/*
```

### Permission errors

Ensure the DokuWiki data directory is owned by the correct user:

```bash
# Docker (linuxserver images use abc:abc)
docker exec dokuwiki chown -R abc:abc /config/dokuwiki/data/

# Native
chown -R www-data:www-data /var/www/dokuwiki/data/
```
