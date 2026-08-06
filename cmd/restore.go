package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
)

var restoreCmd = &cobra.Command{
	Use:   "restore <backup-dir> [service]",
	Short: "Restore service volumes and databases from a backup",
	Long: `Restore from a directory produced by 'homelab backup'.

  homelab restore ~/.config/homelab/backups/20260805-101500
  homelab restore <dir> vaultwarden        # just one service from the backup
  homelab restore <dir> --config           # also overwrite config.yaml/compose

Restore REPLACES current data: each volume is emptied before the archived copy is
unpacked, and each database is restored with --clean, dropping existing objects.
Services are stopped first and started again afterwards.

The database and its role must already exist — run 'homelab setup <service>'
first on a fresh machine. That also re-syncs the role password with the keyring,
which is why secrets are not carried in the backup.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runRestore,
}

var restoreFlags struct {
	config bool
	yes    bool
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreFlags.config, "config", false,
		"Also restore config.yaml / docker-compose.yml / caddy confs")
	restoreCmd.Flags().BoolVarP(&restoreFlags.yes, "yes", "y", false,
		"Skip the confirmation prompt")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(_ *cobra.Command, args []string) error {
	root := configDir()

	src, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving backup path: %w", err)
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

	engine := &backup.Engine{ConfigDir: root, Exec: run.Default()}

	for _, rec := range records {
		if err := validateService(root, rec.Name); err != nil {
			return fmt.Errorf("%w\n  Install it first: homelab add %s && homelab setup %s",
				err, rec.Name, rec.Name)
		}

		wasRunning := serviceIsRunning(root, rec.Name)
		if wasRunning {
			fmt.Printf("  %s stopping %s…\n", styles.Muted.Render("·"), rec.Name)
			if err := run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, rec.Name), buildEnv(root, rec.Name), "stop",
			); err != nil {
				return fmt.Errorf("stopping %s: %w", rec.Name, err)
			}
		}

		// Databases are restored into the shared instance, so it has to be up
		// even though the service itself is stopped.
		if len(rec.Databases) > 0 {
			if err := ensureDBDependencies(cmdContext(), root, rec.Name); err != nil {
				return err
			}
		}

		err := engine.Restore(rec, src, restoreFlags.config)

		if wasRunning {
			if startErr := run.Default().DockerComposeEnv(
				run.ServiceComposeFile(root, rec.Name), buildEnv(root, rec.Name), "up", "-d",
			); startErr != nil && err == nil {
				err = fmt.Errorf("restore of %s succeeded but restarting it failed: %w", rec.Name, startErr)
			}
		}
		if err != nil {
			return err
		}
		fmt.Printf("  %s %s restored\n", styles.Success.Render("✓"), styles.Bold.Render(rec.Name))
	}

	fmt.Println()
	return nil
}
