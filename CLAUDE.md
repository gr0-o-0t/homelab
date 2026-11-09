# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

A modular self-hosted infrastructure stack on Docker Swarm, managed by a Go CLI (`homelab`). One `homelab agent` container runs on every PC; the agent on the Swarm manager (the **leader**) holds all state and runs the edge: an embedded Caddy (with caddy-l4) serving on its own Tailscale node (tsnet). Services are reachable only from within the private tailnet at `https://<service>.home.<domain>`; no public ports are opened. Caddy terminates TLS with a wildcard cert issued via Cloudflare DNS-01. The CLI on any PC forwards commands to the leader agent over the tailnet.

## Building

```bash
make build           # default build, includes the desktop GUI (cgo + OpenGL/X11 headers; links libGL/libX11)
make build-headless  # -tags nogui: pure Go, no GUI — servers, the agent, cross-compiling
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

Every command except the local ones (`init`, `join`, `login`, `logout`, `edit`,
`version`, `completion`, `backup --fetch`, and the bare `homelab` dashboard/GUI)
is forwarded to the leader agent and runs there against its state dir.

### Cluster

```bash
homelab init [--import <dir>] [--image <ref>]      # first PC (minis): swarm init on its tailnet IP, start the leader agent; prints the join command
homelab join [--endpoint <url>] [--secret-stdin]   # another PC: swarm join as a worker, start its agent
homelab login [--endpoint <url>] [--secret-stdin]  # use this PC as a client only (no agent, no swarm)
homelab logout                                     # forget the endpoint and the cluster secret here
```

`init`/`join` advertise the host's Tailscale IPv4 and refuse an address outside
100.64.0.0/10. `init --import ~/.config/homelab` brings a Compose-era config dir
(root and service config, exposure, routing, desired state, keyring secrets)
into the new leader and reuses its named volumes. `homelab upgrade` (rolling the
agents, workers first) is planned (Task 20) and does not exist yet.

### Service lifecycle

```bash
homelab add [name]                                 # install a bundled service from the catalog (no name → list catalog)
homelab new [name]                                 # scaffold wizard (TUI) or --container/--port flags
homelab setup [service]                            # configure vars and secrets interactively (no arg → root wizard)
homelab up <service...>                            # deploy on Swarm, shared databases first (-a/--all, --group)
homelab down <service...>                          # scale to zero; config, volumes and exposure kept (-a, --group)
homelab start <service...>                         # same as up on Swarm (-a, --group)
homelab stop <service...>                          # same as down on Swarm (-a, --group)
homelab restart <service...>                       # force-update every Swarm service of the stack (-a, --group)
homelab pull <service...>                          # pull images only (-a, --group)
homelab update <service...>                        # pull + redeploy, tags resolved to digests (-a, --group)
homelab reload [service]                           # load the routes into the agent's Caddy, or re-render one service's
homelab edit <service> <path>                      # edit a bind-mounted config file in $EDITOR; saved on the leader, service redeployed
homelab delete <service>... (alias: rm)            # Swarm services + exposure + config; type the name or -y
```

The lifecycle verbs take several services, continue past a failing one and
report every failure at the end. With no service they refuse ("name a service,
or use -a/--all"): there is no core stack to act on. Exposure is configuration,
not container state: `down` leaves routes in place (they answer 502 while the
service is down) and `up` brings it back exactly as it was. Only `disable` and
`delete` remove exposure.

### Observation and diagnostics

```bash
homelab                                            # interactive TUI (service browser)
homelab --gui                                      # desktop GUI (giu; absent from -tags nogui builds)
homelab ls [-q]                                    # list installed services (table, --json, -q names)
homelab status [service]   (alias: ps)             # edge + services overview, or per-service detail
homelab logs [service]                             # a service's logs from every node, key-prefixed (-f, -n, -t, --since, -c, --tui); no service: the leader agent's
homelab caddy status                               # whether the agent's Caddy answers on its admin socket
homelab caddy logs                                 # follow the leader agent's logs (Caddy logs there)
homelab doctor [service] [--fix] [--all]           # health checks; --fix creates the overlay network, sites dir
homelab validate                                   # assemble and adapt the Caddy config without loading it
homelab config <service>                           # the compose file as resolved to deploy (contains secrets)
homelab images <service> [-q]                      # images the stack deploys
homelab version                                    # print the homelab version
```

### Container access

```bash
homelab exec <service> <command> [args...]         # relayed to the node running the task; flags after the service go to the command (-c, -u, -w, -e, -T)
```

`exec` and `logs` pick one container of a multi-container stack with
`-c <compose key or container_name>`.

### Backup, restore, prune

```bash
homelab backup [service]                           # volumes + DB dumps + config (--all, --group)
homelab backup [service] --out <dir>               # destination inside the homelab-backups volume (default its top level)
homelab backup [service] --live                    # don't stop the service (risks torn files)
homelab backup --list [--json]                     # existing backups on the leader, newest first
homelab backup --fetch <name> <local-dir>          # copy a backup from the leader to this PC (<local-dir>/<name>)

