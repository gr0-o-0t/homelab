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
	"time"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
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

// Up creates or updates every container of svc with one replica, its
// environment in a Swarm secret. Containers no longer in the compose file
// are removed, as are env secrets and configs nothing uses any more. Volumes
// are never touched: Swarm creates a missing named volume when a task first
// starts, on the (pinned) node it runs on.
//
// Up never upgrades an image (R53): an image already on this engine is used
// as it is (one that is missing is pulled), the registry is never asked for
// a newer digest, a service keeps the digest its last deploy pinned, and a
// new one is pinned to the local image's digest. Update is the deliberate
// upgrade.
//
// A locally built image (pull_policy: never) is never pulled, by either:
// one this engine lacks fails with ErrLocalImageMissing. Its identity is a
// container label, which Up keeps and Update refreshes, so a rebuilt image
// rolls the tasks on Update only.
func (d *Deployer) Up(ctx context.Context, svc string) error {
	return d.deploy(ctx, svc, false)
}

// Update pulls the images and redeploys, each tag resolved to the
// registry's current digest, so a newer image rolls the tasks.
func (d *Deployer) Update(ctx context.Context, svc string) error {
	return d.deploy(ctx, svc, true)
}

// deploy is Up, or with refresh Update.
func (d *Deployer) deploy(ctx context.Context, svc string, refresh bool) error {
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
		ref := spec.TaskTemplate.ContainerSpec.Image
		local := IsLocalImage(spec.TaskTemplate.ContainerSpec)
		switch {
		case local:
			err = d.withLocalImage(ctx, svc, spec.TaskTemplate.ContainerSpec)
		case refresh:
			err = d.pull(ctx, ref, pulled)
		default:
			err = d.ensureImage(ctx, ref, pulled)
		}
		if err != nil {
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

		// No registry has a local image: asking one only fails or warns.
		queryRegistry := refresh && !local
		if cur, ok := existing[spec.Name]; ok {
			delete(existing, spec.Name)
			err := d.update(ctx, cur.ID, swarm.ServiceUpdateOptions{QueryRegistry: queryRegistry},
				func(now *swarm.ServiceSpec) swarm.ServiceSpec {
					next := *spec
					next.TaskTemplate.ForceUpdate = now.TaskTemplate.ForceUpdate
					if !refresh && next.TaskTemplate.ContainerSpec != nil && now.TaskTemplate.ContainerSpec != nil {
						cs := *next.TaskTemplate.ContainerSpec
						cs.Image = keepPinned(cs.Image, now.TaskTemplate.ContainerSpec.Image)
						cs.Labels = keepIdentity(cs.Labels, now.TaskTemplate.ContainerSpec.Labels)
						next.TaskTemplate.ContainerSpec = &cs
					}
					return next
				})
			if err != nil {
				return fmt.Errorf("updating %s: %w", spec.Name, err)
			}
			continue
		}
		if !refresh && !local {
			spec.TaskTemplate.ContainerSpec.Image = d.pinLocal(ctx, spec.TaskTemplate.ContainerSpec.Image)
		}
		if _, err := d.API.ServiceCreate(ctx, *spec, swarm.ServiceCreateOptions{QueryRegistry: queryRegistry}); err != nil {
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

// Replicas is the replica count of each Swarm service of svc, by name (a
// service that is down has zeros, one never deployed an empty map).
func (d *Deployer) Replicas(ctx context.Context, svc string) (map[string]uint64, error) {
	if err := config.ValidServiceName(svc); err != nil {
		return nil, err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return nil, err
	}
	out := make(map[string]uint64, len(list))
	for i := range list {
		r := list[i].Spec.Mode.Replicated
		if r == nil {
			// Global (or job) mode has no count to set to zero: refused
			// here rather than as a stop that never finishes.
			return nil, fmt.Errorf("%s is not a replicated Swarm service, so it cannot be scaled to zero "+
				"to stop it — back it up with --live, or stop it by hand first", list[i].Spec.Name)
		}
		var n uint64
		if r.Replicas != nil {
			n = *r.Replicas
		}
		out[list[i].Spec.Name] = n
	}
	return out, nil
}

// Scale sets the replica count of each named Swarm service of svc and
// changes nothing else: no pull, no registry query, no re-translation of
// the config — the current spec with another count. Services not named are
// left alone. Backup and restore use it to stop a service and bring it back
// exactly as it was.
func (d *Deployer) Scale(ctx context.Context, svc string, replicas map[string]uint64) error {
	return d.eachService(ctx, svc, false, func(spec *swarm.ServiceSpec) {
		n, ok := replicas[spec.Name]
		if !ok || spec.Mode.Replicated == nil {
			return
		}
		spec.Mode.Replicated.Replicas = &n
	})
}

// stopPoll is how often WaitStopped looks at the tasks.
var stopPoll = 250 * time.Millisecond

// WaitStopped waits until no task of svc may still have a running container:
// Down only changes the spec, and Swarm stops the old tasks afterwards, each
// within its stop grace period. A backup waits for this before reading the
// service's volumes.
func (d *Deployer) WaitStopped(ctx context.Context, svc string, timeout time.Duration) error {
	if err := config.ValidServiceName(svc); err != nil {
		return err
	}
	list, err := d.services(ctx, svc)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		busy := ""
		for i := range list {
			tasks, err := d.API.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", list[i].ID))})
			if err != nil {
				return err
			}
			for j := range tasks {
				if !taskStopped(tasks[j].Status.State) {
					busy = list[i].Spec.Name
				}
			}
		}
		if busy == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still running %s after being scaled down", busy, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stopPoll):
		}
	}
}

// taskStopped reports whether a task in state s no longer has a running
// container.
func taskStopped(s swarm.TaskState) bool {
	switch s {
	case swarm.TaskStateShutdown, swarm.TaskStateComplete, swarm.TaskStateFailed,
		swarm.TaskStateRejected, swarm.TaskStateRemove, swarm.TaskStateOrphaned:
		return true
	}
	return false
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
// pull for themselves when the service is updated. A local image
// (pull_policy: never) is not pulled: its reference is returned in skipped.
func (d *Deployer) Pull(ctx context.Context, svc string) (skipped []string, err error) {
	if err := config.ValidServiceName(svc); err != nil {
		return nil, err
	}
	st, err := d.stack(svc)
	if err != nil {
		return nil, err
	}
	pulled := map[string]bool{}
	for i := range st.Services {
		cs := st.Services[i].TaskTemplate.ContainerSpec
		if IsLocalImage(cs) {
			skipped = append(skipped, cs.Image)
			continue
		}
		if err := d.pull(ctx, cs.Image, pulled); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
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

// Status lists the desired-running tasks of svc, by service name.
func (d *Deployer) Status(ctx context.Context, svc string) ([]TaskStatus, error) {
	tasks, err := d.Tasks(ctx, svc)
	if err != nil {
		return nil, err
	}
	out := make([]TaskStatus, 0, len(tasks))
	for i := range tasks {
		out = append(out, TaskStatus{Service: tasks[i].Service, Node: tasks[i].Node, State: tasks[i].State})
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

// ensureImage pulls ref only when this engine does not have it: up uses
// the image that is there, never a newer one (R53).
func (d *Deployer) ensureImage(ctx context.Context, ref string, done map[string]bool) error {
	if done[ref] {
		return nil
	}
	_, _, err := d.API.ImageInspectWithRaw(ctx, ref)
	switch {
	case err == nil:
		done[ref] = true
		return nil
	case errdefs.IsNotFound(err):
		return d.pull(ctx, ref, done)
	default:
		return fmt.Errorf("inspecting %s: %w", ref, err)
	}
}

// keepPinned keeps the digest the running service's image is pinned to when
// next asks for the same reference without one: up must not roll the tasks
// onto whatever each node's tag now points at.
func keepPinned(next, now string) string {
	if base, _, ok := strings.Cut(now, "@"); ok && base == next {
		return now
	}
	return next
}

// pinLocal pins a new service's image reference to the digest of the image
// this engine has for it, so its tasks run that image wherever and whenever
// they start (Swarm pulls a bare tag on every task start). A reference
// already pinned, or an image with no registry digest (built locally), is
// left as it is.
func (d *Deployer) pinLocal(ctx context.Context, ref string) string {
	if strings.Contains(ref, "@") {
		return ref
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ref
	}
	img, _, err := d.API.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return ref
	}
	for _, rd := range img.RepoDigests {
		r, err := reference.ParseNormalizedNamed(rd)
		if err != nil || r.Name() != named.Name() {
			continue
		}
		if c, ok := r.(reference.Canonical); ok {
			return ref + "@" + c.Digest().String()
		}
	}
	return ref
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
	// Closing the stream is what unblocks a read stuck on a silent registry,
	// whether or not the client's body honours ctx itself.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("pulling %s: %w", ref, err)
	}
	done[ref] = true
	return nil
}
