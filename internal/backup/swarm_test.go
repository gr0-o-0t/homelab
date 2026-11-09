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

// fakeLife stands in for the Swarm Deployer: it records each lifecycle call
// and how many docker commands had run by then.
type fakeLife struct {
	reps     map[string]uint64
	fx       *fakeExec
	log      []string
	scaleErr error // for the scale-down only
	waitErr  error
	backErr  error // for the scale-back
}

func (l *fakeLife) note(what string) { l.log = append(l.log, what+"@"+itoa(len(l.fx.calls))) }

func itoa(n int) string { return strconv.Itoa(n) }

func (l *fakeLife) Replicas(context.Context, string) (map[string]uint64, error) {
	out := map[string]uint64{}
	for k, v := range l.reps {
		out[k] = v
	}
	return out, nil
}

func (l *fakeLife) Scale(_ context.Context, _ string, r map[string]uint64) error {
	total := uint64(0)
	for _, n := range r {
		total += n
	}
	if total == 0 {
		l.note("down")
		return l.scaleErr
	}
	l.note("back" + strconv.FormatUint(total, 10))
	return l.backErr
}

func (l *fakeLife) WaitStopped(context.Context, string) error { l.note("wait"); return l.waitErr }

func running(fx *fakeExec) *fakeLife {
	return &fakeLife{reps: map[string]uint64{"app_app": 1, "app_worker": 2}, fx: fx}
}

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
		HelperImage:  "sha256:agentimage",
	}
	return e, backups
}

func TestBackup_ScalesDownAndUp(t *testing.T) {
	fx := &fakeExec{dumpBody: "PGDMP"}
	e, backups := newSwarmEngine(t, fx)
	life := running(fx)
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	rec, err := e.BackupService(context.Background(), p, filepath.Join(backups, "20260101-000000"), false)
	require.NoError(t, err)
	assert.False(t, rec.Live)
	assert.Equal(t, []string{"down@0", "wait@0", "back3@" + itoa(len(fx.calls))}, life.log,
		"scaled to zero and stopped before any snapshot step, back to the exact counts after the last")
}

// A service already scaled down (`homelab down` a moment ago) is not started,
// but its containers may still be in their stop grace period: wait anyway.
func TestBackup_WaitsStoppedEvenWhenAlreadyDown(t *testing.T) {
	fx := &fakeExec{}
	e, backups := newSwarmEngine(t, fx)
	life := &fakeLife{reps: map[string]uint64{"app_app": 0}, fx: fx}
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)
	_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), false)
	require.NoError(t, err)
	assert.Equal(t, []string{"wait@0"}, life.log, "waited, never scaled")
}

func TestBackup_LiveIsNotTouched(t *testing.T) {
	fx := &fakeExec{}
	e, backups := newSwarmEngine(t, fx)
	life := running(fx)
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)
	_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), true)
	require.NoError(t, err)
	assert.Empty(t, life.log)
}

func TestBackup_ScalesBackEvenWhenSnapshotFails(t *testing.T) {
	fx := &fakeExec{err: errors.New("tar exploded")}
	e, backups := newSwarmEngine(t, fx)
	life := running(fx)
	life.backErr = errors.New("scale refused")
	e.Life = life
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)

	_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), false)
	require.Error(t, err)
	require.Len(t, life.log, 3)
	assert.True(t, strings.HasPrefix(life.log[2], "back3@"), "scaled back after a failed snapshot")
	assert.ErrorContains(t, err, "tar exploded", "the snapshot error is reported")
	assert.ErrorContains(t, err, "scale refused", "and so is the scale-back error")
}

