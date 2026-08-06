# Architecture

## Overview

```
                ┌─────────────────────────────────────────────┐
                │                 Internet                     │
                └────────────────────┬────────────────────────┘
                                      │ DNS lookup
                           immich.home.example.com
                                      │
                 ┌────────────────────▼────────────────────────┐
                 │              Cloudflare DNS                  │
                 │                                              │
                 │  *.home.example.com  A  100.x.x.x           │
                 │                       (Caddy Tailscale IP)  │
                 └────────────────────┬────────────────────────┘
                                      │ resolves to Tailscale IP
                                      │ (100.x.x.x) — only reachable
                                      │ from inside the tailnet
                 ┌────────────────────▼────────────────────────┐
                 │              Your Tailnet                    │
                 │                                              │
                 │   ┌──────────────────────────────────────┐  │
                 │   │         Docker Host (home server)     │  │
                 │   │                                       │  │
                 │   │  ┌─────────────┐  network_mode:      │  │
                 │   │  │  Tailscale  │◄─service:tailscale──┤  │
                 │   │  │  container  │        │             │  │
                 │   │  └──────┬──────┘        │             │  │
                 │   │         │ home-services  │             │  │
                 │   │         │ Docker network │             │  │
                 │   │  ┌──────▼──────┐  ┌─────▼──────┐     │  │
                 │   │  │    Caddy    │  │  Cloudflare │     │  │
                 │   │  │  (network   │  │   Tunnel   │     │  │
                 │   │  │  namespace) │  │ (cloudflared)│    │  │
                 │   │  └──────┬──────┘  └──────┬───────┘     │  │
                 │   │         │                │             │  │
                 │   │         │  ┌─────────────▼───┐         │  │
                 │   │         │  │  Tor / I2P /    │         │  │
                 │   │         │  │  Yggdrasil /    │         │  │
                                  │   │         │  └──────┬──────────┘         │  │
                 │   │         │ home-services network         │  │
                 │   │  ┌──────────┐  ┌────────┴──┐         │  │
                 │   │  │  immich  │  │ jellyfin  │  ...    │  │
                 │   │  └──────────┘  └───────────┘         │  │
                 │   └──────────────────────────────────────┘  │
                 └──────────────────────────────────────────────┘
```

## Key design decisions

### 1. `network_mode: service:tailscale` on Caddy

When a container uses `network_mode: service:<other>`, it shares the **entire network namespace** of the target container — not just a network attachment, but the same network interfaces, IP addresses, and routing table.

This means:
- Caddy inherits Tailscale's `tailscale0` TUN interface → it has the node's Tailscale IP
- Caddy inherits the `home-services` Docker bridge that Tailscale is attached to → it can resolve service containers by name

Caddy does **not** get its own `networks:` block in `docker-compose.yml`; that would conflict with `network_mode`.

### 2. `home-services` as an external Docker network

