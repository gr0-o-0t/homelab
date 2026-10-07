package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/scaffold"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// lsCmd lists installed services as a table, like `docker compose ls` — never
// the dashboard, so it is safe in scripts and on a TTY alike.
var lsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List installed services",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		root := configDir()
		svcs, err := discoverServices(root)
		if err != nil {
			return err
		}
		switch {
		case lsQuiet:
			for _, s := range svcs {
				fmt.Println(s.Name)
			}
		case rootFlags.json:
			return printServiceJSON(svcs)
		default:
			printServiceTable(svcs, buildEnv(root, ""), false)
		}
		return nil
	},
}

var lsQuiet bool

func init() {
	lsCmd.Flags().BoolVarP(&lsQuiet, "quiet", "q", false, "Only print service names")
	rootCmd.AddCommand(lsCmd)
}

var logsFlags struct {
	follow, timestamps, tui bool
	tail, since, until      string
}

// ── new ───────────────────────────────────────────────────────────────────────

var newFlags struct {
	container string
	port      string
	dryRun    bool
}

var serviceNewCmd = &cobra.Command{
	Use:   "new [service]",
	Short: "Scaffold a new service directory",
	Long: `Scaffold boilerplate files for a new service.

Interactive wizard (TTY, no flags required):
  homelab new
  homelab new paperless

Non-interactive (all flags required):
  homelab new paperless --container paperless-ngx --port 8000`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := configDir()

		name := ""
		if len(args) > 0 {
			name = args[0]
		}

		// Launch the interactive wizard when running in a terminal and no
		// non-interactive flags were provided.
		if isTTY() && newFlags.container == "" && newFlags.port == "" && !newFlags.dryRun {
			return runWizardTUI(root, name)
		}

		// Non-interactive path — all flags required.
		if name == "" {
			return fmt.Errorf("service name is required in non-interactive mode")
		}
		if newFlags.container == "" || newFlags.port == "" {
			return fmt.Errorf("--container and --port are required in non-interactive mode\n\n  Example: homelab new %s --container %s-app --port 8080", name, name)
		}
		return scaffoldService(root, name, newFlags.container, newFlags.port, newFlags.dryRun)
	},
}

func init() {
	serviceNewCmd.Flags().StringVar(&newFlags.container, "container", "", "Docker container_name used in reverse_proxy")
	serviceNewCmd.Flags().StringVar(&newFlags.port, "port", "", "Port the container listens on")
	serviceNewCmd.Flags().BoolVar(&newFlags.dryRun, "dry-run", false, "Print generated files without writing them")

}

func runServiceUp(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, upFlags.all, upFlags.group, args)
	if err != nil {
		return err
	}
	extra := []string{}
	if upFlags.build {
		extra = append(extra, "--build")
	}
	return forEachService(root, names, func(name string) error { return upOne(root, name, extra...) })
}

// upOne is `up` for one service: shared databases first, then compose up.
// `update` goes through here too, so a pulled service gets the same database
// provisioning as a started one.
func upOne(root, name string, extraArgs ...string) error {
	// Auto-configure root databases section for shared DB services.
	if err := config.EnsureRootDBConfig(rootConfigFile(), name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-configuring databases: %v\n", err)
	}
	ctx := context.Background()
	if err := prepareService(ctx, root, name); err != nil {
		return err
	}
	fmt.Printf("%s Starting %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
	composeFile := run.ServiceComposeFile(root, name)
	env := buildEnv(root, name)
	warnPortCollisions([]string{composeFile}, env, nil)
	if err := run.Default().DockerComposeEnv(composeFile, env, append([]string{"up", "-d"}, extraArgs...)...); err != nil {
		return err
	}
	return bootstrapIfShared(ctx, root, name)
}

// runServiceDown removes a service's containers. Routing is left alone, as
// `docker compose down` leaves port mappings in the compose file: exposure is
// configuration, so `down` then `up` brings a service back exactly as it was.
// While the service is down its routes answer 502. `disable` removes them.
func runServiceDown(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, downFlags.all, downFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		fmt.Printf("%s Removing %s…\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		return run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			"down",
		)
	})
}

