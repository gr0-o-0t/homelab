package backup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultDir is where `homelab backup` writes when --out is not given.
func DefaultDir(configDir string) string { return filepath.Join(configDir, "backups") }

// Listing summarises one backup directory, enough to pick one to restore.
type Listing struct {
	Dir      string    `json:"dir"` // absolute path: the argument `homelab restore` takes
	Created  time.Time `json:"created"`
	Services []string  `json:"services"`
	Live     bool      `json:"live"` // some service was taken with --live
}

// List returns the backups directly under root, newest first. A missing root
// is no backups, not an error; subdirectories without a valid manifest are
// skipped, since restore would refuse them anyway.
func List(root string) ([]Listing, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Listing
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		m, err := ReadManifest(dir)
		if err != nil {
			continue
		}
		l := Listing{Dir: dir, Created: m.Created, Services: []string{}}
		for _, r := range m.Services {
			l.Services = append(l.Services, r.Name)
			l.Live = l.Live || r.Live
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}
