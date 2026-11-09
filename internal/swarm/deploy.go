package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/jsonmessage"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/secrets"
)

// Deployer runs catalog services as Swarm services: the lifecycle verbs
// (up, down, restart, pull, update, delete) on the cluster.
type Deployer struct {
	API  API
	Root string // config dir; services live in <Root>/services/<svc>
	SM   *secrets.Manager
	// ConfigFile is the root config.yaml (--config); empty means
	// <Root>/config.yaml.
	ConfigFile string
}

// TaskStatus is one task of a service: which container, where, in what state.
type TaskStatus struct {
	Service, Node, State string
}

// LabelNodeRole is the node label placement constraints match on.
const LabelNodeRole = "homelab.role"

// ErrNoSecrets means a deploy was asked for without a secrets manager.
var ErrNoSecrets = errors.New("no secrets manager (keyring or vault unavailable): refusing to deploy without the service's secrets")

// stack builds the service's environment and translates its compose file.
// Every value BuildEnv took from the secrets manager is passed on, so
// Translate refuses a spec that would show one (R16).
func (d *Deployer) stack(svc string) (Stack, error) {
	env, keys, err := config.BuildEnvWithSecrets(config.RootConfigFile(d.Root, d.ConfigFile), d.Root, svc, d.SM)
	if err != nil {
		return Stack{}, fmt.Errorf("building environment: %w", err)
	}
	if env["HOME_SUBDOMAIN"] == "" {
		env["HOME_SUBDOMAIN"] = "home"
	}
	values := make([]string, 0, len(keys))
	for _, k := range keys {
		values = append(values, env[k])
	}
	svcCfg, err := config.Load(config.ServiceConfigFile(d.Root, svc))
	if err != nil {
		return Stack{}, err
	}
	_, dbProvider := config.IsSharedDBService(svc)
	return Translate(d.serviceDir(svc), env, values, svcCfg, dbProvider)
}

func (d *Deployer) serviceDir(svc string) string {
	return filepath.Join(d.Root, "services", svc)
}

func byService(svc string) filters.Args {
	return filters.NewArgs(filters.Arg("label", LabelService+"="+svc))
}

// services returns the Swarm services of one catalog service, by name.
func (d *Deployer) services(ctx context.Context, svc string) ([]swarm.Service, error) {
	return d.API.ServiceList(ctx, swarm.ServiceListOptions{Filters: byService(svc)})
}

// Up pulls the images and creates or updates every container of svc with
// one replica, its environment in a Swarm secret. Containers no longer in the
// compose file are removed, as are env secrets and configs nothing uses any
// more. Volumes are never touched: Swarm creates a missing named volume when
// a task first starts, on the (pinned) node it runs on.
func (d *Deployer) Up(ctx context.Context, svc string) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	if d.SM == nil {
		// Deploying anyway would rotate a working env secret to one
		// missing every secret (R23).
		return ErrNoSecrets
	}
	st, err := d.stack(svc)
	if err != nil {
		return err
	}
	if err := d.checkAliases(ctx, st); err != nil {
		return err
	}
	if err := d.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := d.ensureStackNetworks(ctx, st); err != nil {
		return err
	}
	workerReady, err := d.anyWorkerReady(ctx)
	if err != nil {
		return err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return err
	}
	existing := map[string]swarm.Service{}
	for i := range list {
		existing[list[i].Spec.Name] = list[i]
	}

	keep := keepSet{secrets: map[string]bool{}, configs: map[string]bool{}}
	pulled := map[string]bool{}
	for i := range st.Services {
		spec := &st.Services[i]
		if err := d.pull(ctx, spec.TaskTemplate.ContainerSpec.Image, pulled); err != nil {
			return err
		}
		if err := d.withEnvSecret(ctx, svc, spec, st.Env[spec.Name], keep); err != nil {
			return err
		}
		if err := d.withConfigs(ctx, svc, spec, st.Configs, keep); err != nil {
			return err
		}
		if c := Constraints(Role(spec.Labels[LabelRole]), workerReady); len(c) > 0 {
			spec.TaskTemplate.Placement = &swarm.Placement{Constraints: c}
		}
		one := uint64(1)
		spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}}

		if cur, ok := existing[spec.Name]; ok {
			delete(existing, spec.Name)
			err := d.update(ctx, cur.ID, swarm.ServiceUpdateOptions{QueryRegistry: true},
				func(now *swarm.ServiceSpec) swarm.ServiceSpec {
					next := *spec
					next.TaskTemplate.ForceUpdate = now.TaskTemplate.ForceUpdate
					return next
				})
			if err != nil {
				return fmt.Errorf("updating %s: %w", spec.Name, err)
			}
			continue
		}
		if _, err := d.API.ServiceCreate(ctx, *spec, swarm.ServiceCreateOptions{QueryRegistry: true}); err != nil {
			return fmt.Errorf("creating %s: %w", spec.Name, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(existing)) { // orphans
		if err := d.API.ServiceRemove(ctx, existing[name].ID); err != nil {
			return fmt.Errorf("removing %s: %w", name, err)
		}
	}
	return d.prune(ctx, svc, keep)
}

// Down scales every container of svc to zero. The specs, their secrets and
// the volumes stay, so Up brings the service back as it was. Swarm has no
// stopped-but-kept container, so stop is Down too.
func (d *Deployer) Down(ctx context.Context, svc string) error {
	return d.eachService(ctx, svc, false, func(spec *swarm.ServiceSpec) {
		zero := uint64(0)
		spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &zero}}
	})
}

