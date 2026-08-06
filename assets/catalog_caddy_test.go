package assets_test

import (
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

// reverseProxyUpstream captures the host portion of `reverse_proxy host:port`.
var reverseProxyUpstream = regexp.MustCompile(`(?m)^\s*reverse_proxy\s+([a-zA-Z0-9._-]+):(\d+)`)

type caddyComposeFile struct {
	Services map[string]struct {
		ContainerName string `yaml:"container_name"`
	} `yaml:"services"`
}

// A caddy.conf naming an upstream that no container in the same compose file
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
		for name, s := range cf.Services {
			resolvable[name] = true // compose adds the service name as a network alias
			if s.ContainerName != "" {
				resolvable[s.ContainerName] = true
			}
		}

		for _, conf := range []string{"caddy.conf", "caddy.cf.conf", "caddy.routes.conf"} {
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

// siteAddress matches a Caddy site-block opener: anything ending in `{` that
// isn't a directive nested inside a block.
var siteAddress = regexp.MustCompile(`(?m)^\S.*\{\s*$`)

// A shipped caddy.conf must actually define a site block.
//
// Three database services (mariadb, postgres, redis) shipped caddy.conf and
// caddy.cf.conf files containing nothing but a comment saying they have no web
// interface. `homelab enable <db>` symlinked that comment into conf.d and
// reported the service exposed — a route that routes nothing, which is exactly
// the kind of fiction that hides a real misconfiguration. A database with no
// HTTP surface should ship no route file and fail `enable` with "no ports
// defined", which is the truth.
func TestCatalogServices_CaddyConfDefinesASite(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range []string{"caddy.conf", "caddy.cf.conf"} {
			data, err := assets.CatalogFS.ReadFile("services/" + e.Name() + "/" + name)
			if err != nil {
				continue // not shipping one is fine
			}
			assert.Regexp(t, siteAddress, string(data),
				"%s/%s has no site block — ship no file instead", e.Name(), name)
		}
	}
}

// Every service must be routable by exactly one mechanism, and the declared
// ports must be able to express it.
//
// After the port grammar landed (bare / listen:container / subdomain:container),
// 49 of the 57 catalog services had their hand-written caddy.conf + caddy.cf.conf
// deleted because generation produces the same blocks; 6 more moved to a
// layer-agnostic caddy.routes.conf. Two keep a static caddy.conf on purpose and
// this test documents which and why — anything else appearing here means a
// service quietly fell back to hand-maintained routing.
func TestCatalogServices_RoutingIsGeneratedExceptWhereItCannotBe(t *testing.T) {
	staticByDesign := map[string]string{
		"adguardhome": "serves DNS/DoH/DoT/DoQ across seven ports under {$PUB_SUBDOMAIN}, " +
			"which the grammar's HOME/CF subdomains cannot express",
		"minero": "fans one hostname out to several different upstream containers " +
			"(monerod, p2pool), while generation proxies to the service itself",
	}

	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		svc := e.Name()
		if _, err := assets.CatalogFS.Open("services/" + svc + "/caddy.conf"); err != nil {
			continue
		}
		assert.Contains(t, staticByDesign, svc,
			"%s ships a static caddy.conf — declare its ports instead, or add it here with the reason", svc)
	}
}
