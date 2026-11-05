package swarm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// fakeClock hands Run timer channels the test fires by hand, and reports
// each request so the test knows where Run is.
type fakeClock struct{ reqs chan timerReq }

type timerReq struct {
	d  time.Duration
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{reqs: make(chan timerReq, 16)} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.reqs <- timerReq{d, ch}
	return ch
}

func (c *fakeClock) next(t *testing.T) timerReq {
	t.Helper()
	select {
	case r := <-c.reqs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Run requested no timer")
		return timerReq{}
	}
}

func (c *fakeClock) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-c.reqs:
		t.Fatalf("unexpected timer request for %s", r.d)
	default:
	}
}

// rnode is an active node (deploy_test.go's node with availability fixed).
func rnode(id, role string, state dswarm.NodeState) dswarm.Node {
	return node(id, role, state, dswarm.NodeAvailabilityActive)
}

func addService(t *testing.T, f *swarmtest.Fake, name string, role swarm.Role, constraints []string) string {
	t.Helper()
	spec := dswarm.ServiceSpec{Annotations: dswarm.Annotations{Name: name, Labels: map[string]string{
		swarm.LabelService: strings.Split(name, "_")[0],
		swarm.LabelRole:    string(role),
	}}}
	if len(constraints) > 0 {
		spec.TaskTemplate.Placement = &dswarm.Placement{Constraints: constraints}
	}
	r, err := f.ServiceCreate(context.Background(), spec, dswarm.ServiceCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func constraintsOf(t *testing.T, f *swarmtest.Fake, id string) []string {
	t.Helper()
	s, _, err := f.ServiceInspectWithRaw(context.Background(), id, dswarm.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.TaskTemplate.Placement == nil {
		return nil
	}
	return s.Spec.TaskTemplate.Placement.Constraints
}

func updates(f *swarmtest.Fake) []string {
	var out []string
	for _, c := range f.CallLog() {
		if strings.HasPrefix(c, "ServiceUpdate ") {
			out = append(out, strings.TrimPrefix(c, "ServiceUpdate "))
		}
	}
	return out
}

// startRebalancer runs a Rebalancer until the test ends and waits for its
// initial placement (after subscribing), so node changes made after this
// are seen as events.
func startRebalancer(t *testing.T, f *swarmtest.Fake, clk *fakeClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r := &swarm.Rebalancer{API: f, After: clk.After}
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	waitCalls(t, f, "ServiceList", 1)
}

func calls(f *swarmtest.Fake, method string) (n int) {
	for _, c := range f.CallLog() {
		if c == method {
			n++
		}
	}
	return n
}

// waitCalls waits until f has had at least n calls of method.
func waitCalls(t *testing.T, f *swarmtest.Fake, method string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for calls(f, method) < n {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d %s calls, saw %d", n, method, calls(f, method))
		}
		time.Sleep(time.Millisecond)
	}
}

// fire fires a timer and waits for the apply it triggers to list services.
func fire(t *testing.T, f *swarmtest.Fake, r timerReq) {
	t.Helper()
	n := calls(f, "ServiceList")
	r.ch <- time.Now()
	waitCalls(t, f, "ServiceList", n+1)
}

// nodeEvent sends a node event and waits for Run to re-read the nodes for
// it, so the next SetNodes cannot overtake it.
func nodeEvent(t *testing.T, f *swarmtest.Fake, id string) {
	t.Helper()
	n := calls(f, "NodeList")
	f.EventsCh <- events.Message{Type: events.NodeEventType, Action: events.ActionUpdate, Actor: events.Actor{ID: id}}
	waitCalls(t, f, "NodeList", n+1)
}

var (
	onWorker = []string{"node.labels.homelab.role==worker"}
	onCore   = []string{"node.labels.homelab.role==core"}
)

func TestRebalancer_DebouncesFlapping(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	web := addService(t, f, "app_web", swarm.RoleWorker, nil)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	nodeEvent(t, f, "laptop")
	first := clk.next(t)
	if first.d != 2*time.Minute {
		t.Errorf("coming up debounces %s, want the 2m default", first.d)
	}
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	nodeEvent(t, f, "laptop")
	// The flap back replaces the wait with a reconcile of the old state.
	rec := clk.next(t)
	if rec.d != swarm.ReconcileDelay {
		t.Errorf("flap back scheduled %s, want a %s reconcile", rec.d, swarm.ReconcileDelay)
	}
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	nodeEvent(t, f, "laptop")
	second := clk.next(t)

	first.ch <- time.Now() // stale: must not apply
	rec.ch <- time.Now()   // stale too
	clk.none(t)
	if u := updates(f); len(u) != 0 {
		t.Fatalf("flapping within the debounce updated %v", u)
	}

	fire(t, f, second)
	if got := constraintsOf(t, f, web); !slices.Equal(got, onWorker) {
		t.Errorf("after a stable Ready, web constraints = %v, want %v", got, onWorker)
	}
}

func TestRebalancer_LastWorkerDownDropsConstraint(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	web := addService(t, f, "app_web", swarm.RoleWorker, onWorker)
	clk := newClock()
	startRebalancer(t, f, clk)
	if u := updates(f); len(u) != 0 {
		t.Fatalf("services already placed right were updated at start: %v", u)
	}

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	nodeEvent(t, f, "laptop")
	r := clk.next(t)
	if r.d != 20*time.Second {
		t.Errorf("going down debounces %s, want 20s", r.d)
	}
	r.ch <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for len(updates(f)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := constraintsOf(t, f, web); len(got) != 0 {
		t.Errorf("with no worker ready, web constraints = %v, want none (back to the minis)", got)
	}
}

func TestRebalancer_WorkerDownDoesNotTouchPinned(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	web := addService(t, f, "app_web", swarm.RoleWorker, onWorker)
	db := addService(t, f, "app_db", swarm.RoleCore, onCore)
	tool := addService(t, f, "tool_tool", swarm.RoleAny, nil)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	nodeEvent(t, f, "laptop")
	clk.next(t).ch <- time.Now()
	deadline := time.Now().Add(5 * time.Second)
	for len(updates(f)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if u := updates(f); !slices.Equal(u, []string{web}) {
		t.Errorf("updated %v, want only the worker service %s", u, web)
	}
	if got := constraintsOf(t, f, db); !slices.Equal(got, onCore) {
		t.Errorf("pinned db constraints = %v, want %v", got, onCore)
	}
	if got := constraintsOf(t, f, tool); len(got) != 0 {
		t.Errorf("any-role service constraints = %v, want none", got)
	}
}

func TestRebalancer_SecondWorkerReadyIsNoOp(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	laptop := rnode("laptop", "worker", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, laptop, rnode("desktop", "worker", dswarm.NodeStateDown)})
	addService(t, f, "app_web", swarm.RoleWorker, onWorker)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.SetNodes([]dswarm.Node{mini, laptop, rnode("desktop", "worker", dswarm.NodeStateReady)})
	nodeEvent(t, f, "desktop")
	// No transition: only a reconcile, and it finds everything in place.
	r := clk.next(t)
	if r.d != swarm.ReconcileDelay {
		t.Errorf("second worker scheduled %s, want only the %s reconcile", r.d, swarm.ReconcileDelay)
	}
	fire(t, f, r)
	clk.none(t)
	if u := updates(f); len(u) != 0 {
		t.Errorf("a second worker becoming ready updated %v", u)
	}
}

// A deploy during the up-debounce reads "a worker is ready" and places the
// service on workers; the worker leaving within the debounce is no
// transition, but the reconcile it schedules must still bring the service
// back to the minis.
func TestRebalancer_UpDuringDebounceThenFlapReconciles(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	web := addService(t, f, "app_web", swarm.RoleWorker, nil)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	nodeEvent(t, f, "laptop")
	clk.next(t) // the 2m wait, never fired

	// `homelab up web` now: Deployer.Up sees the ready laptop.
	cur, _, err := f.ServiceInspectWithRaw(context.Background(), web, dswarm.ServiceInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec := cur.Spec
	spec.TaskTemplate.Placement = &dswarm.Placement{Constraints: onWorker}
	if _, err := f.ServiceUpdate(context.Background(), web, cur.Version, spec, dswarm.ServiceUpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	nodeEvent(t, f, "laptop")
	r := clk.next(t)
	if r.d != swarm.ReconcileDelay {
		t.Fatalf("flap back scheduled %s, want a %s reconcile", r.d, swarm.ReconcileDelay)
	}
	fire(t, f, r)
	if got := constraintsOf(t, f, web); len(got) != 0 {
		t.Errorf("web constraints = %v, want none: no worker is ready", got)
	}
}

// An apply that fails for one service still moves the others, and a flap
// back before its retry must reconcile the ones that moved.
func TestRebalancer_PartialApplyThenFlapReconciles(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	api := addService(t, f, "api_api", swarm.RoleWorker, nil)
	web := addService(t, f, "web_web", swarm.RoleWorker, nil)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)})
	nodeEvent(t, f, "laptop")
	f.FailServiceUpdateOnce(web, errors.New("rpc error: transient"))
	fire(t, f, clk.next(t))
	retry := clk.next(t)
	if retry.d != 5*time.Second {
		t.Errorf("after a failed apply, timer = %s, want the 5s retry", retry.d)
	}
	if got := constraintsOf(t, f, api); !slices.Equal(got, onWorker) {
		t.Fatalf("api constraints = %v, want %v: one failure must not stop the rest", got, onWorker)
	}

	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	nodeEvent(t, f, "laptop")
	r := clk.next(t)
	if r.d != swarm.ReconcileDelay {
		t.Fatalf("flap back scheduled %s, want a %s reconcile", r.d, swarm.ReconcileDelay)
	}
	fire(t, f, r)
	for _, id := range []string{api, web} {
		if got := constraintsOf(t, f, id); len(got) != 0 {
			t.Errorf("%s constraints = %v, want none: no worker is ready", id, got)
		}
	}
}

// Losing the event stream must not end the rebalancer: it backs off,
// resubscribes and catches up on what it missed.
func TestRebalancer_ResubscribesAfterEventsError(t *testing.T) {
	f := swarmtest.New()
	mini := rnode("mini", "core", dswarm.NodeStateReady)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateDown)})
	addService(t, f, "app_web", swarm.RoleWorker, nil)
	clk := newClock()
	startRebalancer(t, f, clk)

	f.ErrCh <- context.DeadlineExceeded
	backoff := clk.next(t)
	f.SetNodes([]dswarm.Node{mini, rnode("laptop", "worker", dswarm.NodeStateReady)}) // missed while away
	backoff.ch <- time.Now()
	if r := clk.next(t); r.d != 2*time.Minute {
		t.Errorf("after resubscribing, timer = %s, want the 2m debounce for the missed Ready", r.d)
	}
	n := 0
	for _, c := range f.CallLog() {
		if c == "Events" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("subscribed %d times, want 2", n)
	}
}
