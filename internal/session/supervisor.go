package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/groot/homelab/internal/docker"
)

// killMemory is how long a "kill" event marks the next "die" of the same
// container as intentional. `docker stop` sends SIGTERM (a kill event), waits
// up to the stop timeout, then the container dies; a crash has no kill.
const killMemory = 5 * time.Minute

// Supervisor is what the session unit runs (`homelab session run`): it
// restores the desired state at login, restarts crashed containers while the
// session lasts, keeps restart policies off, and stops the stack at logout.
type Supervisor struct {
	Root       string
	ConfigFile string // root config.yaml: its presence means home is mounted
	Home       string
	// CLI is this binary with its global flags and --no-record: how the
	// supervisor brings projects up, so database provisioning and the rest of
	// `homelab up` apply, without its own calls rewriting the desired state.
	CLI    []string
	Docker Docker
	Run    Runner
	Log    *log.Logger

	// Injected for tests; zero values mean the real thing.
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Cutoff func(readySince time.Time) time.Time
	Poll   time.Duration

	scope   Scope
	mu      sync.Mutex
	backoff *Backoff
	kills   map[string]time.Time
	pending map[string]bool
	wg      sync.WaitGroup
	once    sync.Once
}

func (s *Supervisor) init() {
	s.once.Do(func() {
		s.scope = NewScope(s.Root)
		s.backoff = NewBackoff()
		s.kills = map[string]time.Time{}
		s.pending = map[string]bool{}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.Sleep == nil {
			s.Sleep = sleepCtx
		}
		if s.Cutoff == nil {
			s.Cutoff = SessionCutoff
		}
		if s.Poll == 0 {
			s.Poll = 2 * time.Second
		}
		if s.Log == nil {
			s.Log = log.New(os.Stdout, "", 0)
		}
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Serve is the whole session: wait, restore, supervise until ctx is cancelled
// (SIGTERM at logout), then stop the stack. stopTimeout bounds the stop.
func (s *Supervisor) Serve(ctx context.Context, stopTimeout time.Duration) error {
	s.init()
	readySince, err := s.WaitReady(ctx)
	if err != nil {
		// Cancelled before anything was started: nothing to stop.
		s.Log.Printf("session ended before the stack was restored")
		return nil
	}
	cutoff := s.Cutoff(readySince)
	if err := s.Restore(ctx, cutoff); err != nil {
		s.Log.Printf("restore: %v", err)
	}
	s.Watch(ctx, cutoff)
	s.wg.Wait()

	sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return s.Shutdown(sctx)
}

// WaitReady waits for the config dir (home may be mounted just after the user
// manager starts) and then for the Docker daemon. readySince is when the
// config became readable if it had to be waited for, else zero.
func (s *Supervisor) WaitReady(ctx context.Context) (readySince time.Time, err error) {
	s.init()
	waited := false
	for {
		if _, err := os.Stat(s.ConfigFile); err == nil {
			break
		}
		if !waited {
			s.Log.Printf("waiting for %s (home not mounted yet?)", s.ConfigFile)
			waited = true
		}
		if err := s.Sleep(ctx, s.Poll); err != nil {
			return time.Time{}, err
		}
	}
	if waited {
		readySince = s.Now()
		s.Log.Printf("config dir is readable")
	}
	if err := s.waitDocker(ctx); err != nil {
		return time.Time{}, err
	}
	return readySince, nil
}

func (s *Supervisor) waitDocker(ctx context.Context) error {
	logged := false
	for {
		err := s.Docker.PingOK(ctx)
		if err == nil {
			if logged {
				s.Log.Printf("docker is up")
			}
			return nil
		}
		if !logged {
			s.Log.Printf("waiting for docker: %v", err)
			logged = true
		}
		if err := s.Sleep(ctx, s.Poll); err != nil {
			return err
		}
	}
}

// Restore brings the stack to its desired state: restart policies off,
// containers holding pre-session bind mounts restarted, then the core and the
// desired services brought up through the CLI — the core first, shared
// database services before the services using them.
// restoreRetries are the waits between attempts to bring the desired
// services up at login; the last entry is not waited on.
var restoreRetries = []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second, 0}

func (s *Supervisor) Restore(ctx context.Context, cutoff time.Time) error {
	s.init()
	cs, err := s.Docker.ComposeContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	if n, err := DisableRestartPolicies(ctx, s.Root, s.scope, cs, s.Run); err != nil {
		s.Log.Printf("restart policies: %v", err)
	} else if n > 0 {
		s.Log.Printf("restart policy set to \"no\" on %d container(s)", n)
	}

	if rebind := RebindNames(s.scope, cs, cutoff, s.Home); len(rebind) > 0 {
		// Stopped, not restarted: `up` below starts them again in compose's
		// dependency order, which matters for containers sharing another's
		// network namespace (caddy in tailscale's).
		s.Log.Printf("stopping %d container(s) started before this session (their bind mounts may predate home): %s",
			len(rebind), strings.Join(rebind, " "))
		if err := s.Run(ctx, append([]string{"docker", "stop"}, rebind...)...); err != nil {
			s.Log.Printf("docker stop: %v", err)
		}
	}

	d, exists, err := LoadDesired(s.Root)
	if err != nil {
		return err
	}
	if !exists {
		d = Bootstrap(s.scope, cs)
		if err := d.Save(s.Root); err != nil {
			s.Log.Printf("saving desired state: %v", err)
		}
		s.Log.Printf("no desired state yet: recorded the running containers")
	}
	for _, p := range runningNotDesired(s.scope, cs, d) {
		s.Log.Printf("%s is running but not desired running; left alone", p)
	}

	if d.CoreRunning() {
		s.Log.Printf("starting core stack")
		if err := s.Run(ctx, append(s.cliArgs(), "up")...); err != nil {
			s.Log.Printf("core: %v", err)
		}
	}
	if svcs := d.RunningServices(); len(svcs) > 0 {
		s.Log.Printf("starting %d service(s): %s", len(svcs), strings.Join(svcs, " "))
		// Retried: right after login the shared databases and the network
		// can still be settling, and `up` is idempotent — services already up
		// are left as they are.
		for attempt, wait := range restoreRetries {
			err := s.Run(ctx, append(append(s.cliArgs(), "up"), svcs...)...)
			if err == nil {
				break
			}
			if attempt == len(restoreRetries)-1 {
				s.Log.Printf("services: %v (giving up after %d attempts)", err, len(restoreRetries))
				break
			}
			s.Log.Printf("services: %v (retrying in %s)", err, wait)
			if err := s.Sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	// `up` may have created containers, with their compose restart policy.
	if cs, err := s.Docker.ComposeContainers(ctx); err == nil {
		if _, err := DisableRestartPolicies(ctx, s.Root, s.scope, cs, s.Run); err != nil {
			s.Log.Printf("restart policies: %v", err)
		}
	}
	s.Log.Printf("restore complete")
	return nil
}

func (s *Supervisor) cliArgs() []string { return append([]string(nil), s.CLI...) }

// RebindNames lists the running containers to stop and bring up again: every
// running container of each project with one NeedsRebind selects, since the
// project's containers may share namespaces and are started together.
func RebindNames(sc Scope, cs []docker.ContainerInfo, cutoff time.Time, home string) []string {
	affected := map[Project]bool{}
	for i := range cs {
		if p, ok := sc.Classify(cs[i].Labels); ok && NeedsRebind(cs[i], cutoff, home) {
			affected[p] = true
		}
	}
	var out []string
	for i := range cs {
		if p, ok := sc.Classify(cs[i].Labels); ok && affected[p] && cs[i].State == stateRunning {
			out = append(out, cs[i].Name)
		}
	}
	return out
}

func runningNotDesired(sc Scope, cs []docker.ContainerInfo, d *Desired) []string {
	seen := map[string]bool{}
	var out []string
	for i := range cs {
		p, ok := sc.Classify(cs[i].Labels)
		if !ok || cs[i].State != stateRunning || seen[p.String()] {
			continue
		}
		seen[p.String()] = true
		if (p.Core && !d.CoreRunning()) || (!p.Core && !d.ServiceRunning(p.Name)) {
			out = append(out, p.String())
		}
	}
	return out
}

// Watch follows Docker events until ctx is done. When the daemon goes away it
// waits for it to return and restores again.
func (s *Supervisor) Watch(ctx context.Context, cutoff time.Time) {
	s.init()
	for ctx.Err() == nil {
		evs, errs := s.Docker.ContainerEvents(ctx)
		s.Log.Printf("supervising")
	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-evs:
				if !ok {
					break loop
				}
				s.Handle(ctx, ev)
			case err := <-errs:
				if err != nil {
					s.Log.Printf("docker events: %v", err)
				}
				break loop
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := s.Sleep(ctx, s.Poll); err != nil {
			return
		}
		if err := s.waitDocker(ctx); err != nil {
			return
		}
		if err := s.Restore(ctx, cutoff); err != nil {
			s.Log.Printf("restore: %v", err)
		}
	}
}

// Decision is what the supervisor does about one event.
type Decision int

const (
	Ignore     Decision = iota
	FixPolicy           // inspect, record the policy, set it to "no"
	RestartCon          // start the container again, with backoff
)

// Decide classifies one event. It updates the kill memory, and reads the
// desired state and policy record from disk on a "die" — a `homelab stop`
// from another shell has already written "stopped" by the time its die
// arrives.
func (s *Supervisor) Decide(ev docker.Event) Decision {
	s.init()
	p, ok := s.scope.Classify(ev.Attributes)
	if !ok {
		return Ignore
	}
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Action {
	case "kill":
		if stopSignal(ev.Attributes["signal"]) {
			s.kills[ev.ID] = now
		}
	case "destroy":
		delete(s.kills, ev.ID)
	case "start":
		return FixPolicy
	case "die":
		killed, had := s.kills[ev.ID]
		delete(s.kills, ev.ID)
		if had && now.Sub(killed) < killMemory {
			return Ignore // stopped on purpose: docker stop, compose down/restart, backup
		}
		d, _, err := LoadDesired(s.Root)
		if err != nil {
			s.Log.Printf("desired state: %v", err)
			return Ignore
		}
		if (p.Core && !d.CoreRunning()) || (!p.Core && !d.ServiceRunning(p.Name)) {
			return Ignore
		}
		pol, _ := LoadPolicies(s.Root)
		if !ShouldRestart(pol.Original(ev.Attributes["name"]), ev.Attributes["exitCode"]) {
			return Ignore
		}
		return RestartCon
	}
	return Ignore
}

// reloadSignals are the signals sent to running containers to make them
// reload (i2pd and the ygg forwarders take SIGHUP): a kill event carrying one
// is not a stop.
var reloadSignals = map[string]bool{
	"1": true, "HUP": true, "SIGHUP": true,
	"10": true, "USR1": true, "SIGUSR1": true,
	"12": true, "USR2": true, "SIGUSR2": true,
	"28": true, "WINCH": true, "SIGWINCH": true,
}

func stopSignal(sig string) bool { return !reloadSignals[strings.ToUpper(sig)] }

// Handle acts on one event.
func (s *Supervisor) Handle(ctx context.Context, ev docker.Event) {
	switch s.Decide(ev) {
	case FixPolicy:
		c, ok, err := s.Docker.InspectInfo(ctx, ev.ID)
		if err != nil || !ok {
			return
		}
		if n, err := DisableRestartPolicies(ctx, s.Root, s.scope, []docker.ContainerInfo{c}, s.Run); err != nil {
			s.Log.Printf("%s: restart policy: %v", c.Name, err)
		} else if n > 0 {
			s.Log.Printf("%s: restart policy set to \"no\"", c.Name)
		}
	case RestartCon:
		name := ev.Attributes["name"]
		s.Log.Printf("%s died unexpectedly (exit %s)", name, ev.Attributes["exitCode"])
		s.scheduleRestart(ctx, ev.ID, name)
	case Ignore:
	}
}

// ErrGaveUp is logged when a container keeps dying.
var ErrGaveUp = errors.New("gave up")

func (s *Supervisor) scheduleRestart(ctx context.Context, id, name string) {
	s.mu.Lock()
	if s.pending[name] {
		s.mu.Unlock()
		return
	}
	s.pending[name] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { s.mu.Lock(); delete(s.pending, name); s.mu.Unlock() }()
		s.restartLoop(ctx, id, name)
	}()
}

func (s *Supervisor) restartLoop(ctx context.Context, id, name string) {
	for {
		s.mu.Lock()
		delay, giveUp := s.backoff.Next(name, s.Now())
		s.mu.Unlock()
		if giveUp {
			s.Log.Printf("!!! %s: %v — it failed %d times within %s; not restarting it again. Fix it, then `homelab up`.",
				name, ErrGaveUp, s.backoff.Limit, s.backoff.Window)
			return
		}
		s.Log.Printf("%s: restarting in %s", name, delay)
		if err := s.Sleep(ctx, delay); err != nil {
			return
		}
		c, ok, err := s.Docker.InspectInfo(ctx, id)
		if err != nil {
			s.Log.Printf("%s: inspect: %v", name, err)
			continue
		}
		if !ok || c.State == stateRunning {
			return // removed or recreated meanwhile, or already started by someone else
		}
		if p, ok := s.scope.Classify(c.Labels); ok {
			if d, _, err := LoadDesired(s.Root); err == nil &&
				((p.Core && !d.CoreRunning()) || (!p.Core && !d.ServiceRunning(p.Name))) {
				return // stopped on purpose while we waited
			}
		}
		if err := s.Run(ctx, "docker", "start", id); err != nil {
			s.Log.Printf("%s: docker start: %v", name, err)
			continue
		}
		s.Log.Printf("%s: restarted", name)
		return
	}
}

// Shutdown stops every running homelab container — services, then the core —
// with `docker stop`, which honours each container's stop timeout. The desired
// state is not touched: the next login restores the same set. With the detach
// marker present (reinstall, uninstall) it leaves everything running.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.init()
	if _, err := os.Stat(DetachFile(s.Root)); err == nil {
		s.Log.Printf("detaching: containers left running")
		return nil
	}
	cs, err := s.Docker.ComposeContainers(ctx)
	if err != nil {
		s.Log.Printf("stop: listing containers: %v", err)
		return err
	}
	svcs, core := ShutdownPlan(s.scope, cs)
	var errs []error
	for _, group := range []struct {
		what  string
		names []string
	}{{"services", svcs}, {"core", core}} {
		if len(group.names) == 0 {
			continue
		}
		s.Log.Printf("stopping %s: %s", group.what, strings.Join(group.names, " "))
		if err := s.Run(ctx, append([]string{"docker", "stop"}, group.names...)...); err != nil {
			s.Log.Printf("stopping %s: %v", group.what, err)
			errs = append(errs, err)
		}
	}
	s.Log.Printf("stack stopped")
	return errors.Join(errs...)
}
