# Network Extension Services — Implementation Status

> ⚠️ This document was originally an implementation plan. Most phases are now **implemented and shipped**. The content below is kept for historical reference. See [docs/architecture.md](architecture.md) for the current architecture and [docs/service-testing.md](service-testing.md) for testing status.

## Status by Phase

| Phase | Scope | Status |
|-------|-------|--------|
| **1** | Core compose profiles | ✅ Implemented |
| **2** | Profile activation helper | ✅ Implemented |
| **3** | Config schema | ✅ Implemented |
| **4a** | `homelab tor` command | ✅ Implemented — `cmd/tor.go` |
| **4b** | `homelab i2p` command | ✅ Implemented — `cmd/i2p.go` |
| **4c** | `homelab ygg` command | ✅ Implemented — `cmd/yggdrasil.go` |
| **4d** | `homelab ipfs` command | ✅ Implemented — `cmd/ipfs.go` |
| **5** | Asset templates | ✅ Implemented |
| **6** | Service routing (Caddy) | ✅ Implemented |
| **7** | Setup wizard updates | ✅ Implemented |
| **8** | Asset installation | ✅ Implemented |
| **9** | Doctor + health checks | ✅ Implemented |

---

## Original Implementation Plan

## Overview

Add Tor, I2P, Yggdrasil, and IPFS as optional core stack extensions following
the same modular `profile` pattern as `cloudflared`. Each extension starts as a
Docker Compose profile in `assets/core/docker-compose.yml`, activated when its
config is set, and provides per-service `enable`/`disable` commands.

---

## Architecture

```
              Core Stack (Docker compose, profile-activated)
  ┌──────────────────────────────────────────────────────────────────┐
  │  tailscale ─── network_mode ───► caddy                           │
  │  cloudflared  (profile: tunnel)     │                            │
  │  tor          (profile: tor)        │  ← NEW                     │
  │  i2p          (profile: i2p)        │  ← NEW                     │
  │  yggdrasil    (profile: yggdrasil)  │  ← NEW                     │
  │  ipfs         (profile: ipfs)       │  ← NEW                     │
  └──────────────┬───────────────────────────────────────────────────┘
                 │ home-services Docker network
  ┌──────────────▼───────────────────────────────────────────────────┐
  │  Service containers (jellyfin, immich, uptime-kuma, …)           │
  │  Each reachable by container_name on home-services               │
  └──────────────────────────────────────────────────────────────────┘
```

### How each extension exposes services

| Extension | Address scheme | Exposure method | Per-service mechanism |
|-----------|---------------|-----------------|----------------------|
| Tor       | `xyz.onion`   | HiddenServicePort in torrc | Directory of `.onion` service configs |
| I2P       | `xyz.i2p`     | I2P tunnel (eepsite) | i2ptunnel config per service |
| Yggdrasil | `[200:…]:port` | socat TCP6→TCP4 forwarder | socat instance per service |
| IPFS      | `ipfs.home.*` | Caddy reverse proxy | Standard `caddy.cf.conf` |

### Why IPFS is different

IPFS is a content-addressed P2P filesystem, not a "service exposure" network.
It belongs here as a **storage infrastructure** extension — services can pin
content to it, and its HTTP Gateway can be fronted by Caddy at
`ipfs.{$HOME_SUBDOMAIN}.{$DOMAIN}`. It does **not** get a per-service
`enable`/`disable` mechanism.

---

## Phase 1: Core docker-compose.yml additions

Each extension gets a service entry in `assets/core/docker-compose.yml` under
its own `profiles: [<name>]` key, activated by `--profile <name>` when the
corresponding env var is set (same pattern as `withTunnelProfile`).

### Tor service

```yaml
# ── Tor onion service proxy ─────────────────────────────────────────
# Activated when TOR_ENABLED=true.
# Uses gnzsnz/torproxy — Alpine-ish Ubuntu image with tor + nyx.
# Supports %include /etc/tor/torrc.d/*.conf for per-service onion configs.
# Healthcheck: tor-resolve -v google.com
tor:
  image: gnzsnz/torproxy:latest
  container_name: tor
  profiles: ["tor"]
  restart: unless-stopped
  environment:
    TOR_ENABLED: ${TOR_ENABLED}
  volumes:
    # Mount the entire /etc/tor directory (torrc + torrc.d/*.conf)
    - ../tor/torrc:/etc/tor/torrc:ro
    - ../tor/torrc.d:/etc/tor/torrc.d:ro
    # Persistent hidden service keys — if lost, .onion addresses change!
    - tor-data:/var/lib/tor/hidden_service
  networks:
    - home-services
  depends_on:
    tailscale:
      condition: service_healthy
```