func TestBackup_ScalesBackWhenStopFails(t *testing.T) {
	for name, life := range map[string]*fakeLife{
		"scale down": {scaleErr: errors.New("scale failed")},
		"wait":       {waitErr: errors.New("still running")},
	} {
		t.Run(name, func(t *testing.T) {
			fx := &fakeExec{}
			e, backups := newSwarmEngine(t, fx)
			life.fx, life.reps = fx, map[string]uint64{"app_app": 1}
			e.Life = life
			p, err := PlanFor(e.ConfigDir, "app")
			require.NoError(t, err)
			_, err = e.BackupService(context.Background(), p, filepath.Join(backups, "x"), false)
			require.Error(t, err)
			assert.Empty(t, fx.calls, "no snapshot of a half-stopped service")
			assert.Equal(t, "back1@0", life.log[len(life.log)-1], "scaled back")
		})
	}
}

// Helpers run detached and are waited on, so no signal can stop one midway.
func TestBackup_HelperRunsDetachedAndIsWaitedOn(t *testing.T) {
	fx := &fakeExec{dumpBody: "PGDMP"}
	e, backups := newSwarmEngine(t, fx)
	p, err := PlanFor(e.ConfigDir, "app")
	require.NoError(t, err)
	_, err = e.Backup(p, filepath.Join(backups, "b"), false)
	require.NoError(t, err)

	run, ok := fx.find("app_data:/from:ro")
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(run.line(), "docker run -d "), run.line())
	assert.NotContains(t, run.line(), "--rm")
	assert.Contains(t, run.line(), "--entrypoint tar sha256:agentimage czf", "the agent's own image")
	_, ok = fx.find("docker wait cid1")
	assert.True(t, ok)
	_, ok = fx.find("docker rm -f cid1")
	assert.True(t, ok, "the helper is removed once its exit code is read")

	fx.waitCode = "1"
	_, err = e.Backup(p, filepath.Join(backups, "c"), false)
	assert.ErrorContains(t, err, "helper container exited 1: tar: short read")
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
	assert.Contains(t, line, "czf /backups/20260101-000000/volumes/app_data.tar.gz -C /from .")
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

// appRecord matches what the app service declares (twoVolCompose, pgRedisCfg).
func appRecord() ServiceRecord {
	return ServiceRecord{
		Name: "app",
		Volumes: []VolumeRecord{
			{Volume: "app_cache", File: "app_cache.tar.gz"},
			{Volume: "app_data", File: "app_data.tar.gz"},
		},
		Databases: []DatabaseRecord{{Type: config.DBPostgres, Database: "appdb", User: "appuser", File: "app-appdb.dump"}},
	}
}

func restoreFixture(t *testing.T, fx *fakeExec) (*Engine, *fakeLife, string) {
	t.Helper()
	e, backups := newSwarmEngine(t, fx)
	src := filepath.Join(backups, "20260101-000000")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "databases"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "databases", "app-appdb.dump"), []byte("PGDMP"), 0o600))
	life := running(fx)
	e.Life = life
	return e, life, src
}

// helpers are the helper `docker run` lines, in order.
func helpers(fx *fakeExec) []string {
	var out []string
	for _, c := range fx.calls {
		if len(c.Args) > 0 && c.Args[0] == "run" {
			out = append(out, c.line())
		}
	}
	return out
}

func TestRestore_DownRestoreUp(t *testing.T) {
	fx := &fakeExec{}
	e, life, src := restoreFixture(t, fx)
	var prepared []string
	e.PrepareDB = func(_ context.Context, svc string) error {
		prepared = append(prepared, svc+"@"+itoa(len(fx.calls)))
		return nil
	}
	callsBefore := 0
	require.NoError(t, e.RestoreService(context.Background(), appRecord(), src, false))

	require.Len(t, life.log, 3)
	assert.Equal(t, "down", strings.Split(life.log[0], "@")[0])
	assert.Equal(t, "wait", strings.Split(life.log[1], "@")[0])
	assert.Equal(t, "back3@"+itoa(len(fx.calls)), life.log[2], "scaled back to the recorded counts at the end")
	downAt, _ := strconv.Atoi(strings.Split(life.log[1], "@")[1])
	assert.Equal(t, []string{"app@" + itoa(downAt)}, prepared, "the shared DB is readied after the stop, before restoring")
	for _, c := range fx.calls[callsBefore:downAt] {
		assert.Equal(t, "volume", c.Args[0], "only volume checks run before the service is stopped")
	}

	runs := helpers(fx)
	require.Len(t, runs, 2)
	assert.Contains(t, runs[1], "homelab-backups:/backups:ro")
	assert.True(t, strings.HasSuffix(runs[1], " sh /backups/20260101-000000/volumes/app_data.tar.gz"), runs[1])
	pg, ok := fx.find("pg_restore")
	require.True(t, ok)
	assert.Contains(t, pg.line(), "exec -i pg-task-123 pg_restore")
}

