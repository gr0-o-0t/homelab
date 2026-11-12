package cmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/tui/styles"
	"github.com/spf13/cobra"
)

var serviceAddCmd = &cobra.Command{
	Use:   "add [service]",
	Short: "Install a bundled service from the catalog into your config directory",
	Long: `Copy a pre-configured service from the embedded catalog into your homelab
config directory so you can customise and enable it.

List available services:
  homelab add

Install a service:
  homelab add uptime-kuma

After installing, run:
  homelab setup <name>     # configure vars and secrets
  homelab up <name>        # start containers
  homelab enable <name>    # expose on private tailnet`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeAddNames,
	RunE:              runServiceAdd,
}

func runServiceAdd(_ *cobra.Command, args []string) error {
	// No argument: list available catalog entries.
	if len(args) == 0 {
		return printCatalog()
	}

	name := args[0]
	if err := validName(name); err != nil {
		return err
	}
	dir := configDir()
	destDir := filepath.Join(dir, "services", name)

	// Guard: refuse to overwrite an existing service directory.
	if _, err := os.Stat(destDir); err == nil {
		return fmt.Errorf("services/%s already exists in %s\n"+
			"  Edit the files directly or remove the directory to reinstall", name, dir)
	}

	// Verify the catalog entry exists.
	srcDir := "services/" + name
	if _, err := assets.CatalogFS.Open(srcDir); err != nil {
		return fmt.Errorf("no catalog entry for %q\n\n  Run `homelab add` to list available services", name)
	}

	// Copy catalog entry to configDir/services/<name>/
	if err := fs.WalkDir(assets.CatalogFS, srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Strip the leading "services/<name>" prefix to get the relative path.
		rel, _ := filepath.Rel(srcDir, path)
		dest := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}
		data, err := assets.CatalogFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o600)
	}); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}

	fmt.Printf("\n%s  Installed %s → %s\n\n",
		styles.Success.Render("✓"),
		styles.Bold.Render(name),
		destDir,
	)

	// Auto-configure root databases section for shared DB services.
	if err := config.EnsureRootDBConfig(rootConfigFile(), name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-configuring databases: %v\n", err)
	}

	// On a TTY, offer to run setup immediately rather than making the user
	// remember the next step.
	if isTTY() {
		fmt.Printf("  %s", styles.Primary.Render("Run setup now? [Y/n] "))
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			input := strings.TrimSpace(strings.ToLower(sc.Text()))
			if input == "" || input == "y" || input == "yes" {
				fmt.Println()
				return runServiceSetup(nil, []string{name})
			}
		}
		fmt.Println()
	}

	fmt.Printf("%s\n", styles.Muted.Render("Next steps:"))
	fmt.Printf("  1. %s\n", styles.Primary.Render(fmt.Sprintf("homelab setup %s", name)))
	fmt.Printf("  2. %s\n", styles.Primary.Render(fmt.Sprintf("homelab up %s", name)))
	fmt.Printf("  3. %s\n\n", styles.Primary.Render(fmt.Sprintf("homelab enable %s", name)))
	return nil
}

