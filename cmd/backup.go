package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
)

var backupCmd = &cobra.Command{
	Use:   "backup [service...]",
	Short: "Back up service volumes, databases and config",
	Long: `Snapshot everything a service owns: its named Docker volumes, its
databases on the shared instances, and its config files.

Each run writes a timestamped directory containing a manifest.json, so a backup
is inspectable and can be restored selectively.

  homelab backup vaultwarden          # one service
  homelab backup vaultwarden immich   # several services
  homelab backup --all                # every installed service
  homelab backup --group media        # a named group
  homelab backup --out /mnt/nas       # somewhere other than the default
  homelab backup immich --live        # do not stop the service first
  homelab backup --list [--json]      # existing backups, newest first

By default each service is stopped for the duration of its own snapshot and
started again afterwards, because tarring a volume underneath a running process
can capture a torn file. --live skips that at the cost of consistency.

Secrets are NOT included: they stay in the system keyring. After restoring onto a
new machine, run 'homelab setup <service>' to re-enter them — that also re-syncs
the database role password.`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeServiceNames,
	RunE:              runBackup,
}

var backupFlags struct {
	all   bool
	group string
	out   string
	live  bool
	list  bool
}

func init() {
	backupCmd.Flags().BoolVar(&backupFlags.all, "all", false, "Back up all installed services")
	backupCmd.Flags().StringVar(&backupFlags.group, "group", "", "Back up a named service group")
	backupCmd.Flags().StringVar(&backupFlags.out, "out", "", "Destination directory (default: <config-dir>/backups)")
	backupCmd.Flags().BoolVar(&backupFlags.live, "live", false, "Do not stop services (faster, risks torn files)")
	backupCmd.Flags().BoolVar(&backupFlags.list, "list", false, "List existing backups in the destination directory (with --json: machine-readable)")
	_ = backupCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
	rootCmd.AddCommand(backupCmd)
}

func runBackup(_ *cobra.Command, args []string) error {
	root := configDir()
	if backupFlags.list {
		return listBackups(root)
	}

	names, err := resolveTargets(root, backupFlags.all, backupFlags.group, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("nothing to back up")
	}

	destRoot := backupFlags.out
	if destRoot == "" {
		destRoot = backup.DefaultDir(root)
	}
	// Absolute: the directory is bind-mounted into the helper container that
	// reads volume contents, and Docker rejects relative bind sources.
	destRoot, err = filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("resolving destination: %w", err)
	}
	dest := filepath.Join(destRoot, time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}

	engine := &backup.Engine{ConfigDir: root, Exec: run.Default()}

	fmt.Printf("\n%s\n\n", styles.Header.Render("Backup"))
	fmt.Printf("  %s %s\n\n", styles.Muted.Render("→"), dest)

	var (
		records []backup.ServiceRecord
		failed  []error
	)
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
		// A failure is recorded and the loop goes on: the manifest must still
		// be written for every service that did back up, or none of them can
		// be restored.
		plan, err := backup.PlanFor(root, name)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			fmt.Printf("  %s %s — %v\n", styles.Err.Render("✗"), name, err)
			continue
		}
		if plan.Empty() {
			fmt.Printf("  %s %s — nothing to back up\n", styles.Muted.Render("·"), name)
			continue
		}

		rec, err := backupOne(engine, root, name, plan, dest)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", name, err))
			fmt.Printf("  %s %s — %v\n", styles.Err.Render("✗"), name, err)
			continue
		}
		records = append(records, rec)

		fmt.Printf("  %s %s — %d volume(s), %d database(s)",
			styles.Success.Render("✓"), styles.Bold.Render(name),
			len(rec.Volumes), len(rec.Databases))
		if plan.SkippedRedis > 0 {
			fmt.Printf(" %s", styles.Muted.Render("(redis not dumped — cache/queue only)"))
		}
		if plan.S3Buckets > 0 {
			fmt.Printf(" %s", styles.Muted.Render("(files are in the shared garage — back up garage too)"))
		}
		fmt.Println()
	}

	if len(records) == 0 {
		return errors.Join(append(failed, fmt.Errorf("no service produced a backup"))...)
	}
	if err := engine.WriteManifest(dest, records); err != nil {
		return err
	}

	fmt.Printf("\n  %s %s\n", styles.Success.Render("✓"), "manifest written")
	fmt.Printf("  %s Restore with: %s\n\n",
		styles.Muted.Render("→"),
		styles.Primary.Render(fmt.Sprintf("homelab restore %s", dest)))
	return errors.Join(failed...)
}

// backupOne stops the service around its own snapshot unless --live was passed,
// then restores whatever run state it found.
func backupOne(engine *backup.Engine, root, name string, plan backup.Plan, dest string) (backup.ServiceRecord, error) {
	if backupFlags.live {
		return engine.Backup(plan, dest, true)
	}

	wasRunning := serviceIsRunning(root, name)
	if wasRunning {
		fmt.Printf("  %s stopping %s for a consistent snapshot…\n", styles.Muted.Render("·"), name)
		if err := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name), buildEnv(root, name), "stop",
		); err != nil {
			return backup.ServiceRecord{}, fmt.Errorf("stopping %s: %w", name, err)
		}
	}

	rec, err := engine.Backup(plan, dest, false)

	// Always attempt to bring it back, even if the backup failed — leaving a
	// service down because a tar step errored is a worse outcome than the
	// failed backup itself.
	if wasRunning {
		if startErr := run.Default().DockerComposeEnv(
			run.ServiceComposeFile(root, name), buildEnv(root, name), "up", "-d",
		); startErr != nil {
			if err == nil {
				return rec, fmt.Errorf("backup of %s succeeded but restarting it failed: %w", name, startErr)
			}
			fmt.Fprintf(os.Stderr, "warning: could not restart %s after a failed backup: %v\n", name, startErr)
		}
	}
	return rec, err
}

// serviceIsRunning reports whether any container of the service is up.
func serviceIsRunning(root, name string) bool {
	out, err := run.Default().Output("docker", "compose",
		"-f", run.ServiceComposeFile(root, name), "ps", "-q", "--status=running")
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// listBackups prints the backups under --out (default <config-dir>/backups),
// newest first: the directories `homelab restore` accepts.
func listBackups(root string) error {
	dir := backupFlags.out
	if dir == "" {
		dir = backup.DefaultDir(root)
	}
	list, err := backup.List(dir)
	if err != nil {
		return err
	}
	if rootFlags.json {
		if list == nil {
			list = []backup.Listing{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	if len(list) == 0 {
		fmt.Printf("\n  %s\n\n", styles.Muted.Render("No backups in "+dir))
		return nil
	}
	fmt.Printf("\n%s\n\n", styles.Header.Render("Backups"))
	for _, l := range list {
		live := ""
		if l.Live {
			live = " " + styles.Warning.Render("(live)")
		}
		fmt.Printf("  %s  %s  %s%s\n", l.Created.Local().Format("2006-01-02 15:04"),
			l.Dir, styles.Muted.Render(strings.Join(l.Services, ", ")), live)
	}
	fmt.Println()
	return nil
}
