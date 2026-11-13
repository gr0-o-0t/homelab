package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/bootstrap"
	"github.com/groot/homelab/internal/swarm"
)

// A service's logs come from `docker service logs` on the leader, for every
// Swarm service of the stack (or the one -c names): Swarm gathers each
// task's output from whichever node runs it, so no relay is needed. Lines
// are prefixed with the compose key, as docker compose logs does.
//
// Docker applies --tail per task and prints one task after another, so a
// service with an old and a new task gives twice the lines, out of order.
// The backlog is therefore read with --timestamps, every task's and
// container's lines are merged by time, and -n applies after the merge;
// docker's timestamps are printed only with -t. -f then follows each
// service from the last line of its backlog, in arrival order.

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

// serviceLogsArgs builds `docker service logs` for one Swarm service, always
// with timestamps (to merge by). --raw drops docker's own task@node prefix;
// ours is the compose key.
func serviceLogsArgs(name string, follow bool, tail, since string) []string {
	a := []string{"service", cmdLogs, "--raw", "--timestamps"}
	if follow {
		a = append(a, "--follow")
	}
	if tail != "" {
		a = append(a, "--tail", tail)
	}
	if since != "" {
		a = append(a, "--since", since)
	}
	return append(a, name)
}

// parseTail reads -n: a line count, or "all"/"" for every line (-1).
func parseTail(s string) (int, error) {
	if s == "" || s == "all" {
		return -1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--tail %q: want a number of lines or \"all\"", s)
	}
	return n, nil
}

// runStackLogs prints the logs of svc's services (only want's, with -c).
func runStackLogs(root, svc, want string, stdout, stderr io.Writer) error {
	if logsFlags.until != "" {
		return errNoUntil
	}
	tail, err := parseTail(logsFlags.tail)
	if err != nil {
		return err
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
		prefixes := make([]string, len(svcs))
		for i, s := range svcs {
			prefixes[i] = fmt.Sprintf("%-*s | ", width, s.Key)
		}

		backlogs, err := readBacklogs(ctx, svcs)
		printBacklog(backlogs, tail, prefixes, stdout, stderr)
		if err != nil || !logsFlags.follow {
			return err
		}
		return followStack(ctx, svcs, backlogs, prefixes, stdout, stderr)
	})
}

// logEntry is one log line (and the untimestamped lines after it) of one
// Swarm service.
type logEntry struct {
	svc    int
	stderr bool
	ts     time.Time // zero: no timestamp, nothing before it to attach to
	raw    string    // the first line as docker printed it, timestamp included
	lines  []string  // as printed: the first line (timestamp stripped unless -t), then continuations
}

// splitTimestamp parses the RFC3339Nano timestamp docker puts first on a
// line with --timestamps; ok is false for a line without one.
func splitTimestamp(line string) (ts time.Time, rest string, ok bool) {
	stamp, rest, _ := strings.Cut(line, " ")
	if stamp == "" || stamp[0] < '0' || stamp[0] > '9' {
		return time.Time{}, line, false
	}
	ts, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, line, false
	}
	return ts, rest, true
}

// shown is line as the user sees it: without docker's timestamp unless -t.
func shown(line, rest string) string {
	if logsFlags.timestamps {
		return line
	}
	return rest
}

// lineWriter calls fn with each whole line written to it (no newline);
// Flush passes a last line that has none.
type lineWriter struct {
	buf []byte
	fn  func(line string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		w.fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.fn(string(w.buf))
		w.buf = nil
	}
}

// collector gathers one stream of one service's backlog into entries.
func collector(svc int, stderr bool, entries *[]logEntry) *lineWriter {
	return &lineWriter{fn: func(line string) {
		ts, rest, ok := splitTimestamp(line)
		if !ok && len(*entries) > 0 {
			last := &(*entries)[len(*entries)-1]
			last.lines = append(last.lines, line)
			return
		}
		*entries = append(*entries, logEntry{svc: svc, stderr: stderr, ts: ts, raw: line, lines: []string{shown(line, rest)}})
	}}
}

