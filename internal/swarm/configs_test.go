package swarm_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/filters"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

const glanceCompose = `
services:
  glance:
    image: nginx:1
    volumes:
      - ./config:/app/config:ro
      - ./one.conf:/etc/one.conf:ro
`

// glanceService installs a service with a config directory and a bound file.
func glanceService(t *testing.T) (*swarm.Deployer, *swarmtest.Fake, string) {
	t.Helper()
	d, f, root := newDeployer(t)
	writeService(t, root, "glance", glanceCompose)
	dir := filepath.Join(root, "services", "glance")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "config", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "glance.yml"), []byte("v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "sub", "x.yml"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "one.conf"), []byte("one"), 0o644))
	return d, f, dir
}

func configsOf(t *testing.T, f *swarmtest.Fake, svc string) []string {
	t.Helper()
	list, err := f.ConfigList(context.Background(), dswarm.ConfigListOptions{
		Filters: filters.NewArgs(filters.Arg("label", swarm.LabelService+"="+svc)),
	})
	require.NoError(t, err)
	var names []string
	for _, c := range list {
		names = append(names, c.Spec.Name)
	}
	slices.Sort(names)
	return names
}

func TestConfigs_DirectoryExpandsPerFile(t *testing.T) {
	d, f, _ := glanceService(t)
	require.NoError(t, d.Up(context.Background(), "glance"))
	refs := services(t, f, "glance")[0].Spec.TaskTemplate.ContainerSpec.Configs
	got := map[string]os.FileMode{}
	for _, r := range refs {
		got[r.File.Name] = r.File.Mode
	}
	assert.Equal(t, map[string]os.FileMode{
		"/app/config/glance.yml": 0o444,
		"/app/config/sub/x.yml":  0o444,
		"/etc/one.conf":          0o444,
	}, got)
	for _, n := range configsOf(t, f, "glance") {
		assert.True(t, strings.HasPrefix(n, "hl_glance_cfg_"), n)
	}
}

func TestConfigs_ContentAddressedRollover(t *testing.T) {
	ctx := context.Background()
	d, f, dir := glanceService(t)
	require.NoError(t, d.Up(ctx, "glance"))
	before := configsOf(t, f, "glance")
	require.Len(t, before, 3)

	require.NoError(t, d.Up(ctx, "glance")) // unchanged: same names
	assert.Equal(t, before, configsOf(t, f, "glance"))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "glance.yml"), []byte("v2"), 0o644))
	require.NoError(t, d.Up(ctx, "glance"))
	after := configsOf(t, f, "glance")
	require.Len(t, after, 3, "the stale config is pruned")
	assert.NotEqual(t, before, after)
	// every config left is referenced by the service
	used := map[string]bool{}
	for _, r := range services(t, f, "glance")[0].Spec.TaskTemplate.ContainerSpec.Configs {
		used[r.ConfigName] = true
	}
	for _, n := range after {
		assert.True(t, used[n], n)
	}
}

func TestConfigs_OversizeFileRefused(t *testing.T) {
	d, _, dir := glanceService(t)
	big := make([]byte, swarm.MaxConfigSize+1)
	for i := range big {
		big[i] = 'a'
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", "big.yml"), big, 0o644))
	err := d.Up(context.Background(), "glance")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "big.yml")
}

func TestConfigs_SymlinkRefused(t *testing.T) {
	d, _, dir := glanceService(t)
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(dir, "config", "link.yml")))
	err := d.Up(context.Background(), "glance")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "link.yml")
}

func TestFilesPut_RejectsTraversal(t *testing.T) {
	d, _, dir := glanceService(t)
	for _, p := range []string{"../../config.yaml", "../x", "/etc/passwd", "config/../../x", "", "."} {
		err := d.WriteConfigFile(context.Background(), "glance", p, []byte("evil"))
		assert.Error(t, err, p)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config", "glance.yml"))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(b))
}

func TestFilesPut_RejectsNonConfigFiles(t *testing.T) {
	d, _, dir := glanceService(t)
	for _, p := range []string{"config.yaml", "docker-compose.yml", "exposure.yaml", "other.conf"} {
		if p == "exposure.yaml" || p == "other.conf" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte("orig"), 0o644))
		}
		err := d.WriteConfigFile(context.Background(), "glance", p, []byte("evil"))
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "not a bind-mounted config", p)
		_, err = d.ReadConfigFile("glance", p)
		assert.Error(t, err, p)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	assert.Contains(t, string(b), "image: nginx:1")
}

