package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
)

// taashDir copies the taash fixture (compose + config.yaml) to a folder
// named taash and returns its path.
func taashDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "taash")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for _, f := range []string{"docker-compose.yml", "config.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "internal", "swarm", "testdata", "taash", f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), b, 0o600))
	}
	return dir
}

// stubAddLeader stands in for the leader's `files install` and records each
// call with the tar it was sent.
func stubAddLeader(t *testing.T) *[]leaderCall {
	t.Helper()
	t.Setenv("HOMELAB_AGENT", "")
	fakeAgent(t, 0, nil)
	var calls []leaderCall
	prev := addLeader
	addLeader = func(_ context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 1, err
		}
		calls = append(calls, leaderCall{argv: argv, stdin: string(b)})
		_, err = io.WriteString(stdout, "installed\n")
		return 0, err
	}
	t.Cleanup(func() { addLeader = prev })
	return &calls
}

// tarNames lists the regular files in a tar stream.
func tarNames(t *testing.T, data string) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader([]byte(data)))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if h.Typeflag == tar.TypeReg {
			names = append(names, h.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestAddDir_ForwardsTar(t *testing.T) {
	calls := stubAddLeader(t)
	dir := taashDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "conf"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "conf", "app.conf"), []byte("x=1\n"), 0o600))

	assert.Equal(t, 0, executeArgs([]string{"add", dir}))
	require.Len(t, *calls, 1)
	assert.Equal(t, []string{"files", "install", "taash"}, (*calls)[0].argv)
	assert.Equal(t, []string{"conf/app.conf", "config.yaml", "docker-compose.yml"}, tarNames(t, (*calls)[0].stdin))
}

func TestAddDir_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"symlink", func(t *testing.T, dir string) {
			require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")))
		}, "symlink"},
		{"dotfile", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600))
		}, ".env"},
		{"dot directory", func(t *testing.T, dir string) {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("x"), 0o600))
		}, ".git"},
		{"exposure.yaml", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "exposure.yaml"), []byte("layers: [cf]\n"), 0o600))
		}, "exposure.yaml"},
		{"over 10 MiB", func(t *testing.T, dir string) {
			chunk := bytes.Repeat([]byte("x"), swarm.MaxConfigSize)
			for i := 0; i*swarm.MaxConfigSize <= maxAddTree; i++ {
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("part%02d.bin", i)), chunk, 0o600))
			}
		}, "MiB limit"},
		{"file over the Swarm config limit", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 600<<10), 0o600))
		}, "big.bin"},
		{"empty file", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "empty.conf"), nil, 0o600))
		}, "empty.conf"},
		{"missing compose", func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "docker-compose.yml")))
		}, "docker-compose.yml"},
		{"missing config.yaml", func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "config.yaml")))
		}, "config.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubAddLeader(t)
			dir := taashDir(t)
			tc.setup(t, dir)

			code, err := forwardAddDirFor(t, []string{"add", dir})
			assert.NotEqual(t, 0, code)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, *calls, "nothing reaches the leader")
		})
	}
}

func TestAddDir_BareNameStaysCatalog(t *testing.T) {
	calls := stubAddLeader(t)
	t.Chdir(t.TempDir()) // no folder named like the service here
	fwd := fakeAgent(t, 0, nil)

	assert.Equal(t, 0, executeArgs([]string{"add", "uptime-kuma"}))
	assert.Empty(t, *calls)
	assert.Equal(t, [][]string{{"add", "uptime-kuma"}}, *fwd)
}

// A bare name that is both a catalog entry and a folder here is ambiguous:
// refused before anything is sent.
func TestAddDir_CatalogNameAndFolderRefused(t *testing.T) {
	calls := stubAddLeader(t)
	here := t.TempDir()
	t.Chdir(here)
	require.NoError(t, os.MkdirAll(filepath.Join(here, "uptime-kuma"), 0o750))
	fwd := fakeAgent(t, 0, nil)

	code, err := forwardAddDirFor(t, []string{"add", "uptime-kuma"})
	assert.NotEqual(t, 0, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"uptime-kuma" is both a catalog service and a folder here — use ./uptime-kuma`)
	assert.Empty(t, *calls)
	assert.Empty(t, *fwd)

	assert.NotEqual(t, 0, executeArgs([]string{"add", "uptime-kuma"}))
	assert.Empty(t, *calls, "nothing reaches the leader")
	assert.Empty(t, *fwd, "not forwarded as a catalog add either")
}

// A bare name with a folder here and no catalog entry is a folder install.
func TestAddDir_BareNonCatalogFolderInstalls(t *testing.T) {
	calls := stubAddLeader(t)
	dir := taashDir(t)
	t.Chdir(filepath.Dir(dir))

	assert.Equal(t, 0, executeArgs([]string{"add", "taash"}))
	require.Len(t, *calls, 1)
	assert.Equal(t, []string{"files", "install", "taash"}, (*calls)[0].argv)
}

// A folder named like a shared provider is refused on the PC.
func TestAddDir_SharedProviderNameRefused(t *testing.T) {
	calls := stubAddLeader(t)
	src := taashDir(t)
	dir := filepath.Join(t.TempDir(), "postgres")
	require.NoError(t, os.Rename(src, dir))

	code, err := forwardAddDirFor(t, []string{"add", dir})
	assert.NotEqual(t, 0, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared postgres service")
	assert.Empty(t, *calls)
}

// forwardAddDirFor parses argv as Execute would, then runs forwardAddDir on
// it, so the error itself can be checked.
func forwardAddDirFor(t *testing.T, argv []string) (int, error) {
	t.Helper()
	c, rest, err := rootCmd.Find(argv)
	require.NoError(t, err)
	require.NoError(t, c.ParseFlags(rest))
	return forwardAddDir(argv)
}
