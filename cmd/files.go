package cmd

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/desired"
	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// filesCmd is the leader-side half of `homelab edit`: it reads and writes a
// service's compose file, config.yaml and bind-mounted config files in the
// state dir. Hidden plumbing, forwarded to the agent like any command.
var filesCmd = &cobra.Command{
	Use:    "files",
	Short:  "Read and write a service's editable files (used by edit)",
	Hidden: true,
}

var filesGetCmd = &cobra.Command{
	Use:    "get <service> <path>",
	Short:  "Print a config file of the service (path relative to its directory)",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(c *cobra.Command, args []string) error {
		if err := validName(args[0]); err != nil {
			return err
		}
		// Read-only: no Docker, no secrets.
		d := &swarm.Deployer{Root: configDir(), ConfigFile: rootFlags.configFile}
		data, err := d.ReadConfigFile(args[0], args[1])
		if err != nil {
			return err
		}
		_, err = c.OutOrStdout().Write(data)
		return err
	},
}

var filesPutCmd = &cobra.Command{
	Use:    "put <service> <path>",
	Short:  "Replace an editable file of the service from stdin (validated) and redeploy it",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(c *cobra.Command, args []string) error {
		if err := validName(args[0]); err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(c.InOrStdin(), swarm.MaxConfigSize+1))
		if err != nil {
			return err
		}
		root, svc, rel := configDir(), args[0], args[1]
		// The secrets manager is required: validation checks no secret value
		// ends up in a spec.
		if err := withDeployer(root, true, func(_ context.Context, d *swarm.Deployer) error {
			return d.WriteConfigFile(svc, rel, data)
		}); err != nil {
			return err
		}
		return applyEdit(root, svc, rel, c.ErrOrStderr())
	},
}

func init() {
	filesCmd.AddCommand(filesGetCmd, filesPutCmd, filesInstallCmd)
	rootCmd.AddCommand(filesCmd)
}

// applyEdit rolls a saved file out the way `homelab up <svc>` does — shared
// databases and generated secrets prepared first — when the service is
// desired running. A stopped (or never started) service is left so: the
// file waits for the next `homelab up`.
func applyEdit(root, svc, rel string, w io.Writer) error {
	d, _, err := desired.LoadDesired(root)
	if err != nil {
		return err
	}
	if st := d.Services[svc]; st != desired.Running {
		state := "not running"
		if st == desired.Stopped {
			state = "stopped"
		}
		_, _ = fmt.Fprintf(w, "%s/%s saved; %s is %s — run `homelab up %s` to apply\n", svc, rel, svc, state, svc)
		return nil
	}
	recordService(root, svc, desired.Running)
	if err := upOne(root, svc); err != nil {
		return fmt.Errorf("saved %s, but redeploying %s failed: %w — the file is saved; run `homelab up %s` to retry", rel, svc, err, svc)
	}
	_, _ = fmt.Fprintf(w, "%s/%s saved and %s redeployed\n", svc, rel, svc)
	return nil
}

var filesInstallCmd = &cobra.Command{
	Use:    "install <service>",
	Short:  "Install a new service from a tar of its folder on stdin (used by add <dir>)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		svc := args[0]
		if err := validName(svc); err != nil {
			return err
		}
		root := configDir()
		// The secrets manager is required: validation checks no secret value
		// ends up in a spec.
		if err := withDeployer(root, true, func(_ context.Context, d *swarm.Deployer) error {
			if err := installService(d, svc, c.InOrStdin()); err != nil {
				return err
			}
			warnQualifiedLocal(c.ErrOrStderr(), root, svc, d.SM)
			return nil
		}); err != nil {
			return err
		}
		if err := config.EnsureRootDBConfig(rootConfigFile(), svc); err != nil {
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "warning: auto-configuring databases: %v\n", err)
		}
		_, _ = fmt.Fprintf(c.OutOrStdout(), "installed %s: run homelab setup %s (if it has secrets), homelab up %s, homelab enable %s\n",
			svc, svc, svc, svc)
		return nil
	},
}

// installService extracts the tar r (a service folder, `homelab add <dir>`)
// into a staging dir beside services/<svc>, validates it as svc's the way
// `files put` validates a spec file, and renames it into place. svc must not
// exist; on any failure nothing is left behind.
func installService(d *swarm.Deployer, svc string, r io.Reader) error {
	services := filepath.Join(d.Root, "services")
	dest := filepath.Join(services, svc)
	exists := func() error {
		if _, err := os.Lstat(dest); err == nil {
			return fmt.Errorf("services/%s already exists: remove it with homelab delete %s first, or change it with homelab edit", svc, svc)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := exists(); err != nil {
		return err
	}
	if err := os.MkdirAll(services, 0o750); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(services, ".install-"+svc+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // a no-op once renamed
	if err := extractTree(tmp, r); err != nil {
		return fmt.Errorf("%s refused, nothing installed: %w", svc, err)
	}
	if err := d.CheckServiceDir(svc, tmp); err != nil {
		return fmt.Errorf("%s refused, nothing installed: %w", svc, err)
	}
	if err := os.Chmod(tmp, 0o750); err != nil { // #nosec G302 -- a directory, as add creates them
		return err
	}
	if err := exists(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// extractTree writes the tar r into dir: directories and regular files only,
// under checkAddPath's rules, at most maxAddTree bytes of content.
func extractTree(dir string, r io.Reader) error {
	// Headers and padding fit easily in the second maxAddTree.
	tr := tar.NewReader(io.LimitReader(r, 2*maxAddTree))
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the folder: %w", err)
		}
		rel := strings.TrimSuffix(h.Name, "/")
		if err := checkAddPath(rel); err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(rel)) // #nosec G305 -- checkAddPath: relative, no dot components
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > maxAddTree {
				return fmt.Errorf("the folder is over the %d MiB limit", maxAddTree>>20)
			}
			if err := writeTarFile(target, tr, h.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: only regular files and directories are installed", h.Name)
		}
	}
}

func writeTarFile(target string, r io.Reader, size int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- under the staging dir
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// warnQualifiedLocal warns about each local image (pull_policy: never) of
// svc whose name has a namespace or registry host: `homelab update` never
// pulls it, but anything else that pulls by that name would get the
// registry's image instead of the one built here.
func warnQualifiedLocal(w io.Writer, root, svc string, sm *secrets.Manager) {
	env, _ := config.BuildEnv(rootConfigFile(), root, svc, sm)
	p, err := swarm.LoadProject(filepath.Join(root, "services", svc), env)
	if err != nil {
		return // validated already; nothing more to say
	}
	for _, key := range slices.Sorted(maps.Keys(p.Services)) {
		cs := p.Services[key]
		if cs.PullPolicy != types.PullPolicyNever {
			continue
		}
		named, err := reference.ParseNormalizedNamed(cs.Image)
		if err != nil {
			continue
		}
		if reference.Domain(named) == "docker.io" && strings.Count(reference.Path(named), "/") == 1 &&
			strings.HasPrefix(reference.Path(named), "library/") {
			continue
		}
		_, _ = fmt.Fprintf(w, "warning: local image %s has a namespace or registry host; a registry image of that name would replace it\n", cs.Image)
	}
}
