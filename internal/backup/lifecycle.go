package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Lifecycle stops and starts a service around a snapshot or a restore. On
// the cluster it is the Swarm Deployer: Down scales to zero (keeping the
// volumes) and returns once the containers have stopped, Up deploys again.
type Lifecycle interface {
	// Running reports whether the service is meant to be up now; a service
	// that is down is neither stopped nor started.
	Running(ctx context.Context, svc string) (bool, error)
	Down(ctx context.Context, svc string) error
	Up(ctx context.Context, svc string) error
}

// around runs fn with the service stopped when it was running: Down first,
// and Up afterwards whatever happened — a service left down because a tar
// step failed is a worse outcome than the failed step. Both errors are
// reported. fn is skipped when Down fails, since a half-stopped service is
// not a consistent snapshot either.
func (e *Engine) around(ctx context.Context, svc string, fn func() error) error {
	if e.Life == nil {
		return errors.New("no way to stop and start services")
	}
	running, err := e.Life.Running(ctx, svc)
	if err != nil {
		return fmt.Errorf("checking whether %s is running: %w", svc, err)
	}
	if !running {
		return fn()
	}
	if err = e.Life.Down(ctx, svc); err != nil {
		err = fmt.Errorf("stopping %s: %w", svc, err)
	} else {
		err = fn()
	}
	// Not cancelled with ctx: an interrupt during the snapshot must still
	// bring the service back.
	if upErr := e.Life.Up(context.WithoutCancel(ctx), svc); upErr != nil {
		err = errors.Join(err, fmt.Errorf("starting %s again: %w", svc, upErr))
	}
	return err
}

// BackupService snapshots one service, stopping it for the duration unless
// live (a stopped service is backed up as it is).
func (e *Engine) BackupService(ctx context.Context, p Plan, destDir string, live bool) (ServiceRecord, error) {
	if live {
		return e.Backup(p, destDir, true)
	}
	var rec ServiceRecord
	err := e.around(ctx, p.Service, func() error {
		var err error
		rec, err = e.Backup(p, destDir, false)
		return err
	})
	if rec.Name == "" {
		rec.Name = p.Service
	}
	return rec, err
}

// RestoreService restores one service: Down, its shared databases readied,
// volumes and databases replaced, Up — when it was running beforehand.
func (e *Engine) RestoreService(ctx context.Context, rec ServiceRecord, srcDir string, restoreConfig bool) error {
	return e.around(ctx, rec.Name, func() error {
		// Dumps are restored into the shared instance, so it has to be up
		// even though the service itself is stopped.
		if len(rec.Databases) > 0 && e.PrepareDB != nil {
			if err := e.PrepareDB(ctx, rec.Name); err != nil {
				return err
			}
		}
		return e.Restore(rec, srcDir, restoreConfig)
	})
}

// ResolveUnder resolves p (--out, or a backup to restore) inside the backups
// volume mounted at root: a relative p is taken relative to root, and the
// result — after symlinks in its existing part are resolved — must still lie
// under root. Anything else is refused: a helper container can only reach the
// backups volume, and backups must never land in the agent's state volume.
func ResolveUnder(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("the backups volume is not mounted at %s (backups run on the leader agent): %w", root, err)
	}
	target := p
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target = filepath.Clean(target)

	// Resolve the longest existing prefix; the rest is created later and
	// cannot be a symlink yet.
	existing, rest := target, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", p, err)
	}
	if _, ok := under(realRoot, filepath.Join(resolved, rest)); !ok {
		return "", fmt.Errorf("%s is outside the backups volume (%s): backups are kept in the leader's backups volume — "+
			"pass a directory under it, or copy one to this PC with `homelab backup --fetch <name> <dir>`", p, root)
	}
	return target, nil
}

// under reports whether p lies inside (or is) dir, and its slash-separated
// path relative to dir.
func under(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
