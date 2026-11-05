package swarm_test

import (
	"context"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"

	"github.com/groot/homelab/internal/swarm/swarmtest"
)

var ctx = context.Background()

func fl(kv ...string) filters.Args {
	a := filters.NewArgs()
	for i := 0; i < len(kv); i += 2 {
		a.Add(kv[i], kv[i+1])
	}
	return a
}

func TestFake_ServiceFilters(t *testing.T) {
	f := swarmtest.New()
	for _, n := range []string{"zeta", "alpha", "alpine"} {
		s := spec(n)
		s.Labels = map[string]string{"app": n, "tier": "x"}
		if _, err := f.ServiceCreate(ctx, s, swarm.ServiceCreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(a filters.Args) []string {
		out, err := f.ServiceList(ctx, swarm.ServiceListOptions{Filters: a})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range out {
			names = append(names, s.Spec.Name)
		}
		return names
	}
	if got := list(filters.NewArgs()); len(got) != 3 || got[0] != "alpha" || got[1] != "alpine" || got[2] != "zeta" {
		t.Fatalf("order %v", got)
	}
	if got := list(fl("name", "alp")); len(got) != 2 {
		t.Fatalf("name %v", got)
	}
	if got := list(fl("label", "app=zeta")); len(got) != 1 || got[0] != "zeta" {
		t.Fatalf("label kv %v", got)
	}
	if got := list(fl("label", "tier")); len(got) != 3 {
		t.Fatalf("label key %v", got)
	}
	if got := list(fl("id", "svc1")); len(got) != 1 || got[0] != "zeta" {
		t.Fatalf("id %v", got)
	}
	if _, err := f.ServiceList(ctx, swarm.ServiceListOptions{Filters: fl("bogus", "1")}); err == nil ||
		err.Error() != "swarmtest: unsupported filter bogus" {
		t.Fatalf("err %v", err)
	}
}

func TestFake_NetworkSecretConfigVolumeFilters(t *testing.T) {
	f := swarmtest.New()
	_, _ = f.NetworkCreate(ctx, "home-services", network.CreateOptions{Labels: map[string]string{"k": "v"}})
	_, _ = f.NetworkCreate(ctx, "other", network.CreateOptions{})
	nets, err := f.NetworkList(ctx, network.ListOptions{Filters: fl("name", "home")})
	if err != nil || len(nets) != 1 {
		t.Fatalf("%v %v", nets, err)
	}
	nets, _ = f.NetworkList(ctx, network.ListOptions{Filters: fl("label", "k=v")})
	if len(nets) != 1 || nets[0].Name != "home-services" {
		t.Fatalf("%v", nets)
	}
	nets, _ = f.NetworkList(ctx, network.ListOptions{Filters: fl("id", "net2")})
	if len(nets) != 1 || nets[0].Name != "other" {
		t.Fatalf("%v", nets)
	}

	_, _ = f.SecretCreate(ctx, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a", Labels: map[string]string{"x": "1"}}})
	_, _ = f.SecretCreate(ctx, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "b"}})
	if s, _ := f.SecretList(ctx, swarm.SecretListOptions{Filters: fl("label", "x")}); len(s) != 1 || s[0].Spec.Name != "a" {
		t.Fatalf("%v", s)
	}
	if s, _ := f.SecretList(ctx, swarm.SecretListOptions{Filters: fl("name", "b")}); len(s) != 1 {
		t.Fatalf("%v", s)
	}
	if s, _ := f.SecretList(ctx, swarm.SecretListOptions{Filters: fl("id", "sec")}); len(s) != 2 {
		t.Fatalf("%v", s)
	}

	_, _ = f.ConfigCreate(ctx, swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "c1", Labels: map[string]string{"x": "1"}}})
	_, _ = f.ConfigCreate(ctx, swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "c2"}})
	if c, _ := f.ConfigList(ctx, swarm.ConfigListOptions{Filters: fl("label", "x=1")}); len(c) != 1 {
		t.Fatalf("%v", c)
	}
	if c, _ := f.ConfigList(ctx, swarm.ConfigListOptions{Filters: fl("name", "c")}); len(c) != 2 {
		t.Fatalf("%v", c)
	}
	if c, _ := f.ConfigList(ctx, swarm.ConfigListOptions{Filters: fl("id", "cfg6")}); len(c) != 1 {
		t.Fatalf("%v", c)
	}

	f.SetVolumes(&volume.Volume{Name: "data-b"}, &volume.Volume{Name: "data-a", Labels: map[string]string{"l": "1"}})
	v, _ := f.VolumeList(ctx, volume.ListOptions{})
	if len(v.Volumes) != 2 || v.Volumes[0].Name != "data-a" {
		t.Fatalf("%v", v.Volumes)
	}
	v, _ = f.VolumeList(ctx, volume.ListOptions{Filters: fl("label", "l")})
	if len(v.Volumes) != 1 {
		t.Fatalf("%v", v.Volumes)
	}
	v, _ = f.VolumeList(ctx, volume.ListOptions{Filters: fl("name", "data-b")})
	if len(v.Volumes) != 1 {
		t.Fatalf("%v", v.Volumes)
	}
	if _, err := f.SecretList(ctx, swarm.SecretListOptions{Filters: fl("x", "y")}); err == nil {
		t.Fatal("unsupported filter accepted")
	}
	if _, err := f.VolumeList(ctx, volume.ListOptions{Filters: fl("dangling", "true")}); err == nil {
		t.Fatal("unsupported filter accepted")
	}
}