func TestFilesPut_RejectsSymlinkEscape(t *testing.T) {
	d, _, dir := glanceService(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s"), 0o644))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "config", "link.yml")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "config", "dir")))

	for _, p := range []string{"config/link.yml", "config/dir/secret", "config/dir/new"} {
		assert.Error(t, d.WriteConfigFile(context.Background(), "glance", p, []byte("evil")), p)
		_, err := d.ReadConfigFile("glance", p)
		assert.Error(t, err, p)
	}
	b, _ := os.ReadFile(secret)
	assert.Equal(t, "s", string(b))
	_, err := os.Stat(filepath.Join(outside, "new"))
	assert.True(t, os.IsNotExist(err))
}

func TestFilesPut_RedeploysService(t *testing.T) {
	ctx := context.Background()
	d, f, dir := glanceService(t)
	require.NoError(t, d.Up(ctx, "glance"))
	before := configsOf(t, f, "glance")

	got, err := d.ReadConfigFile("glance", "config/glance.yml")
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))

	require.NoError(t, d.WriteConfigFile(ctx, "glance", "config/glance.yml", []byte("v2 new")))
	b, err := os.ReadFile(filepath.Join(dir, "config", "glance.yml"))
	require.NoError(t, err)
	assert.Equal(t, "v2 new", string(b))
	assert.NotEqual(t, before, configsOf(t, f, "glance"), "a new config rolled out")

	// a new file under a bound directory is a config too; a single bound file works
	require.NoError(t, d.WriteConfigFile(ctx, "glance", "config/sub/new.yml", []byte("n")))
	require.NoError(t, d.WriteConfigFile(ctx, "glance", "one.conf", []byte("uno")))
	targets := map[string]bool{}
	for _, r := range services(t, f, "glance")[0].Spec.TaskTemplate.ContainerSpec.Configs {
		targets[r.File.Name] = true
	}
	assert.True(t, targets["/app/config/sub/new.yml"])
	assert.Len(t, configsOf(t, f, "glance"), 4)
}

func TestFilesPut_RejectsOversizeAndEmpty(t *testing.T) {
	d, _, _ := glanceService(t)
	assert.Error(t, d.WriteConfigFile(context.Background(), "glance", "one.conf", nil))
	assert.Error(t, d.WriteConfigFile(context.Background(), "glance", "one.conf", make([]byte, swarm.MaxConfigSize+1)))
}

func TestDeployer_RefusesInvalidServiceNames(t *testing.T) {
	ctx := context.Background()
	d, _, dir := glanceService(t)
	root := filepath.Dir(filepath.Dir(dir))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "core", "config"), 0o755))
	for _, svc := range []string{"../core", "a/b", "..", ".", "Foo", ""} {
		assert.ErrorContains(t, d.Up(ctx, svc), "invalid service name", svc)
		assert.ErrorContains(t, d.Down(ctx, svc), "invalid service name", svc)
		assert.ErrorContains(t, d.Restart(ctx, svc), "invalid service name", svc)
		assert.ErrorContains(t, d.Pull(ctx, svc), "invalid service name", svc)
		assert.ErrorContains(t, d.Update(ctx, svc), "invalid service name", svc)
		assert.ErrorContains(t, d.Remove(ctx, svc), "invalid service name", svc)
		_, err := d.Status(ctx, svc)
		assert.ErrorContains(t, err, "invalid service name", svc)
		_, err = d.ReadConfigFile(svc, "config/glance.yml")
		assert.ErrorContains(t, err, "invalid service name", svc)
		assert.ErrorContains(t, d.WriteConfigFile(ctx, svc, "config/x", []byte("x")), "invalid service name", svc)
	}
	_, err := os.Stat(filepath.Join(root, "core", "config", "x"))
	assert.True(t, os.IsNotExist(err))
}
