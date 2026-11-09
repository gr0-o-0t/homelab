# homelab

A modular, secure homelab on Docker Swarm, exposing self-hosted services exclusively over a private [Tailscale](https://tailscale.com) network, fronted by [Caddy](https://caddyserver.com) with automatic wildcard TLS via [Cloudflare](https://cloudflare.com).

One `homelab` agent container runs on every PC. The one on the Swarm manager (the leader) holds all state and runs Caddy on its own Tailscale node; stateless services can run on the other PCs. No ports are opened on your home router. Every service gets a clean `https://<service>.home.<yourdomain>.com` URL, reachable only from devices on your tailnet. Optionally expose services publicly via Cloudflare Tunnel.

---

## Architecture at a glance

```
any PC:  homelab (CLI) ──tailnet──▶ leader agent API (:7443)

Client (on tailnet)
  └─► immich.home.example.com
        └─► Cloudflare DNS (A record → the leader's Tailscale IP, DNS only — not proxied)
              └─► the leader agent's tailnet node (100.x.x.x — private, tailnet-only)
                    └─► Caddy inside the agent (TLS termination, wildcard cert via DNS-01)
                          └─► home-services Swarm overlay
                                └─► service task, on any PC of the cluster

Optional layer:
  Cloudflare Tunnel (public) ──► cloudflared ──► Caddy ──► service
```

See [docs/architecture.md](docs/architecture.md) for the full design rationale.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Docker Engine (rootful) | On every PC; `homelab init`/`join` set up Swarm |
| Tailscale on every PC | Swarm traffic between PCs rides the tailnet |
| Go 1.25+ | To build the `homelab` CLI |
| Tailscale account | Free tier supports 100 devices; see [docs/tailscale-setup.md](docs/tailscale-setup.md) |
| Cloudflare-managed domain | Any registrar; just delegate DNS to Cloudflare |

---

## Quick start

### 1. Build and install the CLI

```bash
git clone https://github.com/you/homelab
cd homelab
make build           # homelab binary with the desktop GUI (needs cgo + OpenGL/X11 headers:
                     #   Debian/Ubuntu: sudo apt install libgl1-mesa-dev xorg-dev)
make build-headless  # pure Go, no GUI — for servers without a desktop
make install      # go install → puts 'homelab' on your PATH
```

### 2. Get the credentials

- Follow [docs/tailscale-setup.md](docs/tailscale-setup.md) to generate a reusable auth key (`TS_AUTHKEY`).
- Follow [docs/cloudflare-setup.md](docs/cloudflare-setup.md) to create the API token (`CLOUDFLARE_API_TOKEN`).

### 3. Create the cluster

On the always-on machine:

```bash
homelab init        # Swarm manager + the leader agent; asks for the secrets it needs
homelab setup       # DOMAIN, HOME_SUBDOMAIN, ACME_EMAIL, … (stored on the leader)
homelab status      # the Edge table: caddy running
```

`init` prints the cluster secret and the command that adds other PCs:
`homelab join` (a Swarm worker running its own agent), or `homelab login` on a
PC that should only run the CLI. The leader keeps the `*.home.<domain>` DNS
record pointed at its tailnet node.

### 4. Add and start a service

```bash
homelab add uptime-kuma        # install from the embedded catalog into the leader's state
homelab setup uptime-kuma      # configure vars and secrets interactively
homelab up uptime-kuma         # deploy on Swarm
homelab enable uptime-kuma     # expose on the tailnet via Caddy
```

Visit `https://status.home.example.com` from any device on your tailnet.

---

## CLI reference

Except `init`, `join`, `login`, `logout`, `edit`, `version`, `completion` and
`backup --fetch`, every command runs on the leader agent: the CLI forwards it
over the tailnet with the cluster secret.

### Cluster

```
homelab init [--import <dir>]   Create the cluster on this machine (Swarm manager, leader agent)
homelab join                    Add this machine as a Swarm worker running its own agent
homelab login                   Use this machine as a client of the cluster only
homelab logout                  Forget the cluster endpoint and secret here
```

`init --import ~/.config/homelab` brings a Compose-era install along (config,
services, exposure, desired state, keyring secrets) and reuses its volumes.
`homelab upgrade` (rolling the agents) is planned and does not exist yet.

### Service lifecycle

```
homelab add [name]              Install from catalog (no name → list catalog)
homelab new [name]              Scaffold a new service directory (interactive wizard)
homelab setup [service]         Configure vars and secrets (no arg → root setup wizard)
homelab up <service...>         Deploy on Swarm (shared databases first)
homelab down <service...>       Scale to zero (config, volumes and exposure kept)
homelab start <service...>      Same as up (Swarm keeps no stopped containers)
homelab stop <service...>       Same as down
homelab restart <service...>    Replace every task of the service
homelab reload [service]        Load the routes into Caddy, or re-render one service's
homelab pull <service...>       Pull latest images without redeploying
homelab update <service...>     Pull latest images and redeploy
homelab edit <service> <path>   Edit a bind-mounted config file in $EDITOR and roll it out
homelab delete <service>...     Remove service entirely, asks for the name (alias: rm, -y to skip)
homelab ls [-q]                 List installed services
homelab status [service]        Show status overview or per-service detail (alias: ps)
homelab logs [service]          A service's logs from every node (-f, -n, -t, --since, -c; --tui); no service: the leader agent's
homelab exec <svc> <cmd...>     Run a command in a service container, on whichever node runs it (-c, -u, -w, -e, -T)
homelab config <service>        The compose file as resolved to deploy (contains secrets)
homelab images <service>        Images the stack deploys
homelab doctor [service]        Health checks
homelab --gui                   Desktop GUI (not in headless builds)
```

Lifecycle commands need a service: with none they refuse ("name a service, or
use -a/--all"). Several services may be named at once, and a batch keeps going
past a failing service:

```
homelab up --all                     # deploy all installed services
homelab up --group media             # by group (defined in config.yaml)
homelab up --group media --all       # error: mutually exclusive
homelab down -a                      # scale every installed service to zero
homelab down --group media           # by group
```

### Network routing

```
homelab enable <service>           Expose on tailnet via Caddy (private)
homelab enable <service> --cf      Also expose via Cloudflare Tunnel (public)

homelab disable <service>          Remove private tailnet route
homelab disable <service> --cf     Remove Cloudflare Tunnel route
homelab disable <service> -a       Remove every layer
```

Tor, I2P and Yggdrasil (`--tor`, `--i2p`, `--ygg`) are not available in
cluster mode yet.

### Diagnostics

```
homelab doctor                     Environment, the agent's Caddy, routing
homelab doctor <service>           Per-service health check
homelab doctor --all               Check all installed services
homelab validate                   Assemble and adapt the Caddy config without loading it
homelab caddy status               Whether the agent's Caddy answers
homelab caddy logs                 Follow the leader agent's logs
```

### Backup and restore

```
homelab backup [service]                     Volumes + DB dumps + config, on the leader (--all, --group, --live)
homelab backup --list                        Existing backups, newest first
homelab backup --fetch <name> <local-dir>    Copy a backup to this PC
homelab restore <backup-dir> [service]       Replace volumes + databases (--config, --create-volumes)
homelab prune [service...]                   Remove Swarm services, volumes and images (--keep-volumes)
```

### Shell completion

```
homelab completion bash|zsh|fish|powershell   Generate shell completion scripts
```

### Network extensions

```
homelab ext enable cf              Turn Cloudflare Tunnel on and deploy cloudflared
homelab ext disable cf             Remove it and turn it off
homelab ext list                   List extensions and their enabled/disabled status
homelab ext status [ext]           Show their state
homelab ext logs [ext]             Stream their logs
homelab cf route add <service>     Add the Cloudflare DNS route (enable --cf does it)
homelab cf route rm <service>      Remove it (needs CLOUDFLARE_API_TOKEN with DNS:Edit)
```

---

## Repository structure

```
homelab/
├── main.go                   # CLI entry point
├── Makefile                  # build / install / tidy / test / catalog
├── assets/                   # embedded into binary via //go:embed
│   ├── caddy/Caddyfile       # the global Caddy block the agent assembles its config from
│   └── services/             # bundled service catalog (canonical copy)
├── cmd/                      # Cobra command definitions
├── internal/                 # Go packages: agent, bootstrap, remote, edge, swarm, importer,
│                             #   backup, db, config, secrets, vault, tui, gui, … (see CLAUDE.md)
├── services/                 # gitignored export for local browsing (run 'make catalog')
└── docs/
    ├── architecture.md
    ├── tailscale-setup.md
    ├── cloudflare-setup.md
    └── adding-a-service.md
```

> **Note:** `assets/services/` is the embedded catalog — this is the **canonical copy**. The root `services/` directory is gitignored and only populated by running `make catalog` for local browsing. When adding or modifying a service, update **only** `assets/services/<name>/`.

---

## Where state lives

All state lives on the leader, in the `homelab-agent` Docker volume
(`/var/lib/homelab` inside the agent container):

```
/var/lib/homelab/
├── vault.bin, vault.key   # encrypted secrets; the key file is root-only
└── state/
    ├── config.yaml        # root vars (DOMAIN, HOME_SUBDOMAIN, …)
    ├── caddy/sites/       # generated routes, one file per service
    └── services/          # populated by homelab add
        └── uptime-kuma/
            ├── docker-compose.yml
            ├── config.yaml    # vars, secrets, ports (routes are generated)
            └── exposure.yaml  # the layers it is exposed on
```

A PC running only the CLI keeps `~/.config/homelab/client.yaml` (the leader's
endpoint) and the cluster secret in its system keyring. Secrets are **never**
written to disk in plain text, and never appear in a Swarm service spec.

---

## Included services

| Service | Private subdomain | Purpose |
|---|---|---|
| [Actual Budget](https://actualbudget.org) | `budget.home.*` | Personal finance |
| [AdGuard Home](https://adguard.com/adguard-home/overview.html) | `adguard.home.*` | Network-wide ad blocking |
| [Audiobookshelf](https://audiobookshelf.org) | `audiobooks.home.*` | Audiobook/Podcast server |
| [Bazarr](https://bazarr.media) | `bazarr.home.*` | Movie/TV subtitle manager |
| [Beszel Hub](https://github.com/henrygd/beszel) | `beszel.home.*` | Server monitoring |
| [Copyparty](https://github.com/9001/copyparty) | `share.home.*` | File sharing server |
| [CyberChef](https://cyberchef.org) | `cyberchef.home.*` | All-in-one data processing |
| [Dozzle](https://dozzle.dev) | `logs.home.*` | Docker log viewer |
| [Excalidraw](https://excalidraw.com) | `draw.home.*` | Whiteboard/diagramming |
| [Forgejo](https://forgejo.org) | `forgejo.home.*` | Git hosting |
| [Gitea](https://gitea.io) | `git.home.*` | Git hosting |
| [Glance](https://github.com/glanceapp/glance) | `dashboard.home.*` | Dashboard |
| [Gokapi](https://github.com/Forceu/gokapi) | `files.home.*` | File sharing |
| [Gotify](https://gotify.net) | `notify.home.*` | Notification server |
| [Homarr](https://homarr.dev) | `homarr.home.*` | Dashboard |
| [Home Assistant](https://home-assistant.io) | `home.home.*` | Home automation |
| [Homebox](https://homebox.software) | `inventory.home.*` | Inventory management |
| [Homepage](https://gethomepage.dev) | `start.home.*` | Landing page/dashboard |
| [Immich](https://immich.app) | `immich.home.*` | Photo & video backup |
| [IT-Tools](https://it-tools.tech) | `tools.home.*` | Developer utilities |
| [Jellyfin](https://jellyfin.org) | `jellyfin.home.*` | Media server |
| [Kavita](https://kavita.io) | `kavita.home.*` | Comic/Manga reader |
| [Mattermost](https://mattermost.com) | `chat.home.*` | Team communication |
| [Mealie](https://mealie.io) | `recipes.home.*` | Recipe manager |
| [MeTube](https://github.com/alexta69/metube) | `youtube.home.*` | YouTube downloader |
| [Minero](https://github.com/gr0-o-0t/minero) | `minero.home.*` | AIO xmr miner |
| [Miniflux](https://miniflux.app) | `rss.home.*` | RSS reader |
| [Navidrome](https://navidrome.org) | `music.home.*` | Music server |
| [NetBox](https://netbox.dev) | `netbox.home.*` | IPAM/DCIM |
| [Nextcloud](https://nextcloud.com) | `nextcloud.home.*` | File sync & collaboration |
| [ntfy](https://ntfy.sh) | `push.home.*` | Push notifications |
| [Ollama](https://ollama.ai) | `ollama.home.*` | Local LLM server |
| [Open WebUI](https://openwebui.com) | `ai.home.*` | LLM web interface |
| [Pi-hole](https://pi-hole.net) | `pihole.home.*` | Network-wide ad blocking |
| [Plex](https://plex.tv) | `plex.home.*` | Media server |
| [Pocket ID](https://github.com/stonith404/pocket-id) | `sso.home.*` | SSO provider |
| [Portainer](https://portainer.io) | `portainer.home.*` | Docker management |
| [Prowlarr](https://prowlarr.com) | `prowlarr.home.*` | Indexer manager |
| [qBittorrent](https://qbittorrent.org) | `torrent.home.*` | BitTorrent client |
| [Radarr](https://radarr.video) | `radarr.home.*` | Movie management |
| [SearXNG](https://searxng.org) | `search.home.*` | Metasearch engine |
| [Seerr](https://seerr.dev) | `requests.home.*` | Media requests |
| [Sonarr](https://sonarr.tv) | `sonarr.home.*` | TV show management |
| [Speedtest Tracker](https://github.com/henrygd/speedtest-tracker) | `speedtest.home.*` | Speed test history |
| [Stirling-PDF](https://stirlingpdf.com) | `pdf.home.*` | PDF manipulation |
| [Tandoor](https://tandoor.dev) | `tandoor.home.*` | Recipe manager |
| [Tautulli](https://tautulli.com) | `stats.home.*` | Plex statistics |
| [Technitium](https://technitium.com) | `dns.home.*` | DNS server |
| [Uptime Kuma](https://github.com/louislam/uptime-kuma) | `status.home.*` | Service monitoring |
| [Vaultwarden](https://github.com/dani-garcia/vaultwarden) | `vault.home.*` | Password manager |

> **⚠️ Service Testing Status**: These services are included in the catalog but have varying levels of real-world testing. See the [Service Testing Status](docs/service-testing.md) document for what has been verified on each network layer. Contributions and test reports welcome — see [Contributing](#contributing) below.

---

## Security notes

- **No public ports required.** Tailscale's DERP relay infrastructure means the agent's Caddy is reachable without opening any firewall ports.
- **Defence-in-depth.** Tailscale enforces network-level ACLs. Caddy enforces TLS and can add HTTP-layer access controls. The remote CLI requires the cluster secret.
- **Wildcard cert stays private.** The private key never leaves the leader; Cloudflare only sees a temporary TXT record during DNS-01 challenge.
- **Service isolation.** Only a service's UI container joins `home-services`; stack-internal traffic can use an `internal: true` network.
- **Secrets encrypted at rest.** All secrets (auth keys, API tokens, passwords) are in the leader's AES-256-GCM vault; service env reaches containers through Swarm secrets, never the service spec.

---

## Troubleshooting

**Caddy can't reach a service container**

```bash
docker network inspect home-services          # on the leader
homelab status <service>                      # is a task running?
```

**TLS cert not issuing**

```bash
homelab logs | grep -i "acme\|tls\|cloudflare"
# Check CLOUDFLARE_API_TOKEN has DNS:Edit permission
```

**Tailscale not connecting**

```bash
homelab logs | grep -i tailnet
# Check TS_AUTHKEY hasn't expired
```

**`homelab enable` fails: Caddy rejected the config**

```bash
homelab validate
# Check the service's caddy.routes.conf, if it has one
```

**Service resolves with `dig` but not `curl` or browser**

`dig` queries the DNS stub directly; `curl` and the OS resolver go through NSS. If they disagree, check for a stale negative cache in the NSS chain:

```bash
getent hosts <service>.<subdomain>.<domain>
```

If `getent` returns nothing while `dig` succeeds, run `resolvectl status` and look for a per-interface DNS domain routing rule (`DNS Domain:` entries) that routes your domain to the wrong upstream. A common culprit is Tailscale's DNS being given authority over a broader domain via Split DNS in the Tailscale admin console.

**DNS resolves correctly but browser shows `ERR_DNS_SECURE_RESOLVER_HOSTNAME_RESOLUTION_FAILED`**

The browser's Secure DNS (DNS-over-HTTPS) provider has a stale or negative cache entry independent of the system resolver. Either flush the browser's DNS cache (`brave://net-internals/#dns`, `chrome://net-internals/#dns`) or set Secure DNS to "Use current service provider" so it follows the system resolver:

- Brave: `brave://settings/security` → Use secure DNS
- Chrome: `chrome://settings/security` → Use secure DNS
- Firefox: Settings → Network Settings → DNS over HTTPS

**systemd-resolved timing out (`communications error to 127.0.0.53`)**

Usually caused by DNS-over-TLS (`+DNSOverTLS`) being applied to a link whose DNS server (e.g. a home router) does not support TLS on port 853. Diagnose with `resolvectl status` — look for links with `+DNSOverTLS` and a non-DoT-capable DNS server. Fix by telling NetworkManager to ignore the DHCP-provided DNS for that link:

```bash
nmcli connection modify "<wifi-connection-name>" ipv4.ignore-auto-dns yes
nmcli connection up "<wifi-connection-name>"
```

This leaves your configured global DNS (e.g. NextDNS) as the sole upstream.

---

## Contributing

Contributions are welcome! The project is in active development, and services in the catalog need real-world testing across all network layers.

### Ways to contribute

1. **Test services**: Deploy services from the catalog, verify they work on your network layers, and submit a PR updating the [Service Testing Status](docs/service-testing.md) table with your results
2. **Add new services**: Follow [docs/adding-a-service.md](docs/adding-a-service.md) to contribute services to the catalog
3. **Report bugs**: Open issues with detailed reproduction steps and logs — include the service name, network layer, and relevant logs from `homelab logs`
4. **Improve documentation**: Fix typos, clarify instructions, add examples
5. **Submit PRs**: Bug fixes, new features, and improvements are welcome

### Adding a new service to the catalog

See [docs/adding-a-service.md](docs/adding-a-service.md) for step-by-step instructions. The key requirements:

- Create `assets/services/<name>/` with `docker-compose.yml` and `config.yaml` (declare `ports:`; Caddy config is generated)
- Follow the network pattern: main container on `home-services`, databases/workers on `internal: true` network
- Use sensible defaults in `config.yaml` with `vars` (non-secrets) and `secrets` (stored in the leader's vault) sections
- Test the service end-to-end before submitting

### Development

```bash
# Build and install locally
make build
make install

# Run tests
make test

# Lint and run tests with race detector
make ci

# Export catalog for local browsing
make catalog
```

### Code style

- Follow existing patterns in the codebase
- Run `go vet` and `golint` before committing
- Add tests for new functionality
- Keep PRs focused and well-described

---

## License

MIT License

Copyright (c) 2026 <kafi.groot@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
DEALINGS IN THE SOFTWARE.
