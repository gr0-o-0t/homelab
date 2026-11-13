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
state dir (`/var/lib/homelab/state` in the `homelab-agent` volume on the leader).
Edit the generated files, then follow the steps below to test and optionally
contribute to the catalog.

> **Editing on the leader**: edit the files from any PC with `homelab edit
> <service> <path>`: `docker-compose.yml`, `config.yaml`, or a file the
> service bind-mounts. The file opens in your `$EDITOR`; when you save a change
> it goes back to the leader and, if the service runs, is applied as `homelab
> up` does (shared databases and generated secrets first). The compose file and
> config.yaml are checked first (they must parse and translate to Swarm
> services, with no secret in a spec); an invalid one is refused and the
> previous content kept. `exposure.yaml` is owned by `homelab enable`/`disable`.

> **Port config format**: `config.yaml` lists the ports to route as strings: a bare number for a single HTTP port (`- 8000`), `name:port` for a subdomain (`- web:8000`), `listen:port/proto` for a raw port (`- 22:2222/tcp`). See the port grammar below.

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

### 2. Edit `docker-compose.yml` (`homelab edit paperless docker-compose.yml`)

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
host path or the docker socket is pinned to the leader (core node); a stateless one prefers a
worker PC. Override per container in `config.yaml` with `placement:`
(`core`, `worker`, `any`), and list re-creatable volumes (model caches) under
`cache_volumes:` so they do not pin.

**What Swarm honours.** Only the keys the translator maps are deployed:
`image`, `entrypoint`, `command`, `environment` (delivered through the Swarm
secret), `volumes` (a writable `./` bind mount is refused; read-only ones become configs) and `tmpfs` (long-form `size` is kept; short-form `path:options` drops its options), `networks` (and
`container_name` as an alias), `labels`, `hostname`, `working_dir`, `user`,
`group_add`, `tty`, `stdin_open`, `read_only`, `init`, `stop_signal`,
`stop_grace_period`, `healthcheck`, `extra_hosts`, `sysctls`, `cap_add`,
`cap_drop`, `ulimits`, `security_opt: no-new-privileges`, and `restart`.
Everything else in the compose file is **ignored, without an error**, notably `ports:`
(publish through `config.yaml`'s `ports:` and `homelab enable` instead — nothing is
published on a host), `deploy.resources` / `mem_limit` / `cpus`, `depends_on`
(start order is not enforced: make the app retry its connections),
`shm_size`, `dns` / `dns_search`, `logging`, `expose`, `pid`, `ipc`, and other
`security_opt` values. If a service needs one of those, it will not work as it
did under plain Compose.

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

### 5. Edit `config.yaml` (`homelab edit paperless config.yaml`)

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

> **Port grammar**: one list entry per exposed port. The token left of the
> colon decides the form:
>
> | Entry | Meaning | `--ports=` name |
> |---|---|---|
> | `8000` | bare port: `<service>.<home>.<domain>` → container :8000 | `default` |
> | `web:8000` | a **subdomain**: `web.<home>.<domain>` → container :8000; it *replaces* the service name | `web` |
> | `22:2222/tcp` | all digits = a **listen port**: raw TCP on the tailnet node, `:22` → container :2222 (caddy-l4) | `22` |
>
> `ssh:22` is therefore a subdomain (`ssh.<home>.<domain>` → :22, over HTTPS),
> **not** a named raw port; for raw SSH write `22:22/tcp`. A raw port is
> tailnet-only, unique per protocol across the cluster, and both tcp and udp
> when the protocol suffix is left off, so declare `/tcp` unless you need udp.
> `homelab enable <service> --ports=web,22` then routes only those names;
> `homelab status <service>` lists each declared port.

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

## A custom service with a locally built image

For a service that is not in the catalog, with an image you build yourself and
push to no registry (a game server, your own app). Keep it in a folder on the
PC where you build:

```
taash/
├── docker-compose.yml
├── config.yaml
└── conf/app.toml        # optional bind-mounted files
```

The folder's base name is the service name. In `docker-compose.yml`:

```yaml
services:
  taash:
    image: taash-server:current   # unqualified: no namespace, no registry host
    pull_policy: never            # marks the image local: never pulled
    container_name: homelab-taash
    networks: [home-services]
```

- `pull_policy: never` is what makes the image local. Use an unqualified name:
  a reference like `someuser/app:tag` could be replaced at task start by an
  image of that name on Docker Hub (`add`, `up` and `update` warn about it).
- A local image runs on the core node unless `config.yaml` sets `placement:`.
  Shared databases are declared under `databases:` as for any service.
- The folder must not hold symlinks, dotfiles or an `exposure.yaml`; a file over
  500 KiB or a folder over 10 MiB is refused; postgres, mariadb, redis and
  garage are reserved names.

Install, deploy, expose (from the PC with the image):

```bash
homelab add ./taash       # a bare name works unless it is also a catalog service
homelab setup taash       # if it declares secrets
homelab up taash          # ships the image to the leader if it lacks it
homelab enable taash
```

`add` refuses a service that already exists; change its files with `homelab edit`
(`homelab edit taash docker-compose.yml --from docker-compose.yml` in a script).

After a rebuild under the same tag:

```bash
docker build -t taash-server:current .
homelab update taash      # ships the image if it differs, rolls the tasks
```

`homelab up` never replaces an image the leader already has, and a plain
`docker build` on the leader itself goes live only at the next task restart; use
`update` to roll deliberately. If the PC has no Docker daemon, nothing is shipped
and the leader reports the missing image.

### Read-only access to another service's database

To let a consumer (e.g. grafana) read another service's Postgres database, add
to the consumer's `config.yaml` (`homelab edit grafana config.yaml`):

```yaml
databases:
  - postgres:
      database: taash        # the owner's database; must exist (up taash first)
      user: taash_ro
      readonly: true
      env:
        host: TAASH_PG_HOST
        port: TAASH_PG_PORT
        user: TAASH_RO_USER
        password: TAASH_RO_PASSWORD
        database: TAASH_PG_DB
```

Pass those variables on in the consumer's compose file, then `homelab up grafana`.
The role is SELECT-only (also on tables created later), and is dropped when
the consumer is deleted. A pre-existing role of that name must be a plain login
role.

---

## Contributing to the catalog

Once your service is tested and working, contribute it to the embedded catalog:

1. **Copy to assets directory**:

```bash
# from any PC: `files get` prints a file from the leader (repeat for any file it bind-mounts)
mkdir -p assets/services/paperless
for f in docker-compose.yml config.yaml; do
  homelab files get paperless "$f" > "assets/services/paperless/$f"
done
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
