package bootstrap

// `homelab upgrade`: roll a new agent image across the cluster. The leader
// pulls the image on every node (each worker through its own agent), then
// replaces the worker agents one by one, each back on the new version before
// the next, and its own agent last (replace.go: how an agent is replaced).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/remote"
	"github.com/groot/homelab/internal/swarm"
)

// LeaderReplaceMarker is the line upgrade prints on the leader right before
// it replaces its own agent: a client that saw it expects the session to end.
const LeaderReplaceMarker = "Replacing the leader agent — agent restarting"

// VersionArgv asks an agent its version and boot identity
// (`homelab version --agent`).
var VersionArgv = []string{"version", "--agent"}

// AgentID is what `homelab version --agent` reports: the version, and the
// agent container's ID — a replacement is a new container, so a changed
// Boot proves the agent answering is not the old one.
type AgentID struct {
	Version string
	Boot    string
}

// ParseAgentID reads `homelab version --agent` output: the version on the
// first line, `boot <container ID>` on the next (absent outside an agent).
func ParseAgentID(out string) AgentID {
	var id AgentID
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if i == 0 {
			id.Version = line
		} else if b, ok := strings.CutPrefix(line, "boot "); ok {
			id.Boot = strings.TrimSpace(b)
		}
	}
	return id
}

// WaitReplaced polls ask until the agent answering is a new one (its Boot
// differs from before.Boot) that has kept answering for Settle — the
// helper may still roll back a crashing one — and then checks its version
// against want (when known). It gives up after WaitBack. When before.Boot is
// unknown, the first answer after ReplaceDelay counts as new.
func WaitReplaced(ctx context.Context, ask func(context.Context) (AgentID, error), before AgentID, want string,
	sleep func(context.Context, time.Duration) bool) (AgentID, error) {
	if !sleep(ctx, ReplaceDelay) { // the old agent answers until the helper stops it
		return AgentID{}, ctx.Err()
	}
	var last, cand AgentID
	steady := time.Duration(-1)
	for range int(WaitBack / pollEvery) {
		got, err := ask(ctx)
		if err == nil && got.Version != "" {
			last = got
			isNew := before.Boot == "" || (got.Boot != "" && got.Boot != before.Boot)
			switch {
			case !isNew:
				steady = -1
			case steady < 0 || got.Boot != cand.Boot:
				cand, steady = got, 0
			default:
				steady += pollEvery
			}
			if steady >= Settle || (steady >= 0 && before.Boot == "") {
				if want != "" && cand.Version != want {
					return cand, fmt.Errorf("version mismatch: the new agent reports %s, expected %s", cand.Version, want)
				}
				return cand, nil
			}
		}
		if !sleep(ctx, pollEvery) {
			return last, ctx.Err()
		}
	}
	switch {
	case last.Version == "":
		return last, fmt.Errorf("did not answer within %s of being replaced — see `docker logs %s` there", WaitBack, AgentContainer)
	case before.Boot != "" && last.Boot == before.Boot:
		return last, fmt.Errorf("still the old agent (%s): its replacement failed and was rolled back — see `docker logs %s` there",
			last.Version, HelperContainer)
	default:
		return last, fmt.Errorf("the new agent did not answer steadily within %s — see `docker logs %s` there", WaitBack, HelperContainer)
	}
}

// WorkerStatus is how upgrade left one worker.
type WorkerStatus string

const (
	// WorkerUpgraded: replaced, and back on the new version.
	WorkerUpgraded WorkerStatus = "upgraded"
	// WorkerCurrent: already on the new version; not touched.
	WorkerCurrent WorkerStatus = "already current"
	// WorkerSkipped: offline (node down, or its agent unreachable).
	WorkerSkipped WorkerStatus = "skipped"
	// WorkerFailed: its pull or replacement failed, or it came back on
	// another version. The rollout halts there.
	WorkerFailed WorkerStatus = "failed"
	// WorkerNotReached: the rollout halted before it.
	WorkerNotReached WorkerStatus = "not reached"
)

// WorkerResult is one worker's outcome.
type WorkerResult struct {
	Host    string
	Status  WorkerStatus
	Version string // what its agent reports, when known
	Detail  string
}

// AgentCall runs argv on the agent of worker host and returns its stdout.
// An agent that cannot be reached is an error matching remote.ErrUnreachable.
type AgentCall func(ctx context.Context, host string, argv []string) (string, error)

// Upgrade is `homelab upgrade` on the leader.
type Upgrade struct {
	API   swarm.API // the leader's engine
	Image string
	// Version is what the upgraded agents must report; "" reads the image's
	// VersionLabel, and without one versions are reported, not checked.
	Version string
	Call    AgentCall
	// Replace replaces the leader's own agent; default SelfReplace.
	Replace func(ctx context.Context) error
	Out     io.Writer
	// Sleep waits between polls; default Sleep.
	Sleep func(ctx context.Context, d time.Duration) bool
}

func (u *Upgrade) sleep(ctx context.Context, d time.Duration) bool {
	if u.Sleep != nil {
		return u.Sleep(ctx, d)
	}
	return Sleep(ctx, d)
}

func (u *Upgrade) printf(format string, args ...any) { _, _ = fmt.Fprintf(u.Out, format, args...) }

