package assets_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/assets"
)

func TestCloudflaredImage_FromEmbeddedCompose(t *testing.T) {
	img, err := assets.CloudflaredImage()
	require.NoError(t, err)
	assert.Equal(t, "cloudflare/cloudflared:2026.10.7", img)
}

// dependabot.yml must parse and cover the four ecosystems, each weekly with a
// minor/patch group.
func TestDependabotConfig(t *testing.T) {
	raw, err := os.ReadFile("../.github/dependabot.yml")
	require.NoError(t, err)
	var cfg struct {
		Version int `yaml:"version"`
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Schedule  struct {
				Interval string `yaml:"interval"`
			} `yaml:"schedule"`
			Groups map[string]struct {
				UpdateTypes []string `yaml:"update-types"`
			} `yaml:"groups"`
		} `yaml:"updates"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	assert.Equal(t, 2, cfg.Version)
	var got []string
	for _, u := range cfg.Updates {
		got = append(got, u.Ecosystem)
		assert.Equal(t, "weekly", u.Schedule.Interval, u.Ecosystem)
		require.NotEmpty(t, u.Groups, u.Ecosystem)
		for _, g := range u.Groups {
			assert.ElementsMatch(t, []string{"minor", "patch"}, g.UpdateTypes, u.Ecosystem)
		}
	}
	assert.ElementsMatch(t, []string{"gomod", "github-actions", "docker", "docker-compose"}, got)
}
