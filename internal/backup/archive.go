package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/groot/homelab/internal/config"
)

// ManifestVersion is bumped when the on-disk layout changes incompatibly.
const ManifestVersion = 1

// ManifestName is the file that makes a directory a homelab backup.
const ManifestName = "manifest.json"

// Manifest records what a backup contains so restore never has to guess.
type Manifest struct {
	Version  int             `json:"version"`
	Created  time.Time       `json:"created"`
	Services []ServiceRecord `json:"services"`
}

// ServiceRecord is one service's entry in a manifest.
type ServiceRecord struct {
	Name        string           `json:"name"`
	Volumes     []VolumeRecord   `json:"volumes,omitempty"`
	Databases   []DatabaseRecord `json:"databases,omitempty"`
	ConfigFiles []string         `json:"configFiles,omitempty"`
	// SkippedRedis is carried through so a restore can report the same caveat
	// the backup did.
	SkippedRedis int `json:"skippedRedis,omitempty"`
	// Live is true when the service was not stopped for the snapshot, meaning
	// volume contents may be torn.
	Live bool `json:"live"`
}

// VolumeRecord maps a Docker volume to its archive file.
type VolumeRecord struct {
	Volume string `json:"volume"`
	File   string `json:"file"`
}

// DatabaseRecord maps a database to its dump file.
type DatabaseRecord struct {
	Type     config.DBType `json:"type"`
	Database string        `json:"database"`
	User     string        `json:"user"`
	File     string        `json:"file"`
}