homelab restore <backup-dir> [service]             # replace volumes + databases (--create-volumes on a fresh cluster)
homelab restore <backup-dir> --config              # also overwrite config.yaml / compose

homelab prune [service...]                         # remove Swarm services, then volumes and images
homelab prune [service] --keep-volumes             # reclaim images only, no data loss
homelab prune --dangling                           # unreferenced images + build cache only
```

Backups run on the leader, where every data volume is pinned, and land in the
`homelab-backups` volume. Each writes a timestamped directory with a
`manifest.json` listing every volume, dump and config file. Each service is
scaled to zero for its own snapshot unless `--live` is given; `backup` and
`restore` keep running on the leader if the client disconnects.

Secrets are deliberately **not** in backups; they stay in the leader's vault.
After restoring onto a new cluster run `homelab setup <service>`, which
re-enters them and re-syncs the database role password (`ALTER USER … PASSWORD`).

Redis is never dumped: in this catalog it is only ever a cache or job queue, and
a stale restored queue is worse than an empty one.

`restore` and `prune` are destructive and both confirm first. `prune` requires
typing the service name when volumes will be deleted, and refuses outright
without a TTY unless `--yes` is passed. It removes images on the leader only.

### Network exposure

```bash
homelab enable <service>                           # private tailnet only (default)
homelab enable <service> --cf                      # + Cloudflare Tunnel (public)
homelab enable <service> --all                     # all available layers
homelab enable <service> --name=<custom>           # custom display name/subdomain
homelab enable <service> --ports=web,ssh           # expose specific named ports only

homelab disable <service>                          # remove private tailnet route
homelab disable <service> --cf                     # remove Cloudflare Tunnel
homelab disable <service> -a                       # remove every layer, private included
homelab disable <service> -a --stop                # …and take the service down
```

Day-one layers are the tailnet (`ts`) and Cloudflare (`cf`). `--tor`, `--i2p`
and `--ygg` are accepted but fail with "not available in cluster mode yet"; the
layer packages stay for a later plan.

### Configuration

```bash
homelab setup                                      # interactive root config wizard (config.yaml + vault)
homelab setup <service>                            # per-service config wizard
homelab setup [service] --json                     # declared vars + secrets (set: bool, never values)
homelab setup [service] --set KEY=VALUE            # write vars without prompting (repeatable)
homelab setup [service] --secrets-stdin            # secrets as {"NAME":"value"} JSON on stdin
```

### Shell completion

```bash
homelab completion bash|zsh|fish|powershell        # generate shell completion scripts
```

### Network extensions (`homelab ext`)

```bash
homelab ext enable <ext>                           # turn an extension on (config.yaml) and deploy it
homelab ext disable <ext>                          # remove it and turn it off
homelab ext list                                   # list extensions and their enabled/disabled status
homelab ext status [ext]                           # state of all or one extension
homelab ext logs [ext]                             # stream logs for all or one extension
```

The only extension in cluster mode is `cf`: cloudflared, a Swarm service
pinned to the core node. Its own command:

```bash
homelab cf status                                  # tunnel status
homelab cf logs                                    # docker service logs of cloudflared
homelab cf route add <service>                     # proxied CNAME → <tunnel>.cfargotunnel.com via the API (enable --cf does it)
homelab cf route rm <service>                      # remove that CNAME (disable --cf does it)
```

## Architecture

### Topology

```
any PC:  homelab (CLI) ──tailnet, cluster-secret auth──▶ agent API (:7443)
                                                         │
