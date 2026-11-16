package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStderr runs fn while redirecting os.Stderr to a buffer, then returns
// the captured output.
func captureStderr(fn func()) string {
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w

	fn()

	_ = w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestBuildEnv_ConfigError_LogsWarning(t *testing.T) {
	root := t.TempDir()
	setConfigDir(t, root)

	// Write an unparseable config.yaml so config.Load returns an error.
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "config.yaml"),
		[]byte("{{invalid: yaml: ]}"),
		0o644,
	))

	var env map[string]string
	stderr := captureStderr(func() { env = buildEnv(root, "test-svc") })

	assert.Contains(t, stderr, "warning: config error", "buildEnv should log config error to stderr")
	// HOME_SUBDOMAIN still defaults, so Caddy never builds a double-dot domain.
	assert.Equal(t, "home", env["HOME_SUBDOMAIN"])
}

func TestBuildEnv_ValidConfig_Success(t *testing.T) {
	root := t.TempDir()
	setConfigDir(t, root)

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "config.yaml"),
		[]byte("vars:\n  DOMAIN:\n    value: example.com\n    required: true\n"),
		0o644,
	))

	env := buildEnv(root, "test-svc")

	assert.NotNil(t, env)
	assert.Equal(t, "example.com", env["DOMAIN"])
}
