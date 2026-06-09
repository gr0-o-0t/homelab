// Package assets embeds the homelab core stack files and service catalog into
// the binary so that `homelab setup` and `homelab add` can install
// them to the user's config directory without needing the source repository.
package assets

import "embed"

// CoreFS contains the core infrastructure files:
//   - core/docker-compose.yml     — Tailscale + Caddy + cloudflared stack
//   - core/Dockerfile.caddy       — custom Caddy build with Cloudflare DNS plugin
//   - core/Dockerfile.ygg         — custom Yggdrasil build with socat
//   - core/entrypoint.ygg.sh      — Yggdrasil entrypoint script
//   - caddy/...                   — Caddy configuration
//   - tor/...                     — Tor onion service configuration
//   - i2p/...                     — I2P router configuration
//   - yggdrasil/...               — Yggdrasil mesh node configuration
//
//go:embed core caddy tor i2p yggdrasil
var CoreFS embed.FS

// CatalogFS contains the bundled service catalog. Each subdirectory under
// services/ is a ready-to-use service that can be installed with
// `homelab service add <name>`. Files per service:
//   - docker-compose.yml  — container stack definition
//   - caddy.conf          — private (tailnet) reverse-proxy snippet
//   - caddy.cf.conf       — Cloudflare Tunnel reverse-proxy snippet
//   - config.yaml         — vars + secrets schema with sensible defaults
//
//go:embed services
var CatalogFS embed.FS