func printCatalog() error {
	entries, err := assets.CatalogFS.ReadDir("services")
	if err != nil {
		return fmt.Errorf("reading catalog: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	dir := configDir()
	fmt.Printf("\n%s\n\n", styles.Header.Render("Available services"))
	for _, name := range names {
		destDir := filepath.Join(dir, "services", name)
		var status string
		if _, err := os.Stat(destDir); err == nil {
			status = styles.Muted.Render("(installed)")
		}
		fmt.Printf("  %s  %s %s\n",
			styles.Primary.Render("•"),
			styles.Width(20).Render(name),
			status,
		)
	}
	fmt.Printf("\n%s\n\n",
		styles.Muted.Render("Install with: homelab add <name>"))
	return nil
}

func init() {
}

// catalogNames returns the list of service names in the embedded catalog.
// Used by shell completion.
func catalogNames() []string {
	entries, err := assets.CatalogFS.ReadDir("services")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// completeAddNames provides tab-completion for `service add`.
func completeAddNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, n := range catalogNames() {
		if strings.HasPrefix(n, toComplete) {
			names = append(names, n)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// maxAddTree caps the file contents `homelab add <dir>` sends to the leader.
const maxAddTree = 10 << 20

// addLeader runs argv on the leader agent with stdin (shipLeader's shape);
// a var so tests can stand in for the agent.
var addLeader = func(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
	return shipLeader(ctx, argv, stdin, stdout)
}

// isLocalDirArg reports whether `homelab add arg` names a folder on this PC
// rather than a catalog entry: arg contains a slash or is an existing
// directory.
func isLocalDirArg(arg string) bool {
	if strings.Contains(arg, "/") {
		return true
	}
	i, err := os.Stat(arg)
	return err == nil && i.IsDir()
}

// forwardAddDir is `homelab add <dir>` on a PC: the folder is a custom
// service (named after it) the leader does not have. It is checked here,
// then sent as a tar to the leader's `files install`, which validates it
// again and installs it.
func forwardAddDir(argv []string) (int, error) {
	resolveMu.Lock()
	c, _, err := rootCmd.Find(argv)
	resolveMu.Unlock()
	if err != nil {
		return 1, err
	}
	args := c.Flags().Args()
	if len(args) != 1 {
		return forwardRun(argv)
	}
	name, data, err := packServiceDir(args[0])
	if err != nil {
		return 1, err
	}
	return addLeader(cmdContext(), append(rootFlagsOf(argv), "files", "install", name),
		bytes.NewReader(data), os.Stdout)
}

// packServiceDir tars the service folder dir: regular files and directories
// only, at most maxAddTree bytes of content, docker-compose.yml and
// config.yaml required. It returns the service name (the folder's).
func packServiceDir(dir string) (string, []byte, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	name := filepath.Base(abs)
	if err := validName(name); err != nil {
		return "", nil, fmt.Errorf("%s: the folder's name is the service's: %w", dir, err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, err
	}
	if i, err := os.Stat(root); err != nil {
		return "", nil, err
	} else if !i.IsDir() {
		return "", nil, fmt.Errorf("%s is not a directory", dir)
	}

	// Files are read through rt, so a symlink swapped in after the walk saw a
	// regular file cannot lead outside the folder.
	rt, err := os.OpenRoot(root)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rt.Close() }()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := checkAddPath(rel); err != nil {
			return err
		}
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink: copy the file into the folder instead", rel)
		case e.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		case !e.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		data, err := rt.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		if total += int64(len(data)); total > maxAddTree {
			return fmt.Errorf("%s is over the %d MiB limit for a service folder", dir, maxAddTree>>20)
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return "", nil, fmt.Errorf("not sending %s: %w", dir, err)
	}
	for _, f := range slices.Sorted(maps.Keys(swarm.SpecFiles)) {
		if i, err := os.Lstat(filepath.Join(root, f)); err != nil || !i.Mode().IsRegular() {
			return "", nil, fmt.Errorf("not sending %s: %s is missing", dir, f)
		}
	}
	if err := tw.Close(); err != nil {
		return "", nil, err
	}
	return name, buf.Bytes(), nil
}

// checkAddPath refuses a path (slash-separated, relative to the service
// folder) `homelab add <dir>` never installs: absolute or escaping ones, any
// dot file or directory (.env, .git: secrets and VCS data stay on the PC),
// and exposure.yaml (enable and disable own it).
func checkAddPath(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return fmt.Errorf("%q: path must be relative to the service folder", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return fmt.Errorf("%s: dot files and directories are not installed", rel)
		}
	}
	if rel == "exposure.yaml" {
		return errors.New("exposure.yaml is not installed: homelab enable and disable write it")
	}
	return nil
}
