package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/groot/homelab/internal/remote"
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
	Short: "Edit a service's bind-mounted config file and roll it out",
	Long: `Edit a config file the service bind-mounts (glance's config/glance.yml,
grafana's provisioning files, ...) from any PC: the file is fetched from the
leader, opened in $VISUAL / $EDITOR (default vi), and if you changed it, sent
back and the service redeployed so the new content reaches the task. <path> is
relative to the service's directory.

  homelab edit glance config/glance.yml`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeServiceNames,
	RunE: func(c *cobra.Command, args []string) error {
		if os.Getenv("HOMELAB_AGENT") == "1" {
			return errors.New("run homelab edit from your PC, not inside the agent")
		}
		return runEdit(c.Context(), args[0], args[1], c.OutOrStdout())
	},
}

func runEdit(ctx context.Context, svc, rel string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var orig bytes.Buffer
	if code, err := editLeader(ctx, []string{"files", "get", svc, rel}, nil, &orig); err != nil || code != 0 {
		return leaderErr("fetching "+rel, code, err)
	}
	tmp, err := os.CreateTemp(os.TempDir(), "homelab-edit-*-"+filepath.Base(rel)) // 0600
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(orig.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := editRunEditor(editorCommand(), tmp.Name()); err != nil {
		return fmt.Errorf("editor failed, nothing sent: %w", err)
	}
	edited, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	if bytes.Equal(edited, orig.Bytes()) {
		fmt.Fprintln(out, "no changes")
		return nil
	}
	if code, err := editLeader(ctx, []string{"files", "put", svc, rel}, bytes.NewReader(edited), io.Discard); err != nil || code != 0 {
		return leaderErr("saving "+rel, code, err)
	}
	fmt.Fprintf(out, "%s/%s updated\n", svc, rel)
	return nil
}

func leaderErr(what string, code int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: the leader exited %d", what, code)
}

func init() { rootCmd.AddCommand(editCmd) }
