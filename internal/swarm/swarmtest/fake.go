// Package swarmtest is an in-memory swarm.API for tests; no Docker daemon.
package swarmtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	hswarm "github.com/groot/homelab/internal/swarm"
)

var _ hswarm.API = (*Fake)(nil)

// Fake holds cluster state in memory. All state is unexported and guarded by
// a mutex; tests use the Set*/Add*/CallLog accessors. Calls are recorded as
// "<Method> <key arg>" for order assertions.
type Fake struct {
	// EventsCh and ErrCh are what Events returns. EventsCh has capacity 64,
	// ErrCh capacity 1; sending beyond that blocks the test.
	EventsCh chan events.Message
	ErrCh    chan error

	mu         sync.Mutex
	calls      []string
	nodes      []swarm.Node
	tasks      []swarm.Task
	volumes    []*volume.Volume
	images     map[string]image.InspectResponse
	containers []container.Summary
	swarm      swarm.Swarm
	services   map[string]swarm.Service
	networks   []network.Summary
	secrets    []swarm.Secret
	configs    []swarm.Config
	seq        int
	failUpdate map[string]error // service ID → error for its next ServiceUpdate
	info       system.Info
	inits      []swarm.InitRequest
	joins      []swarm.JoinRequest
	execs      []*Exec
	execExit   int
	created    []*CreatedContainer
	pulls      map[string]string // ref → ImagePull's JSON stream
	leaves     int
	copies     []Copy
	restartErr error
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		services: map[string]swarm.Service{},
		images:   map[string]image.InspectResponse{},
		EventsCh: make(chan events.Message, 64),
		ErrCh:    make(chan error, 1),
	}
}

// CallLog returns a copy of the recorded calls.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// SetNodes replaces the node list.
func (f *Fake) SetNodes(n []swarm.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = append([]swarm.Node(nil), n...)
}

// Nodes returns a copy of the node list (reflecting NodeUpdate calls).
func (f *Fake) Nodes() []swarm.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]swarm.Node(nil), f.nodes...)
}

// SetTasks replaces the task list.
func (f *Fake) SetTasks(t []swarm.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append([]swarm.Task(nil), t...)
}

// AddTask appends a task.
func (f *Fake) AddTask(t swarm.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, t)
}

// SetVolumes replaces the volume list.
func (f *Fake) SetVolumes(v ...*volume.Volume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes = append([]*volume.Volume(nil), v...)
}

// SetImage makes ImageInspectWithRaw succeed for ref.
func (f *Fake) SetImage(ref string, img image.InspectResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = img
}

// SetContainers replaces the container list.
func (f *Fake) SetContainers(c []container.Summary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append([]container.Summary(nil), c...)
}

func (f *Fake) rec(method string, key ...string) {
	s := method
	for _, k := range key {
		s += " " + k
	}
	f.calls = append(f.calls, s)
}

func (f *Fake) nextID(prefix string) string {
	f.seq++
	return prefix + strconv.Itoa(f.seq)
}

func nyi(m string) error { return fmt.Errorf("swarmtest: %s not implemented", m) }

func conflict(kind, name string) error {
	return fmt.Errorf("swarmtest: %s %q already exists: %w", kind, name, errdefs.ErrConflict)
}

// checkKeys rejects filter keys outside allowed, so a test can never pass on
// an accidentally unfiltered result.
func checkKeys(fl filters.Args, allowed ...string) error {
	for _, k := range fl.Keys() {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("swarmtest: unsupported filter %s", k)
		}
	}
	return nil
}

// matchCommon applies the id, name and label filters. id is a prefix match,
// name a substring match; label is "key" or "key=value" and all label values
// must match. Values of id/name are OR-ed. Absent filters match everything.
func matchCommon(fl filters.Args, id string, names []string, labels map[string]string) bool {
	if vs := fl.Get("id"); len(vs) > 0 {
		ok := false
		for _, v := range vs {
			ok = ok || strings.HasPrefix(id, v)
		}
		if !ok {
			return false
		}
	}
	if vs := fl.Get("name"); len(vs) > 0 {
		ok := false
		for _, v := range vs {
			for _, n := range names {
				ok = ok || strings.Contains(n, v)
			}
		}
		if !ok {
			return false
		}
	}
	for _, l := range fl.Get("label") {
		k, v, hasV := strings.Cut(l, "=")
		got, present := labels[k]
		if !present || (hasV && got != v) {
			return false
		}
	}
	return true
}

