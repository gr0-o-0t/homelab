// Package network_test tests the NetworkLayer interface contract and Registry.
package network_test

import (
	"testing"

	"github.com/groot/homelab/internal/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Fake layer for testing ────────────────────────────────────────────────────

type fakeLayer struct {
	name          string
	label         string
	containerName string
}

func (f *fakeLayer) Name() string           { return f.name }
func (f *fakeLayer) Label() string          { return f.label }
func (f *fakeLayer) ContainerName() string  { return f.containerName }
func (f *fakeLayer) Profile() string        { return f.name }
func (f *fakeLayer) Start() error           { return nil }
func (f *fakeLayer) Stop() error            { return nil }
func (f *fakeLayer) Status() network.Status { return network.Status{ContainerState: "running"} }
func (f *fakeLayer) Enable(_, _ string, _ network.ServiceInfo, _ []network.PortSelection) error {
	return nil
}
func (f *fakeLayer) Disable(_ string) error { return nil }
func (f *fakeLayer) CaddyConfigDir(_ string) string {
	return "caddy/conf.d-" + f.name
}

// ── Registry tests ────────────────────────────────────────────────────────────

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := network.NewRegistry()
	layer := &fakeLayer{name: "test", label: "Test Layer", containerName: "test"}
	r.Register(layer)

	got, ok := r.Get("test")
	require.True(t, ok, "Get should find registered layer")
	assert.Equal(t, "test", got.Name())
	assert.Equal(t, "Test Layer", got.Label())
	assert.Equal(t, "test", got.ContainerName())
}

func TestRegistry_RegisterDuplicatePanics(t *testing.T) {
	r := network.NewRegistry()
	r.Register(&fakeLayer{name: "dup", label: "first", containerName: "first"})
	assert.Panics(t, func() {
		r.Register(&fakeLayer{name: "dup", label: "second", containerName: "second"})
	}, "duplicate registration should panic")
}

func TestRegistry_GetNotFound(t *testing.T) {
	r := network.NewRegistry()
	_, ok := r.Get("nonexistent")
	assert.False(t, ok, "Get should return false for unregistered layer")
}

func TestRegistry_All(t *testing.T) {
	r := network.NewRegistry()
	r.Register(&fakeLayer{name: "a"})
	r.Register(&fakeLayer{name: "b"})

	all := r.All()
	assert.Len(t, all, 2)
}

func TestRegistry_Names(t *testing.T) {
	r := network.NewRegistry()
	r.Register(&fakeLayer{name: "tor"})
	r.Register(&fakeLayer{name: "cf"})

	names := r.Names()
	assert.ElementsMatch(t, []string{"tor", "cf"}, names)
}

func TestRegistry_Has(t *testing.T) {
	r := network.NewRegistry()
	r.Register(&fakeLayer{name: "tor"})

	assert.True(t, r.Has("tor"))
	assert.False(t, r.Has("cf"))
}

func TestRegistry_Empty(t *testing.T) {
	r := network.NewRegistry()
	assert.Empty(t, r.All())
	assert.Empty(t, r.Names())
	assert.False(t, r.Has("anything"))
}
