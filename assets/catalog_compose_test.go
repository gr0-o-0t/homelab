package assets_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

type auditComposeFile struct {
	Services map[string]struct {
		ContainerName string   `yaml:"container_name"`
		Image         string   `yaml:"image"`
		Ports         []string `yaml:"ports"`
	} `yaml:"services"`
	Volumes map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"volumes"`
}

func loadCatalog(t *testing.T) map[string]auditComposeFile {
	t.Helper()

	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	out := make(map[string]auditComposeFile, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := assets.CatalogFS.ReadFile("services/" + e.Name() + "/docker-compose.yml")
		if err != nil {
			continue // covered by TestCatalogServices_ComposeFileUsesYmlExtension
		}
		var cf auditComposeFile
		require.NoError(t, yaml.Unmarshal(data, &cf), "service %q compose must parse", e.Name())
		out[e.Name()] = cf
	}
	return out
}

// Two services binding the same host port cannot run at once — the second fails
// to start with "port is already allocated". Most of the catalog publishes
// nothing (Caddy fronts everything), so the few that do are worth pinning:
// today that is the DNS servers, which are genuinely alternatives, plus
// BitTorrent and Monero peer ports.
func TestCatalogServices_HostPortConflictsAreKnown(t *testing.T) {
	// Services that deliberately contend for the same host port because they are
	// alternatives to each other, not meant to run together.
	knownAlternatives := map[string][]string{
		"53":   {"adguardhome", "pihole", "technitium"},
		"443":  {"adguardhome"},
		"853":  {"adguardhome"},
		"784":  {"adguardhome"},
		"8853": {"adguardhome"},
		"5443": {"adguardhome"},
	}

	owners := map[string]map[string]bool{}
	for svc, cf := range loadCatalog(t) {
		for _, s := range cf.Services {
			for _, p := range s.Ports {
				// "53:53/udp" → host port "53"; skip bare "8080" (no host bind).
				hostPort, _, found := strings.Cut(p, ":")
				if !found {
					continue
				}
				if owners[hostPort] == nil {
					owners[hostPort] = map[string]bool{}
				}
				owners[hostPort][svc] = true
			}
		}
	}

	for port, svcSet := range owners {
		if len(svcSet) < 2 {
			continue
		}
		svcs := make([]string, 0, len(svcSet))
		for s := range svcSet {
			svcs = append(svcs, s)
		}
		sort.Strings(svcs)

		expected := knownAlternatives[port]
		sort.Strings(expected)
		assert.Equal(t, expected, svcs,
			"host port %s is claimed by %v — either they are known alternatives "+
				"(add them to knownAlternatives) or this is an unintended collision", port, svcs)
	}
}

// Docker names an unnamed volume "<project>_<key>", which puts it outside the
// catalog's global volume-uniqueness guard and makes it hard to find by hand
// when restoring. Every volume gets an explicit name.
func TestCatalogServices_VolumesAreExplicitlyNamed(t *testing.T) {
	var unnamed []string
	for svc, cf := range loadCatalog(t) {
		for key, v := range cf.Volumes {
			if v.Name == "" {
				unnamed = append(unnamed, fmt.Sprintf("%s/%s", svc, key))
			}
		}
	}
	sort.Strings(unnamed)
	assert.Empty(t, unnamed, "these volumes need an explicit `name:`")
}

// Caddy configs and `homelab status` both address containers by name. Without
// container_name Docker generates "<project>-<service>-1", so a caddy.conf
// upstream resolves only by accident (via the compose network alias) and status
// output has no stable handle.
func TestCatalogServices_ContainersAreExplicitlyNamed(t *testing.T) {
	var unnamed []string
	for svc, cf := range loadCatalog(t) {
		for key, s := range cf.Services {
			if s.ContainerName == "" {
				unnamed = append(unnamed, fmt.Sprintf("%s/%s", svc, key))
			}
		}
	}
	sort.Strings(unnamed)
	assert.Empty(t, unnamed, "these containers need an explicit `container_name:`")
}

// LinuxServer deprecated its Overseerr image when upstream merged into Seerr;
// deprecated LSIO images stop getting updates and eventually lose `latest`.
// Pin the lesson so nobody reintroduces a retired image.
func TestCatalogServices_NoRetiredImages(t *testing.T) {
	retired := map[string]string{
		"linuxserver/overseerr": "merged into Seerr — use ghcr.io/seerr-team/seerr " +
			"(https://info.linuxserver.io/issues/2026-02-16-overseerr/)",
	}

	for svc, cf := range loadCatalog(t) {
		for _, s := range cf.Services {
			for needle, why := range retired {
				assert.NotContains(t, s.Image, needle,
					"service %q uses a retired image: %s", svc, why)
			}
		}
	}
}
