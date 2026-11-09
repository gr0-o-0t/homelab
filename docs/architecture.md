# Architecture

## Overview

homelab runs on Docker Swarm with one `homelab` agent container per PC. The
Swarm manager (minis) runs the **leader** agent, which holds all state and is
the network edge: Caddy runs inside it, serving on its own Tailscale node. The
CLI on any PC forwards its commands to the leader over the tailnet.

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
laptop / desktop  [docker: swarm worker, label homelab.role=worker]
         └─ homelab agent (thin: tsnet node homelab-<host>, exec/logs relay)
         └─ stateless services (preferred here)
```

Hosts need only Docker Engine (rootful) and their existing Tailscale. Swarm
control (2377), gossip (7946) and the VXLAN data path (4789) use each host's
Tailscale IPv4 as advertise and data-path address; `homelab init` and
`homelab join` refuse any address outside 100.64.0.0/10.

## Key design decisions

### 1. One agent per PC, minis as the single manager

`homelab init` on minis runs `docker swarm init` on its tailnet IP, generates
the cluster secret and starts the leader agent (`homelab-agent` container,
restart `unless-stopped`, docker socket mounted, state in the `homelab-agent`
volume). `homelab join` on another PC fetches a worker join token from the
leader, joins Swarm on its own tailnet IP and starts a worker agent. `homelab
login` makes a PC a client only. There is no HA: minis holds every piece of
state, by design.

The leader, at start: creates the overlay, starts the edge, points the
wildcard DNS record at its tailnet node, deploys cloudflared, brings up every
service recorded as running (shared databases first), and serves the remote
CLI. A worker agent serves `exec` and `logs` for the containers on its node.

### 2. The CLI forwards to the agent

On a PC the CLI does not run commands itself: it sends its argv over a
websocket to `https://homelab.<tailnet>:7443/v1/run`, authenticated with an
HMAC of the cluster secret (stored in the PC's keyring by `homelab login` or
`join`). The agent runs the same binary against its state dir and streams the
terminal back. CLI and agent must share major.minor version. Only `init`,
`join`, `login`, `logout`, `edit`, `version`, `completion`, `backup --fetch`
and the dashboard/GUI run locally; the dashboard's buttons exec the CLI, so
each is forwarded too.

### 3. The edge is in-process

Caddy (with caddy-l4 and the Cloudflare DNS module) runs inside the leader
agent and listens on the agent's tsnet node, not on host ports. Tailnet sites
bind to that node; raw TCP/UDP ports listen on it. Routes are loaded through
Caddy's admin API on a unix socket: `homelab enable` assembles the whole
config, adapts it and loads it. A config Caddy rejects leaves the running one
in place, and the service's routing files are restored.

### 4. `home-services` overlay

Every service's UI container joins the `home-services` overlay (attachable,
MTU 1230 for VXLAN inside WireGuard, not `encrypted`: the data path already
rides the tailnet). The leader agent is attached as `homelab-edge`, so Caddy
reaches every service by its alias on any node. A compose `container_name`
becomes a network alias, so `homelab-postgres`, `gitea` etc. keep resolving.

Internal service-to-service traffic (e.g. a stack's own worker ↔ server) can
use a stack-local network marked `internal: true`.

### 5. Placement

Placement is decided per container. A container with a data volume, a host
bind path, the docker socket, or a shared-database provider role is pinned to
`node.labels.homelab.role == core` (minis). Stateless containers prefer
workers: Swarm has no soft affinity, so the constraint is `role == worker`
while any worker is Ready and none otherwise; the rebalancer force-updates
those services when a worker returns. A service's `config.yaml` can override
per container (`placement: {svc: core|worker|any}`) and mark re-creatable
volumes (`cache_volumes:`) so they do not pin.

### 6. Secrets

The leader's secrets (Cloudflare token, Tailscale auth key, tunnel token,
shared-database credentials, the cluster secret, every service secret) are in
an AES-256-GCM vault in the agent volume; the key file is root-only. No unlock
prompt; Swarm autolock is off. A service's env, secrets included, reaches its
tasks as a Swarm secret read by a small entrypoint shim — never in the service
spec, so `docker service inspect` shows none. Backups do not include secrets.

### 7. Wildcard TLS via Cloudflare DNS-01