func (f *Fake) ServiceList(_ context.Context, o swarm.ServiceListOptions) ([]swarm.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceList")
	if err := checkKeys(o.Filters, "id", "name", "label"); err != nil {
		return nil, err
	}
	out := []swarm.Service{}
	for _, s := range f.services {
		if matchCommon(o.Filters, s.ID, []string{s.Spec.Name}, s.Spec.Labels) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
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
		return swarm.Service{}, nil, fmt.Errorf("swarmtest: service %q not found: %w", id, errdefs.ErrNotFound)
	}
	return s, []byte("{}"), nil
}

func (f *Fake) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ServiceCreate", spec.Name)
	if _, ok := f.find(spec.Name); ok {
		return swarm.ServiceCreateResponse{}, conflict("service", spec.Name)
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
		return swarm.ServiceUpdateResponse{}, fmt.Errorf("swarmtest: service %q not found: %w", id, errdefs.ErrNotFound)
	}
	if version.Index != s.Version.Index {
		return swarm.ServiceUpdateResponse{}, errors.New("swarmtest: update out of sequence")
	}
	if err, ok := f.failUpdate[s.ID]; ok {
		delete(f.failUpdate, s.ID)
		return swarm.ServiceUpdateResponse{}, err
	}
	prev := s.Spec
	s.PreviousSpec = &prev
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
		return fmt.Errorf("swarmtest: service %q not found: %w", id, errdefs.ErrNotFound)
	}
	delete(f.services, s.ID)
	return nil
}

// TaskList supports the service (ID or name), node (ID or hostname),
// desired-state and label filters over the settable task list.
func (f *Fake) TaskList(_ context.Context, o swarm.TaskListOptions) ([]swarm.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("TaskList")
	if err := checkKeys(o.Filters, "service", "node", "desired-state", "label"); err != nil {
		return nil, err
	}
	out := []swarm.Task{}
	for _, t := range f.tasks {
		if f.taskMatches(o.Filters, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *Fake) taskMatches(fl filters.Args, t swarm.Task) bool {
	if vs := fl.Get("service"); len(vs) > 0 {
		ok := false
		for _, v := range vs {
			if s, found := f.find(v); found {
				ok = ok || s.ID == t.ServiceID
			}
		}
		if !ok {
			return false
		}
	}
	if vs := fl.Get("node"); len(vs) > 0 {
		ok := false
		for _, v := range vs {
			if t.NodeID == v {
				ok = true
			}
			for _, n := range f.nodes {
				ok = ok || (n.ID == t.NodeID && n.Description.Hostname == v)
			}
		}
		if !ok {
			return false
		}
	}
	if vs := fl.Get("desired-state"); len(vs) > 0 && !slices.Contains(vs, string(t.DesiredState)) {
		return false
	}
	for _, l := range fl.Get("label") {
		k, v, hasV := strings.Cut(l, "=")
		got, present := t.Labels[k]
		if !present || (hasV && got != v) {
			return false
		}
	}
	return true
}

func (f *Fake) NodeList(_ context.Context, _ swarm.NodeListOptions) ([]swarm.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NodeList")
	return append([]swarm.Node(nil), f.nodes...), nil
}

func (f *Fake) NodeUpdate(_ context.Context, id string, version swarm.Version, spec swarm.NodeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NodeUpdate", id)
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			if version.Index != f.nodes[i].Version.Index {
				return errors.New("swarmtest: update out of sequence")
			}
			f.nodes[i].Spec = spec
			f.nodes[i].Version.Index++
			return nil
		}
	}
	return fmt.Errorf("swarmtest: node %q not found: %w", id, errdefs.ErrNotFound)
}

