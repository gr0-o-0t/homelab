// Package swarmtest is an in-memory swarm.API for tests; no Docker daemon.
package swarmtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"

	hswarm "github.com/groot/homelab/internal/swarm"
)

var _ hswarm.API = (*Fake)(nil)

// Fake holds cluster state in memory. Exported fields may be set by tests
// before use; Calls records "<Method> <key arg>" for order assertions.
type Fake struct {
	mu sync.Mutex

	Calls    []string
	Nodes    []swarm.Node
	Tasks    []swarm.Task
	Volumes  []*volume.Volume
	Images   map[string]image.InspectResponse
	EventsCh chan events.Message
	ErrCh    chan error
	Swarm    swarm.Swarm

	services map[string]swarm.Service
	networks []network.Summary
	secrets  []swarm.Secret
	configs  []swarm.Config
	seq      int
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		services: map[string]swarm.Service{},
		Images:   map[string]image.InspectResponse{},
		EventsCh: make(chan events.Message, 64),
		ErrCh:    make(chan error, 1),
	}
}

func (f *Fake) rec(method string, key ...string) {
	s := method
	for _, k := range key {
		s += " " + k
	}
	f.Calls = append(f.Calls, s)
}

func (f *Fake) nextID(prefix string) string {
	f.seq++
	return prefix + strconv.Itoa(f.seq)
}

func nyi(m string) error { return fmt.Errorf("swarmtest: %s not implemented", m) }

func (f *Fake) ServiceList(_ context.Context, _ swarm.ServiceListOptions) ([]swarm.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceList")
	out := make([]swarm.Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, s)
	}
	return out, nil
}

// find resolves a service by ID or name.
func (f *Fake) find(idOrName string) (swarm.Service, bool) {
	if s, ok := f.services[idOrName]; ok {
		return s, true
	}
	for _, s := range f.services {
		if s.Spec.Name == idOrName {
			return s, true
		}
	}
	return swarm.Service{}, false
}

func (f *Fake) ServiceInspectWithRaw(_ context.Context, id string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceInspectWithRaw", id)
	s, ok := f.find(id)
	if !ok {
		return swarm.Service{}, nil, fmt.Errorf("swarmtest: service %q not found", id)
	}
	return s, []byte("{}"), nil
}

func (f *Fake) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceCreate", spec.Name)
	if _, ok := f.find(spec.Name); ok {
		return swarm.ServiceCreateResponse{}, fmt.Errorf("swarmtest: service %q already exists", spec.Name)
	}
	id := f.nextID("svc")
	f.services[id] = swarm.Service{ID: id, Meta: swarm.Meta{Version: swarm.Version{Index: 1}}, Spec: spec}
	return swarm.ServiceCreateResponse{ID: id}, nil
}

func (f *Fake) ServiceUpdate(_ context.Context, id string, version swarm.Version, spec swarm.ServiceSpec, _ swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceUpdate", id)
	s, ok := f.find(id)
	if !ok {
		return swarm.ServiceUpdateResponse{}, fmt.Errorf("swarmtest: service %q not found", id)
	}
	if version.Index != s.Version.Index {
		return swarm.ServiceUpdateResponse{}, errors.New("swarmtest: update out of sequence")
	}
	s.PreviousSpec = &s.Spec
	s.Spec = spec
	s.Version.Index++
	f.services[s.ID] = s
	return swarm.ServiceUpdateResponse{}, nil
}

func (f *Fake) ServiceRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceRemove", id)
	s, ok := f.find(id)
	if !ok {
		return fmt.Errorf("swarmtest: service %q not found", id)
	}
	delete(f.services, s.ID)
	return nil
}

func (f *Fake) TaskList(_ context.Context, _ swarm.TaskListOptions) ([]swarm.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("TaskList")
	return append([]swarm.Task(nil), f.Tasks...), nil
}

func (f *Fake) NodeList(_ context.Context, _ swarm.NodeListOptions) ([]swarm.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NodeList")
	return append([]swarm.Node(nil), f.Nodes...), nil
}

