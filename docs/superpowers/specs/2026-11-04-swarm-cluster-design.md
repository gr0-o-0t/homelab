# homelab on Docker Swarm — one agent per PC

Status: draft for approval · 2026-11-04

## Intent

The homelab runs on one always-on mini PC (`minis`). A laptop (500 GB SSD,
work machine, on and off constantly) and a desktop (1 TB HDD, on sometimes)
sit idle. The owner wants all three to act as one homelab:

- one name space, `*.home.gr00t.fun`, whichever PC a service runs on — today
  each PC would need its own home subdomain;
- light and compute-heavy work (immich ML, builds, local AI models) pushed to
  the laptop/desktop while they are on, so minis stays free;
- `homelab add <svc>` / `enable` / `up` feel exactly as they do now;
- control the homelab from whichever PC they are sitting at, never SSHing into
  minis;
- the fewest possible moving parts and annoyances. The owner is not a
  Kubernetes user and does not want to install or operate one.

The single feature they care most about: switch a service's exposure between
network layers with one flag, with HTTP *and* raw TCP/UDP routed to the right
place, TLS everywhere it applies.

## Goals

1. **Docker Compose mode is replaced entirely** by Docker Swarm, driven by the
   `homelab` binary. No dual mode.
2. **One container per PC**: the `homelab` agent. Nothing else is installed on
   a host beyond Docker (already present) and the host's existing Tailscale.
3. **One binary, two roles**: `homelab agent` (inside the container) and the
   CLI (on any PC) talking to an agent over the tailnet.
4. **Agents discover each other** with a shared secret and form the cluster
   with no manual `docker swarm join` by the user.
5. **Infrastructure plumbing is in-process Go libraries**, not containers:
   embedded Caddy + caddy-l4 (HTTP sites, raw ports, wildcard cert via
   Cloudflare DNS-01) and Tailscale via `tsnet`. cloudflared stays a container.
6. **Exposure layers on day one: tailnet and Cloudflare.** Tor, I2P and
   Yggdrasil come later and must slot into the same `enable --<layer>` switch.
7. **Encryption between machines**: all cross-PC service traffic is encrypted
   (Swarm overlay with `encrypted` option; Swarm control traffic is TLS by
   default). Devices → homelab stays HTTPS with the wildcard cert.
8. **Placement**: stateless services prefer the laptop/desktop and fall back
   to minis; stateful services are pinned to minis.

## Non-goals

- High availability. If minis is down the homelab is down, as today. The
  part-time PCs cannot provide quorum or hold data.
- Replicated storage. Nothing permanent lives on the laptop or desktop.
- GPU scheduling. All three PCs have integrated GPUs only; iGPU acceleration
  (Vulkan llama.cpp, OpenVINO for immich) is a later nice-to-have.
- Rewriting the apps. gitea, immich, postgres, redis, garage etc. stay
  container images. "Integrate into one binary" applies to plumbing only.
- Per-service mutual TLS / a service mesh.
- Kubernetes, k3s, Nomad.
- Keeping the Docker Compose backend.
- Tor, I2P, Yggdrasil in the first version.

## Anti-goals (a failure even if it works)

- The owner has to SSH into minis or run `docker` commands by hand for
  routine work.
- The laptop sleeping or leaving the house causes errors, alerts or broken
  routes for anything other than the work that was running on it.
