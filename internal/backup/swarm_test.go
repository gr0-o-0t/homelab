package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/config"
)

// fakeLife stands in for the Swarm Deployer: it records the lifecycle calls
// and, for each, how many docker commands had run by then.
type fakeLife struct {
	running bool
	fx      *fakeExec
	log     []string
	downErr error
	upErr   error
}

func (l *fakeLife) note(what string) { l.log = append(l.log, what+"@"+itoa(len(l.fx.calls))) }

func itoa(n int) string { return strconv.Itoa(n) }

func (l *fakeLife) Running(context.Context, string) (bool, error) { return l.running, nil }
func (l *fakeLife) Down(context.Context, string) error            { l.note("down"); return l.downErr }
func (l *fakeLife) Up(context.Context, string) error              { l.note("up"); return l.upErr }

// newSwarmEngine is an Engine whose backups volume is a temp dir.
func newSwarmEngine(t *testing.T, fx *fakeExec) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	writeService(t, root, "app", twoVolCompose, pgRedisCfg)
	backups := t.TempDir()
	e := &Engine{
		ConfigDir: root, Exec: fx, BackupsRoot: backups, BackupsVolume: "homelab-backups",
		ContainerFor: func(config.DBType) (string, error) { return "pg-task-123", nil },
		Now:          func() time.Time { return time.Unix(0, 0).UTC() },
	}
	return e, backups
}

func TestBackup_ScalesDownAndUp(t *testing.T) {
	fx := &fakeExec{dumpBody: "PGDMP"}
	e, backups := newSwarmEngine(t, fx)
	life := &fakeLife{running: true, fx: fx}
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	rec, err := e.BackupService(context.Background(), p, filepath.Join(backups, "20260101-000000"), false)
	require.NoError(t, err)
	assert.False(t, rec.Live)
	require.Len(t, life.log, 2)
	assert.Equal(t, "down@0", life.log[0], "scaled down before any snapshot step")
	assert.Equal(t, "up@"+itoa(len(fx.calls)), life.log[1], "scaled back up after the last snapshot step")
}

func TestBackup_LiveAndStoppedServicesAreNotTouched(t *testing.T) {
	for name, tc := range map[string]struct {
		running, live bool
	}{"live": {running: true, live: true}, "not running": {running: false}} {
		t.Run(name, func(t *testing.T) {
			fx := &fakeExec{}
			e, backups := newSwarmEngine(t, fx)
			life := &fakeLife{running: tc.running, fx: fx}
			e.Life = life
			p, err := PlanFor(e.ConfigDir, "app")
			require.NoError(t, err)
			_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), tc.live)
			require.NoError(t, err)
			assert.Empty(t, life.log, "a stopped service is not started, a live one not stopped")
		})
	}
}

func TestBackup_UpRunsEvenWhenSnapshotFails(t *testing.T) {
	fx := &fakeExec{err: errors.New("tar exploded")}
	e, backups := newSwarmEngine(t, fx)
	life := &fakeLife{running: true, fx: fx, upErr: errors.New("up refused")}
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), false)
	require.Error(t, err)
	require.Len(t, life.log, 2)
	assert.True(t, strings.HasPrefix(life.log[1], "up@"), "Up is attempted after a failed snapshot")
	assert.ErrorContains(t, err, "tar exploded", "the snapshot error is reported")
	assert.ErrorContains(t, err, "up refused", "and so is the restart error")
}

func TestBackup_UpRunsEvenWhenDownFails(t *testing.T) {
	fx := &fakeExec{}
	e, backups := newSwarmEngine(t, fx)
	life := &fakeLife{running: true, fx: fx, downErr: errors.New("scale failed")}
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), false)
	assert.ErrorContains(t, err, "scale failed")
	assert.Equal(t, []string{"down@0", "up@0"}, life.log, "no snapshot of a half-stopped service; Up still runs")
}

// The CLI runs inside the leader agent: a helper container cannot bind its
// paths, so it mounts the backups volume itself and writes inside it.
func TestBackup_HelperMountsBackupsVolume(t *testing.T) {
	fx := &fakeExec{dumpBody: "PGDMP"}
	e, backups := newSwarmEngine(t, fx)
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	_, err = e.Backup(p, filepath.Join(backups, "20260101-000000"), false)
	require.NoError(t, err)

	c, ok := fx.find("app_data:/from:ro")
	require.True(t, ok)
	line := c.line()
	assert.Contains(t, line, "homelab-backups:/backups ")
	assert.Contains(t, line, "tar czf /backups/20260101-000000/volumes/app_data.tar.gz -C /from .")
	_, ok = fx.find("volume inspect --format {{.Name}} app_data")
	assert.True(t, ok, "the volume's presence on this node is checked first")

	dump, ok := fx.find("pg_dump")
	require.True(t, ok)
	assert.Contains(t, dump.line(), "exec pg-task-123 pg_dump", "dumps exec into the provider's task container")
}