// Restart replaces every task of svc with a fresh one. A service that was
// stopped (scaled to zero) comes back with one replica: restart means
// running afterwards.
func (d *Deployer) Restart(ctx context.Context, svc string) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	if d.SM == nil {
		return ErrNoSecrets
	}
	return d.eachService(ctx, svc, true, func(spec *swarm.ServiceSpec) {
		spec.TaskTemplate.ForceUpdate++
		if r := spec.Mode.Replicated; r != nil && (r.Replicas == nil || *r.Replicas == 0) {
			one := uint64(1)
			r.Replicas = &one
		}
	})
}

// eachService applies change to every service of svc and updates it.
func (d *Deployer) eachService(ctx context.Context, svc string, mustExist bool, change func(*swarm.ServiceSpec)) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return err
	}
	if len(list) == 0 && mustExist {
		return fmt.Errorf("%s is not deployed — run `homelab up %s`", svc, svc)
	}
	var errs []error
	for i := range list {
		err := d.update(ctx, list[i].ID, swarm.ServiceUpdateOptions{}, func(now *swarm.ServiceSpec) swarm.ServiceSpec {
			next := *now
			if r := now.Mode.Replicated; r != nil { // don't mutate the inspected spec
				rc := *r
				next.Mode.Replicated = &rc
			}
			change(&next)
			return next
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("updating %s: %w", list[i].Spec.Name, err))
		}
	}
	return errors.Join(errs...)
}

// update inspects the service for its current version right before
// updating it to build(current spec): a version listed before minutes of
// image pulls may be stale. An out-of-sequence rejection (someone updated
// it in between) is retried once from a fresh inspect.
func (d *Deployer) update(ctx context.Context, id string, opts swarm.ServiceUpdateOptions,
	build func(now *swarm.ServiceSpec) swarm.ServiceSpec,
) error {
	var err error
	for range 2 {
		cur, _, ierr := d.API.ServiceInspectWithRaw(ctx, id, swarm.ServiceInspectOptions{})
		if ierr != nil {
			return ierr
		}
		_, err = d.API.ServiceUpdate(ctx, cur.ID, cur.Version, build(&cur.Spec), opts)
		if err == nil || !strings.Contains(err.Error(), "out of sequence") {
			return err
		}
	}
	return err
}

// Pull pulls every image svc uses onto this node. Nodes running its tasks
// pull for themselves when the service is updated.
func (d *Deployer) Pull(ctx context.Context, svc string) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	st, err := d.stack(svc)
	if err != nil {
		return err
	}
	pulled := map[string]bool{}
	for i := range st.Services {
		if err := d.pull(ctx, st.Services[i].TaskTemplate.ContainerSpec.Image, pulled); err != nil {
			return err
		}
	}
	return nil
}

// Update pulls the images and redeploys: Up resolves each tag to its current
// digest, so a newer image rolls the tasks.
func (d *Deployer) Update(ctx context.Context, svc string) error {
	return d.Up(ctx, svc)
}

// Remove deletes svc's services, env secrets and configs. Volumes are kept.
func (d *Deployer) Remove(ctx context.Context, svc string) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return err
	}
	for i := range list {
		if err := d.API.ServiceRemove(ctx, list[i].ID); err != nil {
			return fmt.Errorf("removing %s: %w", list[i].Spec.Name, err)
		}
	}
	if err := d.prune(ctx, svc, keepSet{}); err != nil {
		return err
	}
	return d.removeStackNetworks(ctx, svc)
}

// Status lists the current tasks of svc, by service name.
func (d *Deployer) Status(ctx context.Context, svc string) ([]TaskStatus, error) {
	if err := config.ValidServiceName(svc); err != nil {
		return nil, err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return nil, err
	}
	nodes, err := d.API.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	host := map[string]string{}
	for i := range nodes {
		host[nodes[i].ID] = nodes[i].Description.Hostname
	}
	var out []TaskStatus
	for i := range list {
		s := &list[i]
		tasks, err := d.API.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(
			filters.Arg("service", s.ID), filters.Arg("desired-state", string(swarm.TaskStateRunning)))})
		if err != nil {
			return nil, err
		}
		for j := range tasks {
			t := &tasks[j]
			node := host[t.NodeID]
			if node == "" {
				node = t.NodeID
			}
			out = append(out, TaskStatus{Service: s.Spec.Name, Node: node, State: string(t.Status.State)})
		}
	}
	return out, nil
}

// anyWorkerReady reports whether a worker node can take tasks now.
func (d *Deployer) anyWorkerReady(ctx context.Context) (bool, error) {
	nodes, err := d.API.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return false, fmt.Errorf("listing nodes: %w", err)
	}
	return slices.ContainsFunc(nodes, func(n swarm.Node) bool {
		return n.Spec.Labels[LabelNodeRole] == string(RoleWorker) &&
			n.Status.State == swarm.NodeStateReady &&
			n.Spec.Availability == swarm.NodeAvailabilityActive
	}), nil
}

// pull pulls ref once per deploy, reading the progress stream to its end:
// a failed pull is reported in the stream, not as the call's error.
func (d *Deployer) pull(ctx context.Context, ref string, done map[string]bool) error {
	if done[ref] {
		return nil
	}
	rc, err := d.API.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	done[ref] = true
	return nil
}
