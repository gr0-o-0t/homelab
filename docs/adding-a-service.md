# Adding a New Service

This guide walks through adding a new self-hosted app (e.g. Paperless-ngx) to the homelab catalog.

## Quick start

```bash
# Interactive wizard (recommended)
homelab new

# Non-interactive with flags
homelab new paperless --container paperless-ngx --port 8000
```

This creates `~/.config/homelab/services/paperless/` with boilerplate files. Edit the generated files, then follow the steps below to test and optionally contribute to the catalog.

---

## Full workflow

### 1. Scaffold the service

Run the wizard to generate boilerplate:

```bash
homelab new paperless
```

This creates:

```
~/.config/homelab/services/paperless/
├── docker-compose.yml
├── caddy.conf          # private reverse proxy (tailnet)
├── caddy.cf.conf      # public reverse proxy (Cloudflare Tunnel)
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

### 4. Edit `caddy.cf.conf` (optional public access)

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
homelab setup paperless
```

### 6. Start the service stack

```bash
homelab up paperless
```

Verify containers are healthy:

```bash
homelab status paperless
homelab logs paperless
```

### 7. Expose the service via Caddy

For private (tailnet) access:

```bash
homelab enable paperless
```

This generates Caddy config and reloads Caddy. It will:
- Validate the Caddyfile syntax
- Obtain a TLS certificate (or reuse the wildcard if already issued)
- Start routing `paperless.<HOME_SUBDOMAIN>.<DOMAIN>` → the container

For public internet access (optional):

```bash
# First configure Cloudflare Tunnel DNS route
homelab ext cf route add paperless
homelab enable paperless --cf
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
homelab delete paperless

# Install from catalog
homelab add paperless
homelab setup paperless
homelab up paperless
homelab enable paperless
```

4. **Submit a PR** with your changes to `assets/services/paperless/`

---

## Removing a service

### Remove from Caddy routing (without stopping containers)

```bash
homelab disable paperless
```

### Stop the service completely

```bash
homelab disable paperless
homelab down paperless
```

### Remove the service from your config directory

```bash
homelab delete paperless
```

---

## Checklist

For a working service:

- [ ] `container_name` in `docker-compose.yml` matches the upstream in `caddy.conf`
- [ ] Primary container is on `home-services` network
- [ ] Databases / workers are on a separate `internal: true` network
- [ ] `config.yaml` has sensible defaults and clear descriptions
- [ ] `homelab setup <name>` — configure vars and secrets
- [ ] `homelab up <name>` — containers healthy
- [ ] `homelab enable <name>` — Caddy reloaded without errors
- [ ] Accessible at `https://<service>.<HOME_SUBDOMAIN>.<DOMAIN>` from a tailnet device

For contributing to the catalog:

- [ ] Service tested end-to-end from the catalog
- [ ] `docker-compose.yml` uses official images from the upstream project
- [ ] Network isolation follows the `internal: true` pattern
- [ ] `config.yaml` has required/sensitive fields in `secrets` section
- [ ] Both `caddy.conf` and `caddy.cf.conf` are present and correct
- [ ] README or upstream documentation link included in `config.yaml` description