func (f *Fake) NetworkList(_ context.Context, o network.ListOptions) ([]network.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkList")
	if err := checkKeys(o.Filters, "id", "name", "label"); err != nil {
		return nil, err
	}
	out := []network.Summary{}
	for _, n := range f.networks {
		if matchCommon(o.Filters, n.ID, []string{n.Name}, n.Labels) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkCreate", name)
	for _, n := range f.networks {
		if n.Name == name {
			return network.CreateResponse{}, conflict("network", name)
		}
	}
	id := f.nextID("net")
	f.networks = append(f.networks, network.Summary{
		ID: id, Name: name, Driver: opts.Driver, Scope: opts.Scope,
		Attachable: opts.Attachable, Options: opts.Options, Labels: opts.Labels,
	})
	return network.CreateResponse{ID: id}, nil
}

// NetworkRemove removes a network by ID or name.
func (f *Fake) NetworkRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkRemove", id)
	for i := range f.networks {
		if f.networks[i].ID == id || f.networks[i].Name == id {
			f.networks = append(f.networks[:i], f.networks[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("swarmtest: network %q not found: %w", id, errdefs.ErrNotFound)
}

// AddNetwork adds a network as it is (any driver, attached containers
// included), e.g. the Compose-era home-services bridge.
func (f *Fake) AddNetwork(n network.Summary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n.ID == "" {
		n.ID = f.nextID("net")
	}
	f.networks = append(f.networks, n)
}

// NetworkInspect returns a network by ID or name, attached containers
// included.
func (f *Fake) NetworkInspect(_ context.Context, id string, _ network.InspectOptions) (network.Inspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("NetworkInspect", id)
	for i := range f.networks {
		if f.networks[i].ID == id || f.networks[i].Name == id {
			return f.networks[i], nil
		}
	}
	return network.Inspect{}, fmt.Errorf("swarmtest: network %q not found: %w", id, errdefs.ErrNotFound)
}

func (f *Fake) SecretList(_ context.Context, o swarm.SecretListOptions) ([]swarm.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SecretList")
	if err := checkKeys(o.Filters, "id", "name", "label"); err != nil {
		return nil, err
	}
	out := []swarm.Secret{}
	for _, s := range f.secrets {
		if matchCommon(o.Filters, s.ID, []string{s.Spec.Name}, s.Spec.Labels) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

func (f *Fake) SecretCreate(_ context.Context, spec swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SecretCreate", spec.Name)
	for _, s := range f.secrets {
		if s.Spec.Name == spec.Name {
			return swarm.SecretCreateResponse{}, conflict("secret", spec.Name)
		}
	}
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
	return fmt.Errorf("swarmtest: secret %q not found: %w", id, errdefs.ErrNotFound)
}

func (f *Fake) ConfigList(_ context.Context, o swarm.ConfigListOptions) ([]swarm.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ConfigList")
	if err := checkKeys(o.Filters, "id", "name", "label"); err != nil {
		return nil, err
	}
	out := []swarm.Config{}
	for _, c := range f.configs {
		if matchCommon(o.Filters, c.ID, []string{c.Spec.Name}, c.Spec.Labels) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

func (f *Fake) ConfigCreate(_ context.Context, spec swarm.ConfigSpec) (swarm.ConfigCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ConfigCreate", spec.Name)
	for _, c := range f.configs {
		if c.Spec.Name == spec.Name {
			return swarm.ConfigCreateResponse{}, conflict("config", spec.Name)
		}
	}
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
	return fmt.Errorf("swarmtest: config %q not found: %w", id, errdefs.ErrNotFound)
}

// VolumeList supports name (substring) and label filters; volumes have no id.
func (f *Fake) VolumeList(_ context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("VolumeList")
	if err := checkKeys(o.Filters, "id", "name", "label"); err != nil {
		return volume.ListResponse{}, err
	}
	out := []*volume.Volume{}
	for _, v := range f.volumes {
		if matchCommon(o.Filters, v.Name, []string{v.Name}, v.Labels) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return volume.ListResponse{Volumes: out}, nil
}

func (f *Fake) ImageInspectWithRaw(_ context.Context, ref string) (image.InspectResponse, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ImageInspectWithRaw", ref)
	img, ok := f.images[ref]
	if !ok {
		return image.InspectResponse{}, nil, fmt.Errorf("swarmtest: image %q not found: %w", ref, errdefs.ErrNotFound)
	}
	return img, []byte("{}"), nil
}

func (f *Fake) ImagePull(_ context.Context, ref string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ImagePull", ref)
	return io.NopCloser(strings.NewReader(f.pulls[ref])), nil
}

// SetPullStream makes ImagePull of ref return stream (the engine's JSON
// progress messages, possibly carrying an errorDetail).
func (f *Fake) SetPullStream(ref, stream string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls == nil {
		f.pulls = map[string]string{}
	}
	f.pulls[ref] = stream
}

// SwarmLeave takes this engine out of the swarm.
func (f *Fake) SwarmLeave(_ context.Context, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmLeave", strconv.FormatBool(force))
	f.leaves++
	f.info.Swarm = swarm.Info{LocalNodeState: swarm.LocalNodeStateInactive}
	return nil
}

// Leaves counts SwarmLeave calls.
func (f *Fake) Leaves() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leaves
}

func (f *Fake) SwarmInit(_ context.Context, req swarm.InitRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmInit", req.AdvertiseAddr)
	f.inits = append(f.inits, req)
	f.swarm.ID = f.nextID("swarm")
	id := f.nextID("node")
	// This engine is now the manager of a one-node swarm.
	f.info.Swarm.NodeID = id
	f.info.Swarm.LocalNodeState = swarm.LocalNodeStateActive
	f.info.Swarm.ControlAvailable = true
	f.nodes = append(f.nodes, swarm.Node{ID: id, Description: swarm.NodeDescription{Hostname: f.info.Name}})
	return id, nil
}

func (f *Fake) SwarmJoin(_ context.Context, req swarm.JoinRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmJoin", req.AdvertiseAddr)
	f.joins = append(f.joins, req)
	if f.info.Swarm.NodeID == "" {
		f.info.Swarm.NodeID = f.nextID("node")
	}
	f.info.Swarm.LocalNodeState = swarm.LocalNodeStateActive
	return nil
}

// Inits returns the recorded SwarmInit requests.
func (f *Fake) Inits() []swarm.InitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]swarm.InitRequest(nil), f.inits...)
}

// Joins returns the recorded SwarmJoin requests.
func (f *Fake) Joins() []swarm.JoinRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]swarm.JoinRequest(nil), f.joins...)
}

// SetSwarm sets what SwarmInspect returns (join tokens).
func (f *Fake) SetSwarm(s swarm.Swarm) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swarm = s
}

func (f *Fake) SwarmInspect(_ context.Context) (swarm.Swarm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("SwarmInspect")
	return f.swarm, nil
}

// Events returns the test-fed channels EventsCh and ErrCh.
func (f *Fake) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("Events")
	return f.EventsCh, f.ErrCh
}

