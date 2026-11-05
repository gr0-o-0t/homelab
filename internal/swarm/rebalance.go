package swarm

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

// Default debounces. Coming up waits longer: a laptop that wakes for a
// minute should not drag every stateless container across and back.
// Going down is short so work returns to the minis quickly.
const (
	DefaultUpDebounce   = 2 * time.Minute
	DownDebounce        = 20 * time.Second
	eventsRetryInterval = 5 * time.Second
)

// Rebalancer moves worker-role services onto the worker PCs when the first
// one becomes ready, and back to the minis when the last one leaves. Swarm
// has no soft affinity, so this is done by switching their placement
// constraint (Constraints(RoleWorker, anyWorkerReady)). Core- and any-role
// services are never touched.
type Rebalancer struct {
	API API
	// Debounce is how long a worker must stay ready before work moves onto
	// it; zero means DefaultUpDebounce. Losing the last worker uses
	// DownDebounce.
	Debounce time.Duration
	// After is time.After; tests inject a fake clock.
	After func(time.Duration) <-chan time.Time
	// Logf reports failures Run recovers from; nil means log.Printf.
	Logf func(format string, args ...any)
}

func (r *Rebalancer) after(d time.Duration) <-chan time.Time {
	if r.After != nil {
		return r.After(d)
	}
	return time.After(d)
}

func (r *Rebalancer) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// sleep waits d or until ctx is done, reporting whether to carry on.
func (r *Rebalancer) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-r.after(d):
		return true
	}
}

// Run rebalances until ctx is cancelled, then returns nil. It places the
// worker services for the cluster as it finds it, then follows node events.
// A lost event stream is resubscribed after a pause.
func (r *Rebalancer) Run(ctx context.Context) error {
	d := &Deployer{API: r.API}

	var applied bool
	for {
		ready, err := d.anyWorkerReady(ctx)
		if err == nil {
			if err = r.apply(ctx, d, ready); err == nil {
				applied = ready
				break
			}
		}
		r.logf("rebalancer: initial placement: %v", err)
		if !r.sleep(ctx, eventsRetryInterval) {
			return nil
		}
	}

	var (
		pending bool
		timer   <-chan time.Time // nil: no transition pending
	)
	// observe re-reads the nodes and starts, keeps or cancels the wait for
	// the transition they show.
	observe := func() {
		ready, err := d.anyWorkerReady(ctx)
		if err != nil {
			r.logf("rebalancer: %v", err)
			return
		}
		switch {
		case ready == applied:
			timer = nil // flapped back: nothing to do
		case timer == nil || ready != pending:
			pending = ready
			wait := DownDebounce
			if ready {
				wait = r.Debounce
				if wait <= 0 {
					wait = DefaultUpDebounce
				}
			}
			timer = r.after(wait)
		}
	}

	for {
		subCtx, cancel := context.WithCancel(ctx)
		msgs, errs := r.API.Events(subCtx, events.ListOptions{
			Filters: filters.NewArgs(filters.Arg("type", string(events.NodeEventType))),
		})
		observe() // catch up on anything missed while unsubscribed
		lost := r.follow(ctx, msgs, errs, observe, &timer, func() {
			if err := r.apply(ctx, d, pending); err != nil {
				r.logf("rebalancer: %v", err)
				timer = r.after(eventsRetryInterval) // try again
				return
			}
			applied = pending
		})
		cancel()
		if !lost || !r.sleep(ctx, eventsRetryInterval) {
			return nil
		}
	}
}

// follow handles one event subscription until ctx is done (false) or the
// stream fails (true).
func (r *Rebalancer) follow(ctx context.Context, msgs <-chan events.Message, errs <-chan error,
	observe func(), timer *<-chan time.Time, fire func(),
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case m, ok := <-msgs:
			if !ok {
				return true
			}
			if m.Type == events.NodeEventType {
				observe()
			}
		case err := <-errs:
			if ctx.Err() != nil {
				return false
			}
			r.logf("rebalancer: node events: %v", err)
			return true
		case <-*timer:
			*timer = nil
			fire()
		}
	}
}

// apply sets every worker-role service's constraints for workerReady,
// updating only those that differ.
func (r *Rebalancer) apply(ctx context.Context, d *Deployer, workerReady bool) error {
	list, err := r.API.ServiceList(ctx, swarm.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelRole+"="+string(RoleWorker))),
	})
	if err != nil {
		return fmt.Errorf("listing worker services: %w", err)
	}
	want := Constraints(RoleWorker, workerReady)
	for i := range list {
		s := &list[i]
		if s.Spec.Labels[LabelRole] != string(RoleWorker) || slices.Equal(constraintsOf(&s.Spec), want) {
			continue
		}
		err := d.update(ctx, s.ID, swarm.ServiceUpdateOptions{}, func(now *swarm.ServiceSpec) swarm.ServiceSpec {
			next := *now
			var p swarm.Placement
			if now.TaskTemplate.Placement != nil { // don't mutate the inspected spec
				p = *now.TaskTemplate.Placement
			}
			p.Constraints = want
			next.TaskTemplate.Placement = &p
			return next
		})
		if err != nil {
			return fmt.Errorf("placing %s: %w", s.Spec.Name, err)
		}
	}
	return nil
}

func constraintsOf(spec *swarm.ServiceSpec) []string {
	if spec.TaskTemplate.Placement == nil {
		return nil
	}
	return spec.TaskTemplate.Placement.Constraints
}
