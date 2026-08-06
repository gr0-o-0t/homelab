// Package caddy manages Caddy routing: enabling/disabling services via
// symlinks into caddy/conf.d/ (private) and caddy/conf.d-cf/ (Cloudflare),
// and reloading the running Caddy container.
//
// # ROUTING SYSTEMS
//
// Caddy config is managed through two parallel systems:
//
//  1. Legacy symlink path (this package): services/<name>/caddy.conf →
//     caddy/conf.d/<name>.conf. Used by older services that ship static
//     caddy.conf files. Manager.Enable() symlinks; Manager.Disable() removes.
//
//  2. Modern configgen path (configgen.WriteFile): generates Caddy config
//     blocks from config.yaml's ports: declaration. Used by newer services.
//     Config is written directly (not symlinked).
//
// DisableBoth() tries both paths. New services should prefer the ports:
// declaration approach in config.yaml.
package caddy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/configgen"
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

// ── Private routing (tailnet) ─────────────────────────────────────────────────

// hasRoutes reports whether a service ships a caddy.routes.conf. Such services
// have no caddy.conf / caddy.cf.conf to symlink — their config for every layer
// is generated — so each symlink operation below has to defer to configgen.
// Without this, the TUI's enable/disable actions (which call this package
// directly rather than going through `homelab enable`) fail on them.
func (m *Manager) hasRoutes(name string) bool {
	info, err := configgen.LoadServiceInfo(m.RepoRoot, name)
	return err == nil && info.Routes != ""
}

// writeRoutesLayer generates and writes one layer's config for a routes-driven
// service, then reloads Caddy.
func (m *Manager) writeRoutesLayer(name, ext string) error {
	blocks, err := configgen.Generate(configgen.Request{
		ServiceName: name,
		Extensions:  []string{ext},
		ConfigDir:   m.RepoRoot,
	})
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := configgen.WriteFile(m.RepoRoot, ext, name, b.PortName, b.Content); err != nil {
			return fmt.Errorf("writing %s config: %w", ext, err)
		}
	}
	return m.Reload()
}

// generatedExists reports whether a routes-driven service's generated config for
// a layer is in place. Generated config is a regular file, not a symlink, so
// isSymlink would always say "disabled".
func (m *Manager) generatedExists(name, ext string) (bool, error) {
	_, err := os.Stat(configgen.GeneratedFilePath(m.RepoRoot, ext, name, ""))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Enable symlinks services/<name>/caddy.conf into caddy/conf.d/<name>.conf
// and triggers a graceful Caddy reload. Routes-driven services get their
// private block generated instead.
func (m *Manager) Enable(name string) error {
	if m.hasRoutes(name) {
		return m.writeRoutesLayer(name, "private")
	}
	src := filepath.Join(m.RepoRoot, "services", name, "caddy.conf")
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d", name+".conf")
	relTarget := filepath.Join("..", "..", "services", name, "caddy.conf")
	return m.link(src, dest, relTarget, name, "caddy.conf")
}

// Disable removes the caddy/conf.d/<name>.conf symlink and reloads Caddy.
func (m *Manager) Disable(name string) error {
	if m.hasRoutes(name) {
		if err := configgen.RemoveAllPortFiles(m.RepoRoot, "private", name); err != nil {
			return err
		}
		return m.Reload()
	}
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d", name+".conf")
	return m.unlink(dest, name, "private")
}

// IsEnabled reports whether the private route is active.
func (m *Manager) IsEnabled(name string) (bool, error) {
	if m.hasRoutes(name) {
		return m.generatedExists(name, "private")
	}
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d", name+".conf")
	return isSymlink(dest)
}

// ── Public routing (Cloudflare Tunnel) ───────────────────────────────────────

// EnablePublic symlinks services/<name>/caddy.cf.conf into
// caddy/conf.d-cf/<name>.conf and triggers a graceful Caddy reload.
func (m *Manager) EnablePublic(name string) error {
	if m.hasRoutes(name) {
		return m.writeRoutesLayer(name, "cf")
	}
	src := filepath.Join(m.RepoRoot, "services", name, "caddy.cf.conf")
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d-cf", name+".conf")
	relTarget := filepath.Join("..", "..", "services", name, "caddy.cf.conf")
	return m.link(src, dest, relTarget, name, "caddy.cf.conf")
}

// DisablePublic removes the caddy/conf.d-cf/<name>.conf symlink and reloads Caddy.
func (m *Manager) DisablePublic(name string) error {
	if m.hasRoutes(name) {
		if err := configgen.RemoveAllPortFiles(m.RepoRoot, "cf", name); err != nil {
			return err
		}
		return m.Reload()
	}
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d-cf", name+".conf")
	return m.unlink(dest, name, "public")
}

// IsPublicEnabled reports whether the public route is active.
func (m *Manager) IsPublicEnabled(name string) (bool, error) {
	if m.hasRoutes(name) {
		return m.generatedExists(name, "cf")
	}
	dest := filepath.Join(m.RepoRoot, "caddy", "conf.d-cf", name+".conf")
	return isSymlink(dest)
}

// ── Combined ──────────────────────────────────────────────────────────────────

// DisableBoth removes both the private and public routes for name and
// performs a single Caddy reload. Handles both generated config files
// (written by configgen.WriteFile) and legacy symlinks. NotFound errors
// are silently ignored — the operation is idempotent.
func (m *Manager) DisableBoth(name string) error {
	// Remove generated config files (modern path) — a multi-port service can
	// have one file per port, so this must remove all of them, not just the
	// default-named one.
	_ = configgen.RemoveAllPortFiles(m.RepoRoot, "private", name)
	_ = configgen.RemoveAllPortFiles(m.RepoRoot, "cf", name)

	// Also try legacy symlink removal (backward compat).
	privateLink := filepath.Join(m.RepoRoot, "caddy", "conf.d", name+".conf")
	publicLink := filepath.Join(m.RepoRoot, "caddy", "conf.d-cf", name+".conf")

	for _, link := range []string{privateLink, publicLink} {
		fi, err := os.Lstat(link)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists but is not a symlink — refusing to remove", link)
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("removing symlink: %w", err)
		}
	}

	return m.Reload()
}

