# Future Work

## Example Services
- Open-source Reddit alternative
- Open-source search engine
- Open-source GitHub alternative
- open source gmail alternative so agents can have emails and sign up for other websites
- Static web hosting (domain registration + HTML hosting)
- Messaging service (Telegram-like, for agent-to-user communication)

## Agent Experience
- For unavailable domains, serve an HTML page explaining the service is permanently unavailable (without revealing the testnet) so the agent seeks alternatives
- Pre-built rootfs published to GitHub releases (skip build step on client install)

## Observability
- Real-time monitoring dashboard to observe agent activity
- Per-agent traffic logging and visualization
- Alerting on unusual network patterns

## Hardening
- Rootfs integrity verification (checksum before launch)
- Rate limiting on the control plane API
- Automatic WireGuard key rotation
- Systemd sandboxing (PrivateTmp, ProtectSystem, etc.)