- Data ending up on the laptop or desktop.
- Secrets written to disk in plain text (today's rule: never on disk).
- A homelab that only comes up after the owner logs in to minis.

## Constraints and facts

- Hosts: minis (Alpine edge, Ryzen 9 9955HX, 30 GB RAM, 936 GB ext4, always
  on, home dir on a login-mounted filesystem); laptop (this machine, Linux,
  500 GB SSD); desktop (1 TB HDD). All are already on the owner's tailnet with
  host Tailscale; tailnet IPs are stable.
- Today on minis: homelab v0.4.3, services gitea, immich, vaultwarden,
  postgres, redis, searxng, glance, grafana, beszel-hub — all on the tailnet
  layer only. Data is in Docker named volumes (`/var/lib/docker`, not the home
  dir). Config and generated Caddy files are under `~/.config/homelab`.
- The catalog (`assets/services/`, 60 services) is compose files plus
  `config.yaml`. Every one sets `container_name` (Swarm ignores it); services
  address each other by those names (`homelab-postgres`, `gitea`). Only
  `minero` uses `privileged`/`devices`. None uses `build:` or `network_mode`.
- Uncommitted work in the repo adds caddy-l4 to the Caddy build and renders
  `caddy/l4/<svc>.conf` per service (raw ports, listen-port conflict check).
  Its rendering and conflict logic carry over to the embedded Caddy.
- Raw ports have no hostname on the wire (SSH has no SNI), and every
  `*.home` name resolves to one tailnet IP, so a raw listen port belongs to one
  service cluster-wide. Gitea's SSH is on 2222 (`2222:2222/tcp`).
- Migration: all at once; a long downtime is acceptable.

## Decided (by the owner)

- Replace Compose entirely; build on **Docker Swarm**; the agent sits on top
  of Swarm rather than doing its own scheduling or networking.
- One `homelab` agent container per PC; same binary is the CLI.
- Shared-secret discovery; control from any PC over the network.
- Plumbing embedded as Go libraries; cloudflared remains a container.
- Tailnet + Cloudflare first; Tor/I2P/Ygg later.
- Encryption between machines, not just at the edge.
- Light and heavy work go to laptop/desktop when on; minis stays the core.
- Migrate all at once.

## Design (calls made by Claude, marked)

### Topology

```
any PC:  homelab (CLI) ──tailnet, shared-secret auth──▶ agent API
                                                         │
minis    [docker: swarm manager]                         │
         └─ homelab agent (leader)  ◀────────────────────┘
              ├─ tsnet node "homelab"  → owns *.home.gr00t.fun (DNS A → its 100.x)
              ├─ embedded Caddy + caddy-l4 (:443 sites, raw ports, DNS-01 wildcard)
              ├─ Swarm control via /var/run/docker.sock
              └─ attached to overlay "home-services" (encrypted)
         └─ cloudflared (swarm service, pinned to minis)
         └─ stateful services (pinned to minis)
laptop / desktop  [docker: swarm worker, joined over host tailnet IP]
         └─ homelab agent (thin: health, logs/exec relay, join/leave)
         └─ stateless services (preferred here)
```

### Agent and CLI

- `homelab agent` runs as a Swarm-independent container (`docker run`,
  `--restart unless-stopped`, docker socket mounted, state volume
  `homelab-agent`). Installing it is one command the CLI prints/runs:
  `homelab join` (first PC: `homelab init`). *Claude's call.*
- The agent on the swarm manager is the **leader**: it holds the authoritative
  state (installed services, exposure, desired state) in its volume and runs
  Caddy/tsnet/cloudflared. Other agents forward writes to it. *Claude's call:*
  no Raft — minis is the single manager by design (non-goal: HA).
- The CLI finds an agent by tailnet name (`homelab` via MagicDNS) and
  authenticates with the cluster secret, stored in the CLI host's keyring
  (`homelab login`). Every existing command becomes an API call; the TUI and
  GUI keep running the CLI, so they need no change of model.
- `homelab exec` / `logs` on a service relay to the agent on the node running
  the task (Swarm's service logs API covers logs; exec needs the node-local
  agent).

### Discovery

- `homelab init` on minis: `docker swarm init --advertise-addr <host tailnet
  IP>`, generates the cluster secret, starts the leader agent.
- `homelab join` on another PC: given the secret (pasted once, or read from
  the keyring), it finds the leader over the tailnet, receives the Swarm worker
  join token from the leader API, joins Swarm with `--advertise-addr <its
  tailnet IP>`, starts its agent. *Claude's call:* discovery is "the leader is
  reachable at tailnet name `homelab`" rather than LAN broadcast; the tailnet
  is the network all PCs share wherever the laptop is.

### Services under Swarm

- Each service is a Swarm stack deployed from its catalog compose file. The
  agent rewrites the file before `docker stack deploy`-equivalent API calls:
  - adds `deploy.placement` (see Placement) and `deploy.restart_policy`;
  - converts `container_name: X` into a network alias `X` on
    `home-services`, so `homelab-postgres`, `gitea` etc. keep resolving;
  - injects env and secrets (below);
  - binds named volumes as external volumes so existing data is reused.
- `minero` (privileged/devices) is marked unsupported until Swarm support
  for it is verified. *Claude's call.*
- Lifecycle verbs map: `up` → deploy/scale to 1, `down`/`stop` → scale 0
  (exposure kept, as today), `restart` → force update, `delete` → remove
  stack + exposure + config, `pull`/`update` → update image.

### Placement

- Node labels: minis `homelab.role=core`; laptop/desktop
  `homelab.role=worker`. *Claude's call.*
- Placement is decided **per container**, not per stack: immich-ml can run on
  a worker while immich-server (uploads) stays on minis. *Claude's call.*
- A container is pinned to core (`node.labels.homelab.role == core`) when it
  has a data volume, a host bind path (`${MEDIA_PATH}` etc.), the Docker
  socket, or a `databases:` provider role (postgres, redis, mariadb, garage).
  Host paths and the socket are assumed to exist on minis only. *Claude's
  call.*
- Volumes the catalog marks `cache: true` in `config.yaml` (model caches,
  build caches) are not data: they may be recreated empty on whichever node
  runs the task, and do not pin. The catalog is updated to mark them
  (immich-ml's `model-cache` first). *Claude's call.*
- The first services meant to move are immich-ml, build runners and local AI
  model servers; anything else stateless follows the same rule.
- Stateless services prefer workers. Swarm has no soft affinity, so the
  agent switches a hard constraint: `role==worker` while any worker is
  Ready, none (so minis) otherwise (see plan Tasks 4, 8, 9). When a worker
  leaves, Swarm reschedules onto minis. Swarm never moves tasks back on its
  own, so the leader watches node events and force-updates preferred services
  when a worker returns (debounced, so a laptop flapping does not churn).
- An optional `placement:` key in a service's `config.yaml` overrides
  (`core`, `worker`, `any`). *Claude's call.*

### Networking and exposure

- One encrypted overlay `home-services` (`--opt encrypted`, attachable). The
  leader agent attaches to it so Caddy reaches every service by alias, on any
  node.
- `enable <svc>` (tailnet): renders the service's site blocks and l4 servers
  (current `configgen` + the caddy-l4 rendering) and loads them into the
  embedded Caddy via its config API — no Caddyfile reload, no `caddy validate`
  shell-out; an invalid config is rejected by the load call and the previous
  config stays (replaces `Snapshot`/rollback).
- Caddy listens on the tsnet node's listeners (tailnet only), not on host
  ports. Wildcard cert via the embedded `caddy-dns/cloudflare`.
- `enable --cf`: as today — cloudflared (a Swarm service pinned to minis)
  points at the leader agent's plain-HTTP cf listener on the overlay; the CNAME
  is created via the Cloudflare API. Raw ports are not carried over Cloudflare.
- Raw ports: one service per listen port/protocol cluster-wide; the existing
  conflict check is kept and runs on the leader's state.
- Exposure state (`exposure.yaml` today) moves into the leader's state store
  with the same fields (layers, name, ports).
- Layer interface stays pluggable so Tor/I2P/Ygg can return as either embedded
  libraries or containers later.

### Secrets and config

- **Threat model (owner's decision):** the leader's own secrets (Cloudflare
  token, Tailscale auth key and tsnet node state, shared-database admin
  credentials, the cluster secret) are stored encrypted in the agent's state
  volume on minis, with the encryption key in a root-only file on minis. Root
  on minis can read them; nothing is ever in plain text in config files, the
  repo or a stack spec. No unlock prompt at boot; Swarm autolock is off.
- **The laptop joins as a worker (owner's decision)**, accepting that the
  cluster can run root-equivalent containers on it. It runs rootful Docker
  Engine on Linux Mint, Swarm inactive, with unrelated containers that must
  keep running untouched after `swarm join`.

- Service secrets and generated passwords become Swarm secrets (encrypted in
  the Raft store, mounted as files in tasks). Images that only read env vars
  get them through a tiny entrypoint shim or `*_FILE` variants; the agent
  chooses per service. *Builder to decide the mechanism; constraint: never in
  plain text on disk or in the stack spec.*
- Root config (`DOMAIN`, `HOME_SUBDOMAIN`, CF token, TS auth key) lives in the
  leader's state; secrets among them as Swarm secrets.
- `homelab setup` keeps its interactive flow, writing through the API.

### Bind-mounted config files

- Services that bind-mount config (glance `./config`, grafana
  `./provisioning`, garage, litellm, bifrost) get those files stored in the
  leader's state and delivered as Swarm configs (directories flattened into one
  config per file). Editing is `homelab edit <svc> <file>` from any PC: opens
  `$EDITOR` locally, writes through the API, rotates the config and redeploys.
  Migration imports the current files. *Claude's call.*

### Shared databases

- postgres / redis / garage / mariadb remain single shared instances, pinned
  to minis, provisioned by the existing `internal/db` logic run by the leader
  agent (it reaches them over the overlay).

### Backup / restore

- Run on the leader, which can only snapshot volumes on minis — which is
  where all volumes are by placement. Same manifest format; destination path
  on minis or streamed to the CLI host. *Builder's call on streaming.*

### What is removed

- Compose shell-out (`internal/run` lifecycle), session mode
  (`internal/session`, `homelab session`), restart-policy records,
  bind-mounting files from the login-mounted home, the core compose stack,
  `Dockerfile.caddy`, the tailscale sidecar container, legacy `conf.d*`
  migration.

### Migration (all at once)

1. Back up everything on minis with the current binary (`homelab backup
   --all`) and copy `~/.config/homelab` aside.
2. `homelab down -a` and core down.
3. Install the new binary; `homelab init` on minis imports
   `~/.config/homelab` (root config, services' config.yaml, exposure.yaml,
   keyring secrets → leader state + Swarm secrets) and reuses the existing
   named volumes.
4. `homelab up -a`; re-enable exposure from imported state.
5. Verify every service's URL and gitea SSH on `:2222`; then `homelab join`
   on laptop/desktop.
Rollback: `docker swarm leave --force`, reinstall v0.4.3, `homelab up -a`.

### Distribution and upgrades

- The agent image is built by the existing release pipeline and pushed to a
  public registry (GHCR); every PC pulls it. CLI and agent share a version;
  the API carries that version and the CLI refuses a mismatched major/minor
  with an "upgrade the agent / CLI" message. `homelab upgrade` rolls the
  agents (workers first, leader last). *Claude's call.*

## Left to the builder

- Agent API shape (gRPC vs HTTP/JSON), auth handshake details.
- Leader state format/store (files in a volume, bbolt, …).
- How compose files are translated (Docker's compose loader vs own minimal
  translator).
- Debounce policy for rebalancing.
- Test strategy; tests must still never touch the real Docker daemon.
- Ordering of the work into shippable increments.

## Risks to verify early

- Swarm DNS: aliases from `container_name` resolve across stacks on an
  external overlay.
- Embedded Caddy + caddy-l4 + `tsnet` listeners in one process (Caddy binding
  on a tsnet listener rather than an OS socket).
- Swarm over host tailnet IPs with the laptop roaming and sleeping (worker
  `Down` handling, overlay VXLAN over WireGuard MTU).
- Images with no `*_FILE` support needing secrets in env.

### Spike findings (2026-11-05)

**S1 - Embedded edge: YES (localhost stand-in for tsnet).** Verified with
`caddy/v2 v2.11.9` + `modules/standard`, `mholt/caddy-l4 v0.1.2`,
`caddy-dns/cloudflare v0.2.4` (Go 1.26). A custom network registered with
`caddy.RegisterNetwork` is honoured by BOTH the HTTP `bind` directive and
caddy-l4 `listen` addresses; no fallback needed. An HTTP request through the
site and a raw TCP echo through the layer4 server both worked. A real tsnet
listener (`tsnet.Server.Listen`) replaces the `net.Listen` in the callback;
caddy-tailscale itself was not needed or tested.

```go
caddy.RegisterNetwork("hl-ts", func(ctx context.Context, network, host, portRange string,
	portOffset uint, cfg net.ListenConfig) (any, error) {
	// host is whatever follows "hl-ts/" in bind/listen; portRange is the port
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", portRange)) // tsnet: srv.Listen("tcp", ":"+portRange)
})
cfgJSON, warns, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(cf), nil)
err = caddy.Load(cfgJSON, true) // forceReload
```

```caddyfile
{
	auto_https off
	admin off
	layer4 {
		hl-ts/x:18222 {
			route { proxy 127.0.0.1:9999 }
		}
	}
}
http://:18080 {
	bind hl-ts/x
	reverse_proxy 127.0.0.1:8000
}
```

Notes: the callback is invoked once per listen address (host=`x`,
port=`18222` / `18080`). `caddy.Load` with an invalid config (unknown handler
module) returns an error ("loading http app module: ... unknown module") and
the previous config keeps serving (HTTP 200 afterwards). Imports needed:
`caddyconfig/caddyfile`, `modules/standard`, caddy-l4, caddy-dns/cloudflare.
The module graph is large (go mod tidy pulls cloud SDKs via caddy deps).

**S2 - Swarm naming: YES.** Single-node swarm (`docker swarm init
--advertise-addr 127.0.0.1` accepted), overlay created with
`docker network create -d overlay --attachable --opt encrypted --opt
com.docker.network.driver.mtu=1230`. Stack A `sa` service `postgres`
(postgres:17-alpine) with `networks: {<net>: {aliases: [homelab-postgres]}}`;
stack B `sb` service `client` (alpine). `nc -z <name> 5432` succeeded from
both stack B's task and a plain `docker run --rm --network <net> alpine` for
ALL of: `homelab-postgres` (alias), `postgres` (short service name),
`sa_postgres` (stack-qualified service name), `tasks.sa_postgres`.
Caveat: a local bridge network named `home-services` already exists on this
machine (the current Compose stack); `docker network create` refuses the name,
so the spike used `hlspike`. Migration must remove/rename the Compose bridge
before creating the overlay `home-services`. Also: bare `postgres` resolves
across stacks, so two stacks each with a `postgres` service on the shared
overlay would collide; keep explicit `homelab-*` aliases and avoid relying on
short names.

**S3 - Swarm over tailnet: pending (run with owner).**