The wildcard cert `*.home.example.com` cannot be obtained via HTTP-01 (no public port needed). Caddy uses the `caddy-dns/cloudflare` module to answer the DNS-01 ACME challenge by temporarily creating TXT records in your Cloudflare zone. This requires a **scoped API token** (not the global API key):

- Permissions required: `Zone → Zone → Read`, `Zone → DNS → Edit`
- Zone scope: your domain only

### 8. Cloudflare A record (private access)

```
Type:    A
Name:    *.home        (i.e. *.home.example.com)
Value:   <the leader agent's tsnet node IP>   (100.x.x.x)
Proxy:   DNS only (grey cloud) — NOT proxied
```

The leader agent creates and updates this record itself, with the same API
token, whenever it starts. It must stay **DNS only**: proxied, the traffic
would leave Tailscale and become publicly routed — defeating the entire point.

**Why A record, not CNAME?** A CNAME to `homelab.<tailnet>.ts.net` only works if that ts.net name is publicly resolvable. In practice public resolvers (including the DNS-over-HTTPS providers browsers use) get NXDOMAIN for it, breaking resolution for all clients. A direct A record to the Tailscale IP works everywhere; the IP is stable as long as the node stays in the tailnet.

### 9. Cloudflare Tunnel (optional public access)

For services that need to be publicly reachable, cloudflared runs as a Swarm
service pinned to minis and points at the leader's plain-HTTP listener on the
overlay (`homelab-edge:80`):

- Configure `CF_TUNNEL_TOKEN` and `CF_TUNNEL_NAME` via `homelab setup`
- Turn it on: `homelab ext enable cf` (the leader also deploys it at start when the token is set)
- Expose a service: `homelab enable <service> --cf` (creates the proxied CNAME via the API; `homelab cf route add|rm` manage it directly)

Public services use `<service>.<domain>` and are served through Cloudflare's
network. Raw ports are not carried over Cloudflare.

### 10. Generated, stored exposure

Caddy config is generated, never hand-written per layer. Each service declares
its ports in `config.yaml` (or a `caddy.routes.conf` body for complex routing).
Which layers it is on — plus any `--name` and `--ports` — is stored in
`services/<name>/exposure.yaml` in the leader's state, written only by
`homelab enable`/`disable`. From that state and the declaration, homelab
renders one Caddy file per service, `caddy/sites/<name>.conf`, holding the
blocks of every enabled layer (and `caddy/l4/<name>.conf` for raw ports), and
loads the result into the edge.

| Layer | Mechanism | CLI flag |
|---|---|---|
| Tailnet | the leader's tsnet node, wildcard cert | (default) |
| Cloudflare Tunnel | cloudflared Swarm service, proxied CNAME | `--cf` |

Tor, I2P and Yggdrasil (`--tor`, `--i2p`, `--ygg`) are not available in
cluster mode yet; the flags fail with that message.

A Compose-era config dir kept one Caddy file per layer and port in
`caddy/conf.d-<layer>/`. `homelab init --import` converts those into
exposure.yaml plus the sites file.

### 11. Backups

Backups run on the leader, which can snapshot every data volume because
placement keeps them all on minis. They land in the `homelab-backups` volume;
`homelab backup --fetch <name> <dir>` streams one to the PC running the CLI.
Each service is scaled to zero for its snapshot unless `--live` is given.

## Network traffic flow (per request)

### Private access (tailnet only)

```
Client (on tailnet)
  └─► DNS: immich.home.example.com
        └─► Cloudflare: A → 100.x.x.x (the leader agent's tsnet node)
              └─► Tailscale DERP/direct: 100.x.x.x:443
                    └─► Caddy inside the leader agent
                          └─► TLS termination (wildcard cert)
                                └─► reverse_proxy immich:2283
                                      └─► home-services overlay → immich task (any node)
```

### Public access (via Cloudflare Tunnel)

```
Client (anywhere on internet)
  └─► DNS: immich.example.com
        └─► Cloudflare: proxied CNAME → <tunnel>.cfargotunnel.com
              └─► Cloudflare Edge → encrypted tunnel
                    └─► cloudflared (Swarm task on minis)
                          └─► home-services overlay → homelab-edge:80
                                └─► Caddy inside the leader agent
                                      └─► reverse_proxy immich:2283
```