### I2P service

```yaml
# ── I2P router + eepsite proxy ──────────────────────────────────────
# Activated when I2P_ENABLED=true.
# Hosts eepsites (I2P websites) that proxy to Caddy.
i2p:
  image: geti2p/i2p:latest
  container_name: i2p
  profiles: ["i2p"]
  restart: unless-stopped
  environment:
    JVM_XMX: ${I2P_JVM_XMX:-512m}
    EXT_PORT: ${I2P_EXT_PORT:-45678}
  volumes:
    - i2p-config:/i2p/.i2p
    - ../i2p/i2ptunnel.config:/i2p/.i2p/i2ptunnel.config:ro
  networks:
    - home-services
  ports:
    # I2NP only — router console/proxies are internal to home-services
    - "${I2P_EXT_PORT:-45678}:${I2P_EXT_PORT:-45678}"
    - "${I2P_EXT_PORT:-45678}:${I2P_EXT_PORT:-45678}/udp"
  depends_on:
    tailscale:
      condition: service_healthy
```

### Yggdrasil service

```yaml
# ── Yggdrasil IPv6 mesh node ────────────────────────────────────────
# Activated when YGGDRASIL_ENABLED=true.
# Each service gets a socat TCP6→TCP4 forwarder so it is reachable
# via the Yggdrasil node's IPv6 address.
yggdrasil:
  image: yggdrasilnetwork/yggdrasil-go:latest
  container_name: yggdrasil
  profiles: ["yggdrasil"]
  restart: unless-stopped
  cap_add:
    - NET_ADMIN
  devices:
    - /dev/net/tun
  volumes:
    - ../yggdrasil/yggdrasil.conf:/etc/yggdrasil.conf:ro
    - ../yggdrasil/socat.d:/etc/socat.d:ro   # per-service socat configs
  sysctls:
    - net.ipv6.conf.all.disable_ipv6=0
  networks:
    - home-services
  depends_on:
    tailscale:
      condition: service_healthy
```

### IPFS service

```yaml
# ── IPFS Kubo node ──────────────────────────────────────────────────
# Activated when IPFS_ENABLED=true.
# Provides content-addressed P2P storage + HTTP Gateway.
# Gateway is fronted by Caddy at ipfs.{$HOME_SUBDOMAIN}.{$DOMAIN}.
ipfs:
  image: ipfs/kubo:latest
  container_name: ipfs
  profiles: ["ipfs"]
  restart: unless-stopped
  environment:
    IPFS_PROFILE: ${IPFS_PROFILE:-server}
  volumes:
    - ipfs-data:/data/ipfs
    - ipfs-staging:/export
  networks:
    - home-services
  depends_on:
    tailscale:
      condition: service_healthy
  # Note: RPC API (5001) is NOT exposed — Caddy talks to Gateway (8080) only
```

### New volumes

Add to the `volumes:` block at the bottom of `docker-compose.yml`:

```yaml
  tor-data:
    name: core_tor-data
  i2p-config:
    name: core_i2p-config
  ipfs-data:
    name: core_ipfs-data
  ipfs-staging:
    name: core_ipfs-staging
```

---

## Phase 2: Profile activation in Go CLI

### New helper: `withProfiles`

Generalize `withTunnelProfile` into a multi-profile helper in `cmd/core.go`:

```go
// withProfiles appends --profile <name> for each extension that has
// its enable-flag set in env.
func withProfiles(env map[string]string, args ...string) []string {
    profiles := []string{}
    if env["CF_TUNNEL_TOKEN"] != "" {
        profiles = append(profiles, "tunnel")
    }
    if env["TOR_ENABLED"] == "true" {
        profiles = append(profiles, "tor")
    }
    if env["I2P_ENABLED"] == "true" {
        profiles = append(profiles, "i2p")
    }
    if env["YGGDRASIL_ENABLED"] == "true" {
        profiles = append(profiles, "yggdrasil")
    }
    if env["IPFS_ENABLED"] == "true" {
        profiles = append(profiles, "ipfs")
    }
    if len(profiles) == 0 {
        return args
    }
    flat := []string{}
    for _, p := range profiles {
        flat = append(flat, "--profile", p)
    }
    return append(flat, args...)
}
```

