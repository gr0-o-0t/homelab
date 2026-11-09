package swarm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/config"
)

// MaxConfigSize is Swarm's limit for one config (500 KiB).
const MaxConfigSize = 500 * 1024

// SpecFiles are the service's own definition files `homelab edit` may change
// besides its bind-mounted configs. exposure.yaml is not one: enable and
// disable own it.
var SpecFiles = map[string]bool{"docker-compose.yml": true, "config.yaml": true}

// ErrNotConfig means a path is not a file the service bind-mounts, so it is
// not editable through the agent.
var ErrNotConfig = errors.New("not a bind-mounted config of the service")

// resolveConfigFile validates rel (slash-separated, relative to the service
// directory) as a bind-mounted config file of svc and returns its absolute
// path. Only paths Translate turns into Swarm configs qualify, plus the
// SpecFiles; never exposure.yaml. No component of the path
// may be a symlink, so nothing resolves outside the service directory.
func (d *Deployer) resolveConfigFile(svc, rel string) (string, error) {
	if err := config.ValidServiceName(svc); err != nil {
		return "", err
	}
	if rel == "" || strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%q: path must be relative to the service directory", rel)
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q: path escapes the service directory", rel)
	}
	dir := d.serviceDir(svc)
	if !SpecFiles[clean] {
		if err := d.isConfig(svc, dir, clean); err != nil {
			return "", err
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	// Check-then-use: a symlink planted between this walk and the rename
	// is not a concern. The agent is the sole writer of the state dir, and
	// rename replaces the directory entry atomically (never following one).
	cur := real
	for _, part := range strings.Split(clean, "/") {
		cur = filepath.Join(cur, part)
		i, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break // the rest does not exist yet, so cannot be a symlink
		}
		if err != nil {
			return "", err
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s: %s goes through a symlink", svc, clean)
		}
	}
	return filepath.Join(real, filepath.FromSlash(clean)), nil
}

// ReadConfigFile returns the content of a bind-mounted config file of svc.
func (d *Deployer) ReadConfigFile(svc, rel string) ([]byte, error) {
	if err := config.ValidServiceName(svc); err != nil {
		return nil, err
	}
	abs, err := d.resolveConfigFile(svc, rel)
	if err != nil {
		return nil, err
	}
	i, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %s is not a regular file", svc, rel)
	}
	return os.ReadFile(abs) // #nosec G304 -- validated by resolveConfigFile
}

// WriteConfigFile atomically replaces (or creates) a bind-mounted config file,
// or one of the SpecFiles, of svc and redeploys the service so the new content
// rolls out (a config as a new Swarm config). A SpecFile that does not load
// and translate is refused and the previous content kept. If the redeploy
// fails the file stays written, and the error says so.
func (d *Deployer) WriteConfigFile(ctx context.Context, svc, rel string, data []byte) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("refusing an empty file: Swarm configs cannot be empty")
	}
	if len(data) > MaxConfigSize {
		return fmt.Errorf("%d bytes is over the %d byte limit for a Swarm config", len(data), MaxConfigSize)
	}
	abs, err := d.resolveConfigFile(svc, rel)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if i, err := os.Lstat(abs); err == nil {
		if !i.Mode().IsRegular() {
			return fmt.Errorf("%s: %s is not a regular file", svc, rel)
		}
		mode = i.Mode().Perm()
	}
	spec := SpecFiles[path.Clean(filepath.ToSlash(rel))]
	orig, origErr := os.ReadFile(abs) // #nosec G304 -- validated by resolveConfigFile
	if err := writeFileAtomic(abs, data, mode); err != nil {
		return err
	}
	if spec {
		// The service's own definition: it must load and translate — with
		// the current env and secrets, so a secret interpolated into the
		// spec is refused too — or the previous content goes back.
		if err := d.checkSpec(svc); err != nil {
			if errors.Is(origErr, fs.ErrNotExist) {
				_ = os.Remove(abs)
			} else if rerr := writeFileAtomic(abs, orig, mode); rerr != nil {
				return errors.Join(fmt.Errorf("%s/%s refused: %w", svc, rel, err),
					fmt.Errorf("restoring the previous %s failed: %w", rel, rerr))
			}
			return fmt.Errorf("%s/%s refused, nothing changed: %w", svc, rel, err)
		}
	}
	if err := d.Up(ctx, svc); err != nil {
		return fmt.Errorf("saved %s, but redeploying %s failed: %w — the file is saved; run `homelab up %s` to retry the rollout", rel, svc, err, svc)
	}
	return nil
}

// checkSpec validates svc's config.yaml (schema, placement roles,
// cache_volumes naming compose volumes) and translates its stack as a deploy
// would, without touching Swarm.
func (d *Deployer) checkSpec(svc string) error {
	cfg, err := config.Load(config.ServiceConfigFile(d.Root, svc))
	if err != nil {
		return err
	}
	if cfg != nil && len(cfg.CacheVolumes) > 0 {
		var compose struct {
			Volumes map[string]any `yaml:"volumes"`
		}
		raw, err := os.ReadFile(filepath.Join(d.serviceDir(svc), "docker-compose.yml"))
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(raw, &compose); err != nil {
			return fmt.Errorf("parsing docker-compose.yml: %w", err)
		}
		for _, v := range cfg.CacheVolumes {
			if _, ok := compose.Volumes[v]; !ok {
				return fmt.Errorf("cache_volumes: %q is not a volume of docker-compose.yml", v)
			}
		}
	}
	_, err = d.stack(svc)
	return err
}

// writeFileAtomic replaces path with data through a temp file and a rename.
func writeFileAtomic(abs string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil { // #nosec G301 -- config tree
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".homelab-put-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}

// isConfig reports (nil) whether clean is a file Translate turns into a Swarm
// config of svc, or lies under such a bound directory.
func (d *Deployer) isConfig(svc, dir, clean string) error {
	st, err := d.stack(svc)
	if err != nil {
		return err
	}
	for _, fc := range st.Configs {
		if clean == fc.Source {
			return nil
		}
		if strings.HasPrefix(clean, fc.Source+"/") {
			// under a bound directory; not when Source is a file
			if i, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(fc.Source))); err == nil && i.IsDir() {
				return nil
			}
		}
	}
	return fmt.Errorf("%s: %s: %w", svc, clean, ErrNotConfig)
}
