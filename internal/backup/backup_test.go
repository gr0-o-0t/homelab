package backup

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

type call struct {
	Name string
	Args []string
	// Stdin/Stdout record which streaming variant was used.
	Stdin, Stdout bool
}

func (c call) line() string { return c.Name + " " + strings.Join(c.Args, " ") }

type fakeExec struct {
	calls []call
	// dumpBody is written to the stdout writer of RunTo, standing in for the
	// bytes a real pg_dump would emit.
	dumpBody string
	err      error
}

func (f *fakeExec) Run(name string, args ...string) error {
	f.calls = append(f.calls, call{Name: name, Args: args})
	return f.err
}

func (f *fakeExec) RunTo(w io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, call{Name: name, Args: args, Stdout: true})
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(w, f.dumpBody)
	return err
}

func (f *fakeExec) RunFrom(r io.Reader, name string, args ...string) error {
	f.calls = append(f.calls, call{Name: name, Args: args, Stdin: true})
	if f.err != nil {
		return f.err
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (f *fakeExec) Output(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{Name: name, Args: args})
	return nil, f.err
}

func (f *fakeExec) find(substr string) (call, bool) {
	for _, c := range f.calls {
		if strings.Contains(c.line(), substr) {
			return c, true
		}
	}
	return call{}, false
}

// writeService lays out an installed service with volumes and a database.
func writeService(t *testing.T, root, name, compose, cfg string) {
	t.Helper()
	dir := filepath.Join(root, "services", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600))
}

const twoVolCompose = `
services:
  app:
    image: example:1
volumes:
  data:
    name: app_data
  cache:
    name: app_cache
`

const pgRedisCfg = `
databases:
  - postgres:
      database: appdb
      user: appuser
      env:
        dsn: DATABASE_URL
  - redis:
      env:
        host: REDIS_HOST
ports:
  - 80
`

func TestPlanFor(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "app", twoVolCompose, pgRedisCfg)

	p, err := PlanFor(root, "app")
	require.NoError(t, err)

	assert.Equal(t, []string{"app_cache", "app_data"}, p.Volumes, "named volumes, sorted")
	require.Len(t, p.Databases, 1, "postgres is dumpable")
	assert.Equal(t, "appdb", p.Databases[0].Database)
	assert.Equal(t, "homelab-postgres", p.Databases[0].Container)

	// Redis is a cache/queue here, never a system of record. Restoring a stale
	// queue is worse than starting empty, so it is counted, not dumped.
	assert.Equal(t, 1, p.SkippedRedis)

	assert.Contains(t, p.ConfigFiles, "config.yaml")
	assert.Contains(t, p.ConfigFiles, "docker-compose.yml")
	assert.False(t, p.Empty())
}

// A volume with no explicit `name:` gets a project-derived name at runtime that
// cannot be resolved from the file, so backing it up would silently target the
// wrong volume. Skipping is the honest behaviour.
func TestPlanFor_SkipsUnnamedVolumes(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "app", "services:\n  app:\n    image: x\nvolumes:\n  data: {}\n", "ports:\n  - 80\n")

	p, err := PlanFor(root, "app")
	require.NoError(t, err)
	assert.Empty(t, p.Volumes)
}