minis    [docker: swarm manager, node label homelab.role=core]
         └─ homelab agent (leader)  ◀────────────────────┘
              ├─ tsnet node "homelab"  → owns *.home.<domain> (DNS A → its 100.x)
              ├─ embedded Caddy + caddy-l4 (:443 sites, raw ports, DNS-01 wildcard)
              ├─ Swarm control via /var/run/docker.sock
              └─ attached to overlay "home-services" as homelab-edge
         └─ cloudflared (Swarm service, pinned to core)
         └─ stateful services and shared databases (pinned to core)
laptop / desktop  [docker: swarm worker, label homelab.role=worker, joined over its tailnet IP]
         └─ homelab agent (thin: tsnet node homelab-<host>, exec/logs relay)
         └─ stateless services (preferred here)
```

Swarm control, gossip and the VXLAN data path ride the tailnet: every node's
advertise and data-path address is its Tailscale IPv4. The overlay
`home-services` is attachable, MTU 1230 (VXLAN inside WireGuard), and not
`encrypted` (the tailnet already encrypts between machines).

### The agent (`internal/agent`, `internal/bootstrap`)

`homelab agent` runs in the `homelab-agent` container (`docker run`, restart
`unless-stopped`, docker socket mounted, state volume `homelab-agent` at
`/var/lib/homelab`, shim volume `homelab-shim` at `/hl`; the leader also
mounts `homelab-backups` at `/var/lib/homelab-backups`). It starts at boot
with nobody logged in.

The leader, in order: creates the overlay, installs the shim, starts the edge
(Caddy on the tsnet node; admin API on a unix socket), points the wildcard DNS
A record at the node's tailnet IP via the Cloudflare API, deploys or removes
cloudflared (by `CF_TUNNEL_TOKEN`), brings up every service recorded as
running (shared providers first), starts the rebalancer, and serves the
remote CLI on `:7443`. A worker agent only runs its own tsnet node and serves
`exec`/`logs` for the containers on its node.

### State (in the agent, not on the PC)

```
/var/lib/homelab/                # homelab-agent volume, on the leader
├── vault.bin  vault.key         # AES-256-GCM keyring (secrets), key file 0400
├── cluster.yaml                 # leader address, tsnet hostname, tailnet
├── tsnet/                       # the tailnet node's state
└── state/                       # the config dir: commands run with --config-dir here
    ├── config.yaml              # root vars (DOMAIN, HOME_SUBDOMAIN, …), groups, extensions
    ├── services/<svc>/          # docker-compose.yml, config.yaml, exposure.yaml, bind-mounted files
    ├── caddy/sites/<svc>.conf   # rendered site blocks
    ├── caddy/l4/<svc>.conf      # rendered raw-port servers
    ├── caddy-data/              # certificates and ACME state
    ├── caddy-admin.sock         # the edge's admin API
    └── state/desired.yaml       # which services should run
