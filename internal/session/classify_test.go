package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/docker"
)

func labelsFor(wd string) map[string]string {
	return map[string]string{docker.LabelProject: filepath.Base(wd), docker.LabelWorkingDir: wd}
}

func TestScope_Classify(t *testing.T) {
	root := t.TempDir()
	s := NewScope(root)

	p, ok := s.Classify(labelsFor(filepath.Join(root, "core")))
	assert.True(t, ok)
	assert.True(t, p.Core)

	p, ok = s.Classify(labelsFor(filepath.Join(root, "services", "jellyfin")))
	assert.True(t, ok)
	assert.Equal(t, Project{Name: "jellyfin"}, p)

	for name, l := range map[string]map[string]string{
		"other project":   labelsFor("/srv/elsewhere"),
		"sibling dir":     labelsFor(filepath.Join(root, "caddy")),
		"nested deeper":   labelsFor(filepath.Join(root, "services", "a", "b")),
		"services itself": labelsFor(filepath.Join(root, "services")),
		"no labels":       {},
		"outside via ..":  labelsFor(filepath.Join(root, "..", "core")),
	} {
		_, ok := s.Classify(l)
		assert.False(t, ok, name)
	}

	oneoff := labelsFor(filepath.Join(root, "services", "jellyfin"))
	oneoff[docker.LabelOneOff] = "True"
	_, ok = s.Classify(oneoff)
	assert.False(t, ok, "compose run containers are never managed")
}

func TestBootStartingAndShouldRestart(t *testing.T) {
	assert.False(t, BootStarting(""))
	assert.False(t, BootStarting("no"))
	for _, p := range []string{"always", "unless-stopped", "on-failure"} {
		assert.True(t, BootStarting(p), p)
	}

	assert.True(t, ShouldRestart("always", "0"))
	assert.True(t, ShouldRestart("unless-stopped", "137"))
	assert.True(t, ShouldRestart("on-failure", "1"))
	assert.False(t, ShouldRestart("on-failure", "0"), "a clean exit is not a failure")
	assert.False(t, ShouldRestart("no", "1"), "one-shot containers stay down")
}

func TestNeedsRebind(t *testing.T) {
	home := "/home/u"
	cutoff := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	before, after := cutoff.Add(-time.Hour), cutoff.Add(time.Minute)
	c := docker.ContainerInfo{State: "running", StartedAt: before, BindSources: []string{"/home/u/.config/homelab/caddy/Caddyfile"}}

	assert.True(t, NeedsRebind(c, cutoff, home))

	late := c
	late.StartedAt = after
	assert.False(t, NeedsRebind(late, cutoff, home), "started in this session")

	stopped := c
	stopped.State = "exited"
	assert.False(t, NeedsRebind(stopped, cutoff, home), "not running: up starts it fresh")

	outside := c
	outside.BindSources = []string{"/var/run/docker.sock", "/home/user2/x", "/home/u2"}
	assert.False(t, NeedsRebind(outside, cutoff, home), "nothing under home (prefix is not containment)")

	assert.False(t, NeedsRebind(c, cutoff, ""), "no home known")
}

func TestBootstrapAndShutdownPlan(t *testing.T) {
	root := t.TempDir()
	s := NewScope(root)
	core := labelsFor(filepath.Join(root, "core"))
	svc := func(n string) map[string]string { return labelsFor(filepath.Join(root, "services", n)) }
	cs := []docker.ContainerInfo{
		{Name: "caddy", State: "running", Labels: core},
		{Name: "tailscale", State: "running", Labels: core},
		{Name: "jellyfin", State: "running", Labels: svc("jellyfin")},
		{Name: "homelab-postgres", State: "running", Labels: svc("postgres")},
		{Name: "immich-server", State: "exited", Labels: svc("immich")},
		{Name: "stranger", State: "running", Labels: labelsFor("/srv/x")},
	}

	d := Bootstrap(s, cs)
	assert.True(t, d.CoreRunning())
	assert.True(t, d.ServiceRunning("jellyfin"))
	assert.True(t, d.ServiceRunning("postgres"))
	assert.False(t, d.ServiceRunning("immich"))
	assert.Len(t, d.Services, 2)

	svcs, coreNames := ShutdownPlan(s, cs)
	assert.Equal(t, []string{"jellyfin", "homelab-postgres"}, svcs, "shared databases stop after their users")
	assert.Equal(t, []string{"caddy", "tailscale"}, coreNames)

	empty := Bootstrap(s, nil)
	assert.False(t, empty.CoreRunning())
	assert.Empty(t, empty.RunningServices())
}

func TestPolicies_ObserveAndOriginal(t *testing.T) {
	root := t.TempDir()
	p, err := LoadPolicies(root)
	assert.NoError(t, err)

	assert.True(t, p.Observe("caddy", "always"))
	assert.False(t, p.Observe("caddy", "no"), "already off: the record keeps \"always\"")
	assert.Equal(t, "always", p.Original("caddy"))

	assert.False(t, p.Observe("migrate", "no"))
	assert.Equal(t, "no", p.Original("migrate"), "a container that was \"no\" all along")

	assert.True(t, p.Observe("caddy", "unless-stopped"), "a recreated container brings its compose policy")
	assert.Equal(t, "unless-stopped", p.Original("caddy"))
	assert.Equal(t, "no", p.Original("unknown"))

	assert.NoError(t, p.Save(root))
	got, err := LoadPolicies(root)
	assert.NoError(t, err)
	assert.Equal(t, p, got)
}
