package actions

import "github.com/groot/homelab/internal/network/layers"

// layerIcons are the per-layer glyph names; the private tailnet is "shield".
var layerIcons = map[string]string{"ts": "shield", "cf": "globe", "tor": "onion", "i2p": "i2p", "ygg": "mesh"}

// layersWithList are the layers whose CLI has a `<layer> list` command.
var layersWithList = map[string]bool{"tor": true, "i2p": true, "ygg": true}

func layerIcon(name string) string {
	if i, ok := layerIcons[name]; ok {
		return i
	}
	return "link"
}

// ── input sets ────────────────────────────────────────────────────────────────

var (
	inBuild = Input{Key: "build", Label: "Rebuild images first", Kind: Bool}
	inFix   = Input{Key: "fix", Label: "Repair safe issues", Kind: Bool,
		Help: "Create missing networks and directories, fix broken symlinks"}
	inGroup = Input{Key: "group", Label: "Group", Kind: Choice, Source: SourceGroups,
		Help: "Leave empty for every installed service"}
	inOut = Input{Key: "out", Label: "Destination directory", Kind: Path,
		Help: "Default: <config-dir>/backups"}
	inLive = Input{Key: "live", Label: "Live (don't stop the service)", Kind: Bool,
		Help: "Faster, but may capture torn files"}
	inBackupDir  = Input{Key: "backup", Label: "Backup", Kind: Choice, Source: SourceBackups, Required: true}
	inRestoreCfg = Input{Key: "config", Label: "Also restore config files", Kind: Bool,
		Help: "Overwrites config.yaml, docker-compose.yml and Caddy routes"}
	inKeepVolumes = Input{Key: "keep-volumes", Label: "Keep volumes (no data loss)", Kind: Bool}
	inKeepImages  = Input{Key: "keep-images", Label: "Keep images", Kind: Bool}
	inQuiet       = Input{Key: "quiet", Label: "IDs only", Kind: Bool}
)

// logsInputs are the docker compose logs flags `homelab logs` takes.
func logsInputs() []Input {
	return []Input{
		{Key: "follow", Label: "Follow", Kind: Bool, Default: "true"},
		{Key: "tail", Label: "Lines from the end", Kind: Text, Default: "200", Help: `A number or "all"`},
		{Key: "since", Label: "Since", Kind: Text, Help: `Timestamp or duration, e.g. "30m"`},
		{Key: "until", Label: "Until", Kind: Text, Help: "Timestamp or duration"},
		{Key: "timestamps", Label: "Timestamps", Kind: Bool},
	}
}

func logsArgs(t Target, in Inputs) []string {
	args := []string{"logs"}
	if in.Bool("follow") {
		args = append(args, "--follow")
	}
	if in["tail"] != "" {
		args = append(args, "--tail", in["tail"])
	}
	if in["since"] != "" {
		args = append(args, "--since", in["since"])
	}
	if in["until"] != "" {
		args = append(args, "--until", in["until"])
	}
	if in.Bool("timestamps") {
		args = append(args, "--timestamps")
	}
	if t.Name != "" {
		args = append(args, t.Name)
	}
	return args
}

var logsCommands = []string{"logs", "logs --follow", "logs --tail", "logs --since", "logs --until", "logs --timestamps"}

// ── Args helpers ──────────────────────────────────────────────────────────────

// named is `<verb> [flags…] [<target name>]` — the CLI's form for a service,
// and its no-service (core stack) form when the target has no name.
func named(verb string, flags ...string) func(Target, Inputs) []string {
	return func(t Target, _ Inputs) []string {
		return append(append([]string{verb}, flags...), t.names()...)
	}
}

// withBools appends --<key> for every listed Bool input that is set.
func withBools(args []string, in Inputs, keys ...string) []string {
	for _, k := range keys {
		if in.Bool(k) {
			args = append(args, "--"+k)
		}
	}
	return args
}

