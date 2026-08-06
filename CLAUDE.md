# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

A modular self-hosted infrastructure stack managed by a Go CLI (`homelab`). All services are accessible only from within a private Tailscale tailnet at `https://<service>.home.<domain>`. No public ports are opened. Caddy terminates TLS using wildcard certs issued via Cloudflare DNS-01 challenge.

## Building

```bash
make build      # go build -o homelab .
make install    # go install .
make tidy       # go mod tidy
make test       # go test ./...
make ci         # lint + test-race + build
make catalog    # export assets/services/ → services/ for local browsing
```

Run a single test package:

```bash
go test ./internal/scaffold/...
```

## CLI Commands

### Service lifecycle

```bash
homelab add [name]                                 # install a bundled service from the catalog (no name → list catalog)
homelab new [name]                                 # scaffold wizard (TUI) or --container/--port flags
homelab setup [service]                            # configure vars and secrets interactively (no arg → root wizard)
homelab start [service]     (alias: up)            # start core stack or service(s) (--all, --group, --build)
homelab stop [service]      (alias: down)          # stop core stack or service(s) (--all, --group)
homelab restart [service]                          # restart containers (--all, --group, --build)
homelab reload [service]                           # reload Caddy config or a service's routing
homelab update [service]                           # pull latest images + restart (--all)
homelab delete <service>    (alias: rm)            # remove service entirely
```

### Observation and diagnostics

```bash
homelab                                            # interactive TUI (service browser)
homelab status [service]                           # show status overview or per-service detail
homelab logs [service]                             # logs (TUI on TTY, plain otherwise) — installed services only
homelab caddy status                               # Caddy container status
homelab caddy logs                                 # stream Caddy container logs individually
homelab doctor [service] [--fix] [--all]           # health check with optional auto-repair
homelab validate                                   # validate Caddyfile syntax
homelab config [service]                           # show resolved docker compose configuration
homelab images [service]                           # list Docker images used by services
homelab port <service> <private-port>              # print the public port for a binding
homelab version                                    # print the homelab version
homelab service list                               # list services + exposure status (legacy, hidden)
homelab service ps <name>                          # container status (legacy, hidden)
```

### Container access

```bash
homelab exec <service> <command> [args...]         # run a command in a running service container
homelab pull [service]                             # pull latest images without restarting
```

### Backup, restore, prune

```bash
homelab backup [service]                           # volumes + DB dumps + config (--all, --group)
homelab backup [service] --out /mnt/nas            # destination (default <config-dir>/backups)
homelab backup [service] --live                    # don't stop the service (risks torn files)

homelab restore <backup-dir> [service]             # replace volumes + databases
homelab restore <backup-dir> --config              # also overwrite config.yaml / compose

homelab prune [service]                            # down + remove containers, images, volumes
homelab prune [service] --keep-volumes             # reclaim images only, no data loss
homelab prune --dangling                           # unreferenced images + build cache only
```

`backup` writes a timestamped directory with a `manifest.json` listing every
volume, dump and config file, so a backup is inspectable and restorable per
service. Each service is stopped for the duration of its own snapshot unless
`--live` is given — tarring a volume under a running process can capture a torn
file.

Secrets are deliberately **not** in backups; they stay in the system keyring.
After restoring onto a new machine run `homelab setup <service>`, which re-enters
them and re-syncs the database role password (`ALTER USER … PASSWORD`).

Redis is never dumped: in this catalog it is only ever a cache or job queue, and
a stale restored queue is worse than an empty one.

`restore` and `prune` are destructive and both confirm first. `prune` requires
typing the service name when volumes will be deleted, and refuses outright
without a TTY unless `--yes` is passed.

### Network exposure

```bash
homelab enable <service>                           # private tailnet only (default)
homelab enable <service> --cf                      # + Cloudflare Tunnel (public)
homelab enable <service> --i2p                     # + I2P eepsite
homelab enable <service> --tor                     # + Tor .onion service
homelab enable <service> --ygg                     # + Yggdrasil mesh
homelab enable <service> --all                     # all available extensions
homelab enable <service> --name=<custom>           # custom display name/subdomain
homelab enable <service> --ports=web,ssh           # expose specific named ports only

homelab disable <service>                          # remove private tailnet route
homelab disable <service> --cf                     # remove Cloudflare Tunnel
homelab disable <service> --i2p                    # remove I2P eepsite
homelab disable <service> --tor                    # remove Tor .onion
homelab disable <service> --ygg                    # remove Yggdrasil mesh
homelab disable <service> -a                       # remove all layers + stop container
```

