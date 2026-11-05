package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"gopkg.in/yaml.v3"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/swarm/swarmtest"
)

// fixedNames resolves every engine to its old fixed container name, so tests
// about the SQL/CLI a provisioner issues read as before.
func fixedNames(t config.DBType) (string, error) {
	if c := config.SharedDBContainer(t); c != "" {
		return c, nil
	}
	return "", fmt.Errorf("unknown database type: %s", t)
}

// fakeUp records which services were started.
type fakeUp struct {
	started []string
	err     error
}

func (f *fakeUp) Up(_ context.Context, svc string) error {
	f.started = append(f.started, svc)
	return f.err
}

func taskContainer(id, svcName, state string) container.Summary {
	return container.Summary{
		ID:     id,
		Names:  []string{"/" + svcName + ".1.task" + id},
		State:  state,
		Labels: map[string]string{"com.docker.swarm.service.name": svcName},
	}
}

func TestProvisioner_ResolvesSwarmTaskContainer(t *testing.T) {
	f := swarmtest.New()
	f.SetContainers([]container.Summary{
		taskContainer("old1", "postgres_postgres", "exited"),
		taskContainer("pg42", "postgres_postgres", "running"),
		taskContainer("rd7", "redis_redis", "running"),
		// A homelab-postgres named container from the Compose era is not
		// a Swarm task and must not be picked.
		{ID: "legacy", Names: []string{"/homelab-postgres"}, State: "running"},
	})
	p := &Provisioner{API: f}

	got, err := p.Container(config.DBPostgres)
	if err != nil {
		t.Fatal(err)
	}
	if got != "pg42" {
		t.Errorf("postgres container = %q, want the running task pg42", got)
	}
	if got, _ := p.Container(config.DBRedis); got != "rd7" {
		t.Errorf("redis container = %q, want rd7", got)
	}

	_, err = p.Container(config.DBS3)
	if err == nil || !strings.Contains(err.Error(), "not running on this node") {
		t.Errorf("garage with no task: want a not-running-on-this-node error, got %v", err)
	}
	if _, err := p.Container(config.DBType("mongo")); err == nil {
		t.Error("unknown db type should fail")
	}
}

