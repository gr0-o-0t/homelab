package cmd

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/groot/homelab/assets"
	"github.com/groot/homelab/internal/config"
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
	fmt.Printf("  3. %s\n\n", styles.Primary.Render(fmt.Sprintf("homelab enable %s --private", name)))
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
	serviceCmd.AddCommand(serviceAddCmd)
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