func (f *Fake) NodeUpdate(_ context.Context, id string, version swarm.Version, spec swarm.NodeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NodeUpdate", id)
	for i := range f.Nodes {
		if f.Nodes[i].ID == id {
			if version.Index != f.Nodes[i].Version.Index {
				return errors.New("swarmtest: update out of sequence")
			}
			f.Nodes[i].Spec = spec
			f.Nodes[i].Version.Index++
			return nil
		}
	}
	return fmt.Errorf("swarmtest: node %q not found", id)
}

func (f *Fake) NetworkList(_ context.Context, _ network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkList")
	return append([]network.Summary(nil), f.networks...), nil
}

func (f *Fake) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkCreate", name)
	id := f.nextID("net")
	f.networks = append(f.networks, network.Summary{
		ID: id, Name: name, Driver: opts.Driver, Scope: opts.Scope,
		Attachable: opts.Attachable, Options: opts.Options, Labels: opts.Labels,
	})
	return network.CreateResponse{ID: id}, nil
}

func (f *Fake) SecretList(_ context.Context, _ swarm.SecretListOptions) ([]swarm.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SecretList")
	return append([]swarm.Secret(nil), f.secrets...), nil
}

func (f *Fake) SecretCreate(_ context.Context, spec swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SecretCreate", spec.Name)
	id := f.nextID("sec")
	f.secrets = append(f.secrets, swarm.Secret{ID: id, Spec: spec})
	return swarm.SecretCreateResponse{ID: id}, nil
}

func (f *Fake) SecretRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SecretRemove", id)
	for i, s := range f.secrets {
		if s.ID == id || s.Spec.Name == id {
			f.secrets = append(f.secrets[:i], f.secrets[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("swarmtest: secret %q not found", id)
}

func (f *Fake) ConfigList(_ context.Context, _ swarm.ConfigListOptions) ([]swarm.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ConfigList")
	return append([]swarm.Config(nil), f.configs...), nil
}

func (f *Fake) ConfigCreate(_ context.Context, spec swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ConfigCreate", spec.Name)
	id := f.nextID("cfg")
	f.configs = append(f.configs, swarm.Config{ID: id, Spec: spec})
	return swarm.ConfigCreateResponse{ID: id}, nil
}

func (f *Fake) ConfigRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ConfigRemove", id)
	for i, c := range f.configs {
		if c.ID == id || c.Spec.Name == id {
			f.configs = append(f.configs[:i], f.configs[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("swarmtest: config %q not found", id)
}

func (f *Fake) VolumeList(_ context.Context, _ volume.ListOptions) (volume.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("VolumeList")
	return volume.ListResponse{Volumes: append([]*volume.Volume(nil), f.Volumes...)}, nil
}

func (f *Fake) ImageInspectWithRaw(_ context.Context, ref string) (image.InspectResponse, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ImageInspectWithRaw", ref)
	img, ok := f.Images[ref]
	if !ok {
		return image.InspectResponse{}, nil, fmt.Errorf("swarmtest: image %q not found", ref)
	}
	return img, []byte("{}"), nil
}

func (f *Fake) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ImagePull", ref)
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *Fake) SwarmInit(_ context.Context, req swarm.InitRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmInit", req.AdvertiseAddr)
	f.Swarm.ID = f.nextID("swarm")
	return f.nextID("node"), nil
}

func (f *Fake) SwarmJoin(_ context.Context, req swarm.JoinRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmJoin", req.AdvertiseAddr)
	return nil
}

func (f *Fake) SwarmInspect(_ context.Context) (swarm.Swarm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmInspect")
	return f.Swarm, nil
}

// Events returns the test-fed channels EventsCh and ErrCh.
func (f *Fake) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("Events")
	return f.EventsCh, f.ErrCh
}

func (f *Fake) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerList")
	return nil, nil
}

func (f *Fake) ContainerExecCreate(_ context.Context, c string, _ container.ExecOptions) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerExecCreate", c)
	return container.ExecCreateResponse{}, nyi("ContainerExecCreate")
}

func (f *Fake) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerExecAttach", id)
	return types.HijackedResponse{}, nyi("ContainerExecAttach")
}