func runServiceRestart(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, restartFlags.all, restartFlags.group, args)
	if err != nil {
		return err
	}
	return forEachService(root, names, func(name string) error {
		// Same reasoning as `up`: restarting a service whose database is down
		// just produces connection errors.
		if err := ensureDBDependencies(context.Background(), root, name); err != nil {
			return err
		}
		composeArgs := []string{"restart"}
		verb := "Restarting"
		if restartFlags.build {
			composeArgs = []string{"up", "-d", "--build"}
			verb = "Rebuilding and recreating"
		}
		fmt.Printf("%s %s %s…\n", styles.Primary.Render("→"), verb, styles.Bold.Render(name))
		return run.Default().DockerComposeEnv(run.ServiceComposeFile(root, name), buildEnv(root, name), composeArgs...)
	})
}

func runServiceLogs(_ *cobra.Command, args []string) error {
	name := args[0]
	root := configDir()
	if err := validateService(root, name); err != nil {
		return err
	}
	if logsFlags.tui {
		return runLogTUI(root, name)
	}
	return run.Default().DockerComposeEnv(
		run.ServiceComposeFile(root, name),
		buildEnv(root, name),
		logsArgs()...,
	)
}

// logsArgs translates homelab's logs flags into `docker compose logs` flags;
// they are the same flags, so this is a straight pass-through.
func logsArgs() []string {
	a := []string{"logs"}
	if logsFlags.follow {
		a = append(a, "-f")
	}
	if logsFlags.timestamps {
		a = append(a, "-t")
	}
	for _, kv := range [][2]string{{"--tail", logsFlags.tail}, {"--since", logsFlags.since}, {"--until", logsFlags.until}} {
		if kv[1] != "" {
			a = append(a, kv[0], kv[1])
		}
	}
	return a
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveTargets returns the service names to operate on based on --all, --group, or positional args.
func resolveTargets(root string, all bool, group string, args []string) ([]string, error) {
	if all && group != "" {
		return nil, fmt.Errorf("--all and --group are mutually exclusive")
	}
	if len(args) > 0 && (all || group != "") {
		return nil, fmt.Errorf("cannot combine a service name with --all or --group")
	}
	if len(args) > 0 {
		return args, nil
	}
	if !all && group == "" {
		return nil, fmt.Errorf("service name, --all, or --group <name> required\n\n  Examples:\n    homelab up jellyfin\n    homelab up --all\n    homelab up --group media")
	}

	svcs, err := service.Discover(root)
	if err != nil {
		return nil, err
	}

	if all {
		names := make([]string, len(svcs))
		for i, s := range svcs {
			names[i] = s.Name
		}
		return names, nil
	}

	// group
	cfg, err := config.Load(rootConfigFile())
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if cfg == nil || len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("no groups defined in config.yaml\n\n  Add a groups section:\n    groups:\n      media:\n        - jellyfin\n        - immich")
	}
	members, ok := cfg.Groups[group]
	if !ok {
		groupNames := make([]string, 0, len(cfg.Groups))
		for k := range cfg.Groups {
			groupNames = append(groupNames, k)
		}
		sort.Strings(groupNames)
		return nil, fmt.Errorf("group %q not found\n  Defined groups: %s",
			group, strings.Join(groupNames, ", "))
	}
	return members, nil
}

// ── output helpers ────────────────────────────────────────────────────────────

// discoverServices tries the Docker SDK first for live container data, then
// falls back to plain filesystem discovery if the daemon is unavailable.
func discoverServices(root string) ([]service.Service, error) {
	dc, err := docker.New()
	if err != nil {
		return service.Discover(root)
	}
	defer func() { _ = dc.Close() }()
	return service.DiscoverWithDocker(root, dc)
}

// serviceNameRE is what a service name may look like. Names become path
// segments, Caddy site addresses and compose project names, so anything else
// ("../core", "*", a space) is refused before it reaches any of them.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func validName(name string) error {
	if !serviceNameRE.MatchString(name) {
		return fmt.Errorf("invalid service name %q: use lowercase letters, digits, '-' and '_'", name)
	}
	return nil
}