func TestBackup_RefusesVolumeMissingOnThisNode(t *testing.T) {
	fx := &fakeExec{missing: map[string]bool{"app_data": true}}
	e, backups := newSwarmEngine(t, fx)
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	_, err = e.Backup(p, filepath.Join(backups, "x"), false)
	require.ErrorContains(t, err, `volume "app_data" is not on this node`)
	_, ran := fx.find("app_data:/from:ro")
	assert.False(t, ran, "no helper container for a volume that is not here")
}

func TestBackup_DestinationMustBeInBackupsVolume(t *testing.T) {
	fx := &fakeExec{}
	e, _ := newSwarmEngine(t, fx)
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)
	_, err = e.Backup(p, t.TempDir(), false)
	assert.ErrorContains(t, err, "not inside the backups volume")
}

func TestBackup_OutMustBeUnderBackupsVolume(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nas"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	for _, ok := range []struct{ in, want string }{
		{"", root},
		{"nas", filepath.Join(root, "nas")},
		{filepath.Join(root, "nas", "new"), filepath.Join(root, "nas", "new")},
		{"weekly/new", filepath.Join(root, "weekly", "new")},
	} {
		got, err := ResolveUnder(root, ok.in)
		require.NoError(t, err, ok.in)
		assert.Equal(t, ok.want, got, ok.in)
	}
	for _, bad := range []string{
		outside,
		"/var/lib/homelab/backups",
		filepath.Join(root, "..", filepath.Base(outside)),
		"../x",
		filepath.Join(root, "escape"),
		filepath.Join(root, "escape", "deeper"),
	} {
		_, err := ResolveUnder(root, bad)
		assert.ErrorContains(t, err, "backups volume", bad)
	}
}

// The on-disk format is what old backups and old CLIs read: field names and
// version must not move.
func TestBackup_ManifestUnchanged(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}
	require.NoError(t, e.WriteManifest(dir, []ServiceRecord{{
		Name:         "app",
		Volumes:      []VolumeRecord{{Volume: "app_data", File: "app_data.tar.gz"}},
		Databases:    []DatabaseRecord{{Type: config.DBPostgres, Database: "appdb", User: "appuser", File: "app-appdb.dump"}},
		ConfigFiles:  []string{"config.yaml"},
		SkippedRedis: 1,
	}}))
	got, err := os.ReadFile(filepath.Join(dir, ManifestName))
	require.NoError(t, err)
	assert.Equal(t, 1, ManifestVersion)
	assert.JSONEq(t, `{
	  "version": 1,
	  "created": "2026-01-02T03:04:05Z",
	  "services": [{
	    "name": "app",
	    "volumes": [{"volume": "app_data", "file": "app_data.tar.gz"}],
	    "databases": [{"type": "postgres", "database": "appdb", "user": "appuser", "file": "app-appdb.dump"}],
	    "configFiles": ["config.yaml"],
	    "skippedRedis": 1,
	    "live": false
	  }]
	}`, string(got))
}

func TestRestore_DownRestoreUp(t *testing.T) {
	fx := &fakeExec{}
	e, backups := newSwarmEngine(t, fx)
	src := filepath.Join(backups, "20260101-000000")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "databases"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "databases", "app-appdb.dump"), []byte("PGDMP"), 0o600))
	life := &fakeLife{running: true, fx: fx}
	e.Life = life
	var prepared []string
	e.PrepareDB = func(_ context.Context, svc string) error {
		prepared = append(prepared, svc+"@"+itoa(len(fx.calls)))
		return nil
	}
	rec := ServiceRecord{
		Name:      "app",
		Volumes:   []VolumeRecord{{Volume: "app_data", File: "app_data.tar.gz"}},
		Databases: []DatabaseRecord{{Type: config.DBPostgres, Database: "appdb", User: "appuser", File: "app-appdb.dump"}},
	}
	require.NoError(t, e.RestoreService(context.Background(), rec, src, false))

	require.Len(t, fx.calls, 2)
	assert.Equal(t, []string{"down@0", "up@2"}, life.log)
	assert.Equal(t, []string{"app@0"}, prepared, "the shared DB is readied after Down, before restoring")

	vol := fx.calls[0].line()
	assert.Contains(t, vol, "homelab-backups:/backups:ro")
	assert.True(t, strings.HasSuffix(vol, " sh /backups/20260101-000000/volumes/app_data.tar.gz"), vol)
	assert.Contains(t, fx.calls[1].line(), "exec -i pg-task-123 pg_restore")
}

