package swarm_test

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	hswarm "github.com/groot/homelab/internal/swarm"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

var _ hswarm.API = (*client.Client)(nil)
var _ hswarm.API = (*swarmtest.Fake)(nil)

func TestClientSatisfiesAPI(t *testing.T) {}

func spec(name string) swarm.ServiceSpec {
	return swarm.ServiceSpec{Annotations: swarm.Annotations{Name: name}}
}

func TestFake_ServiceCreateThenList(t *testing.T) {
	ctx := context.Background()
	f := swarmtest.New()
	resp, err := f.ServiceCreate(ctx, spec("web"), swarm.ServiceCreateOptions{})
	if err != nil || resp.ID == "" {
		t.Fatalf("create: %v %+v", err, resp)
	}
	list, err := f.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil || len(list) != 1 || list[0].Spec.Name != "web" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if got := f.Calls; len(got) != 2 || got[0] != "ServiceCreate web" || got[1] != "ServiceList" {
		t.Fatalf("calls = %v", got)
	}
}

func TestFake_ServiceUpdateVersion(t *testing.T) {
	ctx := context.Background()
	f := swarmtest.New()
	resp, _ := f.ServiceCreate(ctx, spec("web"), swarm.ServiceCreateOptions{})
	svc, _, _ := f.ServiceInspectWithRaw(ctx, resp.ID, swarm.ServiceInspectOptions{})
	if _, err := f.ServiceUpdate(ctx, resp.ID, svc.Version, spec("web"), swarm.ServiceUpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ServiceUpdate(ctx, resp.ID, svc.Version, spec("web"), swarm.ServiceUpdateOptions{}); err == nil {
		t.Fatal("stale version accepted")
	}
	svc, _, _ = f.ServiceInspectWithRaw(ctx, resp.ID, swarm.ServiceInspectOptions{})
	if svc.Version.Index < 2 {
		t.Fatalf("version %d", svc.Version.Index)
	}
}
