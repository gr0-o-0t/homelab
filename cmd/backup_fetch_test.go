package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/backup"
	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// stubBackupLeader serves `backup-export` from a temp backups volume, the
// way the leader agent would, and records the argv it was asked to run.
func stubBackupLeader(t *testing.T, root string, code int, err error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := backupLeader
	backupLeader = func(_ context.Context, argv []string, stdout io.Writer) (int, error) {
		calls = append(calls, argv)
		if err != nil || code != 0 {
			_, _ = stdout.Write([]byte("partial garbage"))
			return code, err
		}
		if e := backup.ExportTar(stdout, root, argv[len(argv)-1]); e != nil {
			return 1, nil
		}
		return 0, nil
	}
	t.Cleanup(func() { backupLeader = prev })
	return &calls
}

func writeLeaderBackup(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "databases"), 0o750))
	require.NoError(t, (&backup.Engine{}).WriteManifest(dir, []backup.ServiceRecord{{Name: "app"}}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "databases", "app-app.dump"), []byte("PGDMP"), 0o600))
}

func TestBackupFetch_StreamsTarIntoLocalDir(t *testing.T) {
	leader := t.TempDir()
	writeLeaderBackup(t, leader, "20260101-000000")
	calls := stubBackupLeader(t, leader, 0, nil)
	local := t.TempDir()

	require.NoError(t, runBackupFetch(context.Background(), "20260101-000000", local, io.Discard))
	assert.Equal(t, [][]string{{"backup-export", "20260101-000000"}}, *calls)
	got, err := os.ReadFile(filepath.Join(local, "20260101-000000", "databases", "app-app.dump"))
	require.NoError(t, err)
	assert.Equal(t, "PGDMP", string(got))
	entries, err := os.ReadDir(local)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no partial directory left behind")
}

func TestBackupFetch_RefusesExistingDestination(t *testing.T) {
	calls := stubBackupLeader(t, t.TempDir(), 0, nil)
	local := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(local, "b1"), 0o750))
	err := runBackupFetch(context.Background(), "b1", local, io.Discard)
	assert.ErrorContains(t, err, "already exists")
	assert.Empty(t, *calls, "nothing asked of the leader")
}

func TestBackupFetch_BadNameNeverReachesLeader(t *testing.T) {
	calls := stubBackupLeader(t, t.TempDir(), 0, nil)
	for _, name := range []string{"..", "a/b", "/etc", ""} {
		assert.Error(t, runBackupFetch(context.Background(), name, t.TempDir(), io.Discard), name)
	}
	assert.Empty(t, *calls)
}

func TestBackupFetch_LeaderFailureLeavesNothing(t *testing.T) {
	stubBackupLeader(t, t.TempDir(), 2, nil)
	local := t.TempDir()
	assert.Error(t, runBackupFetch(context.Background(), "b1", local, io.Discard))
	entries, err := os.ReadDir(local)
	require.NoError(t, err)
	assert.Empty(t, entries, "the partial copy is removed")

	stubBackupLeader(t, t.TempDir(), 0, errors.New("connection reset"))
	assert.ErrorContains(t, runBackupFetch(context.Background(), "b1", local, io.Discard), "connection reset")
}

// `backup --fetch` writes to the PC's disk, so it runs on the PC; every other
// form of backup is forwarded to the leader.
func TestRoot_BackupFetchRunsLocally(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := fakeAgent(t, 0, nil)
	stubBackupLeader(t, t.TempDir(), 3, nil)

	resetFlags()
	assert.NotEqual(t, 0, executeArgs([]string{"backup", "--fetch", "b1", t.TempDir()}))
	assert.Empty(t, *calls, "--fetch is not forwarded")

	resetFlags()
	executeArgs([]string{"backup", "web"})
	assert.Equal(t, [][]string{{"backup", "web"}}, *calls)
}