```

A PC that is only a client keeps `${XDG_CONFIG_HOME:-~/.config}/homelab/client.yaml`
(the agent endpoint) and the cluster secret in its system keyring.

Secrets are **never** written to disk in plain text: in the agent they are in
the vault; a service's env (secrets included) reaches its tasks as a Swarm
secret read by the envshim entrypoint (`internal/envshim`), never in the
service spec (`docker service inspect` shows none).

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
2. Root secrets
3. Service `config.yaml` vars  ← overrides root vars
4. Service secrets

A service's `config.yaml` may also set `placement:` (compose service →
`core`, `worker` or `any`) and `cache_volumes:` (volume keys that are
re-creatable and do not pin).

### Remote CLI (`internal/remote`)

The CLI forwards its argv over a websocket (`GET /v1/run` on the leader's
tsnet listener, `https://homelab.<tailnet>:7443`), authenticated with
HMAC-SHA256 of the cluster secret. The agent runs this same binary against its
state dir, under a pty when the client has a terminal, and streams output and
the exit code back. A major.minor version mismatch between CLI and agent is
refused. Mutating commands take a server-wide lock; `backup`/`restore` run on
to completion if the client disconnects. `--config-dir`/`--config` are refused
in a forwarded argv. `exec` and `logs` for a task on another node are relayed
to that node's agent.

### Services under Swarm (`internal/swarm`)

Each service is a Swarm stack translated from its catalog compose file
(compose-go loader): `deploy.placement` per container, restart policy,
`container_name` → network alias on `home-services`, env through the envshim
secret, named volumes reused by name, bind-mounted files under the service
dir delivered as Swarm configs. Privileged containers, devices, `network_mode`
and `env_file` are unsupported (e.g. `minero`).

Placement is per container: a container with a data volume, a host bind path,
the docker socket, or a shared-database provider role is pinned to
`node.labels.homelab.role == core`. Stateless containers prefer workers: the
constraint is `role == worker` while a worker is Ready, none otherwise, and the
rebalancer force-updates preferred services when a worker returns.

### Network Flow

```
Client (on tailnet)
  → Cloudflare DNS (DNS-only, not proxied)
    → *.home.<domain>  A  <leader tsnet node 100.x.x.x>   (kept by the agent)
      → the agent's tsnet node — unreachable off-tailnet
        → embedded Caddy (TLS termination, wildcard cert)
          → home-services overlay
            → service tasks, on any node
```

### Edge (`internal/edge`)

Caddy runs in-process in the leader agent with caddy-l4 and the Cloudflare DNS
module. Tailnet sites `bind hl-ts/homelab`; raw tcp ports listen on
`hl-ts/homelab:<port>`, udp on `hl-tsu/homelab:<port>`. cloudflared reaches the
plain-HTTP `:80` listener on the overlay as `homelab-edge`. `edge.Assemble`
builds the whole Caddyfile from the embedded global block
(`assets/caddy/Caddyfile`) plus every `caddy/l4/*.conf` and
`caddy/sites/*.conf`; `enable`/`disable`/`reload` adapt it and POST it to the
admin socket. A config Caddy rejects leaves the running one in place, and the
service's sites file and exposure.yaml are restored (`caddy.Snapshot`).

### Service Routing

- `caddy/sites/<svc>.conf` — **one generated file per service** holding the site blocks of every layer it is on, in registry order (ts, cf), each layer's blocks in port order
- `caddy/l4/<svc>.conf` — generated caddy-l4 servers for the service's raw ports (tailnet layer only), inlined in the global `layer4 { }` option
- Catch-all `http://:80` and `http://:8081` sites abort unmatched hosts on the tunnel listeners

**Exposure is stored state**, `services/<svc>/exposure.yaml` (`internal/exposure`),
written only by `enable`/`disable` (and the importer):

```yaml
layers: [ts, cf]
name: dev-gitea        # --name; omitted = service name
ports: [web, ssh]      # --ports; omitted = all declared ports
```

A service on no layer has no exposure.yaml. Discovery reads layers from it;
nothing parses generated Caddy files back. `enable`/`disable` = update the
state, re-render `sites/<svc>.conf` (and `l4/<svc>.conf`) from state +
config.yaml, load the result into the agent's Caddy. `--name`/`--ports` are
remembered across enables; pass them again to change them. `homelab reload
<svc>` re-renders from the state.

