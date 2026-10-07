// Package caddy validates and reloads the running Caddy container, and applies
// exposure changes — enable, disable, re-render — to a service's one Caddy
// file, caddy/sites/<service>.conf, making sure a bad config never outlives a
// reload.
//
// That file is rendered by internal/configgen from the service's
// exposure.yaml and its config.yaml `ports:` declaration or caddy.routes.conf.
package caddy

import (
	"errors"
	"fmt"
	"slices"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
	"github.com/groot/homelab/internal/network"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/run"
)

const (
	caddyContainer = "caddy"
	caddyFile      = "/etc/caddy/Caddyfile"
	caddyAdapter   = "caddyfile"
)

// Manager performs caddy routing operations for a given repo root.
type Manager struct {
	RepoRoot string
	runner   *run.Commander
	// reloadFn, if non-nil, replaces the default Reload() implementation.
	// Inject a no-op in tests to avoid requiring a live Docker/Caddy instance.
	reloadFn func() error
}

// New returns a Manager that streams docker output to the terminal.
func New(repoRoot string) *Manager {
	return &Manager{RepoRoot: repoRoot, runner: run.Default()}
}

// NewWithRunner returns a Manager using a custom Commander.
// Pass a Commander backed by a bytes.Buffer when output must be captured
// (e.g. while an animated spinner is active).
func NewWithRunner(repoRoot string, r *run.Commander) *Manager {
	return &Manager{RepoRoot: repoRoot, runner: r}
}

// newForTest returns a Manager whose Reload() is replaced by fn.
// Only used in tests within this package.
func newForTest(repoRoot string, reloadFn func() error) *Manager {
	return &Manager{RepoRoot: repoRoot, runner: run.Default(), reloadFn: reloadFn}
}

// ── Exposure changes ──────────────────────────────────────────────────────────
//
// Each of these changes a service's exposure.yaml, re-renders its one sites
// file, and reloads Caddy — restoring both files if Caddy rejects the result.

// ReloadService re-renders a service's sites file from its exposure.yaml and
// reloads Caddy once, picking up edits to its config.yaml ports or
// caddy.routes.conf without redeploying. Every layer it is on is re-rendered,
// with the --name and --ports it was enabled with; none is switched on.
func (m *Manager) ReloadService(name string) error {
	st, err := exposure.Load(m.RepoRoot, name)
	if err != nil {
		return err
	}
	if len(st.Layers) == 0 {
		return fmt.Errorf("service %q has no active routes to reload", name)
	}
	snap, err := m.Snapshot(name)
	if err != nil {
		return err
	}
	if err := routing.Sync(m.RepoRoot, layers.New(m.RepoRoot, m.runner, nil), name); err != nil {
		return errors.Join(err, snap.Restore())
	}
	return m.ReloadOrRestore(snap)
}

// Enable puts a service on the layers ls (the private layer included, if the
// caller wants it) and reloads Caddy.
//
// name and ports replace the stored --name and --ports when non-nil; nil keeps
// what the service was enabled with. A name equal to the service name and a
// selection of every declared port are stored as "no override". When either
// changes, layers the service is already on are re-configured too, so an i2p
// tunnel or a tor hidden service never disagrees with its site block.
//
// Any failure — a layer's daemon, rendering, or the reload — undoes the whole
// run: layers it turned on are torn down and both files restored. Without
// that, blocks already written would go live on the next unrelated reload, and
// a failed `--all` could make a service public without anyone noticing. added
// names the layers that were undone.
func (m *Manager) Enable(reg *network.Registry, svc string, ls []network.NetworkLayer, name *string, ports []string) (added []string, err error) {
	root := m.RepoRoot
	old, err := exposure.Load(root, svc)
	if err != nil {
		return nil, err
	}
	st := old
	if name != nil {
		st.Name = *name
		if st.Name == svc {
			st.Name = ""
		}
	}
	if ports != nil {
		st.Ports = ports
	}
	e, err := routing.Resolve(root, svc, st)
	if err != nil {
		return nil, err
	}
	st.Ports = normalizePorts(root, svc, st.Ports)
	changed := st.Name != old.Name || !slices.Equal(st.Ports, old.Ports)

	snap, err := m.Snapshot(svc)
	if err != nil {
		return nil, err
	}
	var turnedOn []network.NetworkLayer
	undo := func(cause error) ([]string, error) {
		var names []string
		for _, l := range turnedOn {
			_ = routing.Teardown(l, svc)
			names = append(names, l.Name())
		}
		return names, errors.Join(cause, snap.Restore())
	}

	selected := map[string]bool{}
	for _, l := range ls {
		selected[l.Name()] = true
	}
	for _, l := range reg.All() {
		if !selected[l.Name()] && !(changed && old.On(l.Name())) {
			continue
		}
		if !old.On(l.Name()) {
			turnedOn = append(turnedOn, l)
		}
		if err := routing.Configure(l, svc, st, e); err != nil {
			return undo(fmt.Errorf("%s: %w", l.Name(), err))
		}
	}
	if err := routing.Apply(root, reg, svc, func(s *exposure.State) {
		for _, l := range ls {
			s.Set(l.Name(), true)
		}
		s.Name, s.Ports = st.Name, st.Ports
	}); err != nil {
		return undo(err)
	}
	if err := m.ReloadOrRestore(snap); err != nil {
		return undo(err)
	}
	return nil, nil
}

