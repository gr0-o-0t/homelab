package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/backup"
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