// Engine performs backup and restore operations.
type Engine struct {
	ConfigDir string
	Exec      Executor
	// Now supplies the manifest timestamp; injectable for deterministic tests.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Backup writes a service's volumes, database dumps and config files into
// destDir, returning the record to add to the manifest.
//
// destDir must already exist and be absolute — it is bind-mounted into the
// helper container, and Docker rejects relative bind sources.
func (e *Engine) Backup(p Plan, destDir string, live bool) (ServiceRecord, error) {
	rec := ServiceRecord{Name: p.Service, SkippedRedis: p.SkippedRedis, Live: live}

	if !filepath.IsAbs(destDir) {
		return rec, fmt.Errorf("backup destination must be an absolute path, got %q", destDir)
	}

	svcOut := filepath.Join(destDir, "services", p.Service)
	for _, dir := range []string{svcOut, filepath.Join(destDir, "volumes"), filepath.Join(destDir, "databases")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return rec, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Config files first: they are tiny and describe everything else, so a
	// backup that fails midway is still useful for diagnosis.
	srcDir := filepath.Join(e.ConfigDir, "services", p.Service)
	for _, name := range p.ConfigFiles {
		data, err := os.ReadFile(filepath.Join(srcDir, name)) // #nosec G304 -- name comes from our own scan of the service dir
		if err != nil {
			return rec, fmt.Errorf("reading %s/%s: %w", p.Service, name, err)
		}
		if err := os.WriteFile(filepath.Join(svcOut, name), data, 0o600); err != nil { // #nosec G703 -- name comes from our own scan of the service dir
			return rec, fmt.Errorf("writing %s: %w", name, err)
		}
		rec.ConfigFiles = append(rec.ConfigFiles, name)
	}

	for _, d := range p.Databases {
		file := d.DumpFile(p.Service)
		if err := e.dumpDatabase(d, filepath.Join(destDir, "databases", file)); err != nil {
			return rec, err
		}
		rec.Databases = append(rec.Databases, DatabaseRecord{
			Type: d.Type, Database: d.Database, User: d.User, File: file,
		})
	}

	for _, vol := range p.Volumes {
		file := vol + ".tar.gz"
		if err := e.archiveVolume(vol, filepath.Join(destDir, "volumes"), file); err != nil {
			return rec, err
		}
		rec.Volumes = append(rec.Volumes, VolumeRecord{Volume: vol, File: file})
	}

	return rec, nil
}

// dumpDatabase streams a logical dump out of the shared instance.
func (e *Engine) dumpDatabase(d DatabaseTarget, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- path is built by Engine, not by input
	if err != nil {
		return fmt.Errorf("creating dump file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var args []string
	switch d.Type {
	case config.DBPostgres:
		// Custom format: compressed, and pg_restore can then be selective and
		// use --clean --if-exists on the way back in.
		args = []string{"exec", d.Container, "pg_dump", "-U", "postgres", "-Fc", d.Database}
	case config.DBMariaDB:
		args = []string{"exec", d.Container, "mysqldump", "-u", "root",
			"--single-transaction", "--routines", "--triggers", d.Database}
	default:
		return fmt.Errorf("cannot dump database type %q", d.Type)
	}

	if err := e.Exec.RunTo(f, "docker", args...); err != nil {
		return fmt.Errorf("dumping %s database %q: %w", d.Type, d.Database, err)
	}
	return f.Close()
}

// archiveVolume tars a named volume into destDir/file via a helper container.
func (e *Engine) archiveVolume(vol, destDir, file string) error {
	err := e.Exec.Run("docker", "run", "--rm",
		"-v", vol+":/from:ro",
		"-v", destDir+":/to",
		helperImage,
		"tar", "czf", "/to/"+file, "-C", "/from", ".",
	)
	if err != nil {
		return fmt.Errorf("archiving volume %q: %w", vol, err)
	}
	return nil
}

// Restore puts a service's volumes and databases back from srcDir.
//
// Config files are NOT restored by default: the installed copies are usually
// current and the archived ones may be older than the catalog. Pass
// restoreConfig to overwrite them.
func (e *Engine) Restore(rec ServiceRecord, srcDir string, restoreConfig bool) error {
	if !filepath.IsAbs(srcDir) {
		return fmt.Errorf("backup source must be an absolute path, got %q", srcDir)
	}

	if restoreConfig {
		dstDir := filepath.Join(e.ConfigDir, "services", rec.Name)
		if err := os.MkdirAll(dstDir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", dstDir, err)
		}
		for _, name := range rec.ConfigFiles {
			data, err := os.ReadFile(filepath.Join(srcDir, "services", rec.Name, name)) // #nosec G304 G703 -- names validated by Manifest.Validate
			if err != nil {
				return fmt.Errorf("reading archived %s: %w", name, err)
			}
			if err := os.WriteFile(filepath.Join(dstDir, name), data, 0o600); err != nil { // #nosec G703 -- names validated by Manifest.Validate
				return fmt.Errorf("restoring %s: %w", name, err)
			}
		}
	}

	for _, v := range rec.Volumes {
		if err := e.restoreVolume(v, filepath.Join(srcDir, "volumes")); err != nil {
			return err
		}
	}

	for _, d := range rec.Databases {
		if err := e.restoreDatabase(d, filepath.Join(srcDir, "databases", d.File)); err != nil {
			return err
		}
	}
	return nil
}

// restoreVolume replaces a volume's contents with the archived copy.
//
// The existing contents are cleared first: untarring over a live volume merges
// the two, which silently leaves files that the backup did not contain — the
// opposite of a restore.
func (e *Engine) restoreVolume(v VolumeRecord, srcDir string) error {
	err := e.Exec.Run("docker", "run", "--rm",
		"-v", v.Volume+":/to",
		"-v", srcDir+":/from:ro",
		helperImage,
		"sh", "-c", "rm -rf /to/..?* /to/.[!.]* /to/* 2>/dev/null; tar xzf /from/"+v.File+" -C /to",
	)
	if err != nil {
		return fmt.Errorf("restoring volume %q: %w", v.Volume, err)
	}
	return nil
}

// restoreDatabase loads a dump back into the shared instance. The database and
// role must already exist — `homelab setup <service>` creates them, and on a new
// machine it also re-syncs the role password with the keyring.
func (e *Engine) restoreDatabase(d DatabaseRecord, path string) error {
	f, err := os.Open(path) // #nosec G304 -- d.File validated by Manifest.Validate
	if err != nil {
		return fmt.Errorf("opening dump %s: %w", d.File, err)
	}
	defer func() { _ = f.Close() }()

	container := config.SharedDBContainer(d.Type)
	switch d.Type {
	case config.DBPostgres:
		// --clean --if-exists drops existing objects first so a restore over a
		// populated database replaces it instead of colliding with it.
		if err := e.Exec.RunFrom(f, "docker", "exec", "-i", container,
			"pg_restore", "-U", "postgres", "-d", d.Database,
			"--clean", "--if-exists", "--no-owner", "--role="+d.User); err != nil {
			return fmt.Errorf("restoring postgres database %q: %w", d.Database, err)
		}
	case config.DBMariaDB:
		if err := e.Exec.RunFrom(f, "docker", "exec", "-i", container,
			"mysql", "-u", "root", d.Database); err != nil {
			return fmt.Errorf("restoring mariadb database %q: %w", d.Database, err)
		}
	default:
		return fmt.Errorf("cannot restore database type %q", d.Type)
	}
	return nil
}

// WriteManifest records the backup contents alongside the data.
func (e *Engine) WriteManifest(destDir string, records []ServiceRecord) error {
	m := Manifest{Version: ManifestVersion, Created: e.now(), Services: records}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(destDir, ManifestName), data, 0o600)
}

// safeNameRe matches a single plain path element: no separators, no traversal,
// nothing the shell treats as syntax.
var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// checkName rejects a manifest string that must not reach a path or a shell.
//
// Empty passes: it cannot traverse or inject, and an optional field left unset
// is a schema question, not a safety one — the operation that needs it fails
// on its own terms with a clearer message than this could give.
func checkName(field, value string) error {
	if value == "" {
		return nil
	}
	if value == ".." || !safeNameRe.MatchString(value) {
		return fmt.Errorf("manifest %s %q is not a plain name — refusing to restore", field, value)
	}
	return nil
}

// Validate checks every manifest string that restore turns into a filesystem
// path or a command argument.
//
// A manifest is untrusted input: it arrives with whatever backup directory the
// user points at — a NAS share, a USB stick, a copy from another machine — and
// restore feeds its strings into filepath.Join and, for volumes, straight into
// a `tar xzf /from/<file>` shell command. Before this check, a manifest with
// "file": "x.tgz; curl … | sh" ran that command as part of the restore, and a
// "name": "../../.." wrote service config outside the config directory.
//
// Every one of these names is generated by our own backup code, so the rule is
// an allowlist rather than an attempt to escape hostile input.
func (m Manifest) Validate() error {
	for _, rec := range m.Services {
		if rec.Name == "" {
			return fmt.Errorf("manifest has a service with no name — refusing to restore")
		}
		if err := checkName("service name", rec.Name); err != nil {
			return err
		}
		for _, f := range rec.ConfigFiles {
			if err := checkName("config file", f); err != nil {
				return err
			}
		}
		for _, v := range rec.Volumes {
			if err := checkName("volume", v.Volume); err != nil {
				return err
			}
			if err := checkName("volume file", v.File); err != nil {
				return err
			}
		}
		for _, d := range rec.Databases {
			for field, value := range map[string]string{
				"database": d.Database, "database user": d.User, "database file": d.File,
			} {
				if err := checkName(field, value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ReadManifest loads and validates the manifest from a backup directory.
func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, ManifestName)) // #nosec G304 -- dir is the backup the user named
	if err != nil {
		return m, fmt.Errorf("%s is not a homelab backup (no %s): %w", dir, ManifestName, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parsing %s: %w", ManifestName, err)
	}
	if m.Version > ManifestVersion {
		return m, fmt.Errorf("backup was written by a newer homelab (manifest version %d, this build understands %d)",
			m.Version, ManifestVersion)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Find returns the record for a service, if the backup contains it.
func (m Manifest) Find(svcName string) (ServiceRecord, bool) {
	for _, r := range m.Services {
		if r.Name == svcName {
			return r, true
		}
	}
	return ServiceRecord{}, false
}