// namedBools is named plus optional boolean flags from inputs.
func namedBools(verb string, keys ...string) func(Target, Inputs) []string {
	return func(t Target, in Inputs) []string {
		return append(withBools([]string{verb}, in, keys...), t.names()...)
	}
}

// batch is `<verb> --all|--group <g> [flags…]` over installed services.
func batch(verb string, keys ...string) func(Target, Inputs) []string {
	return func(_ Target, in Inputs) []string {
		args := []string{verb}
		if g := in["group"]; g != "" {
			args = append(args, "--group", g)
		} else {
			args = append(args, "--all")
		}
		return withBools(args, in, keys...)
	}
}

func backupArgs(t Target, in Inputs) []string {
	args := []string{"backup"}
	if names := t.names(); len(names) > 0 {
		args = append(args, names...)
	} else if g := in["group"]; g != "" {
		args = append(args, "--group", g)
	} else {
		args = append(args, "--all")
	}
	if in["out"] != "" {
		args = append(args, "--out", in["out"])
	}
	return withBools(args, in, "live")
}

func restoreArgs(t Target, in Inputs) []string {
	args := []string{"restore", in["backup"]}
	if t.Name != "" {
		args = append(args, t.Name)
	}
	return append(withBools(args, in, "config"), "--yes")
}

// ── availability ──────────────────────────────────────────────────────────────

func hasContainers(t Target) bool { return t.Total > 0 }
func isRunning(t Target) bool     { return t.Running > 0 }
func notAllRunning(t Target) bool { return t.Total > 0 && t.Running < t.Total }

// ── the registry ──────────────────────────────────────────────────────────────

