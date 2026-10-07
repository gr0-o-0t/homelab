package assets_test

import (
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
)

// reverseProxyUpstream captures the host portion of `reverse_proxy host:port`.
var reverseProxyUpstream = regexp.MustCompile(`(?m)^\s*reverse_proxy\s+([a-zA-Z0-9._-]+):(\d+)`)

type caddyComposeFile struct {
	Services map[string]struct {
		ContainerName string `yaml:"container_name"`
	} `yaml:"services"`
}

// A caddy.routes.conf naming an upstream that no container in the same compose file
// answers to is invisible until `homelab enable <svc>` — Caddy accepts the
// config, then every request 502s on DNS failure. Docker resolves both
// container_name and the compose service name (as a network alias), so either
// spelling is valid; anything else is a typo.
func TestCatalogServices_CaddyUpstreamsResolve(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()

		compose, err := assets.CatalogFS.ReadFile("services/" + svc + "/docker-compose.yml")
		if err != nil {
			continue // covered by TestCatalogServices_ComposeFileUsesYmlExtension
		}
		var cf caddyComposeFile
		require.NoError(t, yaml.Unmarshal(compose, &cf), "service %q compose must parse", svc)

		resolvable := make(map[string]bool)
		// A declared shared dependency is on home-services too; appflowy
		// routes presigned S3 URLs straight to homelab-garage.
		for _, d := range catalogDatabases(t, svc) {
			resolvable[config.SharedDBContainer(d.Type)] = true
		}
		for name, s := range cf.Services {
			resolvable[name] = true // compose adds the service name as a network alias
			if s.ContainerName != "" {
				resolvable[s.ContainerName] = true
			}
		}

		for _, conf := range []string{"caddy.routes.conf"} {
			body, err := assets.CatalogFS.ReadFile("services/" + svc + "/" + conf)
			if err != nil {
				continue // covered by TestCatalogService_HasRequiredFiles
			}

			var unknown []string
			for _, m := range reverseProxyUpstream.FindAllStringSubmatch(string(body), -1) {
				if host := m[1]; !resolvable[host] {
					unknown = append(unknown, host)
				}
			}
			sort.Strings(unknown)
			assert.Empty(t, unknown, "service %q: %s proxies to hosts no container in its "+
				"docker-compose.yml provides — requests would 502", svc, conf)
		}
	}
}

// Every route is generated from the ports: declaration or a caddy.routes.conf.
//
// adguardhome and minero were the last two services shipping a hand-written
// caddy.conf + caddy.cf.conf for the retired symlink scheme. Both were wrong
// in ways generation would not have been: adguardhome declared port 80 while
// proxying 3000, and minero put HTTP site blocks in front of P2P, ZMQ and
// stratum ports that do not speak HTTP. Nothing reads these files any more, so
// one appearing here is dead config that looks live.
func TestCatalogServices_ShipNoStaticCaddyConf(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range []string{"caddy.conf", "caddy.cf.conf"} {
			_, err := assets.CatalogFS.Open("services/" + e.Name() + "/" + name)
			assert.Error(t, err, "%s ships %s, which nothing reads — declare ports: "+
				"or write a caddy.routes.conf instead", e.Name(), name)
		}
	}
}
