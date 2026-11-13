package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/agentstate"
	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
)

// backupsRoot is the leader agent's mount of the backups volume; a var so
// tests can point it at a temp dir.
var backupsRoot = agentstate.BackupsRoot

// backupStopTimeout bounds the wait for a scaled-down service's containers
// to stop before its volumes are read.
const backupStopTimeout = 2 * time.Minute

var backupCmd = &cobra.Command{
	Annotations: jsonSupported,
	Use:         "backup [service...]",
	Short:       "Back up service volumes, databases and config",
	Long: `Snapshot everything a service owns: its named Docker volumes, its
databases on the shared instances, and its config files.

Backups run on the leader and land in its backups volume (homelab-backups,
mounted at /var/lib/homelab-backups in the leader agent). Each run writes a
timestamped directory containing a manifest.json, so a backup is inspectable
and can be restored selectively.

  homelab backup vaultwarden          # one service
  homelab backup vaultwarden immich   # several services
  homelab backup --all                # every installed service
  homelab backup --group media        # a named group
  homelab backup --out weekly         # a directory inside the backups volume
  homelab backup immich --live        # do not stop the service first
  homelab backup --list [--json]      # existing backups, newest first
  homelab backup --fetch <name> ~/bk  # copy a backup to this PC (~/bk/<name>)

By default each service is scaled down for the duration of its own snapshot and
deployed again afterwards, because tarring a volume underneath a running
process can capture a torn file. --live skips that at the cost of consistency.
Shared databases are dumped online and never stopped for a consumer.

Secrets are NOT included: they stay in the vault. After restoring onto a new
cluster, run 'homelab setup <service>' to re-enter them — that also re-syncs the
database role password.`,
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
	fetch string
}

func init() {
	backupCmd.Flags().BoolVar(&backupFlags.all, "all", false, "Back up all installed services")
	backupCmd.Flags().StringVar(&backupFlags.group, "group", "", "Back up a named service group")
	backupCmd.Flags().StringVar(&backupFlags.out, "out", "", "Destination directory inside the backups volume (default: its top level)")
	backupCmd.Flags().BoolVar(&backupFlags.live, "live", false, "Do not stop services (faster, risks torn files)")
	backupCmd.Flags().BoolVar(&backupFlags.list, "list", false, "List existing backups in the destination directory (with --json: machine-readable)")
	backupCmd.Flags().StringVar(&backupFlags.fetch, "fetch", "", "Copy the named backup from the leader into <local-dir>/<name> on this PC")
	_ = backupCmd.RegisterFlagCompletionFunc("group", completeGroupNames)
	rootCmd.AddCommand(backupCmd, backupExportCmd, backupListCmd)
}

// backupListCmd is `backup --list` as a command of its own, for the front
// ends that poll it on every refresh. backup is Mutating, so `backup --list`
// takes the agent's lock (the server decides on the command path, never on
// flags); backup-list is in remote.ReadOnly and never waits behind an up or
// a backup. Hidden plumbing: users have `backup --list`.
var backupListCmd = &cobra.Command{
	Annotations: jsonSupported,
	Use:         "backup-list",
	Short:       "List existing backups, newest first (backup --list without the lock; used by the front ends)",
	Hidden:      true,
	Args:        cobra.NoArgs,
	RunE: func(*cobra.Command, []string) error {
		dir, err := backup.ResolveUnder(backupsRoot, "")
		if err != nil {
			return err
		}
		return listBackups(dir)
	},
}

// backupRunsLocally reports whether c is `backup --fetch`, which writes to
// this PC's disk and so is not forwarded to the leader.
func backupRunsLocally(c *cobra.Command) bool {
	return c == backupCmd && c.Flags().Changed("fetch")
}

func runBackup(c *cobra.Command, args []string) error {
	if c.Flags().Changed("fetch") {
		if backupFlags.fetch == "" {
			return errors.New("--fetch needs a backup name (one listed by `homelab backup --list`)")
		}
		if os.Getenv("HOMELAB_AGENT") == "1" {
			return errors.New("run homelab backup --fetch from your PC, not inside the agent")
		}
		if len(args) != 1 {
			return errors.New("usage: homelab backup --fetch <name> <local-dir>")
		}
		return runBackupFetch(c.Context(), backupFlags.fetch, args[0], c.OutOrStdout())
	}

	root := configDir()
	destRoot, err := backup.ResolveUnder(backupsRoot, backupFlags.out)
	if err != nil {
		return err
	}
	if backupFlags.list {
		return listBackups(destRoot)
	}

	names, err := resolveTargets(root, "backup", backupFlags.all, backupFlags.group, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("nothing to back up")
	}
	for _, name := range names {
		if err := validateService(root, name); err != nil {
			return err
		}
	}

	// Scaling back needs no secrets: it only restores the replica counts.
	return withDeployer(root, false, func(ctx context.Context, d *swarm.Deployer) error {
		engine, err := newBackupEngine(root, d)
		if err != nil {
			return err
		}
		dest := filepath.Join(destRoot, time.Now().UTC().Format("20060102-150405"))
		if err := os.MkdirAll(dest, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", dest, err)
		}

		fmt.Printf("\n%s\n\n", styles.Header.Render("Backup"))
		fmt.Printf("  %s %s\n\n", styles.Muted.Render("→"), dest)

		var (
			records []backup.ServiceRecord
			failed  []error
		)
		for _, name := range names {
			// A failure is recorded and the loop goes on: the manifest must
			// still be written for every service that did back up, or none
			// of them can be restored.
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

			if !backupFlags.live {
				fmt.Printf("  %s scaling %s down for a consistent snapshot (if running)…\n", styles.Muted.Render("·"), name)
			}
			rec, err := engine.BackupService(ctx, plan, dest, backupFlags.live)
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
		fmt.Printf("  %s Restore with: %s\n", styles.Muted.Render("→"),
			styles.Primary.Render(fmt.Sprintf("homelab restore %s", dest)))
		fmt.Printf("  %s Copy to a PC: %s\n\n", styles.Muted.Render("→"),
			styles.Primary.Render(fmt.Sprintf("homelab backup --fetch %s <dir>", filepath.Base(dest))))
		return errors.Join(failed...)
	})
}

// newBackupEngine wires the backup engine to the cluster: lifecycle through
// the Deployer, dumps through the shared providers' task containers on this
// node, volume tars in helper containers mounting the backups volume.
func newBackupEngine(root string, d *swarm.Deployer) (*backup.Engine, error) {
	img, err := agentImage()
	if err != nil {
		return nil, fmt.Errorf("finding the agent's image for the volume helpers: %w", err)
	}
	p := db.New(root, nil)
	p.API = d.API
	return &backup.Engine{
		ConfigDir:     root,
		Exec:          run.Default(),
		BackupsRoot:   backupsRoot,
		BackupsVolume: agentstate.BackupsVolume,
		ContainerFor:  p.Container,
		Life:          swarmLife{d: d, timeout: backupStopTimeout},
		HelperImage:   img,
	}, nil
}

// agentImage is the image ID of the agent container this runs in: already
// on the node, pinned by version, with busybox tar/sh/rm (docker:<ver>-cli
// is Alpine). A var so tests need no daemon.
var agentImage = func() (string, error) {
	out, err := run.Default().Output("docker", "inspect", "--format", "{{.Image}}", bootstrap.AgentContainer)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("docker inspect printed no image")
	}
	return id, nil
}

