package assets_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

const sharedNetwork = "home-services"

// networkComposeFile is the slice of a compose file that decides which
// containers join the shared network. A service's `networks:` is either a
// list of keys or a map keyed by them.
type networkComposeFile struct {
	Networks map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"networks"`
	Services map[string]struct {
		ContainerName string    `yaml:"container_name"`
		Networks      yaml.Node `yaml:"networks"`
	} `yaml:"services"`
}

func serviceNetworkKeys(n yaml.Node) []string {
	var keys []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			keys = append(keys, c.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}
	}
	return keys
}

// On Swarm, every service is resolvable on each network it joins by its bare
// compose key (and here also by container_name, added as an alias). Two
// catalog services putting the same name on home-services would make it
// resolve round-robin across unrelated containers, so those names must be
// unique across the whole catalog.
func TestCatalogServices_UniqueSharedNetworkKeys(t *testing.T) {
	entries, err := assets.CatalogFS.ReadDir("services")
	require.NoError(t, err)

	owners := map[string]map[string]bool{} // DNS name → "svc/key" owners
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := assets.CatalogFS.ReadFile("services/" + e.Name() + "/docker-compose.yml")
		if err != nil {
			continue
		}
		var cf networkComposeFile
		require.NoError(t, yaml.Unmarshal(data, &cf), e.Name())
		for key, s := range cf.Services {
			shared := false
			for _, nk := range serviceNetworkKeys(s.Networks) {
				name := cf.Networks[nk].Name
				if name == "" {
					name = nk
				}
				shared = shared || name == sharedNetwork
			}
			if !shared {
				continue
			}
			owner := e.Name() + "/" + key
			for _, dns := range []string{key, s.ContainerName} {
				if dns == "" {
					continue
				}
				if owners[dns] == nil {
					owners[dns] = map[string]bool{}
				}
				owners[dns][owner] = true
			}
		}
	}

	var collisions []string
	for dns, o := range owners {
		if len(o) > 1 {
			var list []string
			for k := range o {
				list = append(list, k)
			}
			sort.Strings(list)
			collisions = append(collisions, dns+": "+strings.Join(list, ", "))
		}
	}
	sort.Strings(collisions)
	assert.Empty(t, collisions, "names on %s must be unique across the catalog", sharedNetwork)
}
