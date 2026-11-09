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

	"github.com/groot/homelab/internal/config"
)

// MaxConfigSize is Swarm's limit for one config (500 KiB).
const MaxConfigSize = 500 * 1024

// ErrNotConfig means a path is not a file the service bind-mounts, so it is
// not editable through the agent.
var ErrNotConfig = errors.New("not a bind-mounted config of the service")

// resolveConfigFile validates rel (slash-separated, relative to the service
// directory) as a bind-mounted config file of svc and returns its absolute
// path. Only paths Translate turns into Swarm configs qualify: never
// config.yaml, docker-compose.yml or exposure.yaml. No component of the path
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
	st, err := d.stack(svc)
	if err != nil {
		return "", err
	}
	dir := d.serviceDir(svc)
	ok := false
	for _, fc := range st.Configs {
		if clean == fc.Source {
			ok = true
		} else if strings.HasPrefix(clean, fc.Source+"/") {
			ok = true // under a bound directory; refused below if Source is a file
			if i, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(fc.Source))); err != nil || !i.IsDir() {
				ok = false
			}
		}
		if ok {
			break
		}
	}
	if !ok {
		return "", fmt.Errorf("%s: %s: %w", svc, clean, ErrNotConfig)
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

// WriteConfigFile atomically replaces (or creates) a bind-mounted config file
// of svc and redeploys the service so the new content rolls out as a new
// Swarm config. If the redeploy fails the file stays written, and the error
// says so.
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
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return err
	}
	if err := d.Up(ctx, svc); err != nil {
		return fmt.Errorf("saved %s, but redeploying %s failed: %w — the file is saved; run `homelab up %s` to retry the rollout", rel, svc, err, svc)
	}
	return nil
}