// The resolver looks the provider up as <provider>_<provider>: its Swarm
// service is named <stack>_<compose key>, and each provider's compose key is
// its own name. Pin that so renaming a compose key cannot silently break it.
func TestProvisioner_ProviderComposeKeyIsItsName(t *testing.T) {
	for _, dbType := range []config.DBType{config.DBPostgres, config.DBMariaDB, config.DBRedis, config.DBS3} {
		name := config.SharedDBName(dbType)
		raw, err := os.ReadFile(filepath.Join("..", "..", "assets", "services", name, "docker-compose.yml"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Services[name]; !ok || len(doc.Services) != 1 {
			t.Errorf("%s compose must have exactly one service keyed %q, has %v", name, name, doc.Services)
		}
	}
}

func TestProvisioner_NoDockerAPI(t *testing.T) {
	p := &Provisioner{}
	if _, err := p.Container(config.DBPostgres); err == nil {
		t.Fatal("resolving without a Docker API should fail, not panic")
	}
}

func TestEnsureRunning(t *testing.T) {
	ctx := context.Background()

	t.Run("running task: nothing started", func(t *testing.T) {
		up := &fakeUp{}
		p := &Provisioner{ContainerFor: fixedNames, Deployer: up}
		if err := p.EnsureRunning(ctx, config.DBPostgres); err != nil {
			t.Fatal(err)
		}
		if len(up.started) != 0 {
			t.Errorf("started %v, want nothing", up.started)
		}
	})

	notRunning := func(config.DBType) (string, error) { return "", errors.New("postgres is not running on this node") }

	t.Run("not running: starts the provider through the deployer", func(t *testing.T) {
		up := &fakeUp{}
		p := &Provisioner{ContainerFor: notRunning, Deployer: up}
		if err := p.EnsureRunning(ctx, config.DBS3); err != nil {
			t.Fatal(err)
		}
		if len(up.started) != 1 || up.started[0] != "garage" {
			t.Errorf("started %v, want [garage]", up.started)
		}
	})

	t.Run("deploy failure is reported", func(t *testing.T) {
		p := &Provisioner{ContainerFor: notRunning, Deployer: &fakeUp{err: errors.New("no secrets")}}
		err := p.EnsureRunning(ctx, config.DBPostgres)
		if err == nil || !strings.Contains(err.Error(), "no secrets") {
			t.Errorf("want the deploy error, got %v", err)
		}
	})

	t.Run("no deployer: check only", func(t *testing.T) {
		p := &Provisioner{ContainerFor: notRunning}
		err := p.EnsureRunning(ctx, config.DBPostgres)
		if err == nil || !strings.Contains(err.Error(), "homelab up postgres") {
			t.Errorf("want a not-running error naming `homelab up postgres`, got %v", err)
		}
	})

	t.Run("unknown db type", func(t *testing.T) {
		up := &fakeUp{}
		p := &Provisioner{ContainerFor: fixedNames, Deployer: up}
		if err := p.EnsureRunning(ctx, config.DBType("foobar")); err == nil {
			t.Fatal("expected error for unknown db type")
		}
		if len(up.started) != 0 {
			t.Errorf("started %v for an unknown type", up.started)
		}
	})
}

// A freshly deployed provider has no task container for a while, then one
// that is starting, then a healthy one: WaitHealthy rides all of it out.
func TestWaitHealthy(t *testing.T) {
	ctx := context.Background()
	const inspect = "docker inspect --format={{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}} "

	t.Run("waits for the task container, then its health", func(t *testing.T) {
		calls := 0
		resolve := func(config.DBType) (string, error) {
			calls++
			if calls < 3 {
				return "", errors.New("postgres is not running on this node")
			}
			return "pg42", nil
		}
		polls := 0
		fc := &fakeCommander{outputFn: func(key string) ([]byte, error, bool) {
			if key != inspect+"pg42" {
				return nil, fmt.Errorf("unexpected %q", key), true
			}
			polls++
			if polls < 2 {
				return []byte("starting\n"), nil, true
			}
			return []byte("healthy\n"), nil, true
		}}
		p := &Provisioner{ContainerFor: resolve, RC: fc, PollInterval: time.Millisecond}
		if err := p.WaitHealthy(ctx, config.DBPostgres, time.Second); err != nil {
			t.Fatal(err)
		}
		if polls != 2 {
			t.Errorf("inspected %d times, want 2", polls)
		}
	})

	t.Run("accepts running when no healthcheck is defined", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspect + "homelab-postgres": []byte("running\n")}}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, PollInterval: time.Millisecond}
		if err := p.WaitHealthy(ctx, config.DBPostgres, time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("times out with actionable guidance", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspect + "homelab-postgres": []byte("starting\n")}}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, PollInterval: time.Millisecond}
		err := p.WaitHealthy(ctx, config.DBPostgres, 5*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout")
		}
		for _, want := range []string{"did not become healthy", "starting", "homelab logs postgres", "homelab setup postgres"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
	})

	t.Run("times out naming why no container was found", func(t *testing.T) {
		resolve := func(config.DBType) (string, error) { return "", errors.New("postgres is not running on this node") }
		p := &Provisioner{ContainerFor: resolve, RC: &fakeCommander{}, PollInterval: time.Millisecond}
		err := p.WaitHealthy(ctx, config.DBPostgres, 5*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "not running on this node") {
			t.Errorf("want the resolver's reason in the timeout, got %v", err)
		}
	})

	t.Run("honours context cancellation", func(t *testing.T) {
		fc := &fakeCommander{outputData: map[string][]byte{inspect + "homelab-postgres": []byte("starting\n")}}
		p := &Provisioner{ContainerFor: fixedNames, RC: fc, PollInterval: time.Hour}
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.WaitHealthy(cctx, config.DBPostgres, time.Hour); !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("rejects unknown db type", func(t *testing.T) {
		p := &Provisioner{ContainerFor: fixedNames, RC: &fakeCommander{}, PollInterval: time.Millisecond}
		if err := p.WaitHealthy(ctx, config.DBType("mongo"), time.Second); err == nil {
			t.Fatal("expected an error for an unsupported engine")
		}
	})
}

// execSQL used to pick psql or mysql by looking for "postgres" in the
// container name; a Swarm task ID carries no such hint.
func TestProvisionPostgres_TaskIDContainerUsesPSQL(t *testing.T) {
	fc := &fakeCommander{}
	byID := func(config.DBType) (string, error) { return "3f9a1c", nil }
	p := &Provisioner{ContainerFor: byID, RC: fc, SM: &fakeSM{store: map[string]string{}}}
	decl := config.ServiceDBDecl{Database: "app", User: "app"}
	if err := p.Provision(context.Background(), config.DBPostgres, "app", decl); err != nil {
		t.Fatal(err)
	}
	var sawPSQL bool
	for _, c := range fc.runCalls {
		if strings.Contains(c.line(), "mysql") {
			t.Errorf("postgres provisioning ran mysql: %s", c.line())
		}
		sawPSQL = sawPSQL || strings.HasPrefix(c.line(), "exec -i 3f9a1c psql")
	}
	if !sawPSQL {
		t.Errorf("no psql exec into the task container: %v", fc.runCalls)
	}
}
