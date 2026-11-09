package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
)

var restoreCmd = &cobra.Command{
	Use:   "restore <backup-dir> [service]",
	Short: "Restore service volumes and databases from a backup",
	Long: `Restore from a directory produced by 'homelab backup'.

  homelab restore 20260805-101500          # a backup in the leader's backups volume
  homelab restore <dir> vaultwarden        # just one service from the backup
  homelab restore <dir> --config           # also overwrite config.yaml/compose

<backup-dir> is a name listed by 'homelab backup --list', or a path inside the
backups volume (/var/lib/homelab-backups on the leader).

Restore REPLACES current data: each volume is emptied before the archived copy is
unpacked, and each database is restored with --clean, dropping existing objects.
Running services are scaled down first and wait for their containers to stop.

The database and its role must already exist — run 'homelab setup <service>'
first on a fresh machine. That also re-syncs the role password with the keyring,
which is why secrets are not carried in the backup.

A volume of the service missing on this node is refused, so that data is never
restored into a volume the service does not use; pass --create-volumes when
restoring onto a fresh cluster. The backup must hold exactly the volumes and
databases the service declares (its archived config with --config).

A failed restore leaves the service stopped rather than start it on
half-replaced data; a running service is otherwise scaled back to its replica
counts afterwards, with nothing re-pulled or re-deployed.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runRestore,
}

var restoreFlags struct {
	config        bool
	createVolumes bool
	yes           bool
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreFlags.config, "config", false,
		"Also restore config.yaml / docker-compose.yml / caddy confs")
	restoreCmd.Flags().BoolVarP(&restoreFlags.yes, "yes", "y", false,
		"Skip the confirmation prompt")
	restoreCmd.Flags().BoolVar(&restoreFlags.createVolumes, "create-volumes", false,
		"Create a volume missing on this node (restoring onto a fresh cluster) instead of refusing")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(_ *cobra.Command, args []string) error {
	root := configDir()

	// The helper containers that unpack volumes only reach the backups
	// volume, so the backup must be in it: a name from `backup --list` or a
	// path under it.
	src, err := backup.ResolveUnder(backupsRoot, args[0])
	if err != nil {
		return err
	}
	manifest, err := backup.ReadManifest(src)
	if err != nil {
		return err
	}

	records := manifest.Services
	if len(args) == 2 {
		rec, ok := manifest.Find(args[1])
		if !ok {
			return fmt.Errorf("backup does not contain service %q", args[1])
		}
		records = []backup.ServiceRecord{rec}
	}

	fmt.Printf("\n%s\n\n", styles.Header.Render("Restore"))
	fmt.Printf("  %s %s\n", styles.Muted.Render("from"), src)
	fmt.Printf("  %s %s\n\n", styles.Muted.Render("taken"), manifest.Created.Format("2006-01-02 15:04:05 MST"))

	for _, rec := range records {
		fmt.Printf("  %s %s — %d volume(s), %d database(s)",
			styles.Warning.Render("!"), styles.Bold.Render(rec.Name),
			len(rec.Volumes), len(rec.Databases))
		if rec.Live {
			fmt.Printf(" %s", styles.Warning.Render("(taken live — may be inconsistent)"))
		}
		fmt.Println()
	}

	fmt.Printf("\n  %s\n", styles.Warning.Render("This replaces current data. Volumes are emptied before unpacking."))
	if !restoreFlags.yes {
		ok, err := confirm("Proceed with restore?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("\nAborted.")
			return nil
		}
	}
	fmt.Println()

	for _, rec := range records {
		if err := validateService(root, rec.Name); err != nil {
			return fmt.Errorf("%w\n  Install it first: homelab add %s && homelab setup %s",
				err, rec.Name, rec.Name)
		}
	}

	// Scaling back needs no secrets (replica counts only); readying the
	// shared databases opens them itself.
	return withDeployer(root, false, func(ctx context.Context, d *swarm.Deployer) error {
		engine, err := newBackupEngine(root, d)
		if err != nil {
			return err
		}
		engine.CreateVolumes = restoreFlags.createVolumes
		engine.PrepareDB = func(ctx context.Context, svc string) error {
			return ensureDBDependencies(ctx, root, svc)
		}
		for _, rec := range records {
			fmt.Printf("  %s restoring %s (scaled down first if running)…\n", styles.Muted.Render("·"), rec.Name)
			if err := engine.RestoreService(ctx, rec, src, restoreFlags.config); err != nil {
				return err
			}
			fmt.Printf("  %s %s restored\n", styles.Success.Render("✓"), styles.Bold.Render(rec.Name))
		}
		fmt.Println()
		return nil
	})
}