// Disable takes a service off the layers ls and reloads Caddy, then tears the
// layers' daemon side down. If Caddy rejects the new config, both files are
// restored and no daemon is touched. If Caddy merely is not running, the
// change stands — it applies when Caddy starts — and the error is returned
// for the caller to report.
func (m *Manager) Disable(reg *network.Registry, svc string, ls []network.NetworkLayer) error {
	snap, err := m.Snapshot(svc)
	if err != nil {
		return err
	}
	if err := routing.Apply(m.RepoRoot, reg, svc, func(s *exposure.State) {
		for _, l := range ls {
			s.Set(l.Name(), false)
		}
	}); err != nil {
		return errors.Join(err, snap.Restore())
	}
	reloadErr := m.ReloadOrRestore(snap)
	if errors.Is(reloadErr, ErrInvalidConfig) {
		return reloadErr
	}
	// The route is what exposes the service, so a daemon that will not tear
	// down is not a failure of disable — as it never was.
	for _, l := range ls {
		_ = routing.Teardown(l, svc)
	}
	return reloadErr
}

// normalizePorts stores a --ports selection only when it is one: a selection
// naming every declared port, or any selection for a routes-driven service
// (which routes by path in one block), is "all ports" and stored as nothing.
func normalizePorts(root, svc string, selected []string) []string {
	if len(selected) == 0 {
		return nil
	}
	info, err := configgen.LoadServiceInfo(root, svc)
	if err != nil || info.Routes != "" {
		return nil
	}
	for name := range info.Ports {
		if !slices.Contains(selected, name) {
			return selected
		}
	}
	return nil
}

// ── Caddy lifecycle ───────────────────────────────────────────────────────────

// Validate runs `caddy validate` inside the caddy container.
func (m *Manager) Validate() error {
	return m.runner.DockerExec(caddyContainer,
		"caddy", "validate",
		"--config", caddyFile,
		"--adapter", caddyAdapter,
	)
}

// Reload validates then gracefully reloads Caddy (zero downtime, no cert re-issuance).
// If reloadFn is set (e.g. in tests), it is called instead.
func (m *Manager) Reload() error {
	if m.reloadFn != nil {
		return m.reloadFn()
	}
	if err := m.Validate(); err != nil {
		// A stopped Caddy fails validation too; that is not a config problem
		// and must not trigger a rollback in ReloadOrRestore.
		if m.runner.ContainerStatus(caddyContainer) != "running" {
			return fmt.Errorf("caddy validate failed (is the caddy container running?): %w", err)
		}
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	// Use --force to ensure the admin API fully replaces the active config
	// rather than skipping if the new config is structurally identical.
	return m.runner.DockerExec(caddyContainer,
		"caddy", "reload",
		"--config", caddyFile,
		"--adapter", caddyAdapter,
		"--force",
	)
}