All service `docker-compose.yml` files attach their primary container to the `home-services` external network. Caddy (via Tailscale's namespace) sees every container on this network and can proxy to them by container name.

Internal service-to-service traffic (e.g., Immich ↔ Postgres) uses a separate, stack-local network marked `internal: true` so it is never reachable from Caddy or any other service.

### 3. Wildcard TLS via Cloudflare DNS-01

The wildcard cert `*.home.example.com` cannot be obtained via HTTP-01 (no public port needed). Caddy uses the `caddy-dns/cloudflare` plugin to answer the DNS-01 ACME challenge by temporarily creating TXT records in your Cloudflare zone. This requires a **scoped API token** (not the global API key):

- Permissions required: `Zone → Zone → Read`, `Zone → DNS → Edit`
- Zone scope: your domain only

### 4. Cloudflare A record (private access)

```
Type:    A
Name:    *.home        (i.e. *.home.example.com)
Value:   <Caddy node's Tailscale IP>   (100.x.x.x — get it from `homelab status`)
Proxy:   DNS only (grey cloud) — NOT proxied
TTL:     Auto
```

Set proxy status to **DNS only**. If you proxy through Cloudflare the traffic would leave Tailscale and become publicly routed — defeating the entire point.

**Why A record, not CNAME?** The natural approach is a CNAME pointing to `<ts-hostname>.<tailnet>.ts.net`. This only works if that ts.net hostname is publicly resolvable via the internet. In practice, public resolvers (including DNS-over-HTTPS providers used by browsers) query Tailscale's authoritative DNS (dnsimple) for the CNAME target and receive NXDOMAIN, breaking resolution for all clients. A direct A record to the Tailscale IP is simpler and works everywhere. Tailscale IPs are stable — they only change if the node is removed and re-added to the tailnet.

### 5. Cloudflare Tunnel (optional public access)

For services that need to be publicly accessible on the internet, Cloudflare Tunnel (`cloudflared`) provides an encrypted tunnel from your home server to Cloudflare's edge network. This works alongside private Tailscale access:

- Configure `CF_TUNNEL_TOKEN` and `CF_TUNNEL_NAME` via `homelab setup`
- Cloudflare Tunnel starts with the core stack: `homelab up`
- Add DNS routes: `homelab ext cf route add <service>`
- Enable public Caddy config: `homelab enable <service> --cf`

Public services use a separate subdomain (default: `pub.example.com`) and are served through Cloudflare's global network.

### 6. Multi-layer service exposure

Each service directory ships with Caddyfile snippets:
- `caddy.conf` — Private reverse proxy (tailnet-only)
- `caddy.cf.conf` — Public reverse proxy (Cloudflare Tunnel)

Running `homelab enable <name>` symlinks `caddy.conf` into `caddy/conf.d/` and reloads Caddy. Running `homelab enable <name> --cf` symlinks `caddy.cf.conf` instead.

For alternative network extensions:

| Extension | Mechanism | CLI flag |
|---|---|---|
| Cloudflare Tunnel | cloudflared sidecar, DNS routing | `--cf` |
| Tor onion service | torrc.d configs, SIGHUP reload | `--tor` |
| I2P eepsite | i2ptunnel config, container restart | `--i2p` |
| Yggdrasil mesh | socat TCP6→TCP4 forwarder | `--ygg` |

`homelab disable <name>` removes any exposure. Extension containers (Tor, I2P, Yggdrasil) are managed via `homelab ext` subcommands.

## Network traffic flow (per request)

### Private access (tailnet only)

```
Client (on tailnet)
  └─► DNS: immich.home.example.com
        └─► Cloudflare: A → 100.x.x.x (Caddy's Tailscale IP)
              └─► Tailscale DERP/direct: 100.x.x.x:443
                    └─► Caddy (in Tailscale network namespace)
                          └─► TLS termination (wildcard cert)
                                └─► reverse_proxy immich:2283
                                      └─► Docker DNS → immich container
                                            └─► Immich app
```

### Public access (via Cloudflare Tunnel)

```
Client (anywhere on internet)
  └─► DNS: immich.pub.example.com
        └─► Cloudflare: CNAME → Cloudflare Tunnel
              └─► Cloudflare Edge → encrypted tunnel
                    └─► cloudflared (on your home server)
                          └─► Docker home-services network
                                └─► Caddy (TLS termination)
                                      └─► reverse_proxy immich:2283
                                            └─► Immich app
```

### Tor onion service access

```
Tor client (anywhere)
  └─► Tor network: <onion>.onion
        └─► Tor container (hidden service endpoint)
              └─► HiddenServicePort 80 → <service>:<port>
                    └─► Docker home-services network
                          └─► Target service container
```

### I2P eepsite access

```
I2P client
  └─► I2P network: <b32>.b32.i2p  (Host header <name>.<home>.i2p)
  └─► Tor network: <onion>       (HTTP via Caddy; non-HTTP ports direct)
        └─► I2P container (eepsite router)
              └─► i2ptunnel: HTTP proxy to service
                    └─► Docker home-services network
                          └─► Target service container
```

### Yggdrasil mesh access

```
Yggdrasil peer
  └─► Yggdrasil IPv6: [200:...]:<port>
        └─► Yggdrasil container (mesh node)
              └─► socat: TCP6 → TCP4 forwarder
                    └─► Docker home-services network
                          └─► Target service container
```
