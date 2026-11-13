package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/groot/homelab/internal/release"
)

// `homelab upgrade` on a PC replaces this PC's homelab binary with the
// target release's before rolling the agents. The swap is atomic: a temp
// file next to the resolved executable, renamed over it. A binary the user
// cannot write (/usr/local/bin) is swapped by `sudo -- <exe> upgrade
// --cli-only --version <tag>` — only that step runs as root — and this
// process then execs the new binary to roll the agents as the invoking
// user, with their client.yaml and keyring (root's HOME, XDG dirs and D-Bus
// session would find neither).

// selfExecutable is the path of the running homelab binary, symlinks
// resolved; a var so tests point it at a temp file.
var selfExecutable = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// runSudo runs `sudo -- args…` on this terminal (sudo prompts there; no
// password passes through homelab); a var so tests never run sudo.
var runSudo = func(args []string) error {
	c := exec.Command("sudo", append([]string{"--"}, args...)...) //nolint:gosec // our own binary, fixed argv
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// execSelf replaces this process with path (it returns only on failure); a
// var so tests never exec.
var execSelf = func(path string, argv []string) error {
	return syscall.Exec(path, argv, os.Environ()) //nolint:gosec // the binary just installed, verified
}

// sudoUsable reports whether a non-writable binary can be swapped through
// sudo: not when this already is root, nor without a sudo on PATH. A var
// for tests.
var sudoUsable = func() (string, bool) {
	if os.Geteuid() == 0 {
		return "this already runs as root", false
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return "sudo is not installed", false
	}
	return "", true
}

// versionFlag is upgrade's --version, in the argv built for the leader,
// sudo and the exec of the new binary.
const versionFlag = "--version"

// notWritableError: the directory of the binary refuses a new file.
type notWritableError struct {
	path string
	err  error
}

func (e *notWritableError) Error() string { return fmt.Sprintf("%s is not writable: %s", e.path, e.err) }
func (e *notWritableError) Unwrap() error { return e.err }

// replaceSelf downloads rel's binary for this build (verified against the
// release's SHA256SUMS) and renames it over path. Nothing is downloaded
// when path's directory refuses a new file (a *notWritableError), and
// path is left untouched on any failure.
func replaceSelf(ctx context.Context, path string, rel release.Release, w io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".homelab-upgrade-*")
	if errors.Is(err, fs.ErrPermission) {
		return &notWritableError{path: path, err: err}
	}
	if err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	_, _ = fmt.Fprintf(w, "Downloading homelab %s …\n", rel.Tag)
	data, err := release.FetchBinary(ctx, releaseClient(), rel)
	if err != nil {
		return fmt.Errorf("downloading homelab %s (%s left unchanged): %w", rel.Tag, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing the new binary next to %s: %w", path, err)
	}
	if err := tmp.Chmod(0o755); err != nil { //nolint:gosec // an executable
		return fmt.Errorf("writing the new binary next to %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing the new binary next to %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the new binary next to %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	ok = true
	_, _ = fmt.Fprintf(w, "homelab CLI %s → %s (%s)\n", Version, rel.Tag, path)
	return nil
}

// upgradeSelf is upgrade's CLI step: this binary becomes rel's. When the
// binary is not writable it is swapped under sudo, then this process execs
// the new binary with `upgrade --version <tag>` and rootFlags (the CLI is
// then the target, so it goes straight to the agents); execd reports that
// the rollout is no longer this process's.
func upgradeSelf(ctx context.Context, rel release.Release, rootFlags []string, w io.Writer) (execd bool, err error) {
	path, err := selfExecutable()
	if err != nil {
		return false, fmt.Errorf("locating this homelab binary: %w", err)
	}
	err = replaceSelf(ctx, path, rel, w)
	var nw *notWritableError
	if !errors.As(err, &nw) {
		return false, err
	}
	if why, ok := sudoUsable(); !ok {
		return false, fmt.Errorf("%w — and %s: replace it yourself (`sudo %s upgrade --cli-only --version %s`), then run `homelab upgrade --version %s`",
			nw, why, path, rel.Tag, rel.Tag)
	}
	_, _ = fmt.Fprintf(os.Stderr, "homelab: %s is not writable; re-running with sudo…\n", path)
	if err := runSudo([]string{path, "upgrade", "--cli-only", versionFlag, rel.Tag}); err != nil {
		return false, fmt.Errorf("replacing %s with sudo: %w", path, err)
	}
	argv := append([]string{path, "upgrade", versionFlag, rel.Tag}, rootFlags...)
	if err := execSelf(path, argv); err != nil {
		return true, fmt.Errorf("this CLI is now %s, but starting it failed: %w — run `homelab upgrade --version %s`", rel.Tag, err, rel.Tag)
	}
	return true, nil
}

// runCLIOnly is the hidden `upgrade --cli-only --version <tag>`, the step
// upgradeSelf runs under sudo: replace this binary with that release's and
// exit, without contacting the leader. It never re-runs itself with sudo.
func runCLIOnly(ctx context.Context, w io.Writer) error {
	if upgradeFlags.version == "" {
		return errors.New("--cli-only needs --version <tag>")
	}
	if upgradeFlags.image != "" {
		return errors.New("--cli-only replaces the CLI only: --image is for the agents")
	}
	release.UserAgent = "homelab/" + Version
	rel, err := release.ByTag(ctx, releaseClient(), releaseAPI, upgradeFlags.version)
	if err != nil {
		return fmt.Errorf("finding release %s: %w", upgradeFlags.version, err)
	}
	path, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("locating this homelab binary: %w", err)
	}
	return replaceSelf(ctx, path, rel, w)
}
