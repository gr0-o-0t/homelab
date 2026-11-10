package clistate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A call the leader never answers ends at the timeout instead of hanging.
func TestCLI_TimesOut(t *testing.T) {
	c := CLI{Argv: []string{"homelab"}, Timeout: 50 * time.Millisecond,
		Run: func(ctx context.Context, _ []string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	start := time.Now()
	_, err := c.Status()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer from the leader within 50ms")
	assert.Less(t, time.Since(start), 2*time.Second)
}

// The real runner kills the child at the deadline.
func TestExecOutput_KillsTheChild(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hang")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700))
	c := CLI{Argv: []string{script}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := c.Backups()
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the child was not left running to completion")
}
