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
// With -n N the backlog is therefore read with --timestamps (at most N
// lines per task), every task's and container's lines are merged by time,
// and -n applies after the merge; -f then follows each service from the
// last line of its backlog, in arrival order. Without -n nothing is
// buffered: every line is streamed as docker prints it (task after task),
// so a whole log is never held in the agent, which also runs the edge.
// Docker's timestamps are printed only with -t.

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

		plans := make([]*streamPlan, len(svcs))
		if tail < 0 {
			// No -n: stream as docker prints it (task after task), never
			// buffering a whole log in the agent.
			for i := range plans {
				plans[i] = &streamPlan{tail: logsFlags.tail, since: logsFlags.since}
			}
			return streamStack(ctx, svcs, plans, logsFlags.follow, prefixes, stdout, stderr)
		}
		backlogs, errs := readBacklogs(ctx, svcs)
		printBacklog(backlogs, tail, prefixes, stdout, stderr)
		if !logsFlags.follow {
			return errors.Join(errs...)
		}
		for i := range svcs {
			if errs[i] == nil { // a service whose backlog failed is not followed
				plans[i] = followPlan(backlogs[i])
			}
		}
		return errors.Join(append(errs, streamStack(ctx, svcs, plans, true, prefixes, stdout, stderr))...)
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

// readBacklogs reads every service's backlog (no --follow) in parallel,
// with an error per service. --tail goes to docker too: N per task is at
// least what the merged tail needs, and bounds what is held in memory.
func readBacklogs(ctx context.Context, svcs []bootstrap.StackService) ([][]logEntry, []error) {
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
	return backlogs, errs
}

// printBacklog merges every backlog by time and prints its last tail
// entries. An entry is a timestamped line plus the unstamped lines after it
// (docker stamps every line, so in practice an entry is one line). Lines
// with no timestamp and nothing before them (docker's own error text) are
// printed after the others and never cut.
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
	if len(timed) > tail {
		timed = timed[len(timed)-tail:]
	}
	for _, e := range append(timed, untimed...) {
		w := stdout
		if e.stderr {
			w = stderr
		}
		for _, l := range e.lines {
			_, _ = io.WriteString(w, prefixes[e.svc]+l+"\n")
		}
	}
}

// streamPlan is how one service's logs are streamed: docker's --tail and
// --since, and the lines to skip (before cutoff, or at it and in seen).
type streamPlan struct {
	tail, since string
	cutoff      time.Time
	seen        map[string]int // raw lines at cutoff already read, with their count
}

// followPlan follows a service from the newest line of its backlog
// (docker's --since includes it, so lines at that instant already read are
// skipped), from --since with no backlog, else from now.
func followPlan(backlog []logEntry) *streamPlan {
	p := &streamPlan{since: logsFlags.since, seen: map[string]int{}}
	for _, e := range backlog {
		switch {
		case e.ts.After(p.cutoff):
			p.cutoff = e.ts
			clear(p.seen)
			p.seen[e.raw]++
		case e.ts.Equal(p.cutoff) && !e.ts.IsZero():
			p.seen[e.raw]++
		}
	}
	switch {
	case !p.cutoff.IsZero():
		p.since = p.cutoff.UTC().Format(time.RFC3339Nano)
	case p.since == "":
		p.tail = "0"
	}
	return p
}

// streamStack streams every service with a plan (nil: skipped) in arrival
// order, timestamps stripped unless -t.
func streamStack(ctx context.Context, svcs []bootstrap.StackService, plans []*streamPlan, follow bool,
	prefixes []string, stdout, stderr io.Writer) error {
	var mu sync.Mutex // one line at a time across every stream
	var wg sync.WaitGroup
	errs := make([]error, len(svcs))
	for i, s := range svcs {
		p := plans[i]
		if p == nil {
			continue
		}
		follower := func(w io.Writer) *lineWriter {
			drop := false
			return &lineWriter{fn: func(line string) {
				mu.Lock()
				defer mu.Unlock()
				ts, rest, ok := splitTimestamp(line)
				if ok {
					drop = ts.Before(p.cutoff)
					if ts.Equal(p.cutoff) && p.seen[line] > 0 {
						p.seen[line]--
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
			if err := serviceLogsCLI(ctx, serviceLogsArgs(s.Name, follow, p.tail, p.since), out, errOut); err != nil {
				errs[i] = fmt.Errorf("%s: %w", s.Key, err)
			}
			out.Flush()
			errOut.Flush()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