**Legacy routes**: a Compose-era config dir kept one file per layer and port
in `caddy/conf.d*/`. The importer (`init --import`) converts them with
`routing.Migrate` (state inferred from the files, written before they are
removed). `enable`/`disable`/`delete`/`reload` refuse a service that still has
legacy files.

### Embedded Catalog (`assets/`)

`assets/assets.go` embeds two filesystems:
- `CoreFS` — `caddy/Caddyfile`, the global block `internal/edge` assembles from
- `CatalogFS` — `services/` tree; `homelab add <name>` copies one entry into the leader's state

**The canonical service catalog is `assets/services/`.** This is the only copy — edit
files directly in `assets/services/<name>/`. The root `services/` directory is a
gitignored export (`make catalog`) for local browsing only.

Each service directory contains:
- `docker-compose.yml` — UI container joins `home-services`; databases use `internal: true` network
- `config.yaml` — vars + secrets + the ports the service exposes (+ optional placement, cache_volumes)
- optional routing overrides (most services need none)

**Port declarations** drive routing. One line per exposed port:

```yaml
ports:
  - 3000          # forgejo.home.<domain>          → container :3000
  - 22:2222/tcp   # forgejo.home.<domain>:22       → container :2222 (raw TCP)
  - vault:80      # vault.home.<domain>            → container :80
  - 53:53/udp     # protocol suffix; both tcp+udp when omitted
```

The token left of the colon decides the form: all digits means "listen on this
port", anything else names a subdomain, and a subdomain *replaces* the service
name rather than prefixing it.

A port with a listen port is **raw**, not HTTP: it gets no site block but a
caddy-l4 server in `caddy/l4/<svc>.conf` (`:22 → forgejo:2222`, plus
`udp/:53 → udp/svc:53` per udp protocol), on the tailnet layer only. A listen
port has no Host header to route on, so it is unique per protocol across the
cluster: `enable` refuses one that another tailnet service already binds, or
one of Caddy's own (tcp 80, 443, 8081, 2019). Declare `/tcp` unless the
protocol really needs udp, or a pointless udp listener is bound.

From these, `homelab enable` generates the site blocks for every layer. Most
services need nothing else. One override exists:

- `caddy.routes.conf` — the *body* of a site block: directives only, no site
  address and no `import wildcard_tls`. For services whose routing is more than
  one host → one upstream (websocket paths, header rewrites, path fan-out).
  `homelab enable` wraps it in whichever site address each layer needs, so
  private and `--cf` get the same route set. Its leading comment block is
  treated as file-level documentation and stripped from generated output.
The original hand-written, symlinked `caddy.conf` / `caddy.cf.conf` scheme is
gone; a catalog test rejects either file.

### Shared services — one instance of each, isolation inside it

A service never bundles its own database, cache or object store. It declares a
dependency under `databases:` in its `config.yaml` and is isolated inside the
one shared instance. `homelab up <svc>` deploys the shared instance if needed,
waits for it, and provisions (`internal/db`, run by the leader over the
overlay); `BuildEnv` injects the connection values through the declaration's
`env:` mapping. Shared providers are pinned to the core node.

| type | shared service / container | per-service isolation | env keys |
|---|---|---|---|
| `postgres` | `postgres` / `homelab-postgres:5432` (VectorChord pg18) | database + owner role, generated password | host port user password database dsn |
| `mariadb` | `mariadb` / `homelab-mariadb:3306` (no consumers today) | database + account | host port user password database dsn |
| `redis` | `redis` / `homelab-redis:6379` | one shared password (`REDIS_PASSWORD`, generated, redis secrets namespace) + its own database number | host port password db dsn (`redis://:<pw>@host:port/<n>`) |
| `s3` | `garage` / `homelab-garage:3900` (path-style, region `garage`) | bucket (`bucket:`, default service name) + an access key granted on that bucket only, kept in the service's secrets namespace | endpoint region bucket access_key secret_key host port |

