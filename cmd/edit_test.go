package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type leaderCall struct {
	argv  []string
	stdin string
}

// stubEdit stands in for the agent (get returns content) and the editor
// (edit rewrites the temp file; nil leaves it alone).
func stubEdit(t *testing.T, content string, getCode int, edit func(file string) error) *[]leaderCall {
	t.Helper()
	var calls []leaderCall
	pl, pe := editLeader, editRunEditor
	editLeader = func(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
		c := leaderCall{argv: argv}
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			c.stdin = string(b)
		}
		calls = append(calls, c)
		if argv[1] == "get" {
			_, _ = io.WriteString(stdout, content)
			return getCode, nil
		}
		return 0, nil
	}
	editRunEditor = func(_ []string, file string) error {
		fi, err := os.Stat(file)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		if edit != nil {
			return edit(file)
		}
		return nil
	}
	t.Cleanup(func() { editLeader, editRunEditor = pl, pe })
	return &calls
}

func TestEdit_UnchangedFileDoesNotPut(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := stubEdit(t, "a: 1\n", 0, nil)
	var out strings.Builder
	require.NoError(t, runEdit(context.Background(), "glance", "config/glance.yml", &out))
	assert.Contains(t, out.String(), "no changes")
	require.Len(t, *calls, 1)
	assert.Equal(t, []string{"files", "get", "glance", "config/glance.yml"}, (*calls)[0].argv)
}

func TestEdit_ChangedFilePutsContent(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	var tmpName string
	calls := stubEdit(t, "a: 1\n", 0, func(f string) error {
		tmpName = f
		return os.WriteFile(f, []byte("a: 2\n"), 0o600)
	})
	require.NoError(t, runEdit(context.Background(), "glance", "config/glance.yml", io.Discard))
	require.Len(t, *calls, 2)
	assert.Equal(t, []string{"files", "put", "glance", "config/glance.yml"}, (*calls)[1].argv)
	assert.Equal(t, "a: 2\n", (*calls)[1].stdin)
	_, err := os.Stat(tmpName)
	assert.True(t, os.IsNotExist(err), "the temp file is removed")
}

func TestEdit_EditorFailureDoesNotPut(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := stubEdit(t, "a: 1\n", 0, func(f string) error {
		_ = os.WriteFile(f, []byte("changed"), 0o600)
		return errors.New("exit status 1")
	})
	err := runEdit(context.Background(), "glance", "config/glance.yml", io.Discard)
	require.Error(t, err)
	assert.Len(t, *calls, 1, "only the get ran")
}

func TestEdit_GetFailureStopsBeforeEditor(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := stubEdit(t, "", 1, func(string) error { t.Fatal("editor ran"); return nil })
	require.Error(t, runEdit(context.Background(), "glance", "nope", io.Discard))
	assert.Len(t, *calls, 1)
}

func TestEdit_RefusedInsideTheAgent(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "1")
	calls := stubEdit(t, "x", 0, nil)
	resetFlags()
	assert.Equal(t, 1, executeArgs([]string{"edit", "glance", "config/glance.yml"}))
	assert.Empty(t, *calls)
	invokedArgs = nil
}

func TestEditorCommand(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	assert.Equal(t, []string{"vi"}, editorCommand())
	t.Setenv("EDITOR", "nano")
	assert.Equal(t, []string{"nano"}, editorCommand())
	t.Setenv("VISUAL", "code -w")
	assert.Equal(t, []string{"code", "-w"}, editorCommand())
}

func TestEdit_RefusesInvalidServiceNames(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	calls := stubEdit(t, "x", 0, nil)
	for _, svc := range []string{"../core", "a/b", "..", ".", "Foo"} {
		require.Error(t, runEdit(context.Background(), svc, "config/x", io.Discard), svc)
	}
	assert.Empty(t, *calls, "nothing is sent to the leader")
}

func TestFiles_RefuseInvalidServiceNames(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "1")
	for _, svc := range []string{"../core", "a/b", "..", ".", "Foo"} {
		for _, verb := range []string{"get", "put"} {
			resetFlags()
			assert.Equal(t, 1, executeArgs([]string{"--config-dir", t.TempDir(), "files", verb, svc, "config/x"}), verb+" "+svc)
		}
	}
	invokedArgs = nil
}

func TestEdit_CancelRemovesTempDir(t *testing.T) {
	t.Setenv("HOMELAB_AGENT", "")
	started := make(chan string, 1)
	release := make(chan struct{})
	calls := stubEdit(t, "a", 0, func(f string) error {
		started <- f
		<-release
		return nil
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- runEdit(ctx, "glance", "config/glance.yml", io.Discard) }()
	file := <-started
	dir := filepath.Dir(file)
	fi, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	cancel()
	require.Error(t, <-errc)
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the temp dir is removed")
	assert.Len(t, *calls, 1)
}