func TestRestore_UpRunsEvenWhenRestoreFails(t *testing.T) {
	fx := &fakeExec{err: errors.New("bad archive")}
	e, backups := newSwarmEngine(t, fx)
	life := &fakeLife{running: true, fx: fx}
	e.Life = life
	rec := ServiceRecord{Name: "app", Volumes: []VolumeRecord{{Volume: "app_data", File: "app_data.tar.gz"}}}
	err := e.RestoreService(context.Background(), rec, filepath.Join(backups, "x"), false)
	assert.ErrorContains(t, err, "bad archive")
	assert.Equal(t, []string{"down@0", "up@1"}, life.log)
}

// ── fetch: the tar stream from the leader to a PC ─────────────────────────────

func writeBackup(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "volumes"), 0o750))
	require.NoError(t, (&Engine{}).WriteManifest(dir, []ServiceRecord{{Name: "app"}}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "volumes", "app_data.tar.gz"), []byte("TGZ"), 0o600))
}

func TestBackupFetch_StreamsTar(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, root, "20260101-000000")
	var buf bytes.Buffer
	require.NoError(t, ExportTar(&buf, root, "20260101-000000"))

	dest := t.TempDir()
	require.NoError(t, ExtractTar(&buf, dest))
	got, err := os.ReadFile(filepath.Join(dest, "volumes", "app_data.tar.gz"))
	require.NoError(t, err)
	assert.Equal(t, "TGZ", string(got))
	_, err = ReadManifest(dest)
	assert.NoError(t, err, "the fetched copy is a valid backup")
}

func TestBackupFetch_ExportRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, root, "b1")
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(root, "b1", "volumes", "leak")))
	err := ExportTar(&bytes.Buffer{}, root, "b1")
	assert.ErrorContains(t, err, "not a regular file")

	require.NoError(t, os.Symlink(filepath.Join(root, "b1"), filepath.Join(root, "b2")))
	assert.Error(t, ExportTar(&bytes.Buffer{}, root, "b2"), "the backup itself may not be a symlink")
}

func TestBackupFetch_RejectsBadName(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, root, "ok")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nomanifest"), 0o750))
	for _, name := range []string{"", "..", ".", "../etc", "a/b", "/etc", "missing", "nomanifest", ".hidden"} {
		err := ExportTar(&bytes.Buffer{}, root, name)
		assert.Error(t, err, "%q must be refused", name)
	}
}

func TestBackupFetch_RejectsTraversalInTar(t *testing.T) {
	entry := func(hdr tar.Header, body string) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		hdr.Size = int64(len(body))
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o600
		}
		require.NoError(t, tw.WriteHeader(&hdr))
		_, _ = tw.Write([]byte(body))
		require.NoError(t, tw.Close())
		return b.Bytes()
	}
	for name, stream := range map[string][]byte{
		"dot-dot":    entry(tar.Header{Name: "../evil"}, "x"),
		"nested":     entry(tar.Header{Name: "volumes/../../evil"}, "x"),
		"absolute":   entry(tar.Header{Name: "/tmp/evil"}, "x"),
		"symlink":    entry(tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, ""),
		"hardlink":   entry(tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}, ""),
		"device":     entry(tar.Header{Name: "dev", Typeflag: tar.TypeChar}, ""),
		"duplicates": append(entry(tar.Header{Name: "a"}, "1")[:1024], entry(tar.Header{Name: "a"}, "2")...),
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			require.NoError(t, os.Mkdir(dest, 0o750))
			err := ExtractTar(bytes.NewReader(stream), dest)
			require.Error(t, err)
			_, statErr := os.Lstat(filepath.Join(parent, "evil"))
			assert.True(t, os.IsNotExist(statErr), "nothing written outside dest")
		})
	}
}

func TestList_FromBackupsVolume(t *testing.T) {
	root := t.TempDir()
	writeBackup(t, root, "20260101-000000")
	got, err := List(root)
	require.NoError(t, err)
	require.Len(t, got, 1)
	b, _ := json.Marshal(got[0])
	assert.Contains(t, string(b), `"dir":"`+filepath.Join(root, "20260101-000000"))
}
