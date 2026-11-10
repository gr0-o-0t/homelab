package swarm_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/99designs/keyring"
	"github.com/docker/docker/api/types/filters"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/secrets"
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
		err := d.WriteConfigFile("glance", p, []byte("evil"))
		assert.Error(t, err, p)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config", "glance.yml"))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(b))
}

func TestFilesPut_RejectsNonConfigFiles(t *testing.T) {
	d, _, dir := glanceService(t)
	for _, p := range []string{"exposure.yaml", "other.conf"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte("orig"), 0o644))
		err := d.WriteConfigFile("glance", p, []byte("evil"))
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "not a bind-mounted config", p)
		_, err = d.ReadConfigFile("glance", p)
		assert.Error(t, err, p)
		b, _ := os.ReadFile(filepath.Join(dir, p))
		assert.Equal(t, "orig", string(b), p)
	}
}

// R49: the service's compose file and config.yaml are editable too; a valid
// write is saved and the service redeployed.
func TestFilesPut_SpecFileSavedAndRedeployed(t *testing.T) {
	ctx := context.Background()
	d, f, dir := glanceService(t)
	require.NoError(t, d.Up(ctx, "glance"))

	got, err := d.ReadConfigFile("glance", "docker-compose.yml")
	require.NoError(t, err)
	assert.Contains(t, string(got), "image: nginx:1")

	edited := strings.Replace(glanceCompose, "image: nginx:1", "image: nginx:1\n    hostname: edited", 1)
	require.NoError(t, d.WriteConfigFile("glance", "docker-compose.yml", []byte(edited)))
	require.NoError(t, d.Up(ctx, "glance")) // what files put runs next (via the up path)
	b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	assert.Equal(t, edited, string(b))
	assert.Equal(t, "edited", services(t, f, "glance")[0].Spec.TaskTemplate.ContainerSpec.Hostname, "redeployed")

	require.NoError(t, d.WriteConfigFile("glance", "config.yaml", []byte("vars:\n  X:\n    value: \"1\"\n")))
	assert.FileExists(t, filepath.Join(dir, "config.yaml"))
}

// An invalid spec file is refused with its error and nothing is written:
// YAML that does not parse, a secret interpolated into the spec, an unknown
// placement role.
func TestFilesPut_SpecFileRefusedNothingWritten(t *testing.T) {
	d, f, dir := glanceService(t)
	sm := secrets.NewForTest(keyring.NewArrayKeyring(nil))
	require.NoError(t, sm.Set("glance", "TOKEN", "s3cr3t-token"))
	d.SM = sm
	cfg := "secrets:\n  TOKEN:\n    required: true\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644))
	compose, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))

	for _, tc := range []struct{ file, content, want string }{
		{"docker-compose.yml", "services: [nope\n", "docker-compose.yml"},
		{"docker-compose.yml", strings.Replace(glanceCompose, "image: nginx:1",
			"image: nginx:1\n    command: [\"serve\", \"--token\", \"${TOKEN}\"]", 1), "secret"},
		{"config.yaml", "secrets: [nope\n", "config.yaml"},
		{"config.yaml", cfg + "placement:\n  glance: somewhere\n", "unknown role"},
		{"config.yaml", cfg + "cache_volumes: [nosuch]\n", "not a volume"},
	} {
		err := d.WriteConfigFile("glance", tc.file, []byte(tc.content))
		require.Error(t, err, tc.content)
		assert.Contains(t, err.Error(), tc.want, tc.content)
		assert.Contains(t, err.Error(), "nothing changed", tc.content)
		assert.NotContains(t, err.Error(), "s3cr3t-token")
		b, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
		assert.Equal(t, string(compose), string(b), "compose untouched")
		b, _ = os.ReadFile(filepath.Join(dir, "config.yaml"))
		assert.Equal(t, cfg, string(b), "config.yaml untouched")
	}
	assert.Empty(t, services(t, f, "glance"), "nothing deployed")

	_, err := d.ReadConfigFile("glance", "exposure.yaml")
	assert.Error(t, err)
}

// A spec file the service did not have is refused just the same, and stays
// absent; no staging leftovers remain in the service dir or the state root.
func TestFilesPut_NewSpecFileRefusedStaysAbsent(t *testing.T) {
	d, _, dir := glanceService(t)
	cfg := filepath.Join(dir, "config.yaml")
	require.NoFileExists(t, cfg)
	err := d.WriteConfigFile("glance", "config.yaml", []byte("placement:\n  glance: somewhere\n"))
	require.ErrorContains(t, err, "nothing changed")
	assert.NoFileExists(t, cfg)
	for _, p := range []string{dir, filepath.Dir(filepath.Dir(dir))} {
		entries, _ := os.ReadDir(p)
		for _, e := range entries {
			assert.False(t, strings.HasPrefix(e.Name(), "."), "leftover %s in %s", e.Name(), p)
		}
	}
}

func TestFilesPut_RejectsSymlinkEscape(t *testing.T) {
	d, _, dir := glanceService(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s"), 0o644))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "config", "link.yml")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "config", "dir")))

	for _, p := range []string{"config/link.yml", "config/dir/secret", "config/dir/new"} {
		assert.Error(t, d.WriteConfigFile("glance", p, []byte("evil")), p)
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

	require.NoError(t, d.WriteConfigFile("glance", "config/glance.yml", []byte("v2 new")))
	require.NoError(t, d.Up(ctx, "glance"))
	b, err := os.ReadFile(filepath.Join(dir, "config", "glance.yml"))
	require.NoError(t, err)
	assert.Equal(t, "v2 new", string(b))
	assert.NotEqual(t, before, configsOf(t, f, "glance"), "a new config rolled out")

	// a new file under a bound directory is a config too; a single bound file works
	require.NoError(t, d.WriteConfigFile("glance", "config/sub/new.yml", []byte("n")))
	require.NoError(t, d.WriteConfigFile("glance", "one.conf", []byte("uno")))
	require.NoError(t, d.Up(ctx, "glance"))
	targets := map[string]bool{}
	for _, r := range services(t, f, "glance")[0].Spec.TaskTemplate.ContainerSpec.Configs {
		targets[r.File.Name] = true
	}
	assert.True(t, targets["/app/config/sub/new.yml"])
	assert.Len(t, configsOf(t, f, "glance"), 4)
}

func TestFilesPut_RejectsOversizeAndEmpty(t *testing.T) {
	d, _, _ := glanceService(t)
	assert.Error(t, d.WriteConfigFile("glance", "one.conf", nil))
	assert.Error(t, d.WriteConfigFile("glance", "one.conf", make([]byte, swarm.MaxConfigSize+1)))
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
		assert.ErrorContains(t, d.WriteConfigFile(svc, "config/x", []byte("x")), "invalid service name", svc)
	}
	_, err := os.Stat(filepath.Join(root, "core", "config", "x"))
	assert.True(t, os.IsNotExist(err))
}
