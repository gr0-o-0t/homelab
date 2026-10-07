package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestList_NewestFirstSkipsNonBackups(t *testing.T) {
	root := t.TempDir()
	write := func(name string, at time.Time, recs ...ServiceRecord) {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		e := &Engine{Now: func() time.Time { return at }}
		require.NoError(t, e.WriteManifest(dir, recs))
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	write("old", t0, ServiceRecord{Name: "a"})
	write("new", t0.Add(time.Hour), ServiceRecord{Name: "b"}, ServiceRecord{Name: "c", Live: true})
	require.NoError(t, os.MkdirAll(filepath.Join(root, "junk"), 0o750))

	got, err := List(root)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, filepath.Join(root, "new"), got[0].Dir)
	assert.Equal(t, []string{"b", "c"}, got[0].Services)
	assert.True(t, got[0].Live)
	assert.Equal(t, []string{"a"}, got[1].Services)
	assert.False(t, got[1].Live)
}

func TestList_MissingRootIsEmpty(t *testing.T) {
	got, err := List(filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	assert.Empty(t, got)
}