func TestBackup_OutOutsideBackupsVolumeRefused(t *testing.T) {
	prev := backupsRoot
	backupsRoot = t.TempDir()
	t.Cleanup(func() { backupsRoot = prev })
	resetFlags()
	backupFlags.out = "/mnt/nas"
	t.Cleanup(func() { backupFlags.out = "" })
	err := runBackup(backupCmd, []string{"web"})
	assert.ErrorContains(t, err, "backups volume")
}

// swarmLife drives the Deployer's count-only verbs: nothing is pulled or
// re-deployed around a snapshot.
func TestSwarmLife_ScalesCountsOnly(t *testing.T) {
	ctx := context.Background()
	f := swarmtest.New()
	one, two := uint64(1), uint64(2)
	for name, n := range map[string]*uint64{"app_app": &one, "app_worker": &two} {
		_, err := f.ServiceCreate(ctx, dswarm.ServiceSpec{
			Annotations: dswarm.Annotations{Name: name, Labels: map[string]string{swarm.LabelService: "app"}},
			Mode:        dswarm.ServiceMode{Replicated: &dswarm.ReplicatedService{Replicas: n}},
		}, dswarm.ServiceCreateOptions{})
		require.NoError(t, err)
	}
	l := swarmLife{d: &swarm.Deployer{API: f, Root: t.TempDir()}, timeout: time.Second}

	reps, err := l.Replicas(ctx, "app")
	require.NoError(t, err)
	assert.Equal(t, map[string]uint64{"app_app": 1, "app_worker": 2}, reps)
	require.NoError(t, l.Scale(ctx, "app", map[string]uint64{"app_app": 0, "app_worker": 0}))
	require.NoError(t, l.WaitStopped(ctx, "app"))
	require.NoError(t, l.Scale(ctx, "app", reps))
	got, err := l.Replicas(ctx, "app")
	require.NoError(t, err)
	assert.Equal(t, reps, got)
	for _, c := range f.CallLog() {
		assert.NotContains(t, c, "ImagePull")
	}
}

func TestBackupFetch_EmptyNameIsAClearError(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	fakeAgent(t, 0, nil)
	resetFlags()
	err := runBackupWithArgs(t, []string{"--fetch=", t.TempDir()})
	assert.ErrorContains(t, err, "--fetch needs a backup name")
}

// runBackupWithArgs parses args into backupCmd's flags and runs it.
func runBackupWithArgs(t *testing.T, args []string) error {
	t.Helper()
	require.NoError(t, backupCmd.ParseFlags(args))
	t.Cleanup(func() { backupFlags.fetch = ""; resetFlags() })
	return runBackup(backupCmd, backupCmd.Flags().Args())
}

// A local failure (here: a refused entry) is the cause, and is reported
// ahead of the leader's broken pipe.
func TestBackupFetch_ExtractionErrorFirst(t *testing.T) {
	prev := backupLeader
	backupLeader = func(ctx context.Context, _ []string, stdout io.Writer) (int, error) {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		_ = tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o600})
		_ = tw.Close()
		_, _ = stdout.Write(b.Bytes())
		<-ctx.Done() // the fetch cancels the session on a local failure
		return 1, ctx.Err()
	}
	t.Cleanup(func() { backupLeader = prev })
	err := runBackupFetch(context.Background(), "b1", t.TempDir(), io.Discard)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "refusing unsafe path")
	assert.Less(t, strings.Index(msg, "unpacking"), strings.Index(msg, "fetching backup"), msg)
}

func TestForwardDetaches_BackupAndRestoreOnly(t *testing.T) {
	for argv, want := range map[string]bool{
		"backup web": true, "restore 2026 web": true, "--no-color backup --all": true,
		"up web": false, "backup-export b1": false, "status": false,
	} {
		got, _ := forwardDetaches(strings.Fields(argv))
		assert.Equal(t, want, got, argv)
	}
}

func TestInterruptReader_CtrlCIsLocal(t *testing.T) {
	interrupted := 0
	r := &interruptReader{r: strings.NewReader("yes\n\x03more"), interrupt: func() { interrupted++ }}
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "yes\n", string(got), "the keystroke is not forwarded, nor anything after it")
	assert.Equal(t, 1, interrupted)
}