### Configuration

```bash
homelab setup                                      # interactive root config wizard (config.yaml + keyring)
homelab setup <service>                            # per-service config wizard
```

### Shell completion

```bash
homelab completion bash|zsh|fish|powershell        # generate shell completion scripts
```

### Network extensions (`homelab ext`)

```bash
homelab ext list                                   # list extensions and their enabled/disabled status
homelab ext status [ext]                           # show container status for all or one extension
homelab ext logs [ext]                             # stream logs for all or one extension
homelab ext start [ext]                            # start extension container(s)
homelab ext stop [ext]                             # stop extension container(s)
```

Extension-specific management (DNS routes, per-layer status/logs)
lives under each extension's own top-level command, not under `ext`:

```bash
homelab cf route add <service>                     # add Cloudflare DNS route
homelab cf route rm <service>                      # remove Cloudflare DNS route
homelab i2p <status|logs|list>                     # i2pd router management
homelab tor <status|logs|list>                      # Tor onion service management
homelab ygg <status|logs|list>                      # Yggdrasil mesh management
```

## Architecture

### Config Directory

All runtime state lives under `${XDG_CONFIG_HOME:-$HOME/.config}/homelab/`.
Override with `--config-dir <path>` or `--config <file>` (file takes priority).

```
~/.config/homelab/
├── config.yaml          # root vars (DOMAIN, HOME_SUBDOMAIN, …)
├── core/                # installed by homelab setup (from assets/core/)
├── caddy/               # installed by homelab setup (from assets/caddy/)
└── services/            # populated by homelab add (from assets/services/)
```

Secrets (API tokens, passwords) are **never** written to disk — they are stored in the system keyring and injected into docker compose at runtime via `cmd.Env`.

### Config Schema (`internal/config`)

`config.yaml` has two sections:

```yaml
vars:
  DOMAIN:
    value: "example.com"
    required: true
secrets:
  TS_AUTHKEY:
    required: true
```

`BuildEnv()` loading order (later wins — service overrides root):
1. Root `config.yaml` vars
2. Root keyring secrets
3. Service `config.yaml` vars  ← overrides root vars
4. Service keyring secrets

### Network Flow

```
Client (on tailnet)
  → Cloudflare DNS (DNS-only, not proxied)
    → *.home.<domain>  A  <Caddy Tailscale IP 100.x.x.x>
      → Tailscale IP (100.x.x.x) — unreachable off-tailnet
        → Caddy (TLS termination, wildcard cert)
          → Docker home-services network
            → Service containers
```

### Core Stack (`assets/core/`)

Tailscale and Caddy run as a pair. Caddy uses `network_mode: service:tailscale`, sharing Tailscale's network namespace and listening on the Tailscale IP directly. Caddy is a custom build (`core/Dockerfile.caddy`) compiled with xcaddy including `caddy-dns/cloudflare` for DNS-01 cert issuance.

### Service Routing (`assets/caddy/`)

- `caddy/Caddyfile` — global config: ACME settings, Cloudflare DNS-01, wildcard TLS snippet, imports `conf.d/*.conf`
- `caddy/conf.d/` — per-service site blocks, populated by symlinking from `services/<name>/caddy.conf`

`homelab enable <name>` generates Caddy config into `caddy/conf.d/` and reloads Caddy gracefully. `homelab disable` removes it.

### Embedded Catalog (`assets/`)

`assets/assets.go` embeds two filesystems:
- `CoreFS` — `core/` + `caddy/` trees; `homelab setup` installs these to the config dir
- `CatalogFS` — `services/` tree; `homelab add <name>` copies one entry to the config dir

**The canonical service catalog is `assets/services/`.** This is the only copy — edit
files directly in `assets/services/<name>/`. The root `services/` directory is a
gitignored export (`make catalog`) for local browsing only.

Each service directory contains:
- `docker-compose.yml` — UI container joins `home-services`; databases use `internal: true` network
- `config.yaml` — vars + secrets + the ports the service exposes
- optional routing overrides (most services need none)

**Port declarations** drive routing. One line per exposed port:

```yaml
ports:
  - 3000          # forgejo.home.<domain>          → container :3000
  - 22:22         # forgejo.home.<domain>:22       → container :22
  - vault:80      # vault.home.<domain>            → container :80
  - 53:53/udp     # protocol suffix; both tcp+udp when omitted
```

