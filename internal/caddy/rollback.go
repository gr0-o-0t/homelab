package caddy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/groot/homelab/internal/configgen"
	"github.com/groot/homelab/internal/network/layers"
)

// ErrInvalidConfig marks a Reload that failed because `caddy validate`
// rejected the config on disk — as opposed to Caddy not running. Only this
// failure is rolled back: the files are what is wrong, and leaving them in
// place means Caddy refuses to start at its next restart, taking every route
// down with it.
var ErrInvalidConfig = errors.New("caddy validate failed")

// Snapshot is the state of every imported Caddy config directory at one point
// in time, so a write that produced an invalid config can be undone.
//
// It covers whole directories rather than the files a writer meant to touch:
// enable writes through several hands (configgen and each network layer), and
// only the directory listing knows everything they did.
type Snapshot struct {
	dirs map[string]map[string]entry // dir → basename → state
}

type entry struct {
	data []byte
	mode os.FileMode
}

// Snapshot records the current Caddy config. Take it before writing, then
// call ReloadOrRestore instead of Reload.
func (m *Manager) Snapshot() (*Snapshot, error) {
	s := &Snapshot{dirs: map[string]map[string]entry{}}
	for _, l := range layers.New(m.RepoRoot, nil, nil).All() {
		dir := configgen.ConfigDir(m.RepoRoot, l.ConfDir())
		files := map[string]entry{}
		des, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("snapshotting %s: %w", dir, err)
		}
		for _, de := range des {
			if de.IsDir() {
				continue
			}
			path := filepath.Join(dir, de.Name())
			fi, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue // dangling link left by the retired symlink scheme
			}
			if err != nil {
				return nil, err
			}
			data, err := os.ReadFile(path) //nolint:gosec // path from our own listing
			if err != nil {
				return nil, err
			}
			files[de.Name()] = entry{data: data, mode: fi.Mode().Perm()}
		}
		s.dirs[dir] = files
	}
	return s, nil
}

// Restore puts every config directory back the way the snapshot found it:
// files added since are removed, changed or removed ones are rewritten.
func (s *Snapshot) Restore() error {
	var errs []error
	for dir, files := range s.dirs {
		des, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		for _, de := range des {
			if de.IsDir() {
				continue
			}
			if _, kept := files[de.Name()]; !kept {
				if err := os.Remove(filepath.Join(dir, de.Name())); err != nil && !os.IsNotExist(err) {
					errs = append(errs, err)
				}
			}
		}
		for name, e := range files {
			errs = append(errs, restoreEntry(filepath.Join(dir, name), e))
		}
	}
	return errors.Join(errs...)
}

func restoreEntry(path string, e entry) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, e.data, e.mode)
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