// ContainerList filters the settable containers by id (prefix), name
// (substring, ignoring the leading "/"), label and status.
func (f *Fake) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerList")
	if err := checkKeys(o.Filters, "id", "name", "label", "status"); err != nil {
		return nil, err
	}
	out := []container.Summary{}
	for _, c := range f.containers {
		names := make([]string, len(c.Names))
		for i, n := range c.Names {
			names[i] = strings.TrimPrefix(n, "/")
		}
		if !matchCommon(o.Filters, c.ID, names, c.Labels) {
			continue
		}
		if vs := o.Filters.Get("status"); len(vs) > 0 && !slices.Contains(vs, c.State) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Exec is one recorded ContainerExecCreate and the stdin written to it.
type Exec struct {
	Container string
	Options   container.ExecOptions
	Stdin     []byte
}

// CreatedContainer is one recorded ContainerCreate.
type CreatedContainer struct {
	Name       string
	Config     container.Config
	HostConfig container.HostConfig
	Networking network.NetworkingConfig
	Started    bool
	Restarts   int
}

// SetInfo sets what Info returns.
func (f *Fake) SetInfo(i system.Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info = i
}

// Info returns the settable engine info.
func (f *Fake) Info(context.Context) (system.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("Info")
	return f.info, nil
}

// Execs returns a copy of the recorded execs, stdin included.
func (f *Fake) Execs() []Exec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Exec, len(f.execs))
	for i, e := range f.execs {
		out[i] = *e
		out[i].Stdin = append([]byte(nil), e.Stdin...)
	}
	return out
}

// SetExecExitCode makes every exec report code from ContainerExecInspect.
func (f *Fake) SetExecExitCode(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execExit = code
}

// Created returns a copy of the containers created through ContainerCreate.
func (f *Fake) Created() []CreatedContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CreatedContainer, len(f.created))
	for i, c := range f.created {
		out[i] = *c
	}
	return out
}

func (f *Fake) ContainerExecCreate(_ context.Context, c string, o container.ExecOptions) (container.ExecCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerExecCreate", c)
	f.execs = append(f.execs, &Exec{Container: c, Options: o})
	return container.ExecCreateResponse{ID: "exec" + strconv.Itoa(len(f.execs))}, nil
}

func (f *Fake) execByID(id string) (*Exec, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "exec"))
	if err != nil || n < 1 || n > len(f.execs) {
		return nil, false
	}
	return f.execs[n-1], true
}