Replace all `withTunnelProfile` calls with `withProfiles`.

---

## Phase 3: Config schema additions

### New root vars in `cmd/setup.go` defaults

| Var | Default | Required | Description |
|-----|---------|----------|-------------|
| `TOR_ENABLED` | `false` | No | Enable Tor onion service proxy (true/false) |
| `I2P_ENABLED` | `false` | No | Enable I2P router + eepsite proxy |
| `I2P_JVM_XMX` | `512m` | No | I2P JVM heap limit |
| `I2P_EXT_PORT` | `45678` | No | I2P external port for I2NP |
| `YGGDRASIL_ENABLED` | `false` | No | Enable Yggdrasil mesh node |
| `IPFS_ENABLED` | `false` | No | Enable IPFS Kubo node |
| `IPFS_PROFILE` | `server` | No | IPFS configuration profile |

### New keyring secrets (none for phase 1 — extensions are optional and don't require tokens by default)

The setup wizard should ask about each extension:

```
─── Network Extensions (optional) ────────────────────────

Enable Tor onion service proxy? [y/N]:
Enable I2P router + eepsite proxy? [y/N]:
Enable Yggdrasil mesh node? [y/N]:
Enable IPFS Kubo node? [y/N]:
```

---

## Phase 4: Per-extension CLI commands

Each extension follows the same command pattern as `homelab tunnel`:

### Common subcommands

```
homelab <ext> status     # Show connection status
homelab <ext> logs       # Stream container logs
```

### Tor-specific

```
homelab tor enable <service>    # Create onion service for a service
homelab tor disable <service>   # Remove onion service
homelab tor list                # List active onion services with .onion addresses
```

**Implementation**:

`homelab tor enable <name>`:
1. Resolves the service port from `caddy.conf` (grep `reverse_proxy`) or `--port`
2. Writes a config snippet to `tor/torrc.d/<name>.conf`:
   ```
   HiddenServiceDir /var/lib/tor/hidden_service/<name>
   HiddenServicePort 80 <name>:<port>
   ```
3. Sends SIGHUP to tor to reload config:
   ```bash
   docker exec tor sh -c 'kill -HUP $(pidof tor)'
   ```
   gnzsnz/torproxy runs tor under tini (PID 1). The `kill -HUP` approach
   sends the signal directly to the tor process, which triggers a graceful
   config reload — no downtime, existing .onion connections stay alive.

`homelab tor list`:
- Reads `<configDir>/tor/torrc.d/*.conf` to enumerate active services
- For each service, reads the .onion hostname from the container:
  ```bash
  docker exec tor cat /var/lib/tor/hidden_service/<name>/hostname
  ```
- Displays a table: service name, .onion address, status

`homelab tor disable <name>`:
1. Removes `tor/torrc.d/<name>.conf`
2. Sends SIGHUP to tor

**Key detail**: gnzsnz/torproxy's default `torrc` has the `%include /etc/tor/torrc.d/*.conf`
line **commented out**. Our custom `torrc` in `assets/tor/torrc` must uncomment it.

**Hidden service keys**: Stored persistently in the `tor-data` Docker volume
(`/var/lib/tor/hidden_service/<name>/`). These must persist across restarts
or the `.onion` address changes permanently.

### I2P-specific

```
homelab i2p enable <service>    # Create eepsite tunnel for a service
homelab i2p disable <service>   # Remove eepsite tunnel
homelab i2p list                # List active eepsites
```

**Implementation**: `homelab i2p enable <name>` writes I2P tunnel config
to `i2p/i2ptunnel.config` and restarts the I2P container.

### Yggdrasil-specific

```
homelab ygg enable <service>    # Create socat forwarder for a service
homelab ygg disable <service>   # Remove socat forwarder
homelab ygg list                # List active forwarders + Yggdrasil IPv6
homelab ygg peers               # Show Yggdrasil peers
```

