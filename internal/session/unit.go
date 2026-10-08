package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/docker"
)

// UnitName is the systemd user unit session mode installs.
const UnitName = "homelab.service"

// UnitPath is ${XDG_CONFIG_HOME:-~/.config}/systemd/user/homelab.service.
func UnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", UnitName), nil
}

// RenderUnit renders the user unit running argv (binary first) as the
// supervisor. env is written as Environment= lines, sorted.
func RenderUnit(argv []string, env map[string]string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = SystemdQuote(a)
	}
	var b strings.Builder
	b.WriteString(`# Written by "homelab session install"; "homelab session uninstall" removes it.
[Unit]
Description=homelab stack for this login session (restore at login, stop at logout)

[Service]
Type=simple
ExecStart=` + strings.Join(quoted, " ") + `
`)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("Environment=" + SystemdQuote(k+"="+env[k]) + "\n")
	}
	b.WriteString(`Restart=on-failure
RestartSec=5
# Stopping the stack at logout runs "docker stop" on every container.
TimeoutStopSec=120
# Only the supervisor gets SIGTERM; it stops the containers itself.
KillMode=mixed

[Install]
WantedBy=default.target
`)
	return b.String()
}

// SystemdQuote quotes one word for an ExecStart= or Environment= line: double
// quotes with \ and " escaped, % doubled (specifiers) and $ doubled
// (variable expansion), so the word reaches the process exactly as given.
func SystemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// Docker is the read-only Docker access session mode needs; *docker.Client
// implements it.
type Docker interface {
	PingOK(ctx context.Context) error
	ComposeContainers(ctx context.Context) ([]docker.ContainerInfo, error)
	InspectInfo(ctx context.Context, id string) (docker.ContainerInfo, bool, error)
	ContainerEvents(ctx context.Context) (<-chan docker.Event, <-chan error)
}

// Runner runs a program (argv[0]) to completion: docker update/start/stop and
// this binary's own lifecycle commands.
type Runner func(ctx context.Context, argv ...string) error

// Systemctl runs `systemctl --user <args>` and returns its trimmed stdout.
type Systemctl func(ctx context.Context, args ...string) (string, error)

// Manager installs, removes and reports on session mode.
type Manager struct {
	Root      string
	UnitPath  string
	Argv      []string          // the supervisor's command line, binary first
	Env       map[string]string // Environment= lines (DOCKER_HOST, …)
	Systemctl Systemctl
	Docker    Docker // nil when the daemon is unreachable
	Run       Runner
	Out       io.Writer
}

// DetachFile is the marker telling a stopping supervisor to leave the
// containers running: written around a reinstall's restart and uninstall's
// stop, which must not take the stack down.
func DetachFile(root string) string { return filepath.Join(StateDir(root), "session-detach") }

func (m *Manager) printf(format string, a ...any) {
	if m.Out != nil {
		_, _ = fmt.Fprintf(m.Out, format, a...)
	}
}

func (m *Manager) containers(ctx context.Context) ([]docker.ContainerInfo, error) {
	if m.Docker == nil {
		return nil, errors.New("docker unavailable")
	}
	return m.Docker.ComposeContainers(ctx)
}