// ── Per-service config reload ─────────────────────────────────────────────────

// ReloadService re-links the private and public Caddy config symlinks for a
// service and reloads Caddy. Missing config files are silently skipped so the
// command is safe to run on any service regardless of which routes are active.
func (m *Manager) ReloadService(name string) error {
	// Routes-driven services have nothing to re-link: regenerate instead. Only
	// layers already in place are rewritten, so a reload never switches a layer
	// on that the user had not enabled.
	if m.hasRoutes(name) {
		reloaded := false
		for _, ext := range []string{"private", "cf"} {
			active, err := m.generatedExists(name, ext)
			if err != nil {
				return err
			}
			if !active {
				continue
			}
			if err := m.writeRoutesLayer(name, ext); err != nil {
				return err
			}
			reloaded = true
		}
		if !reloaded {
			return fmt.Errorf("service %q has no active routes to reload", name)
		}
		return nil
	}

	linked := false

	privateSrc := filepath.Join(m.RepoRoot, "services", name, "caddy.conf")
	privateDest := filepath.Join(m.RepoRoot, "caddy", "conf.d", name+".conf")
	if _, err := os.Stat(privateSrc); err == nil {
		relTarget := filepath.Join("..", "..", "services", name, "caddy.conf")
		if err := m.replaceSymlink(privateDest, relTarget); err != nil {
			return fmt.Errorf("re-linking private config: %w", err)
		}
		linked = true
	}

	publicSrc := filepath.Join(m.RepoRoot, "services", name, "caddy.cf.conf")
	publicDest := filepath.Join(m.RepoRoot, "caddy", "conf.d-cf", name+".conf")
	if _, err := os.Stat(publicSrc); err == nil {
		relTarget := filepath.Join("..", "..", "services", name, "caddy.cf.conf")
		if err := m.replaceSymlink(publicDest, relTarget); err != nil {
			return fmt.Errorf("re-linking public config: %w", err)
		}
		linked = true
	}

	if !linked {
		return fmt.Errorf("no caddy.conf or caddy.cf.conf found for service %q", name)
	}

	return m.Reload()
}

// replaceSymlink removes any existing symlink at dest and creates a new one.
func (m *Manager) replaceSymlink(dest, relTarget string) error {
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Remove(dest); err != nil {
			return fmt.Errorf("removing old symlink: %w", err)
		}
	}
	return os.Symlink(relTarget, dest)
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
		return fmt.Errorf("caddy validate failed: %w", err)
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

// ── internal helpers ──────────────────────────────────────────────────────────

// link creates a relative symlink at dest pointing to relTarget, first
// verifying that src exists. Any stale symlink at dest is replaced.
func (m *Manager) link(src, dest, relTarget, name, confFile string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return fmt.Errorf("no %s found for service %q (expected %s)", confFile, name, src)
	}

	// Remove stale symlink if present (re-link to pick up any path changes).
	if _, err := os.Lstat(dest); err == nil {
		if err := os.Remove(dest); err != nil {
			return fmt.Errorf("removing existing link: %w", err)
		}
	}

	if err := os.Symlink(relTarget, dest); err != nil {
		return fmt.Errorf("creating symlink: %w", err)
	}

	return m.Reload()
}

// unlink removes a symlink at dest, erroring if it does not exist or is not a symlink.
func (m *Manager) unlink(dest, name, routeType string) error {
	fi, err := os.Lstat(dest)
	if os.IsNotExist(err) {
		return fmt.Errorf("service %q has no active %s route", name, routeType)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s exists but is not a symlink — refusing to remove", dest)
	}
	if err := os.Remove(dest); err != nil {
		return fmt.Errorf("removing symlink: %w", err)
	}
	return m.Reload()
}

// isSymlink reports whether path is an existing symlink.
func isSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}
