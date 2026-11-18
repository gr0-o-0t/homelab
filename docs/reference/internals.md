# Internals reference

Moved out of CLAUDE.md. Per-package behaviour and invariants; read the section for the package you are changing.

## Topology

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

## The agent (`internal/agent`, `internal/bootstrap`)

`homelab agent` runs in the `homelab-agent` container (`docker run`, restart
`unless-stopped`, docker socket mounted, state volume `homelab-agent` at
`/var/lib/homelab`, shim volume `homelab-shim` at `/hl`; the leader also
mounts `homelab-backups` at `/var/lib/homelab-backups`). It starts at boot
with nobody logged in.

The leader, in order: creates the overlay, lowers Swarm's task history to one finished task per service (so stopped and replaced task containers do not pile up; `init` sets it too), installs the shim, starts the edge
(Caddy on the tsnet node; admin API on a unix socket), points the wildcard DNS
A record at the node's tailnet IP via the Cloudflare API, deploys or removes
cloudflared (by `CF_TUNNEL_TOKEN`), serves the remote CLI on `:7443`, then
deploys every service recorded as running that has **no** Swarm services yet
(shared providers first), each under a 10-minute timeout (a stalled pull leaves
it undeployed, logged; `homelab up` retries) and holding the remote server's
lock one service at a time, so a waiting mutating command runs between services
(reads such as `status` never wait), and starts the rebalancer. A
service already in Swarm is left as Swarm keeps it — no pull, no redeploy
(R53) — except that a container left at zero replicas (a backup cut short) is
scaled back to one. A worker agent only runs its own tsnet node and serves
`exec` for the containers on its node (plus `version` and `agent
self-replace`).

## State (in the agent, not on the PC)

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

## Config Schema (`internal/config`)

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

## Remote CLI (`internal/remote`)