**Implementation**: `homelab ygg enable <name>` writes a socat systemd-like
config or adds a supervisor entry running:
```
socat TCP6-LISTEN:<port>,fork,reuseaddr TCP4:<name>:<port>
```

### IPFS-specific

```
homelab ipfs status              # Show IPFS node info + peer count
homelab ipfs logs                # Stream logs
homelab ipfs gateway enable      # Expose IPFS gateway via Caddy
homelab ipfs gateway disable     # Remove Caddy route
homelab ipfs add <file>          # Add file to IPFS (via docker exec)
```

---

## Phase 5: New asset files

### `assets/tor/`

```
assets/tor/
├── torrc                     # Main torrc template
└── torrc.d/                  # Per-service onion configs (symlinked)
    └── README
```

`torrc`:
```
Log notice stdout
DataDirectory /var/lib/tor
User debian-tor

# Enable SOCKS proxy for Tor network access
SOCKSPort 0.0.0.0:9050

# Allow all Docker networks so onion services can reach containers
SOCKSPolicy accept 172.16.0.0/12
SOCKSPolicy accept 192.168.0.0/16
SOCKSPolicy accept 100.64.0.0/10

# Include per-service hidden service configs
%include /etc/tor/torrc.d/*.conf
```

**Note**: The `%include` line is **commented out** in gnzsnz/torproxy's default
`torrc`. Our custom torrc **must** uncomment (or add) it.

**Hidden service keys**: The gnzsnz/torproxy image stores them under
`/var/lib/tor/hidden_service/<name>/`. The `tor-data` volume mounts this path.

**Config reload**: gnzsnz/torproxy runs `/usr/bin/tini -- /usr/bin/tor`.
Send SIGHUP via `docker exec tor sh -c 'kill -HUP $(pidof tor)'`.

### `assets/i2p/`

```
assets/i2p/
├── i2ptunnel.config          # I2P tunnel configuration template
└── README
```

### `assets/yggdrasil/`

```
assets/yggdrasil/
├── yggdrasil.conf            # Peer configuration
└── socat.d/                  # Per-service socat forwards (symlinked)
    └── README
```

`yggdrasil.conf`:
```json
{
    "Peers": [],
    "Listen": "[::]:8008",
    "AdminListen": "127.0.0.1:9001",
    "MulticastInterfaces": []
}
```

### `assets/ipfs/`

```
assets/ipfs/
├── README
```

IPFS needs no template files — it configures via `IPFS_*` env vars.

---

## Phase 6: Service routing (Caddy integration)

### IPFS Caddy config

`assets/services/ipfs/` (if added as a catalog service) or handled via CLI:

`caddy.conf`:
```caddyfile
ipfs.{$HOME_SUBDOMAIN}.{$DOMAIN} {
    import wildcard_tls
    reverse_proxy ipfs:8080
}
```

`caddy.cf.conf`:
```caddyfile
ipfs.{$PUB_SUBDOMAIN}.{$DOMAIN} {
    import wildcard_tls
    reverse_proxy ipfs:8080
}
```

### Tor/I2P/Yggdrasil routing

These do **not** go through Caddy — they expose services natively on their
respective networks. They use their own `enable`/`disable` commands that
manage configuration files and signal the container (SIGHUP for tor, restart
for I2P, socat lifecycle for yggdrasil).

---

## Phase 7: Setup wizard updates

In `cmd/setup.go`, add a section after the Cloudflare Tunnel section:

```go
// ── Network Extensions (optional) ──────────────────────────
fmt.Printf("\n  %s\n", styles.Accent.Render("─── Network Extensions (optional) ─────────────────────"))
fmt.Printf("  %s\n\n", styles.Muted.Render("Enable alternative network exposure. Press Enter to skip."))

torEnabled := cfg.Vars["TOR_ENABLED"]
torEnabled.Value = promptStr(sc, "Enable Tor onion service proxy? (true/false)", torEnabled.Value)
cfg.Vars["TOR_ENABLED"] = torEnabled

i2pEnabled := cfg.Vars["I2P_ENABLED"]
i2pEnabled.Value = promptStr(sc, "Enable I2P router + eepsite proxy? (true/false)", i2pEnabled.Value)
cfg.Vars["I2P_ENABLED"] = i2pEnabled

yggEnabled := cfg.Vars["YGGDRASIL_ENABLED"]
yggEnabled.Value = promptStr(sc, "Enable Yggdrasil mesh node? (true/false)", yggEnabled.Value)
cfg.Vars["YGGDRASIL_ENABLED"] = yggEnabled

ipfsEnabled := cfg.Vars["IPFS_ENABLED"]
ipfsEnabled.Value = promptStr(sc, "Enable IPFS Kubo node? (true/false)", ipfsEnabled.Value)
cfg.Vars["IPFS_ENABLED"] = ipfsEnabled
```

