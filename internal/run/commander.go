// Package run wraps os/exec for docker and docker compose invocations.
//
// All subprocess calls use a hardcoded binary (docker). gosec G204 is
// intentionally suppressed — the binary is never user-controlled.
//
//nolint:gosec
package run

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Commander executes docker commands, writing output to configurable writers.
// Using io.Writer fields (rather than always os.Stdout) lets callers capture
// output into a buffer — e.g. to show it only on error while a spinner runs.
type Commander struct {
	Stdout io.Writer
	Stderr io.Writer
}

// Default returns a Commander that writes directly to the terminal.
func Default() *Commander {
	return &Commander{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// DockerComposeEnv runs docker compose with env vars injected directly into
// the subprocess environment. Vars in env override any same-named vars already
// present in the parent process environment. Nothing is written to disk.
func (c *Commander) DockerComposeEnv(composeFile string, env map[string]string, args ...string) error {
	argv := append([]string{"compose", "-f", composeFile}, args...)
	cmd := exec.Command("docker", argv...)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = MergeEnv(os.Environ(), env)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	return nil
}

// DockerExec runs: docker exec <container> <args...>
func (c *Commander) DockerExec(container string, args ...string) error {
	return c.run("docker", append([]string{"exec", container}, args...)...)
}

// DockerNetworkExists checks whether a named Docker network is present.
func DockerNetworkExists(name string) (bool, error) {
	cmd := exec.Command("docker", "network", "inspect", name)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DockerNetworkCreate creates a named Docker network.
func (c *Commander) DockerNetworkCreate(name string) error {
	return c.run("docker", "network", "create", name)
}

// ContainerStatus returns the Docker container state for the given container
// name. Returns "not found" when the container does not exist, and the state
// string ("running", "exited", "paused", etc.) otherwise.
func (c *Commander) ContainerStatus(name string) string {
	out, err := c.Output("docker", "inspect", "--format={{.State.Status}}", name)
	if err != nil {
		return "not found"
	}
	return strings.TrimSpace(string(out))
}

func (c *Commander) run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Run executes a command with configurable stdout/stderr. Public wrapper.
func (c *Commander) Run(name string, args ...string) error {
	return c.run(name, args...)
}

// Output runs a command and returns stdout. Stderr is discarded.
func (c *Commander) Output(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

// RunTo executes a command streaming its stdout into w. Use it instead of
// Output when the payload can be large — a database dump must not be buffered
// in memory in its entirety just to be written to a file.
func (c *Commander) RunTo(w io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = w
	cmd.Stderr = c.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// RunFrom executes a command feeding r to its stdin, streamed rather than
// buffered. The counterpart to RunTo: restoring a dump pipes a file into
// `docker exec -i … pg_restore`.
func (c *Commander) RunFrom(r io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = r
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// MergeEnv returns a new env slice starting from base, with each entry in
// overrides replacing any same-named key. Our vars always win.
func MergeEnv(base []string, overrides map[string]string) []string {
	merged := make(map[string]string, len(base)+len(overrides))
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// --- Path helpers ---

// CoreComposeFile returns the path to core/docker-compose.yml inside configDir.
func CoreComposeFile(configDir string) string {
	return filepath.Join(configDir, "core", "docker-compose.yml")
}

// ServiceComposeFile returns the path to services/<name>/docker-compose.yml inside configDir.
func ServiceComposeFile(configDir, name string) string {
	return filepath.Join(configDir, "services", name, "docker-compose.yml")
}