The CLI forwards its argv over a websocket (`GET /v1/run` on the leader's
tsnet listener, `https://homelab.<tailnet>:7443`), authenticated with
HMAC-SHA256 of the cluster secret. The agent runs this same binary against its
state dir, under a pty when the client has a terminal, and streams output and
the exit code back. A major.minor version mismatch between CLI and agent is
refused, except for `upgrade` and `version` (and `agent self-replace` on a
worker agent). Mutating commands take a server-wide lock; `backup`/`restore`/`upgrade` run on
to completion if the client disconnects. A command waiting for the lock
holds its client's stdin back (back-pressure), never cuts it; a stdin cut
short once it runs (client gone, or left unread for two ping intervals)
stops a non-detached command instead of ending its input. At most 32 sessions run at once
(`MaxSessions`; front-end polling and each `logs -f` hold one).
`--config-dir`/`--config` are refused in a forwarded argv, except after
`exec`'s service name, where they belong to the container's command
(`remote.ExecValueFlags` lets the check find the service name; a cmd test
pins it to exec's flag set). `exec` for a task on another node is relayed to
that node's agent; `logs` reads every node's tasks on the leader.

## Services under Swarm (`internal/swarm`)

Each service is a Swarm stack translated from its catalog compose file
(compose-go loader): `deploy.placement` per container, restart policy,
`container_name` → network alias on `home-services`, env through the envshim
secret, named volumes reused by name, bind-mounted files under the service
dir delivered as Swarm configs. Privileged containers, devices, `network_mode`
and `env_file` are unsupported (e.g. `minero`).

**Local images.** A compose service with `pull_policy: never` has a local image
(label `homelab.image-local`), built on a PC and in no registry. It is never
pulled (`up`/`update`/`pull` and the boot bring-up skip it; `pull` prints
"<ref> skipped (local image)"), must exist in the leader's engine, and stays
a bare tag in the spec. Placement: `core` unless the service's `config.yaml`
`placement:` says otherwise (the image is then the owner's business on that
node). Its identity (label `homelab.image-identity`) is a hash of the image's
RootFS layers + Created time, not its ID (minis runs the containerd image
store, a PC usually overlay2: the same image has different IDs). `update` rolls
the tasks when the identity on the leader differs from the one the service was
deployed with. A rebuild that changes only image metadata (ENV, CMD, labels:
same layers, same Created) keeps the identity, so `update` neither ships nor
rolls it; any code change adds a layer and does. `up`/`update` run from a PC that has the image ship it first:
the CLI asks the leader `images <svc> --json`, then streams `docker save` to
the hidden `images load` (a mutating, locked command); `up` ships only when the
leader lacks the image (R53), `update` when the identity differs. No local Docker
daemon: shipping is skipped silently (a client-only PC), and the leader then
reports the missing image. A failed image check warns and the command is still
forwarded. Because the tag is bare, R53 holds for local images only against
homelab-triggered rolls: a manual `docker build` on the leader retags it and
goes live at the next task restart or reschedule. `add`/`up`/`update` warn on a local
image with a namespace or registry host (a registry image of that name would
replace it at task start): give local images an unqualified name.

`homelab add <dir>` (local half) reads the folder: `docker-compose.yml` and
`config.yaml` required; regular files and directories (bind-mounted config)
come along; refused: symlinks, dotfiles, a top-level `exposure.yaml`, a file over
500 KiB (`swarm.MaxConfigSize`), a folder over 10 MiB, and the names
postgres/mariadb/redis/garage (a custom service must not act as a shared
provider). The service name is the folder's base name. It streams a tar to the
hidden leader command `files install <svc>`, which refuses an existing service
(changes go through `edit`), validates as `edit` does and writes atomically.
A bare name that is both a catalog service and a local folder is refused as
ambiguous (use `./<name>`).

Placement is per container: a container with a data volume, a host bind path,
the docker socket, or a shared-database provider role is pinned to
`node.labels.homelab.role == core`. Stateless containers prefer workers: the
constraint is `role == worker` while a worker is Ready, none otherwise, and the
rebalancer force-updates preferred services when a worker returns.

A container with `deploy.mode: global` is a Swarm global service: one task on
every node, no constraint, role `any` (its host paths and volumes are per node).
`Down` removes it (Swarm can neither scale it to zero nor change its mode) and
`Up` recreates it; `Restart` of a stopped stack therefore goes through `Up`.
`Replicas`/`WaitStopped` skip it, so a backup stops the rest and leaves it running.

## Network Flow

```
Client (on tailnet)
  → Cloudflare DNS (DNS-only, not proxied)
    → *.home.<domain>  A  <leader tsnet node 100.x.x.x>   (kept by the agent)
      → the agent's tsnet node — unreachable off-tailnet
        → embedded Caddy (TLS termination, wildcard cert)
          → home-services overlay
            → service tasks, on any node
```

## Edge (`internal/edge`)

Caddy runs in-process in the leader agent with caddy-l4 and the Cloudflare DNS
module. Tailnet sites `bind hl-ts/homelab`; raw tcp ports listen on
`hl-ts/homelab:<port>`, udp on `hl-tsu/homelab:<port>`. cloudflared reaches the
plain-HTTP `:80` listener on the overlay as `homelab-edge`. `edge.Assemble`
builds the whole Caddyfile from the embedded global block
(`assets/caddy/Caddyfile`) plus every `caddy/l4/*.conf` and
`caddy/sites/*.conf`; `enable`/`disable`/`reload` adapt it and POST it to the
admin socket. A config Caddy rejects leaves the running one in place, and the
service's sites file and exposure.yaml are restored (`caddy.Snapshot`).

## Service Routing

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

## Embedded Catalog (`assets/`)

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

## Shared services — one instance of each, isolation inside it

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
- `homelab delete` deprovisions access but keeps data: a postgres owner role
  becomes `NOLOGIN` with no password (it still owns the database and its
  tables, so a reinstall under the same name picks them up), a read-only
  role and a MariaDB account are dropped, the Garage key is deleted (bucket
  and objects stay), and redis numbers are released.
- Two services whose names map to the same database password key
  (`DBPasswordKey`, e.g. `my-app` and `my_app`) would share a role password,
  so `new`, `add` and `files install` refuse the second one.
- `readonly: true` on a `postgres` entry (with `database:` naming another
  service's database and `user:`) gives the consumer a read-only login role on
  that database: it must already exist (else "`homelab up <owner>` first");
  the role gets CONNECT, USAGE on public, SELECT on tables and sequences, and
  default SELECT for tables the owner role creates later; never CREATE or
  ownership. Its password is the consumer's generated `DB_PASSWORD_<SVC>` in
  the root secrets namespace — the same one as the consumer's own postgres
  role if it has one — re-synced by `setup`; `delete` of the consumer drops the
  role (kept when another installed service declares the same read-only user);
  it is not backed up separately. A `pg_` user is refused (reserved by
  postgres), as is a read-only user another installed service already
  declares. An existing role of that name must be a plain login role (no
  superuser/createrole/createdb/replication/bypassrls, owns no database,
  member of no role, no members of its own, owns nothing in the target db) and
  not the database owner's own role, else provisioning refuses.
- Garage needs a cluster layout before it accepts writes; the provisioner
  applies one on first start (`garage layout assign … && layout apply`).
  `homelab delete` removes the service's key; buckets and data stay.
- Secrets with `generate: password|hex32` in a `config.yaml` are minted into
  the vault by `up`/`setup` and never prompted for.
- `TestCatalogServices_NoBundledDatabases` fails if any catalog compose file
  runs a postgres/mysql/mariadb/mongo/redis/valkey/keydb/minio/garage image
  outside that shared service's own directory.

## Desired state (`internal/desired`)

`<state>/state/desired.yaml` (`services: {name: running|stopped}`). Lifecycle
commands record intent before acting: `up/start/restart/update` → running,
`stop/down/disable --stop` → stopped, `delete/prune` → removed; starting a
service's shared database records that database running too. Recording only
warns on failure. When the leader agent starts it deploys every service
recorded as running that is not in Swarm yet, shared providers first (see
The agent).

## Config Schema — Groups

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

## Go package layout

| Package | Role |
|---|---|
| `assets/` | `//go:embed caddy` (CoreFS: the Caddyfile global block) + `//go:embed services` (CatalogFS) |
| `cmd/` | Cobra command definitions; forwarding to the agent (`remote.go`), exec/logs relay (`relay.go`) |
| `internal/agent` | The `homelab agent` process: leader (edge, DNS, cloudflared, bring-up, rebalancer, remote CLI) or worker |
| `internal/agentstate` | Agent state paths (`/var/lib/homelab`), cluster.yaml, backups volume |
| `internal/bootstrap` | `init`/`join`: swarm init/join on the tailnet IP, agent container spec, import into the agent; `upgrade` rollout and agent self-replacement (helper container) |
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
| `internal/service` | `Discover()` filesystem scan; `DiscoverWith(StateSource)` adds each service's Swarm tasks, cluster-wide (`swarm.Deployer.ServiceContainers`) |
| `internal/docker` | Docker SDK client — read-only (container state, ping, networks) |
| `internal/run` | `Commander` — docker CLI shell-outs for DB dumps/restores, volume helpers, network checks |
| `internal/exposure` | `services/<svc>/exposure.yaml` — the stored exposure state |
| `internal/fsutil` | `WriteFile`: the atomic, private (0600) write every state file and the vault use |
| `internal/configgen` | Renders site blocks per layer and the per-service `caddy/sites/<svc>.conf` |
| `internal/routing` | State → sites file (`Apply`/`Sync`), layer Configure/Teardown, legacy migration for the importer |
| `internal/caddy` | `Enable`/`Disable`/`ReloadService`/`Reload`/`Validate` against the edge, with snapshot/rollback of a service's sites file + exposure.yaml |
| `internal/network` | Layer interface and registry (`layers.New`: ts, cf; tor/i2p/ygg unregistered) |
| `internal/diagnostics` | `doctor`/`status --check` checks |
| `internal/scaffold` | `//go:embed templates/*`; `Render()` + `Write()` for new-service boilerplate |
| `internal/actions` | Registry of every user-facing action (argv, inputs, danger, icon, availability) the TUI and GUI render from |
| `internal/clistate` | What the front ends display, as `homelab status --json` reports it; the CLI-backed `Source` they read through |
| `internal/tui/dashboard` | Bubble Tea fullscreen service browser |
| `internal/tui/logs` | Bubble Tea streaming log viewer |
| `internal/tui/wizard` | Multi-step new-service scaffold wizard |
| `internal/tui/spinner` | Goroutine spinner (TTY-aware) |
| `internal/gui` | giu (Dear ImGui) desktop front end, `homelab --gui`; in the default build, left out with `-tags nogui` |
| `internal/tui/styles` | Lipgloss Tokyo Night palette, shared across TUI and plain output |

## Front-end action registry (`internal/actions`)

The TUI and GUI do nothing themselves: each button is a CLI invocation of this
binary, described once in `internal/actions` (`actions.For(target)` gives the
applicable actions in group order; `Action.Build` gives the argv). The "core"
scope is the edge row: the no-service forms of `logs`, `doctor`, `reload`,
plus `validate` and `caddy status|logs`. Parity rule:
`cmd/actions_parity_test.go` fails when a runnable command or a flag has no
action listing it in `Commands`, unless it is on that test's commented
exclusion list — so a new CLI command or flag needs an action (or a reason).
Every action's argv is also parsed through `rootCmd` in tests.