- Redis numbers come from `<state>/redis-dbs.yaml`: lowest free of
  1..63 (0 is never handed out), stable for the service's lifetime, released
  by `homelab delete`. A service needing several databases declares several
  `redis` entries with distinct `name:` (netbox: `tasks`, `cache`). `db`/`dsn`
  are only injected once a number is allocated.
- Garage needs a cluster layout before it accepts writes; the provisioner
  applies one on first start (`garage layout assign … && layout apply`).
  `homelab delete` removes the service's key; buckets and data stay.
- Secrets with `generate: password|hex32` in a `config.yaml` are minted into
  the vault by `up`/`setup` and never prompted for.
- `TestCatalogServices_NoBundledDatabases` fails if any catalog compose file
  runs a postgres/mysql/mariadb/mongo/redis/valkey/keydb/minio/garage image
  outside that shared service's own directory.

### Desired state (`internal/desired`)

`<state>/state/desired.yaml` (`services: {name: running|stopped}`). Lifecycle
commands record intent before acting: `up/start/restart/update` → running,
`stop/down/disable --stop` → stopped, `delete/prune` → removed; starting a
service's shared database records that database running too. Recording only
warns on failure. When the leader agent starts it deploys every service
recorded as running, shared providers first.

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

`--group` works on `up`, `down`, `start`, `stop`, `restart`, `pull`, `update`,
`backup` and `prune`. `enable` and `disable` operate on one service; there
`--all` means every layer, not every service.

### Go package layout

| Package | Role |
|---|---|
| `assets/` | `//go:embed caddy` (CoreFS: the Caddyfile global block) + `//go:embed services` (CatalogFS) |
| `cmd/` | Cobra command definitions; forwarding to the agent (`remote.go`), exec/logs relay (`relay.go`) |
| `internal/agent` | The `homelab agent` process: leader (edge, DNS, cloudflared, bring-up, rebalancer, remote CLI) or worker |
| `internal/agentstate` | Agent state paths (`/var/lib/homelab`), cluster.yaml, backups volume |
| `internal/bootstrap` | `init`/`join`: swarm init/join on the tailnet IP, agent container spec, import into the agent |
| `internal/importer` | Compose-era config dir → leader state tree + vault secrets (`init --import`) |
| `internal/remote` | Remote CLI protocol, client and server (auth, lock, detach, version check) |
| `internal/edge` | In-process Caddy on the agent's tsnet node; Caddyfile assembly, adapt, load via admin socket |
| `internal/swarm` | Compose → Swarm translation, Deployer (up/down/restart/pull/remove/status), placement, rebalancer, cloudflared |
| `internal/envshim` | A container's env through a Swarm secret instead of the service spec |
| `internal/vault` | AES-256-GCM file keyring for the agent |
| `internal/desired` | Desired state (`state/desired.yaml`) |
| `internal/backup` | Backup/restore engine (volumes, DB dumps, config), manifest, `--fetch` stream |
| `internal/db` | Shared database provisioning (postgres, mariadb, redis, garage) |
| `internal/config` | Config dir resolution, YAML schema types, `BuildEnv()` |
| `internal/secrets` | Secrets manager over the system keyring (CLI host) or the vault (agent) |
| `internal/service` | `Discover()` filesystem scan; `DiscoverWithDocker()` enriches with SDK data |
| `internal/docker` | Docker SDK client — read-only (ContainerList, ContainerInspect) |
| `internal/run` | `Commander` — docker CLI shell-outs for DB dumps/restores, volume helpers, network checks |
| `internal/exposure` | `services/<svc>/exposure.yaml` — the stored exposure state |
| `internal/configgen` | Renders site blocks per layer and the per-service `caddy/sites/<svc>.conf` |
| `internal/routing` | State → sites file (`Apply`/`Sync`), layer Configure/Teardown, legacy migration for the importer |
| `internal/caddy` | `Enable`/`Disable`/`ReloadService`/`Reload`/`Validate` against the edge, with snapshot/rollback of a service's sites file + exposure.yaml |
| `internal/network` | Layer interface and registry (`layers.New`: ts, cf; tor/i2p/ygg unregistered) |
| `internal/diagnostics` | `doctor`/`status --check` checks |
| `internal/scaffold` | `//go:embed templates/*`; `Render()` + `Write()` for new-service boilerplate |
| `internal/actions` | Registry of every user-facing action (argv, inputs, danger, icon, availability) the TUI and GUI render from |
| `internal/tui/dashboard` | Bubble Tea fullscreen service browser |
| `internal/tui/logs` | Bubble Tea streaming log viewer |
| `internal/tui/wizard` | Multi-step new-service scaffold wizard |
| `internal/tui/spinner` | Goroutine spinner (TTY-aware) |
| `internal/gui` | giu (Dear ImGui) desktop front end, `homelab --gui`; in the default build, left out with `-tags nogui` |
| `internal/tui/styles` | Lipgloss Tokyo Night palette, shared across TUI and plain output |

