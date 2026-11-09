package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Lifecycle stops a service around a snapshot or a restore and brings it
// back exactly as it was. On the cluster it is the Swarm Deployer: replica
// counts per Swarm service, Scale (only the counts — no pull, no registry,
// no re-translated config) and WaitStopped (scaling only changes the spec;
// containers stop within their grace period).
type Lifecycle interface {
	Replicas(ctx context.Context, svc string) (map[string]uint64, error)
	Scale(ctx context.Context, svc string, replicas map[string]uint64) error
	WaitStopped(ctx context.Context, svc string) error
}

// stopped records svc's replica counts, scales it to zero and waits for its
// containers to stop — waiting even when it was already scaled down, since
// a `homelab down` issued a moment ago leaves containers in their stop
// grace period. It returns the counts to scale back to (nil: nothing to do).
// On failure it has already scaled back.
func (e *Engine) stopped(ctx context.Context, svc string) (map[string]uint64, error) {
	if e.Life == nil {
		return nil, errors.New("no way to stop and start services")
	}
	reps, err := e.Life.Replicas(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("reading %s's replica counts: %w", svc, err)
	}
	running := false
	zero := make(map[string]uint64, len(reps))
	for name, n := range reps {
		running = running || n > 0
		zero[name] = 0
	}
	if !running {
		reps = nil
	} else if err := e.Life.Scale(ctx, svc, zero); err != nil {
		return nil, errors.Join(fmt.Errorf("scaling %s down: %w", svc, err), e.scaleBack(ctx, svc, reps))
	}
	if err := e.Life.WaitStopped(ctx, svc); err != nil {
		return nil, errors.Join(fmt.Errorf("waiting for %s to stop: %w", svc, err), e.scaleBack(ctx, svc, reps))
	}
	return reps, nil
}

// scaleBack restores the recorded counts. Not cancelled with ctx: an
// interrupt must still bring the service back.
func (e *Engine) scaleBack(ctx context.Context, svc string, reps map[string]uint64) error {
	if reps == nil {
		return nil
	}
	if err := e.Life.Scale(context.WithoutCancel(ctx), svc, reps); err != nil {
		return fmt.Errorf("scaling %s back up: %w", svc, err)
	}
	return nil
}

// BackupService snapshots one service, scaled down for the duration unless
// live. It is always scaled back to its recorded counts, even when the
// snapshot failed: a service left down because a tar step failed is a worse
// outcome than the failed backup. Both errors are reported.
func (e *Engine) BackupService(ctx context.Context, p Plan, destDir string, live bool) (ServiceRecord, error) {
	if live {
		return e.Backup(p, destDir, true)
	}
	reps, err := e.stopped(ctx, p.Service)
	if err != nil {
		return ServiceRecord{Name: p.Service}, err
	}
	rec, err := e.Backup(p, destDir, false)
	return rec, errors.Join(err, e.scaleBack(ctx, p.Service, reps))
}

// RestoreService restores one service: checked against the installed
// service, scaled down and stopped, its shared databases readied, volumes
// and databases replaced, then scaled back to its recorded counts — only
// when all of that succeeded. A failed restore leaves the service down:
// starting it on half-replaced data would be worse.
func (e *Engine) RestoreService(ctx context.Context, rec ServiceRecord, srcDir string, restoreConfig bool) error {
	if err := e.checkRestorable(rec, srcDir, restoreConfig); err != nil {
		return err
	}
	reps, err := e.stopped(ctx, rec.Name)
	if err != nil {
		return err
	}
	// Dumps are restored into the shared instance, so it has to be up even
	// though the service itself is stopped.
	if len(rec.Databases) > 0 && e.PrepareDB != nil {
		err = e.PrepareDB(ctx, rec.Name)
	}
	if err == nil {
		err = e.Restore(rec, srcDir, restoreConfig)
	}
	if err != nil {
		return fmt.Errorf("restore failed; %s is left stopped — rerun `homelab restore %s %s`, "+
			"or `homelab up %s` to start it on the current data: %w", rec.Name, srcDir, rec.Name, rec.Name, err)
	}
	return e.scaleBack(ctx, rec.Name, reps)
}

// checkRestorable refuses a restore before anything is stopped: the
// backup's volumes and databases must be the ones the service declares (its
// archived config with --config, else the installed one), and every volume
// must exist on this node unless CreateVolumes.
func (e *Engine) checkRestorable(rec ServiceRecord, srcDir string, restoreConfig bool) error {
	root := e.ConfigDir
	if restoreConfig {
		root = srcDir // a backup holds services/<svc>/ like a config dir
	}
	p, err := PlanFor(root, rec.Name)
	if err != nil {
		return fmt.Errorf("checking the backup of %s against its config: %w", rec.Name, err)
	}
	var haveVols, wantVols, haveDBs, wantDBs []string
	for _, v := range rec.Volumes {
		haveVols = append(haveVols, v.Volume)
	}
	for _, d := range rec.Databases {
		haveDBs = append(haveDBs, string(d.Type)+"/"+d.Database)
	}
	wantVols = append(wantVols, p.Volumes...)
	for _, d := range p.Databases {
		wantDBs = append(wantDBs, string(d.Type)+"/"+d.Database)
	}
	for _, s := range [][]string{haveVols, wantVols, haveDBs, wantDBs} {
		sort.Strings(s)
	}
	if !slices.Equal(haveVols, wantVols) || !slices.Equal(haveDBs, wantDBs) {
		return fmt.Errorf("the backup of %s does not match the service: backup has volumes %v and databases %v, "+
			"the service declares %v and %v — refusing to restore", rec.Name, haveVols, haveDBs, wantVols, wantDBs)
	}
	if e.CreateVolumes {
		return nil
	}
	for _, v := range rec.Volumes {
		if !e.volumeExists(v.Volume) {
			return fmt.Errorf("volume %q is not on this node — refusing to create it; "+
				"pass --create-volumes to restore into a new volume (a fresh cluster)", v.Volume)
		}
	}
	return nil
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
