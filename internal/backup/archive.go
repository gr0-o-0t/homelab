package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
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

	// BackupsRoot is where the backups volume is mounted in this (the leader
	// agent's) container; every backup and restore directory lies under it.
	// BackupsVolume is that volume's name: a helper container cannot bind a
	// path of this container, so it mounts the volume itself.
	BackupsRoot   string
	BackupsVolume string
	// ContainerFor names the shared database's task container to `docker
	// exec` into (db.Provisioner.Container).
	ContainerFor func(config.DBType) (string, error)
	// Life stops and starts a service around its snapshot or restore.
	Life Lifecycle
	// PrepareDB readies a service's shared databases before their dumps are
	// restored into them (optional).
	PrepareDB func(ctx context.Context, svc string) error
	// HelperImage runs the volume helpers: the agent's own image, already
	// on the node and pinned by version (busybox tar, sh and rm). Its
	// entrypoint is overridden per helper.
	HelperImage string
	// CreateVolumes lets a restore create a volume missing on this node
	// (restore --create-volumes, onto a fresh cluster).
	CreateVolumes bool
}

// helperMount is where helper containers mount the backups volume.
const helperMount = "/backups"

// helperPath is dir as a helper container sees it: dir must lie inside the
// backups volume, since that volume is all the helper can reach.
func (e *Engine) helperPath(dir string) (string, error) {
	if e.BackupsRoot == "" || e.BackupsVolume == "" {
		return "", errors.New("no backups volume configured")
	}
	rel, ok := under(e.BackupsRoot, dir)
	if !ok {
		return "", fmt.Errorf("%s is not inside the backups volume (%s)", dir, e.BackupsRoot)
	}
	return path.Join(helperMount, rel), nil
}

