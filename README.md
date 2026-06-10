# homelab

A modular, secure homelab stack exposing self-hosted services exclusively over a private [Tailscale](https://tailscale.com) network, fronted by [Caddy](https://caddyserver.com) reverse proxy with automatic wildcard TLS via [Cloudflare](https://cloudflare.com).

No ports are opened on your home router. Every service gets a clean `https://<service>.home.<yourdomain>.com` URL, reachable only from devices on your tailnet.

---

## Architecture at a glance

```
Client (on tailnet)
  └─► immich.home.example.com
        └─► Cloudflare DNS (A record → Tailscale IP, DNS only — not proxied)
              └─► Tailscale (100.x.x.x — private, tailnet-only)
                    └─► Caddy (TLS termination, wildcard cert via DNS-01)
                          └─► Docker home-services network
                                └─► service container
```

See [docs/architecture.md](docs/architecture.md) for the full design rationale.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Docker + Docker Compose v2 | `docker compose` (not `docker-compose`) |
| Go 1.25.0 | To build the `homelab` CLI |
| Linux host with `/dev/net/tun` | See [docs/tailscale-setup.md](docs/tailscale-setup.md) for WSL2 |
| Tailscale account | Free tier supports 100 devices |
| Cloudflare-managed domain | Any registrar; just delegate DNS to Cloudflare |

---

## Quick start

### 1. Build and install the CLI

```bash
git clone https://github.com/you/homelab
cd homelab
make install      # go install → puts 'homelab' on your PATH
```

### 2. Configure

Run the interactive wizard. Non-secret values are saved to
`~/.config/homelab/config.yaml`; secrets go to the system keyring
(SecretService on Linux, Keychain on macOS).

```bash
homelab setup
```

You will be prompted for:

| Variable | Description |
|---|---|
| `DOMAIN` | Your base domain (e.g. `example.com`) |
| `HOME_SUBDOMAIN` | Subdomain prefix for private services (e.g. `home`) |
| `ACME_EMAIL` | Let's Encrypt registration email |
| `TS_HOSTNAME` | Tailscale node name for the Caddy host |
| `TS_AUTHKEY` | Tailscale auth key (stored in keyring) |
| `CLOUDFLARE_API_TOKEN` | API token with `Zone:DNS:Edit` (stored in keyring) |
| `CF_TUNNEL_TOKEN` | Cloudflare Tunnel token — optional, for public exposure |

### 3. Set up Tailscale and Cloudflare

- Follow [docs/tailscale-setup.md](docs/tailscale-setup.md) to generate an auth key.
- Follow [docs/cloudflare-setup.md](docs/cloudflare-setup.md) to create the API token and A record.

### 4. Start the core stack

```bash
homelab start                   # starts Tailscale + Caddy + enabled extensions
homelab status                  # check everything is running
```

Get the Tailscale IP from `homelab status` and set it as the A record value in Cloudflare DNS.

### 5. Add and start a service

```bash
homelab add uptime-kuma        # copy from embedded catalog to ~/.config/homelab/services/
homelab setup uptime-kuma      # configure vars and secrets interactively
homelab start uptime-kuma      # start (or: homelab up uptime-kuma)
homelab enable uptime-kuma     # expose on tailnet via Caddy
```

Visit `https://status.home.example.com` from any device on your tailnet.

---

## CLI reference

### Global flags

```
--config-dir <path>   config directory (default: ~/.config/homelab)
--config <file>       root config file; overrides config-dir/config.yaml
--json                output as JSON (on commands that support it)
--no-color            disable coloured output
```

### Service lifecycle

```
homelab add [name]              Install from catalog (no name → list catalog)
homelab new [name]              Scaffold a new service directory (interactive wizard)
homelab setup [service]         Configure vars and secrets (no arg → root setup wizard)
homelab start [service]         Start core stack or service(s)
homelab stop [service]          Stop core stack or service(s)
homelab restart [service]       Restart containers
homelab reload [service]        Reload Caddy config or a service's routing config
homelab update [service]        Pull latest images and recreate containers
homelab delete <service>        Remove service entirely (alias: rm)
homelab status [service]        Show status overview or per-service detail
homelab logs [service]          Tail logs (TTY → interactive TUI log viewer)
```

`start`, `stop`, and `restart` accept batch flags:

```
homelab start --all                  # all services
homelab start --group media          # by group (defined in config.yaml)
homelab start --group media --all    # error: mutually exclusive
homelab start --build                # rebuild images before starting
```

`restart` also supports `--build`:

```
homelab restart --build              # rebuild and recreate
```

Aliases: `start` ↔ `up`, `stop` ↔ `down`

### Network routing

```
homelab enable <service>           Expose on tailnet via Caddy (private)
homelab enable <service> --cf      Also expose via Cloudflare Tunnel (public)
homelab enable <service> --tor     Also expose as Tor .onion service
homelab enable <service> --i2p     Also expose as I2P eepsite
homelab enable <service> --ygg     Also expose on Yggdrasil mesh

homelab disable <service>          Remove private tailnet route
homelab disable <service> --cf     Remove Cloudflare Tunnel route
homelab disable <service> --tor    Remove Tor .onion service
homelab disable <service> --i2p    Remove I2P eepsite tunnel
homelab disable <service> --ygg    Remove Yggdrasil forwarder
```

### Diagnostics

```
homelab doctor                     Environment health check
homelab doctor <service>           Per-service health check
homelab doctor --all               Check all installed services
homelab validate                   Validate Caddyfile syntax only
```

### Shell completion

```
homelab completion bash|zsh|fish|powershell   Generate shell completion scripts
```

### Network extensions

Extensions are managed via the `ext` subcommand:

```
homelab ext list                   List extensions and their enabled/disabled status
homelab ext status [ext]           Show container status for all or one
homelab ext logs [ext]             Stream container logs for all or one
homelab ext start [ext]            Start extension container(s)
homelab ext stop [ext]             Stop extension container(s)
```

Service-level exposure (through an extension) is managed via the root
`enable`/`disable` commands:

```
homelab enable <svc> --i2p    expose via I2P eepsite
homelab enable <svc> --tor    expose as Tor .onion service
homelab enable <svc> --ygg    expose on Yggdrasil mesh
homelab disable <svc> --i2p   remove I2P exposure
```

Extension-specific advanced subcommands:

```
homelab ext cf route add <service>     # add Cloudflare DNS route
homelab ext cf route rm <service>      # remove Cloudflare DNS route
homelab ext ipfs gateway enable        # enable IPFS Gateway Caddy route
homelab ext ipfs gateway disable       # disable IPFS Gateway Caddy route
```

### Service subcommand (legacy, hidden)

The `service` subcommand still exists for backward compatibility:

```
homelab service list               # list all services and their exposure status
homelab service ps <service>       # show container status
```

---

## Repository structure

```
homelab/
├── main.go                   # CLI entry point
├── go.mod / go.sum
├── Makefile                  # build / install / tidy / test / catalog
│
├── assets/                   # embedded into binary via //go:embed
│   ├── assets.go             # CoreFS + CatalogFS embed declarations
│   ├── core/                 # Tailscale + Caddy compose stack
│   ├── caddy/                # Caddyfile + conf.d templates
│   └── services/             # bundled service catalog (canonical copy)
│       ├── actual-budget/
│       ├── adguardhome/
│       ├── audiobookshelf/
│       ├── bazarr/
│       ├── beszel-hub/
│       ├── copyparty/
│       ├── cyberchef/
│       ├── dozzle/
│       ├── excalidraw/
│       ├── forgejo/
│       ├── gitea/
│       ├── glance/
│       ├── gokapi/
│       ├── gotify/
│       ├── homarr/
│       ├── home-assistant/
│       ├── homebox/
│       ├── homepage/
│       ├── immich/
│       ├── it-tools/
│       ├── jellyfin/
│       ├── kavita/
│       ├── mattermost/
│       ├── mealie/
│       ├── metube/
│       ├── minero/
│       ├── miniflux/
│       ├── navidrome/
│       ├── netbox/
│       ├── nextcloud/
│       ├── ntfy/
│       ├── ollama/
│       ├── open-webui/
│       ├── pihole/
│       ├── plex/
│       ├── pocket-id/
│       ├── portainer/
│       ├── prowlarr/
│       ├── qbittorrent/
│       ├── radarr/
│       ├── searxng/
│       ├── seerr/
│       ├── sonarr/
│       ├── speedtest-tracker/
│       ├── stirlingpdf/
│       ├── tandoor/
│       ├── tautulli/
│       ├── technitium/
│       ├── uptime-kuma/
│       └── vaultwarden/
│
├── cmd/                      # Cobra command definitions
│   ├── root.go               # --config-dir / --config flags, TUI launcher
│   ├── service.go            # service lifecycle functions (runServiceUp, etc.)
│   ├── add.go                # homelab add
│   ├── caddy.go              # homelab reload + homelab validate
│   ├── delete.go             # homelab delete (alias: rm)
│   ├── disable.go            # homelab disable
│   ├── doctor.go             # homelab doctor
│   ├── enable.go             # homelab enable
│   ├── ext.go                # homelab ext (list + extension command hub)
│   ├── i2p.go                # homelab ext i2p
│   ├── ipfs.go               # homelab ext ipfs
│   ├── logs.go               # homelab logs
│   ├── new.go                # homelab new
│   ├── restart.go            # homelab restart
│   ├── setup.go              # homelab setup
│   ├── start.go              # homelab start (+ up alias)
│   ├── status.go             # homelab status
│   ├── stop.go               # homelab stop (+ down alias)
│   ├── tor.go                # homelab ext tor
│   ├── tunnel.go             # homelab ext cf
│   ├── update.go             # homelab update
│   ├── yggdrasil.go          # homelab ext ygg
│   ├── completion.go         # shell completion generation
│   ├── commands_test.go      # integration tests
│   └── service_add_test.go   # add command tests
│
├── internal/                 # Go packages
│   ├── caddy/                # symlink management + Caddy reload
│   ├── config/               # XDG config dir, YAML schema, BuildEnv
│   ├── docker/               # Docker SDK client (read-only status)
│   ├── run/                  # Commander — shells out to docker compose
│   ├── scaffold/             # embedded templates for new-service wizard
│   ├── secrets/              # system keyring (SecretService / Keychain)
│   ├── service/              # filesystem + Docker service discovery
│   └── tui/                  # Bubble Tea UI (list, logs, wizard, spinner, styles)
│
├── services/                 # gitignored export for local browsing (run 'make catalog')
└── docs/
    ├── architecture.md
    ├── tailscale-setup.md
    ├── cloudflare-setup.md
    └── adding-a-service.md
```

> **Note:** `assets/services/` is the embedded catalog — this is the **canonical copy**. The root `services/` directory is gitignored and only populated by running `make catalog` for local browsing. When adding or modifying a service, update **only** `assets/services/<name>/`.

---

## Config directory

All runtime state lives under `${XDG_CONFIG_HOME:-$HOME/.config}/homelab/`:

```
~/.config/homelab/
├── config.yaml          # root vars (DOMAIN, HOME_SUBDOMAIN, …)
├── core/                # installed by homelab setup
│   └── docker-compose.yml
├── caddy/               # installed by homelab setup
│   ├── Caddyfile
│   └── conf.d/
└── services/            # populated by homelab add
    └── uptime-kuma/
        ├── docker-compose.yml
        ├── caddy.conf
        ├── caddy.cf.conf
        └── config.yaml  # vars + secrets schema
```

Secrets (API tokens, passwords) are **never** written to disk — they are stored in
the system keyring and injected into docker compose at runtime.

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

> **⚠️ Service Testing Status**: These services are included in the catalog but have **not yet been tested for functional completeness**. The configurations are based on official Docker images and best practices, but actual deployment behavior may vary. Contributors and testers are welcome to verify and improve these service definitions. Please see [Contributing](#contributing) below.

---

## Security notes

- **No public ports required.** Tailscale's DERP relay infrastructure means Caddy is reachable without opening any firewall ports.
- **Defence-in-depth.** Tailscale enforces network-level ACLs. Caddy enforces TLS and can add HTTP-layer access controls.
- **Wildcard cert stays private.** The private key never leaves your server; Cloudflare only sees a temporary TXT record during DNS-01 challenge.
- **Service isolation.** Each service uses a dedicated `internal: true` network for databases and workers. Only the UI container joins `home-services`.
- **Secrets in keyring only.** All secrets (auth keys, API tokens, passwords) are stored in the OS keyring — never written to any file on disk.

---

## Troubleshooting

**Caddy can't reach a service container**

```bash
docker network inspect home-services
docker exec caddy ping <container-name>
```

**TLS cert not issuing**

```bash
homelab logs | grep -i "acme\|tls\|cloudflare"
# Check CLOUDFLARE_API_TOKEN has DNS:Edit permission
```

**Tailscale not connecting**

```bash
homelab logs | grep tailscale
# Check TS_AUTHKEY hasn't expired
```

**`homelab enable` fails Caddyfile validation**

```bash
homelab validate
# Check caddy/conf.d/<service>.conf for syntax errors
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

Contributions are welcome! The project is in active development, and many services in the catalog would benefit from real-world testing and refinement.

### Ways to contribute

1. **Test services**: Deploy services from the catalog and report issues or improvements
2. **Add new services**: Follow [docs/adding-a-service.md](docs/adding-a-service.md) to contribute services
3. **Report bugs**: Open issues with detailed reproduction steps and logs
4. **Improve documentation**: Fix typos, clarify instructions, add examples
5. **Submit PRs**: Bug fixes, new features, and improvements are welcome

### Adding a new service to the catalog

See [docs/adding-a-service.md](docs/adding-a-service.md) for step-by-step instructions. The key requirements:

- Create `assets/services/<name>/` with `docker-compose.yml`, `caddy.conf`, `caddy.cf.conf`, and `config.yaml`
- Follow the network pattern: main container on `home-services`, databases/workers on `internal: true` network
- Use sensible defaults in `config.yaml` with `vars` (non-secrets) and `secrets` (keyring-stored) sections
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