func TestBackup_WritesEverything(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "app", twoVolCompose, pgRedisCfg)
	dest := t.TempDir()

	fx := &fakeExec{dumpBody: "PGDMP-fake"}
	e := &Engine{ConfigDir: root, Exec: fx, Now: func() time.Time { return time.Unix(0, 0).UTC() }}

	p, err := PlanFor(root, "app")
	require.NoError(t, err)

	rec, err := e.Backup(p, dest, false)
	require.NoError(t, err)

	// Config files copied verbatim.
	got, err := os.ReadFile(filepath.Join(dest, "services", "app", "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, pgRedisCfg, string(got))

	// Dump streamed to a file, not buffered through the manifest.
	dump, err := os.ReadFile(filepath.Join(dest, "databases", "app-appdb.dump"))
	require.NoError(t, err)
	assert.Equal(t, "PGDMP-fake", string(dump))

	c, ok := fx.find("pg_dump")
	require.True(t, ok, "should have invoked pg_dump")
	assert.Contains(t, c.line(), "-Fc", "custom format so pg_restore can use --clean")
	assert.True(t, c.Stdout, "dump must stream to the file")

	// Each volume archived through a helper container, read-only on the source.
	for _, vol := range []string{"app_data", "app_cache"} {
		c, ok := fx.find(vol + ":/from:ro")
		require.True(t, ok, "volume %s should be archived", vol)
		assert.Contains(t, c.line(), "tar czf /to/"+vol+".tar.gz")
	}

	assert.Equal(t, "app", rec.Name)
	assert.Len(t, rec.Volumes, 2)
	assert.Len(t, rec.Databases, 1)
	assert.Equal(t, 1, rec.SkippedRedis)
	assert.False(t, rec.Live)
}

// Docker rejects relative bind-mount sources, and the failure surfaces as an
// opaque daemon error, so catch it before spawning anything.
func TestBackup_RequiresAbsoluteDestination(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "app", twoVolCompose, pgRedisCfg)

	e := &Engine{ConfigDir: root, Exec: &fakeExec{}}
	p, err := PlanFor(root, "app")
	require.NoError(t, err)

	_, err = e.Backup(p, "relative/dir", false)
	assert.ErrorContains(t, err, "absolute")
}

// The property that makes a restore a restore: the volume is emptied first.
// Untarring over live contents merges the two and leaves files the backup never
// contained.
func TestRestore_ClearsVolumeBeforeUnpacking(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	fx := &fakeExec{}
	e := &Engine{ConfigDir: root, Exec: fx}

	rec := ServiceRecord{
		Name:    "app",
		Volumes: []VolumeRecord{{Volume: "app_data", File: "app_data.tar.gz"}},
	}
	require.NoError(t, e.Restore(rec, src, false))

	c, ok := fx.find("app_data:/to")
	require.True(t, ok, "volume should be mounted writable")
	line := c.line()
	assert.Contains(t, line, "rm -rf", "existing contents must be removed first")
	assert.Contains(t, line, "tar xzf /from/app_data.tar.gz -C /to")
	assert.Less(t, strings.Index(line, "rm -rf"), strings.Index(line, "tar xzf"),
		"the clear must precede the unpack")
	assert.Contains(t, line, filepath.Join(src, "volumes")+":/from:ro",
		"the archive directory is mounted read-only")
}

func TestRestore_PostgresUsesCleanRestore(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "databases"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "databases", "app-appdb.dump"),
		[]byte("PGDMP-fake"), 0o600))

	fx := &fakeExec{}
	e := &Engine{ConfigDir: root, Exec: fx}

	rec := ServiceRecord{
		Name: "app",
		Databases: []DatabaseRecord{{
			Type: config.DBPostgres, Database: "appdb", User: "appuser", File: "app-appdb.dump",
		}},
	}
	require.NoError(t, e.Restore(rec, src, false))

	c, ok := fx.find("pg_restore")
	require.True(t, ok)
	line := c.line()
	assert.Contains(t, line, "-d appdb")
	assert.Contains(t, line, "--clean")
	assert.Contains(t, line, "--if-exists", "a restore into a fresh database must not fail on missing objects")
	assert.Contains(t, line, "--role=appuser", "objects should end up owned by the service role")
	assert.Contains(t, line, "exec -i", "the dump is piped in")
	assert.True(t, c.Stdin, "dump must stream from the file")
}

// Config files are the one thing not restored by default: the installed copies
// are usually newer than the archived ones.
func TestRestore_ConfigOnlyOnRequest(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "services", "app"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "services", "app", "config.yaml"),
		[]byte("archived: true\n"), 0o600))

	installed := filepath.Join(root, "services", "app", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o750))
	require.NoError(t, os.WriteFile(installed, []byte("installed: true\n"), 0o600))

	e := &Engine{ConfigDir: root, Exec: &fakeExec{}}
	rec := ServiceRecord{Name: "app", ConfigFiles: []string{"config.yaml"}}

	require.NoError(t, e.Restore(rec, src, false))
	body, err := os.ReadFile(installed)
	require.NoError(t, err)
	assert.Equal(t, "installed: true\n", string(body), "config must be left alone by default")

	require.NoError(t, e.Restore(rec, src, true))
	body, err = os.ReadFile(installed)
	require.NoError(t, err)
	assert.Equal(t, "archived: true\n", string(body), "--config should overwrite it")
}

