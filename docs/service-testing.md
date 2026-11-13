# Service Testing Status

This document tracks which services and network combinations have been tested and verified to work. Contributions welcome — see [Contributing](../CONTRIBUTING.md).

Tor, I2P and Yggdrasil are not available in cluster mode yet, so only the tailnet and Cloudflare Tunnel layers are tracked.

## Legend

| Symbol | Meaning |
|--------|---------|
| [v] | Tested — works |
| [ ] | Not tested |
| [x] | Tested — fails |

## Testing table

| Service | Tailnet | Cloudflare Tunnel |
|---|---|---|
| actual-budget | [ ] | [ ] |
| adguardhome | [ ] | [ ] |
| audiobookshelf | [ ] | [ ] |
| bazarr | [ ] | [ ] |
| beszel-hub | [ ] | [ ] |
| copyparty | [ ] | [ ] |
| cyberchef | [ ] | [ ] |
| dozzle | [ ] | [ ] |
| excalidraw | [ ] | [ ] |
| forgejo | [ ] | [ ] |
| gitea | [ ] | [ ] |
| glance | [ ] | [ ] |
| gokapi | [ ] | [ ] |
| gotify | [ ] | [ ] |
| homarr | [ ] | [ ] |
| home-assistant | [ ] | [ ] |
| homebox | [ ] | [ ] |
| homepage | [ ] | [ ] |
| immich | [ ] | [ ] |
| it-tools | [ ] | [ ] |
| jellyfin | [ ] | [ ] |
| kavita | [ ] | [ ] |
| mattermost | [ ] | [ ] |
| mealie | [ ] | [ ] |
| metube | [ ] | [ ] |
| miniflux | [ ] | [ ] |
| navidrome | [ ] | [ ] |
| netbox | [ ] | [ ] |
| nextcloud | [ ] | [ ] |
| ntfy | [ ] | [ ] |
| ollama | [ ] | [ ] |
| open-webui | [ ] | [ ] |
| pihole | [ ] | [ ] |
| plex | [ ] | [ ] |
| pocket-id | [ ] | [ ] |
| portainer | [ ] | [ ] |
| prowlarr | [ ] | [ ] |
| qbittorrent | [ ] | [ ] |
| radarr | [ ] | [ ] |
| searxng | [v] | [v] |
| seerr | [ ] | [ ] |
| sonarr | [ ] | [ ] |
| speedtest-tracker | [ ] | [ ] |
| stirlingpdf | [ ] | [ ] |
| tandoor | [ ] | [ ] |
| tautulli | [ ] | [ ] |
| technitium | [ ] | [ ] |
| uptime-kuma | [ ] | [ ] |
| vaultwarden | [ ] | [ ] |

## How to contribute test results

1. Deploy a service from the catalog (`homelab add <name>`)
2. Test it on each network layer:
   - **Tailnet**: `homelab enable <name>` → access via `<name>.home.<domain>`
   - **Cloudflare Tunnel**: `homelab enable <name> --cf` → access via `<name>.<domain>` (needs `homelab ext enable cf`; see [cloudflare-setup.md](cloudflare-setup.md))
3. Open a PR updating this table with your results — mark verified cells as `[v]`, failures as `[x]`.
4. For failed tests, include reproduction steps, `homelab doctor` output, and relevant logs in the PR description or a linked issue.

> 💡 Tip: Use GitHub Issues to report failures with reproduction steps. Use PRs to batch-test multiple services and update this table.
