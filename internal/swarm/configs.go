package swarm

import (
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
// or one of the SpecFiles, of svc. It does not redeploy: the caller brings
// the service up the way `homelab up` does (provisioning included). A
// SpecFile is validated before it replaces anything: the new content, staged
// in the service dir, must load and translate in a copy of the service's
// state, or it is refused and the file left as it was.
func (d *Deployer) WriteConfigFile(svc, rel string, data []byte) error {
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
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: reading %s: %w", svc, rel, err)
	}
	staged, err := stageFile(abs, data, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if clean := path.Clean(filepath.ToSlash(rel)); SpecFiles[clean] {
		// The service's own definition: it must load and translate — with
		// the current env and secrets, so a secret interpolated into the
		// spec is refused too.
		if err := d.checkStaged(svc, clean, data); err != nil {
			return fmt.Errorf("%s/%s refused, nothing changed: %w", svc, rel, err)
		}
	}
	return os.Rename(staged, abs)
}

// checkStaged runs checkSpec against a copy of the service's state — the
// root's own files and the service dir — with clean replaced by data, so
// the real files never hold content that has not passed.
func (d *Deployer) checkStaged(svc, clean string, data []byte) error {
	tmp, err := os.MkdirTemp(d.Root, ".spec-check-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if err := copyFile(filepath.Join(d.Root, e.Name()), filepath.Join(tmp, e.Name())); err != nil {
				return err
			}
		}
	}
	dst := filepath.Join(tmp, "services", svc)
	if err := copyTree(d.serviceDir(svc), dst); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, filepath.FromSlash(clean)), data, 0o600); err != nil {
		return err
	}
	c := *d
	c.Root = tmp
	return c.checkSpec(svc)
}

// copyTree copies the regular files and directories under src to dst;
// symlinks and other special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(target, 0o700)
		case e.Type().IsRegular():
			return copyFile(p, target)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) // #nosec G304 -- files of the state dir
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600) // #nosec G703 -- dst is under a temp dir this process made
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

// stageFile writes data to a temp file next to abs, ready to be renamed into
// place; the caller removes it if it is not.
func stageFile(abs string, data []byte, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil { // #nosec G301 -- config tree
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".homelab-put-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
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
