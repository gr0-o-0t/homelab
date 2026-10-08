package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"github.com/groot/homelab/internal/docker"
)

// fakeDocker serves a fixed container list and lets tests change it.
type fakeDocker struct {
	mu      sync.Mutex
	cs      []docker.ContainerInfo
	listErr error
	events  chan docker.Event
}

func (f *fakeDocker) PingOK(context.Context) error { return nil }

func (f *fakeDocker) ComposeContainers(context.Context) ([]docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]docker.ContainerInfo(nil), f.cs...), nil
}

func (f *fakeDocker) InspectInfo(_ context.Context, id string) (docker.ContainerInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cs {
		if c.ID == id || c.Name == id {
			return c, true, nil
		}
	}
	return docker.ContainerInfo{}, false, nil
}

func (f *fakeDocker) ContainerEvents(ctx context.Context) (<-chan docker.Event, <-chan error) {
	errs := make(chan error, 1)
	if f.events == nil {
		out := make(chan docker.Event)
		go func() { <-ctx.Done(); close(out) }()
		return out, errs
	}
	return f.events, errs
}

func (f *fakeDocker) setState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.cs {
		if f.cs[i].ID == id {
			f.cs[i].State = state
		}
	}
}

// fakeRunner records every command line; fail makes matching calls fail.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	fail  func(argv []string) error
	after func(argv []string)
}

func (r *fakeRunner) run(_ context.Context, argv ...string) error {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(argv, " "))
	fail, after := r.fail, r.after
	r.mu.Unlock()
	if fail != nil {
		if err := fail(argv); err != nil {
			return err
		}
	}
	if after != nil {
		after(argv)
	}
	return nil
}

func (r *fakeRunner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

var errFake = errors.New("fake failure")

type fixture struct {
	root string
	core map[string]string
}

func newFixture(root string) fixture {
	return fixture{root: root, core: labelsFor(filepath.Join(root, "core"))}
}

func (f fixture) svc(name string) map[string]string {
	return labelsFor(filepath.Join(f.root, "services", name))
}

// ev builds a container event whose attributes carry the labels, as Docker's do.
func ev(action, id, name string, labels map[string]string, extra ...string) docker.Event {
	a := map[string]string{"name": name}
	for k, v := range labels {
		a[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return docker.Event{Action: action, ID: id, Attributes: a}
}