func build() []Action {
	var a []Action
	add := func(x ...Action) { a = append(a, x...) }

	// Container lifecycle, for a service and for the core stack (the CLI's
	// no-service form). Names and icons are shared so both rows look alike.
	for _, sc := range []Scope{Service, Core} {
		p := sc.String() + "."
		add(
			Action{ID: p + "up", Multi: sc == Service, Label: "Up", Group: GroupLifecycle, Icon: "play", Scope: sc,
				Help:   "Create and start containers",
				Inputs: []Input{inBuild}, Args: namedBools("up", "build"),
				Commands: []string{"up", "up --build"}},
			Action{ID: p + "start", Multi: sc == Service, Label: "Start", Group: GroupLifecycle, Icon: "play", Scope: sc,
				Help: "Start existing stopped containers", Args: named("start"),
				Available: func(t Target) bool { return t.Scope == Core || notAllRunning(t) },
				Commands:  []string{"start"}},
			Action{ID: p + "stop", Multi: sc == Service, Label: "Stop", Group: GroupLifecycle, Icon: "pause", Scope: sc,
				Help: "Stop containers, keep them", Args: named("stop"),
				Available: func(t Target) bool { return t.Scope == Core || isRunning(t) },
				Commands:  []string{"stop"}},
			Action{ID: p + "restart", Multi: sc == Service, Label: "Restart", Group: GroupLifecycle, Icon: "restart", Scope: sc,
				Help:   "Restart containers",
				Inputs: []Input{inBuild}, Args: namedBools("restart", "build"),
				Available: func(t Target) bool { return t.Scope == Core || hasContainers(t) },
				Commands:  []string{"restart", "restart --build"}},
			Action{ID: p + "pull", Multi: sc == Service, Label: "Pull images", Group: GroupMaintenance, Icon: "download", Scope: sc,
				Help: "Pull the latest images without restarting", Args: named("pull"),
				Commands: []string{"pull"}},
			Action{ID: p + "update", Multi: sc == Service, Label: "Update", Group: GroupMaintenance, Icon: "upgrade", Scope: sc,
				Help: "Pull the latest images and recreate the containers", Args: named("update"),
				Commands: []string{"update"}},
			Action{ID: p + "logs", Label: "Logs", Group: GroupInspect, Icon: iconLogs, Scope: sc,
				Help: "Container logs", Inputs: logsInputs(), Args: logsArgs, Stream: true,
				Commands: logsCommands},
			Action{ID: p + "config", Label: "Compose config", Group: GroupInspect, Icon: "eye", Scope: sc,
				Help: "Resolved docker compose configuration — contains secrets in plain text",
				Args: named("config"), Commands: []string{"config"}},
			Action{ID: p + "images", Label: "Images", Group: GroupInspect, Icon: "box", Scope: sc,
				Help: "Images used by the containers", Inputs: []Input{inQuiet}, Args: namedBools("images", "quiet"),
				Commands: []string{"images", "images --quiet"}},
			Action{ID: p + "doctor", Label: "Doctor", Group: GroupInspect, Icon: "doctor", Scope: sc,
				Help: "Health checks", Inputs: []Input{inFix}, Args: namedBools("doctor", "fix"),
				Commands: []string{"doctor", "doctor --fix"}},
			Action{ID: p + "reload", Label: "Reload routing", Group: GroupMaintenance, Icon: "refresh", Scope: sc,
				Help: "Re-render Caddy routes and reload Caddy gracefully", Args: named("reload"),
				Commands: []string{"reload"}},
		)
	}
	add(
		Action{ID: "service.down", Multi: true, Label: "Down", Group: GroupLifecycle, Icon: "stop", Scope: Service,
			Help: "Stop and remove the containers; exposure and data are kept", Args: named("down"),
			Available: hasContainers, Commands: []string{"down"}},
		Action{ID: "core.down", Label: "Down", Group: GroupLifecycle, Icon: "stop", Scope: Core, Danger: Confirm,
			Help: "Stop and remove Tailscale, Caddy and the extensions — every route goes offline", Args: named("down"),
			Commands: []string{"down"}},
		Action{ID: "service.status", Label: "Status", Group: GroupInspect, Icon: "info", Scope: Service,
			Help: "Containers, ports and addresses", Args: named("status"), Commands: []string{"status"}},
		Action{ID: "service.port", Label: "Public port", Group: GroupInspect, Icon: "hash", Scope: Service,
			Help:      "The host port a container port is published on",
			Inputs:    []Input{{Key: "port", Label: "Container port", Kind: Text, Required: true}},
			Args:      func(t Target, in Inputs) []string { return []string{"port", t.Name, in["port"]} },
			Available: isRunning, Commands: []string{"port"}},
		Action{ID: "core.validate", Label: "Validate Caddyfile", Group: GroupInspect, Icon: "check", Scope: Core,
			Help: "Check the Caddy config without reloading", Args: named("validate"), Commands: []string{"validate"}},
		Action{ID: "core.caddy.status", Label: "Caddy status", Group: GroupInspect, Icon: "info", Scope: Core,
			Help: "Caddy container status", Args: named("caddy", "status"), Commands: []string{"caddy status"}},
		Action{ID: "core.caddy.logs", Label: "Caddy logs", Group: GroupInspect, Icon: iconLogs, Scope: Core, Stream: true,
			Help: "Stream the Caddy container's logs", Args: named("caddy", "logs"), Commands: []string{"caddy logs"}},
		Action{ID: "core.ts.status", Label: "Tailscale status", Group: GroupInspect, Icon: "shield", Scope: Core,
			Help: "Tailscale container status", Args: named("ts", "status"), Commands: []string{"ts status"}},
		Action{ID: "core.ts.logs", Label: "Tailscale logs", Group: GroupInspect, Icon: iconLogs, Scope: Core, Stream: true,
			Help: "Stream the Tailscale container's logs", Args: named("ts", "logs"), Commands: []string{"ts logs"}},
	)

	// ── exposure: one toggle per layer ────────────────────────────────────────
	for _, l := range layers.Static() {
		name, flag, label := l.Name(), l.Flag(), l.Label()
		if flag == "" { // the private tailnet: the bare enable/disable
			add(
				Action{ID: "service.enable", Label: "Expose on tailnet", Group: GroupExposure, Icon: layerIcon(name), Scope: Service,
					Help: "Route https://<service>.<home>.<domain> on the private tailnet", Args: named("enable"),
					Available: func(t Target) bool { return !t.exposed(name) }, Commands: []string{"enable"}},
				Action{ID: "service.disable", Label: "Hide from tailnet", Group: GroupExposure, Icon: "unlink", Scope: Service,
					Help: "Remove the private tailnet route; other layers stay", Args: named("disable"),
					Available: func(t Target) bool { return t.exposed(name) }, Commands: []string{"disable"}},
			)
			continue
		}
		add(
			Action{ID: "service.enable." + name, Label: "Expose via " + label, Group: GroupExposure, Icon: layerIcon(name), Scope: Service,
				Help: "Also serve the service over " + label, Args: named("enable", "--"+flag),
				Available: func(t Target) bool { return t.offers(name) && !t.exposed(name) },
				Commands:  []string{"enable --" + flag}},
			Action{ID: "service.disable." + name, Label: "Remove " + label, Group: GroupExposure, Icon: "unlink", Scope: Service,
				Help: "Stop serving the service over " + label, Args: named("disable", "--"+flag),
				Available: func(t Target) bool { return t.exposed(name) },
				Commands:  []string{"disable --" + flag}},
		)
	}
	add(
		Action{ID: "service.enable.all", Label: "Expose everywhere", Group: GroupExposure, Icon: "link", Scope: Service,
			Help: "Expose on the tailnet and every enabled extension", Args: named("enable", "--all"),
			Available: func(t Target) bool {
				for _, l := range t.Layers {
					if !t.exposed(l) {
						return true
					}
				}
				return false
			},
			Commands: []string{"enable --all"}},
		Action{ID: "service.rename", Label: "Name and ports", Group: GroupExposure, Icon: "tag", Scope: Service,
			Help: "Serve under another subdomain or only some declared ports; remembered for every layer",
			Inputs: []Input{
				{Key: "name", Label: "Subdomain", Kind: Text, Help: "Empty keeps the current one; the service name resets it"},
				{Key: "ports", Label: "Ports", Kind: Text, Help: "Comma-separated port names; empty keeps the current selection"},
			},
			Args: func(t Target, in Inputs) []string {
				args := []string{"enable", t.Name}
				if in["name"] != "" {
					args = append(args, "--name", in["name"])
				}
				if in["ports"] != "" {
					args = append(args, "--ports", in["ports"])
				}
				return args
			},
			Available: func(t Target) bool { return len(t.Exposed) > 0 },
			Commands:  []string{"enable --name", "enable --ports"}},
		Action{ID: "service.disable.all", Label: "Remove all exposure", Group: GroupExposure, Icon: "unlink", Scope: Service,
			Danger: Confirm, Help: "Remove every route, private included — the service becomes unreachable",
			Inputs: []Input{{Key: "stop", Label: "Also take the containers down", Kind: Bool}},
			Args: func(t Target, in Inputs) []string {
				return withBools([]string{"disable", t.Name, "--all"}, in, "stop")
			},
			Available: func(t Target) bool { return len(t.Exposed) > 0 },
			Commands:  []string{"disable --all", "disable --stop"}},
		Action{ID: "service.cf.route.add", Label: "Add Cloudflare DNS route", Group: GroupExposure, Icon: "globe", Scope: Service,
			Help: "Create the public CNAME the Cloudflare Tunnel serves", Args: named("cf", "route", "add"),
			Available: func(t Target) bool { return t.offers("cf") }, Commands: []string{"cf route add"}},
		Action{ID: "service.cf.route.rm", Label: "Remove Cloudflare DNS route", Group: GroupExposure, Icon: "unlink", Scope: Service,
			Help: "Remove the service's public CNAME", Args: named("cf", "route", "rm"),
			Available: func(t Target) bool { return t.offers("cf") }, Commands: []string{"cf route rm"}},
	)

	// ── service maintenance, configuration and destruction ────────────────────
	add(
		Action{ID: "service.backup", Multi: true, Label: "Back up", Group: GroupMaintenance, Icon: "backup", Scope: Service,
			Help:   "Snapshot volumes, databases and config (secrets stay in the keyring)",
			Inputs: []Input{inOut, inLive}, Args: backupArgs,
			Commands: []string{"backup", "backup --out", "backup --live"}},
		Action{ID: "service.restore", Label: "Restore", Group: GroupMaintenance, Icon: "restore", Scope: Service,
			Danger: Confirm, Help: "Replace the service's volumes and databases with a backup",
			Inputs: []Input{inBackupDir, inRestoreCfg}, Args: restoreArgs,
			Commands: []string{"restore", "restore --config", "restore --yes"}},
		Action{ID: "service.setup", Label: "Configure", Group: GroupConfigure, Icon: "gear", Scope: Service,
			Interactive: true, Help: "Set variables and secrets (front ends can also use setup --json/--set/--secrets-stdin)",
			Args: named("setup"), Commands: []string{"setup", "setup --set", "setup --secrets-stdin"}},
		Action{ID: "service.shell", Label: "Shell", Group: GroupConfigure, Icon: "terminal", Scope: Service,
			Interactive: true, Help: "Open a shell in the service's container",
			Inputs: []Input{{Key: "shell", Label: "Shell", Kind: Text, Default: "sh"}},
			Args: func(t Target, in Inputs) []string {
				return append([]string{"exec", t.Name}, fields(in["shell"])...)
			},
			Available: isRunning, Commands: []string{"exec"}},
		Action{ID: "service.exec", Label: "Run command", Group: GroupConfigure, Icon: "terminal", Scope: Service,
			Help: "Run a command in the container and show its output",
			Inputs: []Input{
				{Key: "command", Label: "Command", Kind: Text, Required: true},
				{Key: "user", Label: "User", Kind: Text},
				{Key: "workdir", Label: "Working directory", Kind: Text},
				{Key: "env", Label: "Environment", Kind: Text, Help: "Space-separated KEY=VALUE pairs"},
			},
			Args: func(t Target, in Inputs) []string {
				args := []string{"exec", "--no-tty"}
				if in["user"] != "" {
					args = append(args, "--user", in["user"])
				}
				if in["workdir"] != "" {
					args = append(args, "--workdir", in["workdir"])
				}
				for _, e := range fields(in["env"]) {
					args = append(args, "--env", e)
				}
				return append(append(args, t.Name), fields(in["command"])...)
			},
			Available: isRunning,
			Commands:  []string{"exec --no-tty", "exec --user", "exec --workdir", "exec --env"}},
		Action{ID: "service.prune", Multi: true, Label: "Prune", Group: GroupDanger, Icon: "broom", Scope: Service,
			Danger: TypeName, Help: "Take the service down and delete its images and VOLUMES — destroys data unless volumes are kept",
			Inputs: []Input{inKeepVolumes, inKeepImages},
			Args: func(t Target, in Inputs) []string {
				return append(withBools(append([]string{"prune"}, t.names()...), in, "keep-volumes", "keep-images"), "--yes")
			},
			Commands: []string{"prune", "prune --keep-volumes", "prune --keep-images", "prune --yes"}},
		Action{ID: "service.delete", Multi: true, Label: "Delete", Group: GroupDanger, Icon: "trash", Scope: Service,
			Danger: TypeName, Help: "Take the service down, remove its exposure and delete its config directory (volumes are kept)",
			Inputs: []Input{{Key: "force", Label: "Even if it cannot be taken down", Kind: Bool}},
			Args: func(t Target, in Inputs) []string {
				return append(withBools(append([]string{"delete"}, t.names()...), in, "force"), "--yes")
			},
			Commands: []string{"delete", "delete --force", "delete --yes"}},
	)

	// ── catalog ───────────────────────────────────────────────────────────────
	add(Action{ID: "catalog.add", Label: "Install", Group: GroupConfigure, Icon: "plus", Scope: CatalogEntry,
		Help: "Copy the service from the catalog into the config dir", Args: named("add"), Commands: []string{"add"}})

	// ── global: overview, batch operations, scaffolding ───────────────────────
	add(
		Action{ID: "global.status", Label: "Status overview", Group: GroupInspect, Icon: "info", Scope: Global,
			Help:   "Core stack, extensions and every service",
			Inputs: []Input{{Key: "check", Label: "Run health checks", Kind: Bool}}, Args: namedBools("status", "check"),
			Commands: []string{"status", "status --check"}},
		Action{ID: "global.ls", Label: "List services", Group: GroupInspect, Icon: "list", Scope: Global,
			Inputs: []Input{{Key: "quiet", Label: "Names only", Kind: Bool}}, Args: namedBools("ls", "quiet"),
			Commands: []string{"ls", "ls --quiet"}},
		Action{ID: "global.catalog", Label: "Catalog", Group: GroupInspect, Icon: "list", Scope: Global,
			Help: "Services available to install", Args: named("add"), Commands: []string{"add"}},
		Action{ID: "global.version", Label: "Version", Group: GroupInspect, Icon: "info", Scope: Global,
			Args: named("version"), Commands: []string{"version"}},
		Action{ID: "global.doctor", Label: "Doctor (all services)", Group: GroupInspect, Icon: "doctor", Scope: Global,
			Inputs: []Input{inFix}, Args: func(_ Target, in Inputs) []string { return withBools([]string{"doctor", "--all"}, in, "fix") },
			Commands: []string{"doctor --all"}},
		Action{ID: "global.up", Label: "Up all", Group: GroupLifecycle, Icon: "play", Scope: Global,
			Inputs: []Input{inGroup, inBuild}, Args: batch("up", "build"), Commands: []string{"up --all", "up --group"}},
		Action{ID: "global.start", Label: "Start all", Group: GroupLifecycle, Icon: "play", Scope: Global,
			Inputs: []Input{inGroup}, Args: batch("start"), Commands: []string{"start --all", "start --group"}},
		Action{ID: "global.stop", Label: "Stop all", Group: GroupLifecycle, Icon: "pause", Scope: Global,
			Inputs: []Input{inGroup}, Args: batch("stop"), Commands: []string{"stop --all", "stop --group"}},
		Action{ID: "global.restart", Label: "Restart all", Group: GroupLifecycle, Icon: "restart", Scope: Global,
			Inputs: []Input{inGroup, inBuild}, Args: batch("restart", "build"), Commands: []string{"restart --all", "restart --group"}},
		Action{ID: "global.down", Label: "Down all", Group: GroupLifecycle, Icon: "stop", Scope: Global, Danger: Confirm,
			Help:   "Stop and remove every service's containers (or a group's)",
			Inputs: []Input{inGroup}, Args: batch("down"), Commands: []string{"down --all", "down --group"}},
		Action{ID: "global.pull", Label: "Pull all", Group: GroupMaintenance, Icon: "download", Scope: Global,
			Inputs: []Input{inGroup}, Args: batch("pull"), Commands: []string{"pull --all", "pull --group"}},
		Action{ID: "global.update", Label: "Update all", Group: GroupMaintenance, Icon: "upgrade", Scope: Global,
			Inputs: []Input{inGroup}, Args: batch("update"), Commands: []string{"update --all", "update --group"}},
		Action{ID: "global.backup", Label: "Back up all", Group: GroupMaintenance, Icon: "backup", Scope: Global,
			Inputs: []Input{inGroup, inOut, inLive}, Args: backupArgs, Commands: []string{"backup --all", "backup --group"}},
		Action{ID: "global.backup.list", Label: "Backups", Group: GroupMaintenance, Icon: "list", Scope: Global,
			Help: "Existing backups, newest first (in-process: backup.List)", Args: named("backup", "--list"),
			Commands: []string{"backup --list"}},
		Action{ID: "global.restore", Label: "Restore backup", Group: GroupMaintenance, Icon: "restore", Scope: Global,
			Danger: Confirm, Help: "Replace the volumes and databases of every service in the backup",
			Inputs: []Input{inBackupDir, inRestoreCfg}, Args: restoreArgs, Commands: []string{"restore"}},
		Action{ID: "global.prune.dangling", Label: "Reclaim space", Group: GroupMaintenance, Icon: "broom", Scope: Global,
			Help: "Remove unreferenced images and build cache; touches no service", Args: named("prune", "--dangling"),
			Commands: []string{"prune --dangling"}},
		Action{ID: "global.prune", Label: "Prune all", Group: GroupDanger, Icon: "broom", Scope: Global, Danger: TypeName,
			Help:   "Take every service (or a group) down and delete images and VOLUMES — destroys data unless volumes are kept",
			Inputs: []Input{inGroup, inKeepVolumes, inKeepImages},
			Args: func(t Target, in Inputs) []string {
				return append(batch("prune", "keep-volumes", "keep-images")(t, in), "--yes")
			},
			Commands: []string{"prune --all", "prune --group"}},
		Action{ID: "global.new", Label: "New service", Group: GroupConfigure, Icon: "plus", Scope: Global,
			Interactive: true, Help: "Scaffold a new service with the wizard", Args: named("new"), Commands: []string{"new"}},
		Action{ID: "global.new.scaffold", Label: "New service (quick)", Group: GroupConfigure, Icon: "plus", Scope: Global,
			Help: "Scaffold a new service from a container name and port",
			Inputs: []Input{
				{Key: "name", Label: "Service name", Kind: Text, Required: true},
				{Key: "container", Label: "Container name", Kind: Text, Required: true},
				{Key: "port", Label: "Container port", Kind: Text, Required: true},
				{Key: "dry-run", Label: "Preview only", Kind: Bool},
			},
			Args: func(_ Target, in Inputs) []string {
				return withBools([]string{"new", in["name"], "--container", in["container"], "--port", in["port"]}, in, "dry-run")
			},
			Commands: []string{"new --container", "new --port", "new --dry-run"}},
		Action{ID: "global.setup", Label: "Homelab setup", Group: GroupConfigure, Icon: "gear", Scope: Global,
			Interactive: true, Help: "Domain, Tailscale and Cloudflare credentials, extensions", Args: named("setup"),
			Commands: []string{"setup"}},
		Action{ID: "global.migrate", Label: "Migrate routes", Group: GroupMaintenance, Icon: "wrench", Scope: Global,
			Danger: Confirm, Help: "Move routes from the old per-layer conf.d files to caddy/sites",
			Inputs:   []Input{{Key: "force", Label: "Even if the core does not serve caddy/sites yet", Kind: Bool}},
			Args:     func(_ Target, in Inputs) []string { return withBools([]string{"migrate"}, in, "force") },
			Commands: []string{"migrate", "migrate --force"}},
		Action{ID: "global.session.status", Label: "Session status", Group: GroupInspect, Icon: "info", Scope: Global,
			Help: "Whether the stack follows your login session, its desired state, containers Docker still starts at boot",
			Args: named("session", "status"), Commands: []string{"session status"}},
		Action{ID: "global.session.install", Label: "Session mode: install", Group: GroupConfigure, Icon: "power", Scope: Global,
			Help: "Install a systemd user service that starts the stack at login and stops it at logout; turns Docker's boot-time restarts off",
			Args: named("session", "install"), Commands: []string{"session install"}},
		Action{ID: "global.session.uninstall", Label: "Session mode: uninstall", Group: GroupConfigure, Icon: "unlink", Scope: Global,
			Danger: Confirm, Help: "Remove the systemd user service and restore the compose restart policies: Docker starts the stack at boot again",
			Args: named("session", "uninstall"), Commands: []string{"session uninstall"}},
		Action{ID: "global.ext.list", Label: "Extensions", Group: GroupInspect, Icon: "list", Scope: Global,
			Args: named("ext", "list"), Commands: []string{"ext list"}},
		Action{ID: "global.ext.status", Label: "Extension status", Group: GroupInspect, Icon: "info", Scope: Global,
			Args: named("ext", "status"), Commands: []string{"ext status"}},
		Action{ID: "global.ext.logs", Label: "Extension logs", Group: GroupInspect, Icon: iconLogs, Scope: Global, Stream: true,
			Args: named("ext", "logs"), Commands: []string{"ext logs"}},
		Action{ID: "global.ext.start", Label: "Start extensions", Group: GroupLifecycle, Icon: "play", Scope: Global,
			Inputs: []Input{inBuild},
			Args:   func(_ Target, in Inputs) []string { return withBools([]string{"ext", "start"}, in, "build") }, Commands: []string{"ext start", "ext start --build"}},
		Action{ID: "global.ext.stop", Label: "Stop extensions", Group: GroupLifecycle, Icon: "pause", Scope: Global,
			Args: named("ext", "stop"), Commands: []string{"ext stop"}},
	)

	// ── layers: one optional network extension ────────────────────────────────
	add(
		Action{ID: "layer.enable", Label: "Turn on", Group: GroupLifecycle, Icon: "power", Scope: Layer,
			Help: "Enable the extension in config.yaml and start its container", Args: extArgs("enable"),
			Available: func(t Target) bool { return !t.Enabled }, Commands: []string{"ext enable"}},
		Action{ID: "layer.disable", Label: "Turn off", Group: GroupLifecycle, Icon: "power", Scope: Layer, Danger: Confirm,
			Help: "Stop its container and disable it — services exposed on it go dark", Args: extArgs("disable"),
			Available: func(t Target) bool { return t.Enabled }, Commands: []string{"ext disable"}},
		Action{ID: "layer.start", Label: "Start", Group: GroupLifecycle, Icon: "play", Scope: Layer,
			Inputs: []Input{inBuild},
			Args: func(t Target, in Inputs) []string {
				return append(withBools([]string{"ext", "start"}, in, "build"), t.Name)
			},
			Available: func(t Target) bool { return t.Enabled && !isRunning(t) }, Commands: []string{"ext start"}},
		Action{ID: "layer.stop", Label: "Stop", Group: GroupLifecycle, Icon: "pause", Scope: Layer, Args: extArgs("stop"),
			Available: isRunning, Commands: []string{"ext stop"}},
		Action{ID: "layer.container", Label: "Container status", Group: GroupInspect, Icon: "info", Scope: Layer,
			Args: extArgs("status"), Commands: []string{"ext status"}},
		Action{ID: "layer.status", Label: "Details", Group: GroupInspect, Icon: "eye", Scope: Layer,
			Help:     "Layer-specific status (connections, addresses)",
			Args:     func(t Target, _ Inputs) []string { return []string{t.Name, "status"} },
			Commands: []string{"cf status", "tor status", "i2p status", "ygg status"}},
		Action{ID: "layer.logs", Label: "Logs", Group: GroupInspect, Icon: iconLogs, Scope: Layer, Stream: true,
			Args:     func(t Target, _ Inputs) []string { return []string{t.Name, "logs"} },
			Commands: []string{"cf logs", "tor logs", "i2p logs", "ygg logs"}},
		Action{ID: "layer.list", Label: "Exposed services", Group: GroupInspect, Icon: "list", Scope: Layer,
			Help:      "Services this layer serves, with their addresses",
			Args:      func(t Target, _ Inputs) []string { return []string{t.Name, "list"} },
			Available: func(t Target) bool { return layersWithList[t.Name] },
			Commands:  []string{"tor list", "i2p list", "ygg list"}},
	)
	return a
}

// extArgs is `ext <verb> <layer>`.
func extArgs(verb string) func(Target, Inputs) []string {
	return func(t Target, _ Inputs) []string { return []string{"ext", verb, t.Name} }
}
