// Package assets embeds the agent's Caddyfile global block and the service
// catalog into the binary, so `homelab add` can install a service without the
// source repository.
package assets

import "embed"

// CoreFS holds caddy/Caddyfile: the global options and the wildcard TLS
// snippet internal/edge assembles the agent's whole Caddy config from (with
// every caddy/l4 and caddy/sites file of the state root inlined).
//
//go:embed caddy
var CoreFS embed.FS

// CatalogFS contains the bundled service catalog. Each subdirectory under
// services/ is a ready-to-use service that can be installed with
// `homelab add <name>`. Files per service:
//   - docker-compose.yml  — container stack definition, deployed on Swarm
//   - config.yaml         — vars, secrets, databases and the ports it exposes
//   - caddy.routes.conf   — optional: a site block body for routing that is
//     more than one host → one upstream
//
//go:embed services
var CatalogFS embed.FS
