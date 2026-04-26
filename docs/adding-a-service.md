# Adding a New Service

This guide walks through adding a new self-hosted app (e.g. Paperless-ngx) to the homelab catalog.

## Quick start

```bash
# Interactive wizard (recommended)
homelab service new

# Non-interactive with flags
homelab service new paperless --container paperless-ngx --port 8000
```

This creates `~/.config/homelab/services/paperless/` with boilerplate files. Edit the generated files, then follow the steps below to test and optionally contribute to the catalog.

---

## Full workflow

### 1. Scaffold the service

Run the wizard to generate boilerplate:

```bash
homelab service new paperless
```

This creates:

```
~/.config/homelab/services/paperless/
├── docker-compose.yml
├── caddy.conf          # private reverse proxy (tailnet)
├── caddy-pub.conf      # public reverse proxy (Cloudflare Tunnel)
└── config.yaml         # vars + secrets schema
```

### 2. Edit `docker-compose.yml`

Two rules:
- Attach the main (UI-facing) container to the `home-services` external network.
- Use a separate `internal: true` network for any databases or background workers.

```yaml
networks:
  home-services:
    name: home-services
    external: true
  paperless-internal:
    internal: true

services:
  paperless-ngx:           # ← this name is used in caddy.conf
    image: ghcr.io/paperless-ngx/paperless-ngx:latest
    container_name: paperless-ngx
    restart: unless-stopped
    environment:
      PAPERLESS_REDIS: redis://paperless-redis:6379
      PAPERLESS_DBHOST: paperless-postgres
      # ... other vars
    volumes:
      - paperless-data:/usr/src/paperless/data
      - paperless-media:/usr/src/paperless/media
    networks:
      - home-services        # reachable by Caddy
      - paperless-internal   # internal comms

  paperless-redis:
    image: redis:7-alpine
    container_name: paperless-redis
    networks:
      - paperless-internal   # NOT reachable by Caddy — good

  paperless-postgres:
    image: postgres:15
    container_name: paperless-postgres
    environment:
      POSTGRES_USER: paperless
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_DB: paperless
    volumes:
      - paperless-pgdata:/var/lib/postgresql/data
    networks:
      - paperless-internal

volumes:
  paperless-data:
  paperless-media:
  paperless-pgdata:
```

### 3. Edit `caddy.conf` (private/tailnet access)

```caddyfile
paperless.{$HOME_SUBDOMAIN}.{$DOMAIN} {
    import wildcard_tls

    reverse_proxy paperless-ngx:8000
}
```

The hostname `paperless-ngx` matches `container_name` in your compose file. Caddy resolves it via Docker DNS on the `home-services` network.

### 4. Edit `caddy-pub.conf` (optional public access)

```caddyfile
paperless.{$PUB_SUBDOMAIN}.{$DOMAIN} {
    import wildcard_tls

    reverse_proxy paperless-ngx:8000
}
```

This file is used only when exposing services publicly via Cloudflare Tunnel.

### 5. Edit `config.yaml`

Define configuration schema with sensible defaults:

```yaml
vars:
  PAPERLESS_PORT:
    value: "8000"
    description: "Paperless-ngx web UI port"

secrets:
  DB_PASSWORD:
    required: true
    description: "PostgreSQL database password"
  PAPERLESS_ADMIN_PASSWORD:
    required: false
    description: "Initial admin password (optional)"
```

Run the interactive setup wizard to configure values:

```bash
homelab service setup paperless
```

### 6. Start the service stack

```bash
homelab service up paperless
```

Verify containers are healthy:

```bash
homelab service ps paperless
homelab service logs paperless
```

### 7. Expose the service via Caddy

For private (tailnet) access:

```bash
homelab service enable paperless --private
```

This symlinks `caddy.conf` into `caddy/conf.d/` and reloads Caddy. It will:
- Validate the Caddyfile syntax
- Obtain a TLS certificate (or reuse the wildcard if already issued)
- Start routing `paperless.<HOME_SUBDOMAIN>.<DOMAIN>` → the container

For public internet access (optional):

```bash
# First configure Cloudflare Tunnel
homelab tunnel route add paperless
homelab service enable paperless --public
```

### 8. Test from a tailnet-connected device

```
https://paperless.<HOME_SUBDOMAIN>.<DOMAIN>
```

---

## Contributing to the catalog

Once your service is tested and working, contribute it to the embedded catalog:

1. **Copy to assets directory**:

```bash
cp -r ~/.config/homelab/services/paperless assets/services/paperless
```

2. **Verify the service catalog**:

```bash
make catalog
```

This exports `assets/services/` → `services/` for local browsing verification.

3. **Test from the catalog**:

```bash
# Remove the local copy
rm -rf ~/.config/homelab/services/paperless

# Install from catalog
homelab service add paperless
homelab service setup paperless
homelab service up paperless
homelab service enable paperless --private
```

4. **Submit a PR** with your changes to `assets/services/paperless/`

---

## Removing a service

### Remove from Caddy routing (without stopping containers)

```bash
homelab service disable paperless
```

### Stop the service completely

```bash
homelab service disable paperless
homelab service down paperless
```

### Remove the service from your config directory

```bash
rm -rf ~/.config/homelab/services/paperless
```

---

## Checklist

For a working service:

- [ ] `container_name` in `docker-compose.yml` matches the upstream in `caddy.conf`
- [ ] Primary container is on `home-services` network
- [ ] Databases / workers are on a separate `internal: true` network
- [ ] `config.yaml` has sensible defaults and clear descriptions
- [ ] `homelab service setup <name>` — configure vars and secrets
- [ ] `homelab service up <name>` — containers healthy
- [ ] `homelab service enable <name> --private` — Caddy reloaded without errors
- [ ] Accessible at `https://<service>.<HOME_SUBDOMAIN>.<DOMAIN>` from a tailnet device

For contributing to the catalog:

- [ ] Service tested end-to-end from the catalog
- [ ] `docker-compose.yml` uses official images from the upstream project
- [ ] Network isolation follows the `internal: true` pattern
- [ ] `config.yaml` has required/sensitive fields in `secrets` section
- [ ] Both `caddy.conf` and `caddy-pub.conf` are present and correct
- [ ] README or upstream documentation link included in `config.yaml` description
