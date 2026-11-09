# Adding a New Service

This guide walks through adding a new self-hosted app (e.g. Paperless-ngx) to the homelab catalog.

## Quick start

```bash
# Interactive wizard (recommended)
homelab new

# Non-interactive with flags
homelab new paperless --container paperless-ngx --port 8000
```

Like every command but `init`/`join`/`login`/`edit`, `new` runs on the leader
agent: it creates `services/paperless/` with boilerplate files in the leader's
state dir (`/var/lib/homelab/state` in the `homelab-agent` volume on minis).
Edit the generated files, then follow the steps below to test and optionally
contribute to the catalog.

> **Editing on the leader**: `homelab edit <service> <path>` edits the files a
> service bind-mounts from any PC. `docker-compose.yml` and `config.yaml` are
> not editable through the CLI: change them on minis inside the agent volume
> (`docker volume inspect homelab-agent` shows its mountpoint), or write the
> service in `assets/services/` of this repository and install it with
> `homelab add` from an agent built with it.

> **Port config format**: The scaffolded `config.yaml` uses the new list-of-strings format. For a single HTTP port, use a bare port number (e.g. `- 8000`). For services with multiple ports, use named entries (e.g. `- ssh:22`) or mapped entries (e.g. `- 22:22`). See below for examples.

---

## Full workflow

### 1. Scaffold the service

Run the wizard to generate boilerplate:

```bash
homelab new paperless
```

This creates:

```
services/paperless/          # in the leader's state dir
├── docker-compose.yml
└── config.yaml         # vars, secrets and the ports Caddy routes to
```

### 2. Edit `docker-compose.yml`

Two rules:
- Attach the main (UI-facing) container to the `home-services` external network
  (the Swarm overlay; homelab deploys the file as a Swarm stack).
- Never bundle a database, cache or object store. Postgres, MariaDB, Redis and
  S3 (Garage) each run once, shared; declare what you need under `databases:`
  in `config.yaml` and the service gets its own database/role, redis database
  number, or bucket + key inside the shared instance. (A catalog test fails on
  a bundled postgres/mysql/mariadb/mongo/redis/valkey/keydb/minio/garage image.)
  Use a separate `internal: true` network only for other background workers.

It runs on Swarm, so `privileged`, `devices`, `network_mode`, `env_file` and a
`build:` without an `image:` are refused. A container with a data volume, a
host path or the docker socket is pinned to minis; a stateless one prefers a
worker PC. Override per container in `config.yaml` with `placement:`
(`core`, `worker`, `any`), and list re-creatable volumes (model caches) under
`cache_volumes:` so they do not pin.

```yaml
networks:
  home-services:
    name: home-services
    external: true

services:
  paperless:               # ← the generated routes proxy to <service>:<port>
    image: ghcr.io/paperless-ngx/paperless-ngx:latest
    container_name: paperless
    restart: always
    environment:
      PAPERLESS_REDIS: ${PAPERLESS_REDIS}      # injected — see config.yaml
      PAPERLESS_DBHOST: ${PAPERLESS_DBHOST}
      PAPERLESS_DBUSER: ${PAPERLESS_DBUSER}
      PAPERLESS_DBPASS: ${PAPERLESS_DBPASS}
      # ... other vars
    volumes:
      - paperless-data:/usr/src/paperless/data
      - paperless-media:/usr/src/paperless/media
    networks:
      - home-services        # reachable by the agent's Caddy, and by the shared databases

volumes:
  paperless-data:
    name: paperless_data
  paperless-media:
    name: paperless_media
```

And in `config.yaml`:

```yaml
databases:
  - postgres:
      database: paperless
      user: paperless
      env:
        host: PAPERLESS_DBHOST
        user: PAPERLESS_DBUSER
        password: PAPERLESS_DBPASS   # generated, kept in the leader's vault
  - redis:
      env:
        dsn: PAPERLESS_REDIS        # redis://:<shared pw>@homelab-redis:6379/<own db>
```

### 3. Declare the ports in `config.yaml` (routing)

You do not write Caddy config. `homelab enable` generates a site block for
every layer (private, `--cf`) from the ports the
service declares:

```yaml
ports:
  - 8000          # paperless.<HOME_SUBDOMAIN>.<DOMAIN> → paperless:8000
```

The upstream is `<service>:<port>`, resolved by Docker DNS on `home-services`
(a compose `container_name` becomes a network alias there),
so the service name must resolve: name the compose service (or its
`container_name`, or a network alias) after the service directory. See the
port grammar in CLAUDE.md for subdomains and listen ports.

### 4. Optional: `caddy.routes.conf`

Only when routing is more than one host → one upstream (websocket paths,
header rewrites, path fan-out): put the *body* of a site block in
`caddy.routes.conf` — directives only, no site address. `homelab enable`
wraps it for every layer.

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

ports:
  - "{{.Port}}"
```

> **Port format reference**:
> - `- 8000` — bare port number: `<service>.<home>.<domain>` → container :8000
> - `- web:8000` — a subdomain: `web.<home>.<domain>` → container :8000 (it replaces the service name)
> - `- 22:2222/tcp` — a listen port: raw TCP on the tailnet, `:22` → container :2222 (caddy-l4)

Run the interactive setup wizard to configure values:

```bash
homelab setup paperless
```

### 6. Bring up the service stack

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

This renders the service's Caddy file and loads it into the leader agent's Caddy. It will:
- Refuse a config Caddy rejects, leaving the running one in place
- Obtain a TLS certificate (or reuse the wildcard if already issued)
- Start routing `paperless.<HOME_SUBDOMAIN>.<DOMAIN>` → the service, on whichever node runs it

For public internet access (optional):

```bash
# With cloudflared on (homelab ext enable cf); this also creates the DNS route
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
# from minis, out of the leader's state dir
sudo cp -r "$(docker volume inspect -f '{{.Mountpoint}}' homelab-agent)/state/services/paperless" assets/services/paperless
```

2. **Verify the service catalog**:

```bash
make catalog
```

This exports `assets/services/` → `services/` for local browsing verification.

3. **Test from the catalog** (`homelab add` installs from the catalog embedded in the leader agent, so run an agent built from this tree):

```bash
# Remove the scaffolded copy
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

### Tear down the service completely

```bash
homelab disable paperless
homelab down paperless
```

### Remove the service from the leader's state

```bash
homelab delete paperless
```

---

## Checklist

For a working service:

- [ ] The service directory name resolves on `home-services` (service name, `container_name` or alias)
- [ ] Primary container is on `home-services` network
- [ ] No bundled database/cache/object store — shared ones declared under `databases:`
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
- [ ] `config.yaml` declares the ports to route (no `caddy.conf` files — a test rejects them)
- [ ] README or upstream documentation link included in `config.yaml` description