// forEachService runs fn for every target and keeps going past failures, the
// way `docker compose` does: one broken service must not leave the rest of a
// `down --all` running. Failures are reported inline and returned together.
func forEachService(root string, names []string, fn func(name string) error) error {
	var errs []error
	for _, name := range names {
		err := validateService(root, name)
		if err == nil {
			err = fn(name)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", styles.Err.Render("✗"), name, err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if len(errs) > 1 {
		return fmt.Errorf("%d of %d services failed", len(errs), len(names))
	}
	return errors.Join(errs...)
}

// validateService checks the name, and that services/<name>/ and its
// docker-compose.yml exist.
func validateService(root, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	svcDir := filepath.Join(root, "services", name)
	if _, err := os.Stat(svcDir); os.IsNotExist(err) {
		svcs, _ := service.Discover(root)
		hint := buildServiceHint(svcs)
		return fmt.Errorf("service %q not found\n%s", name, hint)
	}
	compose := filepath.Join(svcDir, "docker-compose.yml")
	if _, err := os.Stat(compose); os.IsNotExist(err) {
		return fmt.Errorf("services/%s/ exists but has no docker-compose.yml", name)
	}
	return nil
}

// buildServiceHint returns a styled list of known services for error messages.

func completeServiceNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	root := configDir()
	svcs, err := service.Discover(root)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, s := range svcs {
		if strings.HasPrefix(s.Name, toComplete) {
			names = append(names, s.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func completeGroupNames(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.Load(rootConfigFile())
	if err != nil || cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for k := range cfg.Groups {
		if strings.HasPrefix(k, toComplete) {
			names = append(names, k)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// ── TUI launchers ─────────────────────────────────────────────────────────────

// scaffoldService writes boilerplate for a new service using the embedded
// templates in internal/scaffold. Used by the non-interactive CLI path.
func scaffoldService(root, name, container, port string, dryRun bool) error {
	data := scaffold.ServiceData{Name: name, Container: container, Port: port}
	files, err := scaffold.Render(data)
	if err != nil {
		return fmt.Errorf("rendering templates: %w", err)
	}

	if dryRun {
		fmt.Printf("\n%s\n\n", styles.Warning.Render("── dry run ──"))
		for _, f := range files {
			fmt.Printf("%s\n%s\n\n",
				styles.Primary.Render(f.RelPath),
				styles.Muted.Render(strings.TrimRight(f.Content, "\n")),
			)
		}
		return nil
	}

	if err := scaffold.Write(root, files); err != nil {
		return err
	}

	fmt.Printf("\n%s  Scaffolded services/%s/\n", styles.Success.Render("✓"), name)
	fmt.Printf("  %s docker-compose.yml\n", styles.Muted.Render("├──"))
	fmt.Printf("  %s config.yaml       %s\n\n", styles.Muted.Render("└──"), styles.Muted.Render("(vars, secrets, ports — routes are generated from ports)"))
	fmt.Printf("%s\n", styles.Muted.Render("Next steps:"))
	fmt.Printf("  1. Edit %s\n", styles.Primary.Render(fmt.Sprintf("services/%s/docker-compose.yml", name)))
	fmt.Printf("  2. %s\n", styles.Primary.Render(fmt.Sprintf("homelab setup %s", name)))
	fmt.Printf("  3. %s\n", styles.Primary.Render(fmt.Sprintf("homelab up %s", name)))
	fmt.Printf("  4. %s\n", styles.Primary.Render(fmt.Sprintf("homelab enable %s", name)))
	fmt.Printf("     %s\n\n", styles.Muted.Render(fmt.Sprintf("homelab enable %s --cf   (requires Cloudflare Tunnel)", name)))
	return nil
}

// sharedDBStartTimeout bounds how long we wait for an auto-started shared
// database to become healthy. Postgres recovery after an unclean shutdown is the
// slow case; a minute is generous without hanging a script forever.
