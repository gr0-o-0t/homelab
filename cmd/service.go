package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/groot/homelab/internal/caddy"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/docker"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/scaffold"
	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceCmd = &cobra.Command{
	Use:     "service",
	Aliases: []string{"svc"},
	Hidden:  true,
	Short:   "Manage services",
	Long:    "Start, stop, expose, and inspect individual service stacks.",
	RunE:    runServiceList,
}

// ── list ─────────────────────────────────────────────────────────────────────

var serviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all services and their exposure status",
	RunE:  runServiceList,
}

func runServiceList(_ *cobra.Command, _ []string) error {
	root := configDir()
	if isTTY() && !rootFlags.json {
		return runListTUI(root)
	}
	svcs, err := discoverServices(root)
	if err != nil {
		return err
	}
	if rootFlags.json {
		return printServiceJSON(svcs)
	}
	env := buildEnv(root, "")
	printServiceTable(svcs, env, false)
	return nil
}

// ── up ────────────────────────────────────────────────────────────────────────

var serviceUpCmd = &cobra.Command{
	Use:               "up [service]",
	Short:             "Start a service stack",
	Long:              `Start one or more service containers.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceUp,
}

// ── down ──────────────────────────────────────────────────────────────────────

var serviceDownCmd = &cobra.Command{
	Use:               "down [service]",
	Short:             "Stop a service stack and remove it from all Caddy routing",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceDown,
}

// ── restart ───────────────────────────────────────────────────────────────────

var serviceRestartCmd = &cobra.Command{
	Use:               "restart [service]",
	Short:             "Restart a service stack",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceRestart,
}

// ── logs ──────────────────────────────────────────────────────────────────────

var logsFlags struct {
	follow bool
	tail   string
	since  string
}

var serviceLogsCmd = &cobra.Command{
	Use:               "logs <service>",
	Short:             "Tail service logs",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE:              runServiceLogs,
}

// ── ps ────────────────────────────────────────────────────────────────────────

var servicePsCmd = &cobra.Command{
	Use:               "ps <service>",
	Short:             "Show container status for a service",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeServiceNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		root := configDir()
		if err := validateService(root, name); err != nil {
			return err
		}

		dc, err := docker.New()
		if err != nil {
			// Fall back to docker compose ps if SDK unavailable.
			return run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, name),
				buildEnv(root, name),
				"ps",
			)
		}
		defer func() { _ = dc.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		summaries, err := dc.ServiceContainers(ctx, name)
		if err != nil || len(summaries) == 0 {
			fmt.Printf("\n  %s %s — %s\n\n",
				styles.Dot(false, false),
				styles.Bold.Render(name),
				styles.Muted.Render("no containers found"),
			)
			return err
		}

		details, err := dc.InspectContainers(ctx, summaries)
		if err != nil {
			details = nil
		}

		printPsTable(name, summaries, details)
		return nil
	},
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

	// restart batch flags
	serviceRestartCmd.Flags().BoolVar(&restartFlags.all, "all", false, "Restart all installed services")
	serviceRestartCmd.Flags().StringVar(&restartFlags.group, "group", "", "Restart a named service group")
	_ = serviceRestartCmd.RegisterFlagCompletionFunc("group", completeGroupNames)

	// logs flags
	serviceLogsCmd.Flags().BoolVarP(&logsFlags.follow, "follow", "f", false, "Follow log output")
	serviceLogsCmd.Flags().StringVar(&logsFlags.tail, "tail", "", `Number of lines to show from the end (e.g. "100", "all")`)
	serviceLogsCmd.Flags().StringVar(&logsFlags.since, "since", "", `Show logs since timestamp or relative duration (e.g. "30m", "2h", "2006-01-02T15:04:05Z")`)

	serviceCmd.AddCommand(
		serviceListCmd,
		servicePsCmd,
	)
}

func runServiceUp(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, upFlags.all, upFlags.group, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		// Auto-configure root databases section for shared DB services.
		if err := config.EnsureRootDBConfig(rootConfigFile(), name); err != nil {
			fmt.Fprintf(os.Stderr, "warning: auto-configuring databases: %v\n", err)
		}
		if err := ensureDBDependencies(context.Background(), root, name); err != nil {
			return err
		}
		fmt.Printf("%s Starting %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
		upArgs := []string{"up", "-d"}
		if upFlags.build {
			upArgs = append(upArgs, "--build")
		}
		if err := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			upArgs...,
		); err != nil {
			return err
		}
	}
	return nil
}

func runServiceDown(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, downFlags.all, downFlags.group, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		// Always clean up routing before stopping — leaves Caddy in a valid state.
		if err := runWithSpinner(
			fmt.Sprintf("Disabling %s routes…", name),
			func(r *run.Commander) error {
				return caddy.NewWithRunner(root, r).DisableBoth(name)
			},
		); err != nil {
			fmt.Printf("  %s\n", styles.Muted.Render(fmt.Sprintf("(routing cleanup: %v)", err)))
		}
		fmt.Printf("%s Stopping %s…\n", styles.Warning.Render("→"), styles.Bold.Render(name))
		if err := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name),
			buildEnv(root, name),
			"down",
		); err != nil {
			return err
		}
	}
	return nil
}

func runServiceRestart(_ *cobra.Command, args []string) error {
	root := configDir()
	names, err := resolveTargets(root, restartFlags.all, restartFlags.group, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		// Same reasoning as `up`: restarting a service whose database is down
		// just produces connection errors.
		if err := ensureDBDependencies(context.Background(), root, name); err != nil {
			return err
		}
		if restartFlags.build {
			fmt.Printf("%s Rebuilding and recreating %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
			if err := run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, name),
				buildEnv(root, name),
				"up", "-d", "--build",
			); err != nil {
				return err
			}
		} else {
			fmt.Printf("%s Restarting %s…\n", styles.Primary.Render("→"), styles.Bold.Render(name))
			if err := run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, name),
				buildEnv(root, name),
				"restart",
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func runServiceLogs(_ *cobra.Command, args []string) error {
	name := args[0]
	root := configDir()
	if err := validateService(root, name); err != nil {
		return err
	}
	// Use the interactive TUI only when on a TTY with no explicit log flags.
	if isTTY() && !logsFlags.follow && logsFlags.tail == "" && logsFlags.since == "" {
		return runLogTUI(root, name)
	}
	logArgs := []string{"logs"}
	if logsFlags.follow {
		logArgs = append(logArgs, "-f")
	}
	if logsFlags.tail != "" {
		logArgs = append(logArgs, "--tail", logsFlags.tail)
	}
	if logsFlags.since != "" {
		logArgs = append(logArgs, "--since", logsFlags.since)
	}
	return run.Default().DockerComposeEnv(
		run.ServiceComposeFile(root, name),
		buildEnv(root, name),
		logArgs...,
	)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// resolveTargets returns the service names to operate on based on --all, --group, or positional args.

// resolveTargets returns the service names to operate on based on --all, --group, or positional args.
func resolveTargets(root string, all bool, group string, args []string) ([]string, error) {
	if all && group != "" {
		return nil, fmt.Errorf("--all and --group are mutually exclusive")
	}
	if len(args) > 0 && (all || group != "") {
		return nil, fmt.Errorf("cannot combine a service name with --all or --group")
	}
	if len(args) > 0 {
		return []string{args[0]}, nil
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

// firstOrEmpty returns the first element of args or a placeholder string.

// firstOrEmpty returns the first element of args or a placeholder string.
func firstOrEmpty(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "<service>"
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

// printPsTable renders a rich container table for `service ps`.
// Ports and Restart columns added alongside existing health/uptime/image columns.

// validateService checks that services/<name>/ and its docker-compose.yml exist.
func validateService(root, name string) error {
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
	fmt.Printf("  %s caddy.conf        %s\n", styles.Muted.Render("├──"), styles.Muted.Render("(private — tailnet)"))
	fmt.Printf("  %s caddy.cf.conf     %s\n", styles.Muted.Render("├──"), styles.Muted.Render("(Cloudflare Tunnel)"))
	fmt.Printf("  %s config.yaml       %s\n\n", styles.Muted.Render("└──"), styles.Muted.Render("(vars + secrets schema)"))
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
