package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
)

// editLeader runs argv on the leader agent with stdin, returning its exit
// code; stdout is captured, stderr goes to the terminal. A var so tests can
// stand in for the agent.
var editLeader = func(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
	c, err := remote.LoadClient(Version, remote.KeyringSecret)
	if err != nil {
		return 1, err
	}
	return c.Run(ctx, remote.RunOptions{Argv: argv, Stdin: stdin, Stdout: stdout, Stderr: os.Stderr})
}

// editRunEditor runs the editor on file with the terminal; a var for tests.
var editRunEditor = func(editor []string, file string) error {
	c := exec.Command(editor[0], append(editor[1:], file)...) // #nosec G204 -- the user's own $EDITOR
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// editorCommand is $VISUAL, else $EDITOR, else vi, split into words so
// "code -w" works.
func editorCommand() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(k)); len(f) > 0 {
			return f
		}
	}
	return []string{"vi"}
}

var editCmd = &cobra.Command{
	Use:   "edit <service> <path>",
	Short: "Edit a service's compose file, config.yaml or a bind-mounted config file and roll it out",
	Long: `Edit a service's docker-compose.yml, its config.yaml, or a config file it
bind-mounts (glance's config/glance.yml, grafana's provisioning files, ...) from
any PC: the file is fetched from the leader, opened in $VISUAL / $EDITOR
(default vi), and if you changed it, sent back; the change is then applied
as "homelab up" does if the service runs (a stopped one waits for "up").
<path> is relative to the service's directory.

docker-compose.yml and config.yaml are checked before they are saved: they
must parse (placement roles valid, cache_volumes naming compose volumes) and
translate to Swarm services with the current environment, no secret ending up
in a spec. Otherwise the leader refuses them and keeps the previous content.
exposure.yaml is not editable: homelab enable / disable own it.

  homelab edit glance config/glance.yml
  homelab edit immich docker-compose.yml

--from <file> sends a local file instead of opening an editor, for scripts; a
new file under a bind-mounted directory is created with its subdirectories.

  homelab edit grafana provisioning/dashboards/taash/taash.json --from ./taash.json`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeServiceNames,
	RunE: func(c *cobra.Command, args []string) error {
		if os.Getenv("HOMELAB_AGENT") == "1" {
			return errors.New("run homelab edit from your PC, not inside the agent")
		}
		if from, _ := c.Flags().GetString("from"); from != "" {
			return runEditFrom(c.Context(), args[0], args[1], from, c.OutOrStdout())
		}
		return runEdit(c.Context(), args[0], args[1], c.OutOrStdout())
	},
}

func runEdit(ctx context.Context, svc, rel string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validName(svc); err != nil {
		return err
	}
	var orig bytes.Buffer
	if code, err := editLeader(ctx, []string{filesVerb, "get", svc, rel}, nil, &orig); err != nil || code != 0 {
		return leaderErr("fetching "+rel, code, err)
	}
	dir, err := os.MkdirTemp(os.TempDir(), "homelab-edit-*") // 0700
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, filepath.Base(rel))
	if err := os.WriteFile(file, orig.Bytes(), 0o600); err != nil {
		return err
	}
	// A closed terminal (SIGHUP) or SIGTERM must not leave the copy behind:
	// stop waiting for the editor and let the deferred RemoveAll run.
	sctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- editRunEditor(editorCommand(), file) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("editor failed, nothing sent: %w", err)
		}
	case <-sctx.Done():
		return fmt.Errorf("interrupted, nothing sent: %w", sctx.Err())
	}
	edited, err := os.ReadFile(file) //nolint:gosec // the temp file this command created for the editor
	if err != nil {
		return err
	}
	if bytes.Equal(edited, orig.Bytes()) {
		_, _ = fmt.Fprintln(out, "no changes")
		return nil
	}
	if code, err := sendFile(ctx, svc, rel, edited); err != nil || code != 0 {
		return leaderErr("saving "+rel, code, err)
	}
	_, _ = fmt.Fprintf(out, "%s/%s updated\n", svc, rel)
	return nil
}

func leaderErr(what string, code int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: the leader exited %d", what, code)
}

// runEditFrom sends a local file as rel, without fetching or an editor. The
// file is checked here first (the leader checks again): nothing reaches the
// leader if it cannot be read, is empty or is over Swarm's config limit.
func runEditFrom(ctx context.Context, svc, rel, from string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validName(svc); err != nil {
		return err
	}
	data, err := os.ReadFile(from) //nolint:gosec // the file the user named
	if err != nil {
		return err
	}
	switch {
	case len(data) == 0:
		return fmt.Errorf("%s is empty", from)
	case len(data) > swarm.MaxConfigSize:
		return fmt.Errorf("%s is %d bytes; Swarm configs are limited to %d", from, len(data), swarm.MaxConfigSize)
	}
	if code, err := sendFile(ctx, svc, rel, data); err != nil || code != 0 {
		return leaderErr("saving "+rel, code, err)
	}
	_, _ = fmt.Fprintf(out, "%s/%s updated\n", svc, rel)
	return nil
}

func init() {
	editCmd.Flags().String("from", "", "send this local file instead of opening an editor")
	rootCmd.AddCommand(editCmd)
}

const filesVerb = "files"

// sendFile sends data to the leader as svc's rel.
func sendFile(ctx context.Context, svc, rel string, data []byte) (int, error) {
	return editLeader(ctx, []string{filesVerb, "put", svc, rel}, bytes.NewReader(data), io.Discard)
}
