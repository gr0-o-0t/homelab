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

```bash
homelab                                            # interactive TUI (service browser)
homelab service list                               # list services (--json for machine-readable)
homelab service add [name]                         # install a bundled service from the catalog
homelab service setup <name>                       # configure vars and secrets interactively
homelab service up/down/restart [name|--all|--group <g>]  # lifecycle (batch-capable)
homelab service update [name|--all]               # pull latest images + restart
homelab service logs <name> [-f] [--tail N] [--since T]   # logs (TUI on TTY, plain otherwise)
homelab service ps <name>                          # container status
homelab service enable/disable [name|--all|--group <g>] --private/--public
homelab service doctor [name|--all] [--fix]        # health check with optional auto-repair
homelab service new [name]                         # scaffold wizard (TUI) or --container/--port flags
homelab core start/stop/restart/logs/status        # core stack (Tailscale + Caddy)
homelab caddy reload/validate
homelab ts status
homelab tunnel status/logs                         # Cloudflare Tunnel management
homelab tunnel route add/rm <service>              # manage DNS routes for public exposure
homelab setup                                      # interactive config wizard (config.yaml + keyring)
homelab doctor [--fix]                             # health check with optional auto-repair
homelab completion [bash|zsh|fish|powershell]      # generate shell completion scripts
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
└── services/            # populated by homelab service add (from assets/services/)
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

`homelab service enable` creates a relative symlink `caddy/conf.d/<name>.conf → ../../services/<name>/caddy.conf` and reloads Caddy gracefully. Disable removes it.

### Embedded Catalog (`assets/`)

`assets/assets.go` embeds two filesystems:
- `CoreFS` — `core/` + `caddy/` trees; `homelab setup` installs these to the config dir
- `CatalogFS` — `services/` tree; `homelab service add <name>` copies one entry to the config dir

**The canonical service catalog is `assets/services/`.** This is the only copy — edit
files directly in `assets/services/<name>/`. The root `services/` directory is a
gitignored export (`make catalog`) for local browsing only.

Each service directory contains:
- `docker-compose.yml` — UI container joins `home-services`; databases use `internal: true` network
- `caddy.conf` — private reverse proxy snippet (tailnet)
- `caddy-pub.conf` — public reverse proxy snippet (Cloudflare Tunnel)
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
homelab service up --group media
homelab service enable --group media --private
```

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
| `internal/tui/list` | Bubble Tea fullscreen service browser |
| `internal/tui/logs` | Bubble Tea streaming log viewer |
| `internal/tui/wizard` | Multi-step new-service scaffold wizard |
| `internal/tui/spinner` | Goroutine spinner (TTY-aware) |
| `internal/tui/styles` | Lipgloss Tokyo Night palette, shared across TUI and plain output |

### Key design decisions

- **Docker SDK for status, shell-out for lifecycle**: SDK used only for read-only inspection (ContainerList, ContainerInspect). `docker compose` CLI is shelled out for lifecycle ops to preserve Compose's reconciliation logic.
- **No secrets on disk**: `run.Commander.DockerComposeEnv` injects via `cmd.Env = mergeEnv(os.Environ(), overrides)` — no temp `.env` files.
- **TTY detection**: `isatty.IsTerminal(os.Stdout.Fd()) && !noColor()` — TUI when interactive, plain table when piped/CI.
- **Spinner + captured output**: Caddy reload output is captured in a `bytes.Buffer` Commander while the spinner runs; buffer is printed only on error.

## Adding a New Service

```bash
homelab service new          # interactive scaffold wizard
homelab service setup myapp  # configure vars and secrets
homelab service up myapp
homelab service enable myapp --private
```

Then add the service to the embedded catalog by creating `assets/services/myapp/`.
Run `make catalog` to export it to `services/myapp/` for local reference.
See `docs/adding-a-service.md`.

## Docs

- `docs/architecture.md` — design decisions, network diagrams
- `docs/tailscale-setup.md` — auth key generation, ACL config, TUN device
- `docs/cloudflare-setup.md` — API token scoping, CNAME setup
- `docs/adding-a-service.md` — step-by-step new service guide
