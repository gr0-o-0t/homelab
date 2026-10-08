package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/session"
	"github.com/groot/homelab/internal/tui/styles"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Run the stack with your login session (systemd user service)",
	Long: `Session mode starts the homelab stack when you log in and stops it when you
log out, instead of Docker starting it at boot.

Docker starts "restart: always" containers at boot, before login. With an
encrypted (ecryptfs, fscrypt, systemd-homed) or otherwise late-mounted home,
every bind mount into the config dir then points at an empty placeholder:
single-file mounts fail, directory mounts serve nothing. Session mode sets
every homelab container's restart policy to "no" and runs a supervisor as a
systemd user service that restores the desired state (state/desired.yaml) at
login and restarts crashed containers itself.

  homelab session install     # write and start ~/.config/systemd/user/homelab.service
  homelab session status      # unit state, desired state, boot-starting containers
  homelab session uninstall   # remove it and restore the compose restart policies`,
}

var sessionInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the systemd user service; turn restart policies off",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		m, closeFn, err := sessionManager()
		if err != nil {
			return err
		}
		defer closeFn()
		if err := m.Install(cmdContext()); err != nil {
			return err
		}
		fmt.Printf("\n  %s  The stack now starts at login and stops at logout. Logs: %s\n\n",
			styles.Muted.Render("→"), styles.Primary.Render("journalctl --user -u homelab -f"))
		return nil
	},
}

var sessionUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the systemd user service and restore the compose restart policies",
	Long: `Stop the supervisor (containers keep running), remove the unit, and put back
each container's restart policy as recorded when session mode turned it off
(state/restart-policies.yaml) with docker update. Docker starts the stack at
boot again afterwards.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		m, closeFn, err := sessionManager()
		if err != nil {
			return err
		}
		defer closeFn()
		return m.Uninstall(cmdContext())
	},
}

var sessionStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show session mode: unit, desired state, boot-starting containers",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		m, closeFn, err := sessionManager()
		if err != nil {
			return err
		}
		defer closeFn()
		printSessionStatus(os.Stdout, m.Status(cmdContext()))
		return nil
	},
}

// sessionRunCmd is what the unit runs; not meant to be typed.
var sessionRunCmd = &cobra.Command{
	Use:    "run",
	Short:  "Run the session supervisor (used by the systemd unit)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		dc, err := docker.New()
		if err != nil {
			return err
		}
		defer func() { _ = dc.Close() }()
		root := absPath(configDir())
		home, _ := os.UserHomeDir()
		sup := &session.Supervisor{
			Root:       root,
			ConfigFile: rootConfigFile(),
			Home:       home,
			CLI:        append(selfCLI(root), "--no-record"),
			Docker:     dc,
			Run:        execRunner(os.Stdout, os.Stderr),
			Log:        log.New(os.Stdout, "", 0),
		}
		// Exit 0 even when stopping failed: it is logged, and a failed unit
		// would only be restarted into a session that is ending.
		_ = sup.Serve(ctx, sessionStopTimeout)
		return nil
	},
}

// sessionStopTimeout bounds stopping the stack; below the unit's
// TimeoutStopSec=120 so the supervisor exits before systemd kills it.
const sessionStopTimeout = 110 * time.Second

func init() {
	sessionCmd.AddCommand(sessionInstallCmd, sessionUninstallCmd, sessionStatusCmd, sessionRunCmd)
	rootCmd.AddCommand(sessionCmd)
	rootCmd.PersistentFlags().BoolVar(&rootFlags.noRecord, "no-record", false,
		"don't record the desired state (used by the session supervisor)")
	_ = rootCmd.PersistentFlags().MarkHidden("no-record")
}

// Injected in tests (see TestMain): nothing may reach the real systemd.
var (
	sessionSystemctl session.Systemctl = realSystemctl
	sessionUnitPath                    = session.UnitPath
	sessionRunner                      = func() session.Runner { return execRunner(os.Stdout, os.Stderr) }
	sessionDocker                      = func() (session.Docker, func()) {
		dc, err := docker.New()
		if err != nil {
			return nil, func() {}
		}
		return dc, func() { _ = dc.Close() }
	}
)

func realSystemctl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output() //nolint:gosec // fixed binary
	return strings.TrimSpace(string(out)), err
}

// execRunner runs argv with output to the given writers. On cancellation the
// child gets SIGTERM, not SIGKILL, so a `docker compose up` can finish cleanly.
func execRunner(stdout, stderr io.Writer) session.Runner {
	return func(ctx context.Context, argv ...string) error {
		c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // docker or this binary
		c.Stdout, c.Stderr = stdout, stderr
		c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
		c.WaitDelay = 30 * time.Second
		if err := c.Run(); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
		}
		return nil
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// sessionArgv is the unit's ExecStart: this binary with the config selection
// selfCLI passes, made absolute (systemd runs it from /).
func sessionArgv(root string) []string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	argv := []string{absPath(exe), "--no-color", "--config-dir", absPath(root)}
	if rootFlags.configFile != "" {
		argv = append(argv, "--config", absPath(rootFlags.configFile))
	}
	return append(argv, "session", "run")
}

// sessionEnv carries the Docker connection settings of the installing shell
// into the unit: the user manager does not inherit them.
func sessionEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func sessionManager() (*session.Manager, func(), error) {
	unit, err := sessionUnitPath()
	if err != nil {
		return nil, nil, err
	}
	root := configDir()
	dc, closeFn := sessionDocker()
	return &session.Manager{
		Root:      root,
		UnitPath:  unit,
		Argv:      sessionArgv(root),
		Env:       sessionEnv(),
		Systemctl: sessionSystemctl,
		Docker:    dc,
		Run:       sessionRunner(),
		Out:       os.Stdout,
	}, closeFn, nil
}

func printSessionStatus(w io.Writer, st session.Status) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n%s\n\n", styles.Header.Render("Session mode"))
	installed := styles.Muted.Render("not installed")
	if st.Installed {
		installed = styles.Success.Render("installed")
	}
	p("  %-9s %s (%s)\n", "unit", st.UnitPath, installed)
	if st.Installed {
		active := st.Active
		if active == "active" {
			active = styles.Success.Render(active)
		} else {
			active = styles.Warning.Render(orDash(active))
		}
		p("  %-9s %s, %s\n", "service", active, orDash(st.Enabled))
	}
	p("  %-9s %s\n", "desired", desiredSummary(st.Desired, st.DesiredFile))
	switch {
	case st.DockerErr != nil:
		p("  %-9s %s\n", "restart", styles.Warning.Render("docker unavailable: "+st.DockerErr.Error()))
	case len(st.BootStarting) == 0:
		p("  %-9s %s\n", "restart", "no homelab container starts at boot")
	default:
		p("  %-9s %s\n", "restart", styles.Warning.Render(fmt.Sprintf("%d container(s) still start at boot: %s",
			len(st.BootStarting), strings.Join(st.BootStarting, " "))))
	}
	if !st.Installed {
		p("\n  %s  Run %s to start the stack at login instead of at boot\n",
			styles.Muted.Render("→"), styles.Primary.Render("homelab session install"))
	}
	p("\n")
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func desiredSummary(d *session.Desired, exists bool) string {
	if !exists || d == nil {
		return styles.Muted.Render("not recorded yet (recorded by the next lifecycle command or session install)")
	}
	running, stopped := 0, 0
	for _, s := range d.Services {
		if s == session.Running {
			running++
		} else {
			stopped++
		}
	}
	core := string(d.Core)
	if core == "" {
		core = "not recorded"
	}
	return fmt.Sprintf("core %s · %d service(s) running · %d stopped", core, running, stopped)
}

// sessionStatusLine is the one line `homelab status` shows about session mode.
func sessionStatusLine(root string) string {
	unit, err := sessionUnitPath()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(unit); err == nil {
		active, _ := sessionSystemctl(cmdContext(), "is-active", session.UnitName)
		if active == "active" {
			return fmt.Sprintf("%s  Session mode: active — the stack follows your login session",
				styles.Success.Render("✓"))
		}
		return fmt.Sprintf("%s  Session mode: installed but %s — %s",
			styles.Warning.Render("!"), orDash(active), styles.Primary.Render("systemctl --user status homelab"))
	}
	home, _ := os.UserHomeDir()
	if fstype, late := session.LateMount(absPath(root), home); late {
		n := bootStartingCount(root)
		if n != 0 {
			return fmt.Sprintf("%s  Config dir is on %s, mounted at login: %d container(s) start at boot with empty bind mounts. Run %s",
				styles.Warning.Render("!"), fstype, n, styles.Primary.Render("homelab session install"))
		}
	}
	return fmt.Sprintf("%s  Run %s to start the stack at login instead of at boot",
		styles.Muted.Render("→"), styles.Primary.Render("homelab session install"))
}

// bootStartingCount counts homelab containers Docker starts at boot; -1 when
// Docker cannot be asked.
func bootStartingCount(root string) int {
	dc, closeFn := sessionDocker()
	defer closeFn()
	if dc == nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := dc.ComposeContainers(ctx)
	if err != nil {
		return -1
	}
	return len(session.BootStartingNames(session.NewScope(root), cs))
}

// ── desired state recording ──────────────────────────────────────────────────

// recordCore and recordService note what a lifecycle command asked for in
// state/desired.yaml, before it runs: the intent is what the next login should
// restore, and a die event caused by a stop must already read "stopped".
// Failures only warn — the state file never blocks a lifecycle command.
func recordCore(root string, st session.State) {
	updateDesired(root, func(d *session.Desired) { d.SetCore(st) })
}

func recordService(root, name string, st session.State) {
	updateDesired(root, func(d *session.Desired) { d.SetService(name, st) })
}

// desiredBootstrap lists the containers to infer a first desired state from;
// replaced in tests.
var desiredBootstrap = func(root string) (*session.Desired, error) {
	dc, closeFn := sessionDocker()
	defer closeFn()
	if dc == nil {
		return nil, fmt.Errorf("docker unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := dc.ComposeContainers(ctx)
	if err != nil {
		return nil, err
	}
	return session.Bootstrap(session.NewScope(root), cs), nil
}

func updateDesired(root string, fn func(*session.Desired)) {
	if rootFlags.noRecord {
		return
	}
	d, exists, err := session.LoadDesired(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: desired state: %v\n", err)
		return
	}
	if !exists {
		// First record: start from what runs now. Without Docker, write
		// nothing rather than a file claiming everything else is stopped.
		b, err := desiredBootstrap(root)
		if err != nil {
			return
		}
		d = b
	}
	fn(d)
	if err := d.Save(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: desired state: %v\n", err)
	}
}