// swarmLife is backup.Lifecycle on the cluster: replica counts and Scale
// (counts only — no pull, no registry, no re-translated config), and a
// bounded wait for the tasks to stop, since scaling only changes the spec.
type swarmLife struct {
	d       *swarm.Deployer
	timeout time.Duration
}

func (l swarmLife) Replicas(ctx context.Context, svc string) (map[string]uint64, error) {
	return l.d.Replicas(ctx, svc)
}

func (l swarmLife) Scale(ctx context.Context, svc string, r map[string]uint64) error {
	return l.d.Scale(ctx, svc, r)
}

func (l swarmLife) WaitStopped(ctx context.Context, svc string) error {
	return l.d.WaitStopped(ctx, svc, l.timeout)
}

// listBackups prints the backups under dir (default: the backups volume),
// newest first: the directories `homelab restore` accepts.
func listBackups(dir string) error {
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

// ── --fetch: leader → PC ──────────────────────────────────────────────────────

// backupExportCmd is the leader-side half of `backup --fetch`: it writes one
// backup from the backups volume to stdout as a tar stream. Hidden plumbing,
// forwarded like any command; read-only, so it does not take the lock.
var backupExportCmd = &cobra.Command{
	Use:    "backup-export <name>",
	Short:  "Write a backup from the backups volume to stdout as tar (used by backup --fetch)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		return backup.ExportTar(c.OutOrStdout(), backupsRoot, args[0])
	},
}

// backupLeader runs argv on the leader agent, its stdout going to stdout and
// its stderr to the terminal. A var so tests can stand in for the agent.
var backupLeader = func(ctx context.Context, argv []string, stdout io.Writer) (int, error) {
	c, err := remote.LoadClient(Version, remote.KeyringSecret)
	if err != nil {
		return 1, err
	}
	return c.Run(ctx, remote.RunOptions{Argv: argv, Stdout: stdout, Stderr: os.Stderr})
}

// runBackupFetch copies the leader's backup name into localDir/name. It is
// unpacked into a hidden temporary directory beside it and renamed into
// place only once the leader finished cleanly and the copy has a valid
// manifest, so a failed fetch leaves nothing that looks like a backup.
func runBackupFetch(ctx context.Context, name, localDir string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := backup.ValidName(name); err != nil {
		return err
	}
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return err
	}
	dest := filepath.Join(localDir, name)
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite it", dest)
	}
	tmp, err := os.MkdirTemp(localDir, "."+name+".partial-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(tmp)
		}
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	type result struct {
		code int
		err  error
	}
	res := make(chan result, 1)
	go func() {
		code, err := backupLeader(ctx, []string{"backup-export", name}, pw)
		_ = pw.Close()
		res <- result{code, err}
	}()
	exErr := backup.ExtractTar(pr, tmp)
	if exErr != nil {
		// Stop the transfer: further writes fail and the session ends.
		_ = pr.CloseWithError(exErr)
		cancel()
	} else {
		_, _ = io.Copy(io.Discard, pr) // tar padding after the last entry
	}
	r := <-res

	// The local failure (no space left, a refused entry) first: it is the
	// cause, and the leader then fails only because the pipe was closed.
	var errs []error
	if exErr != nil {
		errs = append(errs, fmt.Errorf("unpacking backup %s: %w", name, exErr))
	}
	if r.err != nil || r.code != 0 {
		errs = append(errs, leaderErr("fetching backup "+name, r.code, r.err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if _, err := backup.ReadManifest(tmp); err != nil {
		return fmt.Errorf("the fetched copy of %s is not a valid backup: %w", name, err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s appeared while fetching — refusing to overwrite it", dest)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	done = true
	_, _ = fmt.Fprintf(out, "fetched %s → %s\n", name, dest)
	return nil
}
