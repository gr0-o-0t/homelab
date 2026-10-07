package caddy

import (
	"errors"
	"fmt"
	"os"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/exposure"
)

// ErrInvalidConfig marks a Reload that failed because `caddy validate`
// rejected the config on disk — as opposed to Caddy not running. Only this
// failure is rolled back: the files are what is wrong, and leaving them in
// place means Caddy refuses to start at its next restart, taking every route
// down with it.
var ErrInvalidConfig = errors.New("caddy validate failed")

// Snapshot is a service's routing files at one point in time — its sites file
// and its exposure.yaml — so a change that produced an invalid config can be
// undone. Those two files are everything enable/disable/reload write for
// Caddy; a layer's daemon side is undone by tearing the layer down.
type Snapshot struct {
	files map[string]*entry // path → content; nil = did not exist
}

type entry struct {
	data []byte
	mode os.FileMode
}

// Snapshot records a service's routing files. Take it before writing, then
// call ReloadOrRestore instead of Reload.
func (m *Manager) Snapshot(svc string) (*Snapshot, error) {
	s := &Snapshot{files: map[string]*entry{}}
	for _, path := range []string{configgen.SitesFile(m.RepoRoot, svc), exposure.Path(m.RepoRoot, svc)} {
		fi, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			s.files[path] = nil
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("snapshotting %s: %w", path, err)
		}
		data, err := os.ReadFile(path) //nolint:gosec // path built from the config dir
		if err != nil {
			return nil, err
		}
		s.files[path] = &entry{data: data, mode: fi.Mode().Perm()}
	}
	return s, nil
}

// Restore puts the files back the way the snapshot found them: rewritten if
// they existed, removed if they did not.
func (s *Snapshot) Restore() error {
	var errs []error
	for path, e := range s.files {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		if e != nil {
			errs = append(errs, os.WriteFile(path, e.data, e.mode))
		}
	}
	return errors.Join(errs...)
}

// ReloadOrRestore reloads Caddy and, if validation rejects the config, restores
// the snapshot so the bad config does not outlive this command. The returned
// error still reports the failure; it says so when the change was undone.
func (m *Manager) ReloadOrRestore(s *Snapshot) error {
	err := m.Reload()
	if err == nil || s == nil || !errors.Is(err, ErrInvalidConfig) {
		return err
	}
	if rerr := s.Restore(); rerr != nil {
		return errors.Join(err, fmt.Errorf("rolling back caddy config: %w", rerr))
	}
	return fmt.Errorf("%w (config change rolled back)", err)
}