### Front-end action registry (`internal/actions`)

The TUI and GUI do nothing themselves: each button is a CLI invocation of this
binary, described once in `internal/actions` (`actions.For(target)` gives the
applicable actions in group order; `Action.Build` gives the argv). The "core"
scope is the edge row: the no-service forms of `logs`, `doctor`, `reload`,
plus `validate` and `caddy status|logs`. Parity rule:
`cmd/actions_parity_test.go` fails when a runnable command or a flag has no
action listing it in `Commands`, unless it is on that test's commented
exclusion list — so a new CLI command or flag needs an action (or a reason).
Every action's argv is also parsed through `rootCmd` in tests.

### Key design decisions

- **One backend**: Swarm only — no Compose mode alongside. Lifecycle goes through the Swarm API (`internal/swarm`); the Docker SDK is used read-only for status; the docker CLI only for DB dumps, volume helpers and `docker service logs`.
- **minis is the single manager**: no HA; all state, data volumes, host bind paths, the docker socket and shared databases pin to the core node. Nothing permanent on laptop/desktop.
- **No secrets on disk in plain text**: the vault in the agent volume (root-only key file, no unlock prompt, Swarm autolock off); service env through a Swarm secret, never the spec, argv or labels.
- **Tests never reach the real Docker daemon or a tailnet**: packages that touch Docker take an interface with fakes (`swarm/swarmtest`), and every test package that can shell out to docker has a `TestMain` pointing `DOCKER_HOST` at a nonexistent socket. New test packages that touch docker need the same guard.
- **Front ends run the CLI**: the TUI dashboard and the GUI never reimplement an action — each button/key execs this binary (`selfCLI`) with the matching command, so they cannot drift from the CLI (and are forwarded to the agent like any other invocation).
- **Spinner + captured output**: Caddy reload output is captured in a `bytes.Buffer` Commander while the spinner runs; buffer is printed only on error.

## Adding a New Service

```bash
homelab new myapp             # interactive scaffold wizard
homelab setup myapp           # configure vars and secrets
homelab up myapp              # deploy on Swarm
homelab enable myapp          # expose on private tailnet
```

Then add the service to the embedded catalog by creating `assets/services/myapp/`.
Run `make catalog` to export it to `services/myapp/` for local reference.
See `docs/adding-a-service.md`.

## Docs

- `docs/architecture.md` — design decisions, network diagrams
- `docs/tailscale-setup.md` — auth key generation, ACL config
- `docs/cloudflare-setup.md` — API token scoping, CNAME setup
- `docs/adding-a-service.md` — step-by-step new service guide
- `docs/superpowers/specs/2026-11-04-swarm-cluster-design.md` — the Swarm cluster design
