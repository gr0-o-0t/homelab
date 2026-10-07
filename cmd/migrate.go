package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network/layers"
	"github.com/groot/homelab/internal/routing"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

// Routes moved from one file per layer and port (caddy/conf.d*/) to one file
// per service (caddy/sites/<svc>.conf) rendered from services/<svc>/
// exposure.yaml. Installs from before that are migrated automatically: by
// `homelab update` once the core it recreates mounts caddy/sites, and lazily
// before any other command once the installed core does. `homelab migrate`
// runs it explicitly.

var migrateFlags struct{ force bool }

var migrateCmd = &cobra.Command{
	Use:    "migrate",
	Short:  "Move routes from the per-layer conf.d* files to caddy/sites",
	Hidden: true,
	Long: `Migrate every service still routed by the old per-layer Caddy files
(caddy/conf.d, conf.d-cf, conf.d-tor, conf.d-i2p, conf.d-ygg) to
services/<name>/exposure.yaml plus one caddy/sites/<name>.conf.

Idempotent: a migrated service is skipped. The new files are written before
the old ones are removed. Daemon-side config (torrc.d, tunnels.conf, socat.d)
is only read. Caddy is not reloaded.

Without --force it refuses unless the installed Caddyfile, the core compose
file and the running caddy container all serve caddy/sites — otherwise the
migrated routes would stop being served. Run 'homelab update' first.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		root := configDir()
		if !migrateFlags.force {
			if err := sitesReady(root); err != nil {
				return err
			}
		}
		_, err := runMigration(root, os.Stdout)
		return err
	},
}

func init() {
	migrateCmd.Flags().BoolVar(&migrateFlags.force, "force", false,
		"Migrate even if the installed core does not serve caddy/sites yet")
	rootCmd.AddCommand(migrateCmd)

	rootCmd.PersistentPreRun = func(c *cobra.Command, _ []string) {
		switch {
		case c == migrateCmd, c == serviceUpdateCmd, strings.HasPrefix(c.Name(), "__"),
			c.Name() == "completion", c.Name() == "help":
			return
		}
		ensureMigrated(configDir())
	}
}

// runMigration migrates every pending service, reporting to w, and returns
// how many it migrated.
func runMigration(root string, w io.Writer) (int, error) {
	done, err := routing.Migrate(root, layers.New(root, run.Default(), nil))
	for _, m := range done {
		_, _ = fmt.Fprintf(w, "%s migrated %s → %s (%s)\n", styles.Success.Render("✓"), m.Service,
			configgen.SitesFile(root, m.Service), strings.Join(m.State.Layers, ", "))
	}
	if err != nil {
		return len(done), fmt.Errorf("migration incomplete (those services keep their old routes): %w", err)
	}
	return len(done), nil
}

// ensureMigrated migrates pending services when the installed core can serve
// them, and otherwise says how to get there. Cheap when nothing is pending.
func ensureMigrated(root string) {
	pending, err := routing.Pending(root, layers.New(root, nil, nil))
	if err != nil || len(pending) == 0 {
		return
	}
	if err := sitesReady(root); err != nil {
		fmt.Fprintf(os.Stderr, "note: %d service(s) still use the old per-layer Caddy files (%s): %v\n",
			len(pending), strings.Join(pending, ", "), err)
		return
	}
	if _, err := runMigration(root, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
}

var errCoreTooOld = errors.New("the installed core predates caddy/sites — run `homelab update` first")

// sitesReady reports whether routes in caddy/sites would be served: the
// installed Caddyfile imports the directory, the core compose file mounts it,
// and the caddy container — if Docker can say — was created with that mount.
func sitesReady(root string) error {
	caddyfile, err := os.ReadFile(filepath.Join(root, "caddy", "Caddyfile")) //nolint:gosec // config dir
	if err != nil || !bytes.Contains(caddyfile, []byte("import /homelab/caddy/sites/")) {
		return errCoreTooOld
	}
	compose, err := os.ReadFile(run.CoreComposeFile(root))
	if err != nil || !bytes.Contains(compose, []byte(":/homelab/caddy/sites:ro")) {
		return errCoreTooOld
	}
	out, err := run.Default().Output("docker", "inspect", "-f",
		"{{range .Mounts}}{{.Destination}} {{end}}", caddyContainerName)
	if err == nil && !strings.Contains(string(out), "/homelab/caddy/sites") {
		return errors.New("the caddy container was created before caddy/sites was mounted — run `homelab update` first")
	}
	return nil
}

// requireSitesLayout is the check for commands that write a sites file and
// reload Caddy: the core must serve caddy/sites, and the service must not
// still be routed by legacy files, or both would define its sites.
func requireSitesLayout(root, svc string) error {
	if err := sitesReady(root); err != nil {
		return err
	}
	return requireMigrated(root, svc)
}

// requireMigrated refuses to change a service still routed by legacy files.
func requireMigrated(root, svc string) error {
	if routing.HasLegacy(root, layers.New(root, nil, nil), svc) {
		return fmt.Errorf("%s is still routed by the old per-layer Caddy files — run `homelab update` (or `homelab migrate`) first", svc)
	}
	return nil
}
