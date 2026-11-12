package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/groot/homelab/internal/swarm"
)

const localCompose = `
services:
  taash:
    image: taash-server:current
    pull_policy: never
    container_name: taash
  cache:
    image: nginx:1
`

// localFixture is a config dir with the local-image service taash.
func localFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "services", "taash")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(localCompose), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("vars: {}\n"), 0o644))
	return root
}

func taashImage() image.InspectResponse {
	img := image.InspectResponse{Created: "2026-11-12T10:00:00Z"}
	img.RootFS.Layers = []string{"sha256:aaa", "sha256:bbb"}
	return img
}

// runCaptured runs the CLI with stdin and returns what it wrote to stdout.
func runCaptured(t *testing.T, root, stdin string, argv ...string) (string, error) {
	t.Helper()
	resetFlags()
	t.Cleanup(resetFlags)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetIn(strings.NewReader(stdin))
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetIn(nil) })
	rootCmd.SetArgs(append([]string{"--config-dir", root}, argv...))
	err := rootCmd.Execute()
	return out.String(), err
}

func TestImages_JSON(t *testing.T) {
	f := fakeSwarm(t)
	f.SetImage("taash-server:current", taashImage())
	root := localFixture(t)

	out, err := runCaptured(t, root, "", "images", "--json", "taash")
	require.NoError(t, err)
	id := swarm.ImageIdentity(taashImage())
	require.NotEmpty(t, id)
	assert.JSONEq(t, `[
		{"service":"taash","container":"cache","ref":"nginx:1","local":false,"identity":""},
		{"service":"taash","container":"taash","ref":"taash-server:current","local":true,"identity":"`+id+`"}
	]`, out)
}

func TestImages_JSON_AllServices(t *testing.T) {
	f := fakeSwarm(t)
	root := localFixture(t)

	out, err := runCaptured(t, root, "", "images", "--json", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, `"service": "taash"`)
	assert.Contains(t, out, `"identity": ""`, "the leader lacks the image: empty identity")
	assert.Zero(t, calls(f, "ImagePull"))
}

func TestImages_WithoutJSONTakesOneService(t *testing.T) {
	fakeSwarm(t)
	root := localFixture(t)
	_, err := runCaptured(t, root, "", "images", "taash", "other")
	require.Error(t, err)
}

func TestImagesLoad_PrintsIdentity(t *testing.T) {
	f := fakeSwarm(t)
	f.SetLoadResult("taash-server:current", taashImage())
	root := localFixture(t)

	out, err := runCaptured(t, root, "TARBYTES", "images", "load", "taash-server:current")
	require.NoError(t, err)
	assert.Equal(t, "TARBYTES", string(f.Loaded()))
	assert.JSONEq(t, `{"ref":"taash-server:current","identity":"`+swarm.ImageIdentity(taashImage())+`"}`, strings.TrimSpace(out))
}

func TestImagesLoad_RefNotLoaded(t *testing.T) {
	fakeSwarm(t)
	root := localFixture(t)

	_, err := runCaptured(t, root, "TARBYTES", "images", "load", "ghost:1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost:1")
}