// container resolves the shared instance's task container for dbType.
func (e *Engine) container(dbType config.DBType) (string, error) {
	if e.ContainerFor == nil {
		return "", fmt.Errorf("no way to find the %s container", dbType)
	}
	c, err := e.ContainerFor(dbType)
	if err != nil {
		return "", fmt.Errorf("finding the %s container: %w", dbType, err)
	}
	return c, nil
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
// destDir must be absolute and inside the backups volume: the helper
// container that tars volumes writes there through its own mount of it.
func (e *Engine) Backup(p Plan, destDir string, live bool) (ServiceRecord, error) {
	rec := ServiceRecord{Name: p.Service, SkippedRedis: p.SkippedRedis, Live: live}

	if !filepath.IsAbs(destDir) {
		return rec, fmt.Errorf("backup destination must be an absolute path, got %q", destDir)
	}
	helperVols, err := e.helperPath(filepath.Join(destDir, "volumes"))
	if err != nil {
		return rec, err
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
		if err := e.archiveVolume(vol, helperVols, file); err != nil {
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

	container, err := e.container(d.Type)
	if err != nil {
		return err
	}
	var args []string
	switch d.Type {
	case config.DBPostgres:
		// Custom format: compressed, and pg_restore can then be selective and
		// use --clean --if-exists on the way back in.
		args = []string{"exec", container, "pg_dump", "-U", "postgres", "-Fc", d.Database}
	case config.DBMariaDB:
		args = []string{"exec", container, "mysqldump", "-u", "root",
			"--single-transaction", "--routines", "--triggers", d.Database}
	default:
		return fmt.Errorf("cannot dump database type %q", d.Type)
	}

	if err := e.Exec.RunTo(f, "docker", args...); err != nil {
		return fmt.Errorf("dumping %s database %q: %w", d.Type, d.Database, err)
	}
	return f.Close()
}

// archiveVolume tars a named volume into helperDir/file (a path inside the
// helper's mount of the backups volume) via a one-shot container on this
// node — the leader, where every data volume is pinned.
//
// The volume must already exist here: `docker run -v` would otherwise create
// an empty one and back that up as if it were the service's data.
func (e *Engine) archiveVolume(vol, helperDir, file string) error {
	if !e.volumeExists(vol) {
		return fmt.Errorf("volume %q is not on this node: backups run on the leader, where data volumes live "+
			"— has the service run here yet?", vol)
	}
	err := e.runHelper(vol,
		"-v", vol+":/from:ro",
		"-v", e.BackupsVolume+":"+helperMount,
		"--entrypoint", "tar",
		e.HelperImage,
		"czf", path.Join(helperDir, file), "-C", "/from", ".",
	)
	if err != nil {
		return fmt.Errorf("archiving volume %q: %w", vol, err)
	}
	return nil
}

// volumeExists reports whether the named volume is on this node.
func (e *Engine) volumeExists(vol string) bool {
	_, err := e.Exec.Output("docker", "volume", "inspect", "--format", "{{.Name}}", vol)
	return err == nil
}

// runHelper runs a one-shot helper container for volume vol detached and
// waits for it. Detached, no signal reaching this process (or its attached
// docker CLI) can stop the helper midway through clearing or unpacking a
// volume. Not --rm: an auto-removed container that exits before `docker
// wait` attaches makes the wait fail on a missing container, so the exit
// code would be lost; it is removed explicitly once its code is read.
func (e *Engine) runHelper(vol string, args ...string) error {
	if e.HelperImage == "" {
		return errors.New("no helper image configured")
	}
	if err := e.clearStaleHelpers(vol); err != nil {
		return err
	}
	out, err := e.Exec.Output("docker", append([]string{"run", "-d",
		"--label", helperLabel, "--label", helperVolumeLabel + "=" + vol}, args...)...)
	if err != nil {
		return fmt.Errorf("starting the helper container: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return errors.New("starting the helper container: docker printed no container id")
	}
	out, err = e.Exec.Output("docker", "wait", id)
	if err != nil {
		// Not removed: `rm -f` would kill it if it is still clearing or
		// unpacking a volume. Its label finds it, and the next run on this
		// volume refuses to start beside it.
		return fmt.Errorf("waiting for the helper container %s (left in place, label %s=%s): %w",
			id, helperVolumeLabel, vol, err)
	}
	defer func() { _, _ = e.Exec.Output("docker", "rm", "-f", id) }()
	if code := strings.TrimSpace(string(out)); code != "0" {
		logs, _ := e.Exec.Output("docker", "logs", "--tail", "20", id)
		return fmt.Errorf("helper container exited %s: %s", code, strings.TrimSpace(string(logs)))
	}
	return nil
}

// clearStaleHelpers deals with helpers an earlier run left on vol (a failed
// `docker wait`, an agent restart): one still working refuses this run — two
// helpers clearing and unpacking one volume would interleave — and finished
// ones are removed, so they never hold the volume against `docker volume rm`.
func (e *Engine) clearStaleHelpers(vol string) error {
	out, err := e.Exec.Output("docker", "ps", "-a", "--filter", "label="+helperVolumeLabel+"="+vol,
		"--format", "{{.ID}} {{.State}}")
	if err != nil {
		return fmt.Errorf("looking for earlier helper containers on volume %q: %w", vol, err)
	}
	for line := range strings.Lines(string(out)) {
		id, state, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch state {
		case "":
			continue
		case "exited", "dead", "created":
			if _, err := e.Exec.Output("docker", "rm", id); err != nil {
				return fmt.Errorf("removing the finished helper container %s on volume %q: %w", id, vol, err)
			}
		default:
			return fmt.Errorf("a helper container (%s, %s) from an earlier backup or restore is still working on volume %q "+
				"— refusing to start another beside it. Follow it with `docker logs -f %s`; once it has exited, "+
				"`docker rm %s` (`docker rm -f %s` stops it midway, which may leave the volume half-written)",
				id, state, vol, id, id, id)
		}
	}
	return nil
}

// helperLabel marks helper containers, so a leftover one is recognisable;
// helperVolumeLabel names the volume it works on.
const (
	helperLabel       = "homelab.helper=backup"
	helperVolumeLabel = "homelab.helper.volume"
)

// Restore puts a service's volumes and databases back from srcDir.
//
// Config files are NOT restored by default: the installed copies are usually
// current and the archived ones may be older than the catalog. Pass
// restoreConfig to overwrite them.
func (e *Engine) Restore(rec ServiceRecord, srcDir string, restoreConfig bool) error {
	if !filepath.IsAbs(srcDir) {
		return fmt.Errorf("backup source must be an absolute path, got %q", srcDir)
	}
	helperVols, err := e.helperPath(filepath.Join(srcDir, "volumes"))
	if err != nil {
		return err
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
		if err := e.restoreVolume(v, helperVols); err != nil {
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
// opposite of a restore. The archive is read end to end before anything is
// cleared, so a missing or truncated archive fails with the volume untouched.
//
// helperDir is the archive directory inside the helper's read-only mount of
// the backups volume. A volume missing on this node is refused unless
// CreateVolumes: silently creating one could restore into a volume the
// service never uses (another node's, a renamed one).
func (e *Engine) restoreVolume(v VolumeRecord, helperDir string) error {
	if !e.CreateVolumes && !e.volumeExists(v.Volume) {
		return fmt.Errorf("volume %q is not on this node — refusing to create it; "+
			"pass --create-volumes to restore into a new volume (a fresh cluster)", v.Volume)
	}
	err := e.runHelper(v.Volume,
		"-v", v.Volume+":/to",
		"-v", e.BackupsVolume+":"+helperMount+":ro",
		"--entrypoint", "sh",
		e.HelperImage,
		"-c", restoreVolumeScript, "sh", path.Join(helperDir, v.File),
	)
	if err != nil {
		return fmt.Errorf("restoring volume %q: %w", v.Volume, err)
	}
	return nil
}

// restoreVolumeScript verifies the archive ($1) before replacing /to with it.
const restoreVolumeScript = `set -e
test -f "$1"
tar tzf "$1" >/dev/null
rm -rf /to/..?* /to/.[!.]* /to/*
tar xzf "$1" -C /to`

// restoreDatabase loads a dump back into the shared instance. The database and
// role must already exist — `homelab setup <service>` creates them, and on a new
// machine it also re-syncs the role password with the keyring.
func (e *Engine) restoreDatabase(d DatabaseRecord, path string) error {
	f, err := os.Open(path) // #nosec G304 -- d.File validated by Manifest.Validate
	if err != nil {
		return fmt.Errorf("opening dump %s: %w", d.File, err)
	}
	defer func() { _ = f.Close() }()

	container, err := e.container(d.Type)
	if err != nil {
		return err
	}
	switch d.Type {
	case config.DBPostgres:
		// --clean --if-exists drops existing objects first so a restore over a
		// populated database replaces it instead of colliding with it.
		// --no-privileges: the dump's grants name roles a fresh cluster may
		// not have (a read-only consumer's); the consumer's next up re-grants.
		if err := e.Exec.RunFrom(f, "docker", "exec", "-i", container,
			"pg_restore", "-U", "postgres", "-d", d.Database,
			"--clean", "--if-exists", "--no-owner", "--no-privileges", "--role="+d.User); err != nil {
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