// Do runs the rollout. It returns every worker's result; an error means it
// halted before replacing the leader.
func (u *Upgrade) Do(ctx context.Context) ([]WorkerResult, error) {
	if u.Image == "" {
		return nil, errors.New("no agent image")
	}
	u.printf("Upgrading the cluster's agents to %s\n", u.Image)
	if err := EnsureImage(ctx, u.API, u.Image, u.Out); err != nil {
		return nil, fmt.Errorf("on the leader: %w", err)
	}
	want := u.Version
	if want == "" {
		if ii, _, err := u.API.ImageInspectWithRaw(ctx, u.Image); err == nil && ii.Config != nil {
			want = ii.Config.Labels[VersionLabel]
		}
	}
	if want == "" {
		u.printf("warning: %s carries no %s label: agents' versions are reported, not checked\n", u.Image, VersionLabel)
	}
	nodes, err := u.API.NodeList(ctx, dswarm.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the swarm's nodes: %w", err)
	}
	workers := slices.DeleteFunc(slices.Clone(nodes), func(n dswarm.Node) bool { return n.Spec.Role != dswarm.NodeRoleWorker })
	sort.SliceStable(workers, func(a, b int) bool { return workers[a].Description.Hostname < workers[b].Description.Hostname })
	res := make([]WorkerResult, len(workers))
	before := make([]AgentID, len(workers))
	for i := range workers {
		res[i].Host = workers[i].Description.Hostname
	}

	// 1. Every worker answers and has the image before any agent changes.
	pull := []string{"agent", "self-replace", "--image", u.Image, "--pull-only"}
	halted := false
	for i := range res {
		r := &res[i]
		if workers[i].Status.State != dswarm.NodeStateReady {
			r.Status, r.Detail = WorkerSkipped, "offline (swarm node not ready)"
			continue
		}
		id, err := u.agentID(ctx, r.Host)
		if err == nil && want != "" && id.Version == want {
			r.Status, r.Version = WorkerCurrent, id.Version
			continue
		}
		if err == nil {
			r.Version, before[i] = id.Version, id
			_, err = u.Call(ctx, r.Host, pull)
		}
		switch {
		case errors.Is(err, remote.ErrUnreachable):
			r.Status, r.Detail = WorkerSkipped, "offline (agent unreachable)"
		case err != nil:
			r.Status, r.Detail, halted = WorkerFailed, "checking it and pulling the image: "+err.Error(), true
		}
	}
	if halted {
		u.notReached(res)
		u.summary(res)
		return res, errors.New("the image is not available on every reachable worker; no agent was replaced — fix the failure above and rerun `homelab upgrade`")
	}

	// 2. Workers one by one, each back before the next.
	for i := range res {
		r := &res[i]
		if r.Status != "" {
			continue
		}
		u.printf("Replacing the agent on %s …\n", r.Host)
		u.replaceWorker(ctx, r, before[i], want)
		if r.Status == WorkerFailed {
			u.notReached(res)
			u.summary(res)
			return res, fmt.Errorf("the agent on %s: %s — halted before the leader; rerun `homelab upgrade` once it is fixed", r.Host, r.Detail)
		}
	}
	u.summary(res)

	// 3. The leader last: this ends the session.
	u.printf("%s; this session ends here.\n", LeaderReplaceMarker)
	replace := u.Replace
	if replace == nil {
		replace = func(ctx context.Context) error { return SelfReplace(ctx, u.API, u.Image, u.Out) }
	}
	if err := replace(ctx); err != nil {
		return res, fmt.Errorf("replacing the leader agent: %w", err)
	}
	return res, nil
}

// agentID is what host's agent reports for VersionArgv.
func (u *Upgrade) agentID(ctx context.Context, host string) (AgentID, error) {
	out, err := u.Call(ctx, host, VersionArgv)
	if err != nil {
		return AgentID{}, err
	}
	return ParseAgentID(out), nil
}

// replaceWorker replaces host's agent and waits for a new one to answer on
// want.
func (u *Upgrade) replaceWorker(ctx context.Context, r *WorkerResult, before AgentID, want string) {
	_, err := u.Call(ctx, r.Host, []string{"agent", "self-replace", "--image", u.Image})
	switch {
	case errors.Is(err, remote.ErrUnreachable):
		r.Status, r.Detail = WorkerSkipped, "went offline before its agent was replaced"
		return
	case err != nil:
		r.Status, r.Detail = WorkerFailed, "replacing: "+err.Error()
		return
	}
	got, err := WaitReplaced(ctx, func(ctx context.Context) (AgentID, error) { return u.agentID(ctx, r.Host) }, before, want, u.sleep)
	if got.Version != "" {
		r.Version = got.Version
	}
	if err != nil {
		r.Status, r.Detail = WorkerFailed, err.Error()
		return
	}
	r.Status = WorkerUpgraded
}

func (u *Upgrade) notReached(res []WorkerResult) {
	for i := range res {
		if res[i].Status == "" {
			res[i].Status = WorkerNotReached
		}
	}
}

func (u *Upgrade) summary(res []WorkerResult) {
	if len(res) == 0 {
		u.printf("No worker nodes.\n")
		return
	}
	u.printf("Workers:\n")
	for _, r := range res {
		line := fmt.Sprintf("  %-20s %s", r.Host, r.Status)
		if r.Version != "" {
			line += " (" + r.Version + ")"
		}
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		u.printf("%s\n", line)
	}
}
