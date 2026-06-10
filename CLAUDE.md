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
homelab logs [service]                             # logs (TUI on TTY, plain otherwise)
homelab doctor [service] [--fix] [--all]           # health check with optional auto-repair
homelab validate                                   # validate Caddyfile syntax
homelab service list                               # list services + exposure status (legacy, hidden)
homelab service ps <name>                          # container status (legacy, hidden)
```

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

homelab ext cf route add <service>                 # add Cloudflare DNS route
homelab ext cf route rm <service>                  # remove Cloudflare DNS route
homelab ext ipfs gateway enable                    # enable IPFS Gateway Caddy route
homelab ext ipfs gateway disable                   # disable IPFS Gateway Caddy route
homelab ext i2p <status|logs|list>                 # i2pd router management
homelab ext tor <status|logs|list>                 # Tor onion service management
homelab ext ygg <status|logs|list>                 # Yggdrasil mesh management
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
- `caddy.conf` — private reverse proxy snippet (tailnet)
- `caddy.cf.conf` — public reverse proxy snippet (Cloudflare Tunnel)
- `config.yaml` — vars + secrets schema with sensible defaults

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