// readBacklogs reads every service's backlog (no --follow) in parallel.
// --tail goes to docker too: per task it is more than the merged tail needs.
func readBacklogs(ctx context.Context, svcs []bootstrap.StackService) ([][]logEntry, error) {
	backlogs := make([][]logEntry, len(svcs))
	errs := make([]error, len(svcs))
	var wg sync.WaitGroup
	for i, s := range svcs {
		wg.Go(func() {
			var out, errOut []logEntry
			ow, ew := collector(i, false, &out), collector(i, true, &errOut)
			if err := serviceLogsCLI(ctx, serviceLogsArgs(s.Name, false, logsFlags.tail, logsFlags.since), ow, ew); err != nil {
				errs[i] = fmt.Errorf("%s: %w", s.Key, err)
			}
			ow.Flush()
			ew.Flush()
			backlogs[i] = append(out, errOut...)
		})
	}
	wg.Wait()
	return backlogs, errors.Join(errs...)
}

// printBacklog merges every backlog by time and prints its last tail
// entries (all with tail < 0). Lines with no timestamp at all (docker's own
// errors) are printed first and never cut.
func printBacklog(backlogs [][]logEntry, tail int, prefixes []string, stdout, stderr io.Writer) {
	var untimed, timed []logEntry
	for _, b := range backlogs {
		for _, e := range b {
			if e.ts.IsZero() {
				untimed = append(untimed, e)
			} else {
				timed = append(timed, e)
			}
		}
	}
	slices.SortStableFunc(timed, func(a, b logEntry) int { return a.ts.Compare(b.ts) })
	if tail >= 0 && len(timed) > tail {
		timed = timed[len(timed)-tail:]
	}
	for _, e := range append(untimed, timed...) {
		w := stdout
		if e.stderr {
			w = stderr
		}
		for _, l := range e.lines {
			_, _ = io.WriteString(w, prefixes[e.svc]+l+"\n")
		}
	}
}

// followStack follows every service from the newest line of its backlog
// (docker's --since includes it, so lines at that instant already printed
// are skipped), from --since with no backlog, else from now.
func followStack(ctx context.Context, svcs []bootstrap.StackService, backlogs [][]logEntry,
	prefixes []string, stdout, stderr io.Writer) error {
	var mu sync.Mutex // one line at a time across every stream
	var wg sync.WaitGroup
	errs := make([]error, len(svcs))
	for i, s := range svcs {
		var cutoff time.Time
		seen := map[string]int{} // raw lines at cutoff already printed
		for _, e := range backlogs[i] {
			switch {
			case e.ts.After(cutoff):
				cutoff = e.ts
				clear(seen)
				seen[e.raw]++
			case e.ts.Equal(cutoff) && !e.ts.IsZero():
				seen[e.raw]++
			}
		}
		tail, since := "", logsFlags.since
		switch {
		case !cutoff.IsZero():
			since = cutoff.UTC().Format(time.RFC3339Nano)
		case since == "":
			tail = "0"
		}
		follower := func(w io.Writer) *lineWriter {
			drop := false
			return &lineWriter{fn: func(line string) {
				mu.Lock()
				defer mu.Unlock()
				ts, rest, ok := splitTimestamp(line)
				if ok {
					drop = ts.Before(cutoff)
					if ts.Equal(cutoff) && seen[line] > 0 {
						seen[line]--
						drop = true
					}
					line = shown(line, rest)
				}
				if !drop {
					_, _ = io.WriteString(w, prefixes[i]+line+"\n")
				}
			}}
		}
		wg.Go(func() {
			out, errOut := follower(stdout), follower(stderr)
			if err := serviceLogsCLI(ctx, serviceLogsArgs(s.Name, true, tail, since), out, errOut); err != nil {
				errs[i] = fmt.Errorf("%s: %w", s.Key, err)
			}
			out.Flush()
			errOut.Flush()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
