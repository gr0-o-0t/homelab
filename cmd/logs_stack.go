package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/swarm"
)

// A service's logs come from `docker service logs` on the leader, for every
// Swarm service of the stack (or the one -c names): Swarm gathers each
// task's output from whichever node runs it, so no relay is needed. Lines
// are prefixed with the compose key, as docker compose logs does.

// serviceLogsCLI runs `docker args…` with its output to stdout and stderr;
// a var so tests stand in for docker.
var serviceLogsCLI = func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed binary; args built here
	c.Stdout, c.Stderr = stdout, stderr
	return c.Run()
}

// errNoUntil: docker service logs has no --until.
var errNoUntil = errors.New("--until is not available for a service's logs: `docker service logs`, which gathers them " +
	"from every node, has no --until — use --since and --tail")

// serviceLogsArgs maps homelab's logs flags onto `docker service logs`.
// --raw drops docker's own task@node prefix; ours is the compose key.
func serviceLogsArgs(name string) []string {
	a := []string{"service", cmdLogs, "--raw"}
	if logsFlags.follow {
		a = append(a, "--follow")
	}
	if logsFlags.timestamps {
		a = append(a, "--timestamps")
	}
	if logsFlags.tail != "" {
		a = append(a, "--tail", logsFlags.tail)
	}
	if logsFlags.since != "" {
		a = append(a, "--since", logsFlags.since)
	}
	return append(a, name)
}

// runStackLogs streams the logs of svc's services (only want's, with -c).
func runStackLogs(root, svc, want string, stdout, stderr io.Writer) error {
	if logsFlags.until != "" {
		return errNoUntil
	}
	if err := validateService(root, svc); err != nil {
		return err
	}
	return withSwarmAPI(func(api swarm.API) error {
		ctx := cmdContext()
		info, err := api.Info(ctx)
		if err != nil {
			return fmt.Errorf("asking Docker for this node's swarm role: %w", err)
		}
		if info.Swarm.LocalNodeState != dswarm.LocalNodeStateActive || !info.Swarm.ControlAvailable {
			return fmt.Errorf("`homelab logs %s` runs on the leader (a Swarm manager)", svc)
		}
		svcs, _, err := bootstrap.StackServices(ctx, api, svc)
		if err != nil {
			return err
		}
		if len(svcs) == 0 {
			return fmt.Errorf("%s has no Swarm services — is it up?", svc)
		}
		if want != "" {
			s, err := bootstrap.PickService(svc, svcs, want)
			if err != nil {
				return err
			}
			svcs = []bootstrap.StackService{s}
		}
		slices.SortFunc(svcs, func(a, b bootstrap.StackService) int { return strings.Compare(a.Key, b.Key) })
		width := 0
		for _, s := range svcs {
			width = max(width, len(s.Key))
		}
		var mu sync.Mutex // one line at a time across every stream
		var wg sync.WaitGroup
		errs := make([]error, len(svcs))
		for i, s := range svcs {
			prefix := fmt.Sprintf("%-*s | ", width, s.Key)
			out := &prefixWriter{mu: &mu, w: stdout, prefix: prefix}
			errOut := &prefixWriter{mu: &mu, w: stderr, prefix: prefix}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := serviceLogsCLI(ctx, serviceLogsArgs(s.Name), out, errOut); err != nil {
					errs[i] = fmt.Errorf("%s: %w", s.Key, err)
				}
				out.Flush()
				errOut.Flush()
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	})
}

// prefixWriter writes whole lines to w, each starting with prefix, under a
// mutex shared by every stream so lines never interleave mid-line.
type prefixWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		p.emit(p.buf[:i+1])
		p.buf = p.buf[i+1:]
	}
}

// Flush writes a final line without a newline.
func (p *prefixWriter) Flush() {
	if len(p.buf) > 0 {
		p.emit(append(p.buf, '\n'))
		p.buf = nil
	}
}

func (p *prefixWriter) emit(line []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = io.WriteString(p.w, p.prefix)
	_, _ = p.w.Write(line)
}