// ContainerExecAttach returns a connection whose writes are recorded as the
// exec's stdin; its output is empty.
func (f *Fake) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerExecAttach", id)
	e, ok := f.execByID(id)
	if !ok {
		return types.HijackedResponse{}, fmt.Errorf("swarmtest: exec %q not found: %w", id, errdefs.ErrNotFound)
	}
	conn := &execConn{f: f, e: e}
	return types.NewHijackedResponse(conn, "application/vnd.docker.raw-stream"), nil
}

func (f *Fake) ContainerExecInspect(_ context.Context, id string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerExecInspect", id)
	if _, ok := f.execByID(id); !ok {
		return container.ExecInspect{}, fmt.Errorf("swarmtest: exec %q not found: %w", id, errdefs.ErrNotFound)
	}
	return container.ExecInspect{ExecID: id, ExitCode: f.execExit}, nil
}

// execConn records writes as stdin and reads as EOF. Only the methods a
// HijackedResponse user calls are implemented.
type execConn struct {
	net.Conn
	f *Fake
	e *Exec
}

func (c *execConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *execConn) Write(b []byte) (int, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.e.Stdin = append(c.e.Stdin, b...)
	return len(b), nil
}
func (c *execConn) Close() error      { return nil }
func (c *execConn) CloseWrite() error { return nil }

// ContainerCreate records the container; a second one of the same name
// conflicts.
func (f *Fake) ContainerCreate(_ context.Context, cfg *container.Config, hc *container.HostConfig,
	nc *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerCreate", name)
	for _, c := range f.created {
		if c.Name == name {
			return container.CreateResponse{}, conflict("container", name)
		}
	}
	c := &CreatedContainer{Name: name}
	if cfg != nil {
		c.Config = *cfg
	}
	if hc != nil {
		c.HostConfig = *hc
	}
	if nc != nil {
		c.Networking = *nc
	}
	f.created = append(f.created, c)
	return container.CreateResponse{ID: name}, nil
}

func (f *Fake) createdByName(name string) (*CreatedContainer, error) {
	for _, c := range f.created {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("swarmtest: container %q not found: %w", name, errdefs.ErrNotFound)
}

func (f *Fake) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerStart", name)
	c, err := f.createdByName(name)
	if err != nil {
		return err
	}
	c.Started = true
	return nil
}

func (f *Fake) ContainerRestart(_ context.Context, name string, _ container.StopOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerRestart", name)
	if f.restartErr != nil {
		return f.restartErr
	}
	c, err := f.createdByName(name)
	if err != nil {
		return err
	}
	c.Restarts++
	return nil
}

// Copy is one recorded CopyToContainer: the tar stream as sent.
type Copy struct {
	Container string
	Path      string
	Tar       []byte
	// Reader is the content's Go type (*io.PipeReader when streamed).
	Reader string
}

// CopyToContainer records the tar stream; the container must exist.
func (f *Fake) CopyToContainer(_ context.Context, name, dst string, content io.Reader, _ container.CopyToContainerOptions) error {
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("CopyToContainer", name, dst)
	if _, err := f.createdByName(name); err != nil {
		return err
	}
	f.copies = append(f.copies, Copy{Container: name, Path: dst, Tar: b, Reader: fmt.Sprintf("%T", content)})
	return nil
}

// ContainerRemove forgets a created container.
func (f *Fake) ContainerRemove(_ context.Context, name string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("ContainerRemove", name)
	for i, c := range f.created {
		if c.Name == name {
			f.created = append(f.created[:i], f.created[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("swarmtest: container %q not found: %w", name, errdefs.ErrNotFound)
}

// VolumeRemove records the removal and forgets the volume if listed.
func (f *Fake) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("VolumeRemove", name)
	for i, v := range f.volumes {
		if v.Name == name {
			f.volumes = append(f.volumes[:i], f.volumes[i+1:]...)
			break
		}
	}
	return nil
}

// FailRestart makes every ContainerRestart fail with err (nil: succeed).
func (f *Fake) FailRestart(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartErr = err
}

// Copies returns the recorded CopyToContainer calls.
func (f *Fake) Copies() []Copy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Copy(nil), f.copies...)
}

// FailServiceUpdateOnce makes the next ServiceUpdate of service id fail
// with err; later updates succeed.
func (f *Fake) FailServiceUpdateOnce(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUpdate == nil {
		f.failUpdate = map[string]error{}
	}
	f.failUpdate[id] = err
}