// A failed restore leaves the service down: scaling it back would start it
// on half-replaced data.
func TestRestore_FailureLeavesServiceStopped(t *testing.T) {
	fx := &fakeExec{waitCode: "1"}
	e, life, src := restoreFixture(t, fx)
	err := e.RestoreService(context.Background(), appRecord(), src, false)
	require.Error(t, err)
	assert.ErrorContains(t, err, "restore failed; app is left stopped")
	assert.ErrorContains(t, err, "`homelab up app`")
	assert.ErrorContains(t, err, "helper container exited 1")
	for _, l := range life.log {
		assert.False(t, strings.HasPrefix(l, "back"), "not scaled back: %v", life.log)
	}
}

func TestRestore_StopFailureScalesBackDataUntouched(t *testing.T) {
	fx := &fakeExec{}
	e, life, src := restoreFixture(t, fx)
	life.waitErr = errors.New("still running")
	err := e.RestoreService(context.Background(), appRecord(), src, false)
	assert.ErrorContains(t, err, "still running")
	assert.Empty(t, helpers(fx), "nothing restored")
	assert.True(t, strings.HasPrefix(life.log[len(life.log)-1], "back3@"), "nothing was touched, so it is brought back")
}

func TestRestore_WaitsStoppedEvenWhenAlreadyDown(t *testing.T) {
	fx := &fakeExec{}
	e, life, src := restoreFixture(t, fx)
	life.reps = map[string]uint64{"app_app": 0}
	require.NoError(t, e.RestoreService(context.Background(), appRecord(), src, false))
	require.Len(t, life.log, 1)
	assert.True(t, strings.HasPrefix(life.log[0], "wait@"), "a just-downed service is waited on before rm -rf")
}

func TestRestore_RefusesMissingVolumeUnlessCreateVolumes(t *testing.T) {
	fx := &fakeExec{missing: map[string]bool{"app_data": true}}
	e, life, src := restoreFixture(t, fx)
	err := e.RestoreService(context.Background(), appRecord(), src, false)
	assert.ErrorContains(t, err, `volume "app_data" is not on this node — refusing to create it`)
	assert.ErrorContains(t, err, "--create-volumes")
	assert.Empty(t, life.log, "refused before anything was stopped")
	assert.Empty(t, helpers(fx))

	e.CreateVolumes = true
	require.NoError(t, e.RestoreService(context.Background(), appRecord(), src, false))
	assert.Len(t, helpers(fx), 2, "both volumes restored, the missing one created by the helper")
}

func TestRestore_RefusesManifestNotMatchingService(t *testing.T) {
	for name, mod := range map[string]func(*ServiceRecord){
		"extra volume": func(r *ServiceRecord) {
			r.Volumes = append(r.Volumes, VolumeRecord{Volume: "other_data", File: "o.tar.gz"})
		},
		"missing volume": func(r *ServiceRecord) { r.Volumes = r.Volumes[:1] },
		"other database": func(r *ServiceRecord) { r.Databases[0].Database = "otherdb" },
		"no database":    func(r *ServiceRecord) { r.Databases = nil },
	} {
		t.Run(name, func(t *testing.T) {
			fx := &fakeExec{}
			e, life, src := restoreFixture(t, fx)
			rec := appRecord()
			mod(&rec)
			err := e.RestoreService(context.Background(), rec, src, false)
			assert.ErrorContains(t, err, "does not match the service")
			assert.Empty(t, life.log)
			assert.Empty(t, fx.calls)
		})
	}
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
