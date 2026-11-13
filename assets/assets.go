// Package assets embeds the agent's Caddyfile global block and the service
// catalog into the binary, so `homelab add` can install a service without the
// source repository.
package assets

import (
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

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

//go:embed cloudflared/docker-compose.yml
var cloudflaredCompose []byte

// CloudflaredImage returns the cloudflared image reference from
// cloudflared/docker-compose.yml, which must declare exactly one service with
// an image. The file exists so Dependabot can bump the tag.
func CloudflaredImage() (string, error) {
	var f struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(cloudflaredCompose, &f); err != nil {
		return "", fmt.Errorf("cloudflared/docker-compose.yml: %w", err)
	}
	if len(f.Services) != 1 {
		return "", fmt.Errorf("cloudflared/docker-compose.yml: want exactly one service, found %d", len(f.Services))
	}
	for _, s := range f.Services {
		if s.Image == "" {
			return "", fmt.Errorf("cloudflared/docker-compose.yml: the service has no image")
		}
		return s.Image, nil
	}
	return "", nil
}