The token left of the colon decides the form: all digits means "listen on this
port", anything else names a subdomain, and a subdomain *replaces* the service
name rather than prefixing it. Caddy only serves the tcp half — a udp
declaration is recorded for compose but gets no site block, because nothing in
this stack proxies datagrams.

From these, `homelab enable` generates the site blocks for every layer. Most
services need nothing else. Two overrides exist:

- `caddy.routes.conf` — the *body* of a site block: directives only, no site
  address and no `import wildcard_tls`. For services whose routing is more than
  one host → one upstream (websocket paths, header rewrites, path fan-out).
  `homelab enable` wraps it in whichever site address each layer needs, so
  private, `--cf`, `--i2p`, `--tor` and `--ygg` all get the same route set. Its
  leading comment block is treated as file-level documentation and stripped from
  generated output.
- `caddy.conf` + `caddy.cf.conf` — hand-written per-layer site blocks, the
  original scheme. Only for routing the grammar cannot express: `adguardhome`
  (seven ports under a different subdomain) and `minero` (several upstream
  containers behind one host). A test enumerates them; anything else shipping
  one is a regression.

Shipping both shapes is a test failure — `caddy.routes.conf` wins at enable
time, which would leave `caddy.conf` as a second, silently stale copy.

### Config Schema — Groups

`config.yaml` supports an optional `groups` section for batch operations:

```yaml
groups:
  media:
    - jellyfin
    - immich
  monitoring:
    - uptime-kuma
```

Groups are used with `--group <name>` on lifecycle commands:

```bash
homelab up --group media              # start all media services
homelab down --group media            # stop all media services
```

Note: `--group` is only supported on `start`/`up`, `stop`/`down`, and `restart`.
`enable` and `disable` operate on single services only — use `--all` to affect
every layer at once.

### Go package layout

| Package | Role |
|---|---|
| `assets/` | `//go:embed core caddy` (CoreFS) + `//go:embed services` (CatalogFS) |
| `cmd/` | Cobra command definitions (root, service, core, caddy, tailscale, setup, doctor, add, tunnel, update, completion) |
| `internal/config` | XDG config dir resolution, YAML schema types, `BuildEnv()` |
| `internal/secrets` | System keyring — SecretService (Linux), Keychain (macOS), encrypted file |
| `internal/service` | `Discover()` filesystem scan; `DiscoverWithDocker()` enriches with SDK data |
| `internal/docker` | Docker SDK client — read-only (ContainerList, ContainerInspect) |
| `internal/run` | `Commander` — shells out to `docker compose`; injects env via `cmd.Env` |
| `internal/caddy` | Symlink management + Caddy validate/reload via docker exec |
| `internal/scaffold` | `//go:embed templates/*`; `Render()` + `Write()` for new-service boilerplate |
| `internal/tui/dashboard` | Bubble Tea fullscreen service browser |
| `internal/tui/logs` | Bubble Tea streaming log viewer |
| `internal/tui/wizard` | Multi-step new-service scaffold wizard |
| `internal/tui/spinner` | Goroutine spinner (TTY-aware) |
| `internal/tui/styles` | Lipgloss Tokyo Night palette, shared across TUI and plain output |

### Key design decisions

- **Docker SDK for status, shell-out for lifecycle**: SDK used only for read-only inspection (ContainerList, ContainerInspect). `docker compose` CLI is shelled out for lifecycle ops to preserve Compose's reconciliation logic.
- **No secrets on disk**: Commander injects via `cmd.Env` — no temp `.env` files.
- **TTY detection**: `isatty.IsTerminal(os.Stdout.Fd()) && !noColor()` — TUI when interactive, plain table when piped/CI.
- **Spinner + captured output**: Caddy reload output is captured in a `bytes.Buffer` Commander while the spinner runs; buffer is printed only on error.

## Adding a New Service

```bash
homelab new myapp             # interactive scaffold wizard
homelab setup myapp           # configure vars and secrets
homelab up myapp              # start containers
homelab enable myapp          # expose on private tailnet
```

Then add the service to the embedded catalog by creating `assets/services/myapp/`.
Run `make catalog` to export it to `services/myapp/` for local reference.
See `docs/adding-a-service.md`.

## Docs

- `docs/architecture.md` — design decisions, network diagrams
- `docs/tailscale-setup.md` — auth key generation, ACL config, TUN device
- `docs/cloudflare-setup.md` — API token scoping, CNAME setup
- `docs/adding-a-service.md` — step-by-step new service guide
