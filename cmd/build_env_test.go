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

	stderr := captureStderr(func() {
		env := buildEnv(root, "test-svc")
		assert.NotNil(t, env, "nil guard should ensure non-nil map")
	})

	assert.Contains(t, stderr, "warning: config error", "buildEnv should log config error to stderr")
}

func TestBuildEnv_ConfigError_NilGuardReturnsDefaults(t *testing.T) {
	root := t.TempDir()
	setConfigDir(t, root)

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "config.yaml"),
		[]byte("{{invalid: yaml: ]}"),
		0o644,
	))

	env := buildEnv(root, "test-svc")

	assert.NotNil(t, env, "returned map must not be nil")
	// buildEnv injects HOME_SUBDOMAIN=home as a fallback even when
	// config loading fails, preventing the fragile case where
	// Caddy's {@literal $HOME_SUBDOMAIN} resolves to "" and produces an
	// invalid double-dot domain.
	assert.Equal(t, "home", env["HOME_SUBDOMAIN"], "should default HOME_SUBDOMAIN on config error")
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