// Install writes the unit, records the desired state if there is none yet
// (from what runs now — before the restart policies change), turns every
// homelab container's restart policy off and (re)starts the supervisor.
// Running it again rewrites the unit and restarts the supervisor without
// stopping any container.
func (m *Manager) Install(ctx context.Context) error {
	// Docker is required: the desired state is recorded from what runs now,
	// and an empty record would restore nothing at the next login.
	cs, err := m.containers(ctx)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if _, exists, err := LoadDesired(m.Root); err != nil {
		return err
	} else if !exists {
		d := Bootstrap(NewScope(m.Root), cs)
		if err := d.Save(m.Root); err != nil {
			return fmt.Errorf("recording desired state: %w", err)
		}
		m.printf("✓ Desired state recorded from running containers: core %s, %d service(s) running\n",
			d.Core, len(d.RunningServices()))
	}

	if err := os.MkdirAll(filepath.Dir(m.UnitPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(m.UnitPath, []byte(RenderUnit(m.Argv, m.Env)), 0o644); err != nil { //nolint:gosec // a unit file is not secret
		return err
	}
	m.printf("✓ Wrote %s\n", m.UnitPath)
	if _, err := m.Systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}

	n, err := DisableRestartPolicies(ctx, m.Root, NewScope(m.Root), cs, m.Run)
	if err != nil {
		return err
	}
	m.printf("✓ Restart policy set to \"no\" on %d container(s)\n", n)

	if _, err := m.Systemctl(ctx, "enable", UnitName); err != nil {
		return fmt.Errorf("systemctl --user enable: %w", err)
	}
	// restart, not start: a reinstall must pick up a changed unit. The detach
	// marker makes a running supervisor exit without stopping the stack.
	if err := m.detached(func() error {
		_, err := m.Systemctl(ctx, "restart", UnitName)
		return err
	}); err != nil {
		return fmt.Errorf("systemctl --user restart: %w", err)
	}
	m.printf("✓ %s enabled and started\n", UnitName)
	return nil
}

func (m *Manager) detached(fn func() error) error {
	if err := os.MkdirAll(StateDir(m.Root), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(DetachFile(m.Root), nil, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(DetachFile(m.Root)) }()
	return fn()
}

// Uninstall stops the supervisor (leaving the containers running), removes
// the unit and puts back the restart policies recorded when session mode
// turned them off. The desired state file stays: lifecycle commands keep it.
func (m *Manager) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(m.UnitPath); err == nil {
		if err := m.detached(func() error {
			_, err := m.Systemctl(ctx, "disable", "--now", UnitName)
			return err
		}); err != nil {
			m.printf("! systemctl --user disable --now: %v\n", err)
		}
		if err := os.Remove(m.UnitPath); err != nil {
			return err
		}
		m.printf("✓ Removed %s\n", m.UnitPath)
		if _, err := m.Systemctl(ctx, "daemon-reload"); err != nil {
			m.printf("! systemctl --user daemon-reload: %v\n", err)
		}
	} else {
		m.printf("· %s is not installed\n", m.UnitPath)
	}

	cs, err := m.containers(ctx)
	if err != nil {
		return fmt.Errorf("restoring restart policies: %w", err)
	}
	n, err := RestoreRestartPolicies(ctx, m.Root, NewScope(m.Root), cs, m.Run)
	if err != nil {
		return err
	}
	m.printf("✓ Restored the compose restart policy on %d container(s)\n", n)
	return nil
}

// DisableRestartPolicies records each managed container's restart policy and
// sets the boot-starting ones to "no" in one `docker update`. Returns how many
// were changed.
func DisableRestartPolicies(ctx context.Context, root string, s Scope, cs []docker.ContainerInfo, run Runner) (int, error) {
	pol, err := LoadPolicies(root)
	if err != nil {
		return 0, err
	}
	var fix []string
	managed := s.Managed(cs)
	present := make(map[string]bool, len(managed))
	for _, c := range managed {
		present[c.Name] = true
		if pol.Observe(c.Name, c.RestartPolicy) {
			fix = append(fix, c.Name)
		}
	}
	// Forget containers that no longer exist (a deleted or pruned service), so
	// the record does not grow forever. Skipped on an empty listing: that is
	// more likely a daemon hiccup than every container gone.
	if len(managed) > 0 {
		for name := range pol {
			if !present[name] {
				delete(pol, name)
			}
		}
	}
	// The record first: a policy set to "no" without it would be lost.
	if err := pol.Save(root); err != nil {
		return 0, err
	}
	if len(fix) == 0 {
		return 0, nil
	}
	sort.Strings(fix)
	if err := run(ctx, append([]string{"docker", "update", "--restart=no"}, fix...)...); err != nil {
		return 0, fmt.Errorf("docker update --restart=no: %w", err)
	}
	return len(fix), nil
}

// RestoreRestartPolicies sets every managed container whose policy is "no"
// back to its recorded one, one `docker update` per policy, then drops the
// record. A container with no record keeps "no": it had no other.
func RestoreRestartPolicies(ctx context.Context, root string, s Scope, cs []docker.ContainerInfo, run Runner) (int, error) {
	pol, err := LoadPolicies(root)
	if err != nil {
		return 0, err
	}
	byPolicy := map[string][]string{}
	for _, c := range s.Managed(cs) {
		if orig := pol.Original(c.Name); BootStarting(orig) && !BootStarting(c.RestartPolicy) {
			byPolicy[orig] = append(byPolicy[orig], c.Name)
		}
	}
	policies := make([]string, 0, len(byPolicy))
	for p := range byPolicy {
		policies = append(policies, p)
	}
	sort.Strings(policies)
	n := 0
	for _, p := range policies {
		names := byPolicy[p]
		sort.Strings(names)
		if err := run(ctx, append([]string{"docker", "update", "--restart=" + p}, names...)...); err != nil {
			return n, fmt.Errorf("docker update --restart=%s: %w", p, err)
		}
		n += len(names)
	}
	if err := os.Remove(PolicyFile(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return n, err
	}
	return n, nil
}

// Status is what `homelab session status` reports.
type Status struct {
	UnitPath     string
	Installed    bool   // the unit file exists
	Active       string // systemctl is-active: "active", "inactive", "failed", …
	Enabled      string // systemctl is-enabled: "enabled", "disabled", …
	Desired      *Desired
	DesiredFile  bool     // desired.yaml exists
	BootStarting []string // managed containers Docker would still start at boot
	DockerErr    error
}

// Status gathers the report. It changes nothing.
func (m *Manager) Status(ctx context.Context) Status {
	st := Status{UnitPath: m.UnitPath}
	if _, err := os.Stat(m.UnitPath); err == nil {
		st.Installed = true
	}
	// is-active / is-enabled exit non-zero for "inactive"/"disabled" but still
	// print the state, which is all that is wanted here.
	st.Active, _ = m.Systemctl(ctx, "is-active", UnitName)
	st.Enabled, _ = m.Systemctl(ctx, "is-enabled", UnitName)
	st.Desired, st.DesiredFile, _ = LoadDesired(m.Root)
	cs, err := m.containers(ctx)
	st.DockerErr = err
	st.BootStarting = BootStartingNames(NewScope(m.Root), cs)
	return st
}

// BootStartingNames lists the managed containers with a boot-starting restart
// policy, sorted.
func BootStartingNames(s Scope, cs []docker.ContainerInfo) []string {
	var out []string
	for _, c := range s.Managed(cs) {
		if BootStarting(c.RestartPolicy) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ActiveNow reports whether the supervisor is installed and running.
func (st Status) ActiveNow() bool { return st.Installed && st.Active == "active" }