func TestFake_TaskFilters(t *testing.T) {
	f := swarmtest.New()
	r, _ := f.ServiceCreate(ctx, spec("web"), swarm.ServiceCreateOptions{})
	f.SetNodes([]swarm.Node{{ID: "n1", Description: swarm.NodeDescription{Hostname: "mini"}}})
	f.AddTask(swarm.Task{ID: "t1", ServiceID: r.ID, NodeID: "n1", DesiredState: swarm.TaskStateRunning,
		Annotations: swarm.Annotations{Labels: map[string]string{"a": "b"}}})
	f.AddTask(swarm.Task{ID: "t2", ServiceID: "other", NodeID: "n2", DesiredState: swarm.TaskStateShutdown})
	n := func(a filters.Args) int {
		out, err := f.TaskList(ctx, swarm.TaskListOptions{Filters: a})
		if err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	if n(filters.NewArgs()) != 2 || n(fl("service", "web")) != 1 || n(fl("service", r.ID)) != 1 ||
		n(fl("service", "nope")) != 0 || n(fl("node", "n1")) != 1 || n(fl("node", "mini")) != 1 ||
		n(fl("desired-state", "shutdown")) != 1 || n(fl("label", "a=b")) != 1 || n(fl("label", "a=c")) != 0 {
		t.Fatal("task filter mismatch")
	}
	if _, err := f.TaskList(ctx, swarm.TaskListOptions{Filters: fl("name", "x")}); err == nil {
		t.Fatal("unsupported filter accepted")
	}
}

func TestFake_ContainerList(t *testing.T) {
	f := swarmtest.New()
	f.SetContainers([]container.Summary{
		{ID: "c1", Names: []string{"/web.1.abc"}, State: "running", Labels: map[string]string{"com.docker.swarm.service.name": "web"}},
		{ID: "c2", Names: []string{"/db.1.def"}, State: "exited"},
	})
	n := func(a filters.Args) int {
		out, err := f.ContainerList(ctx, container.ListOptions{Filters: a})
		if err != nil {
			t.Fatal(err)
		}
		return len(out)
	}
	if n(filters.NewArgs()) != 2 || n(fl("label", "com.docker.swarm.service.name=web")) != 1 ||
		n(fl("name", "db")) != 1 || n(fl("status", "running")) != 1 || n(fl("id", "c2")) != 1 {
		t.Fatal("container filter mismatch")
	}
	if _, err := f.ContainerList(ctx, container.ListOptions{Filters: fl("ancestor", "x")}); err == nil {
		t.Fatal("unsupported filter accepted")
	}
}

func TestFake_DuplicatesConflict(t *testing.T) {
	f := swarmtest.New()
	check := func(name string, err error) {
		t.Helper()
		if err == nil || !cerrdefs.IsConflict(err) {
			t.Fatalf("%s: want conflict, got %v", name, err)
		}
	}
	_, _ = f.ServiceCreate(ctx, spec("a"), swarm.ServiceCreateOptions{})
	_, err := f.ServiceCreate(ctx, spec("a"), swarm.ServiceCreateOptions{})
	check("service", err)
	_, _ = f.SecretCreate(ctx, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}})
	_, err = f.SecretCreate(ctx, swarm.SecretSpec{Annotations: swarm.Annotations{Name: "a"}})
	check("secret", err)
	_, _ = f.ConfigCreate(ctx, swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "a"}})
	_, err = f.ConfigCreate(ctx, swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "a"}})
	check("config", err)
	_, _ = f.NetworkCreate(ctx, "a", network.CreateOptions{})
	_, err = f.NetworkCreate(ctx, "a", network.CreateOptions{})
	check("network", err)
	if err.Error() == "" || !contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestFake_PreviousSpec(t *testing.T) {
	f := swarmtest.New()
	r, _ := f.ServiceCreate(ctx, spec("old"), swarm.ServiceCreateOptions{})
	svc, _, _ := f.ServiceInspectWithRaw(ctx, r.ID, swarm.ServiceInspectOptions{})
	if _, err := f.ServiceUpdate(ctx, r.ID, svc.Version, spec("old2"), swarm.ServiceUpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc, _, _ = f.ServiceInspectWithRaw(ctx, r.ID, swarm.ServiceInspectOptions{})
	if svc.PreviousSpec == nil || svc.PreviousSpec.Name != "old" || svc.Spec.Name != "old2" {
		t.Fatalf("prev=%+v spec=%s", svc.PreviousSpec, svc.Spec.Name)
	}
}

func TestFake_ConcurrentAccess(t *testing.T) {
	f := swarmtest.New()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = f.NodeList(ctx, swarm.NodeListOptions{})
				_, _ = f.ServiceList(ctx, swarm.ServiceListOptions{})
			}
		}
	}()
	for i := 0; i < 200; i++ {
		f.SetNodes([]swarm.Node{{ID: "n"}})
		f.AddTask(swarm.Task{ID: "t"})
		_ = f.CallLog()
	}
	close(done)
	wg.Wait()
}