func TestManifest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{Exec: &fakeExec{}, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}

	records := []ServiceRecord{{
		Name:      "app",
		Volumes:   []VolumeRecord{{Volume: "app_data", File: "app_data.tar.gz"}},
		Databases: []DatabaseRecord{{Type: config.DBPostgres, Database: "appdb", File: "app-appdb.dump"}},
		Live:      true,
	}}
	require.NoError(t, e.WriteManifest(dir, records))

	m, err := ReadManifest(dir)
	require.NoError(t, err)
	assert.Equal(t, ManifestVersion, m.Version)
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), m.Created.UTC())

	rec, ok := m.Find("app")
	require.True(t, ok)
	assert.Equal(t, "app_data", rec.Volumes[0].Volume)
	assert.True(t, rec.Live, "a live snapshot must stay flagged through restore")

	_, ok = m.Find("absent")
	assert.False(t, ok)
}

func TestReadManifest_Errors(t *testing.T) {
	t.Run("not a backup", func(t *testing.T) {
		_, err := ReadManifest(t.TempDir())
		assert.ErrorContains(t, err, "is not a homelab backup")
	})

	// Refusing a newer manifest beats misreading it and restoring the wrong
	// thing into a volume.
	t.Run("newer version refused", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName),
			[]byte(`{"version":999,"services":[]}`), 0o600))
		_, err := ReadManifest(dir)
		assert.ErrorContains(t, err, "newer homelab")
	})

	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), []byte(`{`), 0o600))
		_, err := ReadManifest(dir)
		assert.ErrorContains(t, err, "parsing")
	})
}

func TestDumpFile_ExtensionPerEngine(t *testing.T) {
	pg := DatabaseTarget{Type: config.DBPostgres, Database: "appdb"}
	assert.Equal(t, "app-appdb.dump", pg.DumpFile("app"), "pg_dump custom format")

	my := DatabaseTarget{Type: config.DBMariaDB, Database: "appdb"}
	assert.Equal(t, "app-appdb.sql", my.DumpFile("app"), "mysqldump plain SQL")
}

// ── manifest validation ───────────────────────────────────────────────────────

// A manifest arrives with whatever backup directory the user points at, and
// restore turns its strings into filesystem paths and shell arguments. These
// are the two shapes that used to work.
func TestReadManifest_RejectsHostileNames(t *testing.T) {
	for name, m := range map[string]Manifest{
		"traversal in service name": {Version: ManifestVersion, Services: []ServiceRecord{
			{Name: "../../..", ConfigFiles: []string{"config.yaml"}},
		}},
		"traversal in config file": {Version: ManifestVersion, Services: []ServiceRecord{
			{Name: "app", ConfigFiles: []string{"../../../.bashrc"}},
		}},
		// restoreVolume interpolates File into `tar xzf /from/<file>` in a
		// shell command, so this was command execution during restore.
		"shell metacharacters in volume file": {Version: ManifestVersion, Services: []ServiceRecord{
			{Name: "app", Volumes: []VolumeRecord{{Volume: "app_data", File: "x.tgz; id > /tmp/pwned"}}},
		}},
		"traversal in dump file": {Version: ManifestVersion, Services: []ServiceRecord{
			{Name: "app", Databases: []DatabaseRecord{{Database: "appdb", File: "../../etc/passwd"}}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			data, err := json.Marshal(m)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), data, 0o600))

			_, err = ReadManifest(dir)
			assert.Error(t, err, "restore must refuse this manifest")
			assert.Contains(t, err.Error(), "refusing to restore")
		})
	}
}