---

## Phase 8: Asset installation + config dir layout

The `installAssets` function in `cmd/setup.go` also copies these dirs:

```
~/.config/homelab/
├── config.yaml
├── core/docker-compose.yml
├── caddy/
├── tor/                      # ← NEW
│   ├── torrc
│   └── onion.d/
├── i2p/                      # ← NEW
│   └── i2ptunnel.config
├── yggdrasil/                # ← NEW
│   ├── yggdrasil.conf
│   └── socat.d/
└── services/
```

Either extend `installAssets()` to also walk `assets/tor/`, `assets/i2p/`,
`assets/yggdrasil/`, or add targeted install calls.

---

## Phase 9: Doctor + health checks

`cmd/doctor.go` should check:

- **Tor**: `containerStatus("tor")` when config has `TOR_ENABLED=true`
- **I2P**: `containerStatus("i2p")` when config has `I2P_ENABLED=true`
- **Yggdrasil**: `containerStatus("yggdrasil")` when config has `YGGDRASIL_ENABLED=true`
- **IPFS**: `containerStatus("ipfs")` when config has `IPFS_ENABLED=true`

Also add `/dev/net/tun` check is already present — relevant for yggdrasil.

---

## Implementation order

| Phase | Scope | Files touched | Est. effort |
|-------|-------|--------------|-------------|
| **1** | Core compose profiles | `assets/core/docker-compose.yml` | 2h |
| **2** | Profile activation helper | `cmd/core.go` | 30min |
| **3** | Config schema | `cmd/setup.go` | 1h |
| **4a** | `homelab tor` command | `cmd/tor.go` (new) | 3h |
| **4b** | `homelab i2p` command | `cmd/i2p.go` (new) | 3h |
| **4c** | `homelab ygg` command | `cmd/yggdrasil.go` (new) | 3h |
| **4d** | `homelab ipfs` command | `cmd/ipfs.go` (new) | 2h |
| **5** | Asset templates | `assets/tor/`, `assets/i2p/`, `assets/yggdrasil/` | 1.5h |
| **6** | IPFS Caddy config | `assets/caddy/conf.d/` + `cmd/ipfs.go` | 1h |
| **7** | Setup wizard updates | `cmd/setup.go` | 30min |
| **8** | Asset installation | `cmd/setup.go` + `assets/assets.go` | 30min |
| **9** | Doctor checks | `cmd/doctor.go` | 30min |

**Total**: ~18h spread across ~12 files.

---

## Open questions

1. **Per-service port discovery** — `homelab <ext> enable <service>` needs to
   know the service's port. Should it read from `caddy.conf` (parse
   `reverse_proxy name:port`), from `config.yaml`, or require explicit
   `--port` flag? **Recommendation**: Read from `caddy.conf` (grep for
   `reverse_proxy`) as the source of truth, with `--port` override.

2. **Tor .onion address display** — After enabling Tor for a service, the
   `.onion` address is in `docker exec tor cat
   /var/lib/tor/hidden_service/<name>/hostname`. `homelab tor list` should
   batch-read these and show a table.

3. **I2P eepsite automation** — I2P's eepsites are managed via Router Console
   or by writing `i2ptunnel.config` and restarting. The `i2ptunnel.config`
   format is straightforward for standard HTTP tunnels. **Recommendation**:
   Write tunnel config files and restart the container.

4. **Yggdrasil multi-port** — If a service listens on multiple ports (e.g.,
   Plex), yggdrasil needs one socat per port. Start with single-port, add
   multi-port support as needed.

5. **Caddy wildcard TLS + IPFS** — IPFS Gateway works over HTTP by default.
   Caddy handles TLS termination. Standard `import wildcard_tls` pattern
   applies.

6. **Resource overhead** — I2P needs 512MB heap. These extensions should
   default to **disabled** and only activated when explicitly configured.
