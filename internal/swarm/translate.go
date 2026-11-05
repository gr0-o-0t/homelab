package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/config"
)

// ErrUnsupported marks a compose feature Swarm services cannot run
// (privileged containers, host devices, network_mode, build).
var ErrUnsupported = errors.New("not supported on swarm")

// ErrSecretInSpec means a secret value would be visible in a service spec.
var ErrSecretInSpec = errors.New("secret value in service spec: pass it through environment: instead")

// minSecretLen keeps trivially short values (a "1", "no") from matching
// unrelated spec text.
const minSecretLen = 4

// checkNoSecrets refuses a spec whose JSON contains any secret value, raw or
// JSON-escaped. The error never includes the value.
func checkNoSecrets(spec swarm.ServiceSpec, secretValues []string) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	for _, v := range secretValues {
		if len(v) < minSecretLen {
			continue
		}
		esc, _ := json.Marshal(v)
		if bytes.Contains(data, []byte(v)) || bytes.Contains(data, esc[1:len(esc)-1]) {
			return ErrSecretInSpec
		}
	}
	return nil
}

// environment resolves a container's compose `environment:`. Entries
// without a value (`- NAME`) take it from env, or are left out.
func environment(svc types.ServiceConfig, env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range svc.Environment {
		if v != nil {
			out[k] = *v
		} else if e, ok := env[k]; ok {
			out[k] = e
		}
	}
	return out
}

// Label keys every translated service carries.
const (
	LabelNamespace = "com.docker.stack.namespace"
	LabelService   = "homelab.service"
	LabelRole      = "homelab.role"
)

// Stack is one catalog service translated into Swarm objects. Only Env may
// hold secrets; the specs never do.
type Stack struct {
	Name     string
	Services []swarm.ServiceSpec
	// Volumes are the named volumes the services mount, to ensure exist.
	Volumes []string
	// Configs are bind mounts of files or directories under the service
	// directory; the deployer turns them into Swarm configs.
	Configs []FileConfig
	// Networks are the stack's own overlays (not home-services).
	Networks []NetworkSpec
	// Env is each container's resolved compose `environment:`, keyed by
	// service spec name. It may hold secrets; Task 7 passes it through a
	// Swarm secret, never the spec.
	Env map[string]map[string]string
}

// FileConfig is a bind mount of a path under the service directory.
type FileConfig struct {
	Service string // service spec name, <svc>_<compose key>
	Target  string
	Source  string // path relative to the service directory; may be a directory
	Mode    os.FileMode
}

// Translate loads <svcDir>/docker-compose.yml, interpolating env, and turns
// each compose service into a Swarm service spec named <svc>_<compose key>.
// env is used for ${VAR} interpolation and to resolve `environment:`, which
// lands in Stack.Env, never in a spec. A spec containing any of secretValues
// (those of 4+ bytes) is refused with ErrSecretInSpec: a secret interpolated
// into command, labels or a healthcheck would show in `docker service
// inspect`.
func Translate(svcDir string, env map[string]string, secretValues []string, cfg *config.Config, dbProvider bool) (Stack, error) {
	abs, err := filepath.Abs(svcDir)
	if err != nil {
		return Stack{}, err
	}
	name := filepath.Base(abs)
	proj, err := loadCompose(abs, name, env)
	if err != nil {
		return Stack{}, err
	}

	nets := planNetworks(name, proj)
	st := Stack{Name: name, Networks: nets.specs, Env: map[string]map[string]string{}}
	volumes := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(proj.Services)) {
		svc := proj.Services[key]
		spec, cfgs, vols, err := translateService(name, abs, proj, svc, nets, cfg, dbProvider)
		if err != nil {
			return Stack{}, err
		}
		if err := checkNoSecrets(spec, secretValues); err != nil {
			return Stack{}, fmt.Errorf("%s/%s: %w", name, key, err)
		}
		if e := environment(svc, env); len(e) > 0 {
			st.Env[spec.Name] = e
		}
		st.Services = append(st.Services, spec)
		st.Configs = append(st.Configs, cfgs...)
		for _, v := range vols {
			volumes[v] = true
		}
	}
	st.Volumes = slices.Sorted(maps.Keys(volumes))
	return st, nil
}

func loadCompose(dir, name string, env map[string]string) (*types.Project, error) {
	file := filepath.Join(dir, "docker-compose.yml")
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	proj, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: file, Content: data}},
		Environment: types.Mapping(env),
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.ResolvePaths = true
		o.SkipResolveEnvironment = true // env goes through a secret, not the spec
	})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", file, err)
	}
	return proj, nil
}

func unsupported(stack, key, what string) error {
	return fmt.Errorf("%s/%s: %s %w", stack, key, what, ErrUnsupported)
}

func translateService(stack, dir string, proj *types.Project, svc types.ServiceConfig,
	nets networkPlan, cfg *config.Config, dbProvider bool,
) (swarm.ServiceSpec, []FileConfig, []string, error) {
	key := svc.Name
	switch {
	case svc.Privileged:
		return swarm.ServiceSpec{}, nil, nil, unsupported(stack, key, "privileged")
	case len(svc.Devices) > 0:
		return swarm.ServiceSpec{}, nil, nil, unsupported(stack, key, "devices")
	case svc.NetworkMode != "":
		return swarm.ServiceSpec{}, nil, nil, unsupported(stack, key, "network_mode")
	case svc.Image == "":
		return swarm.ServiceSpec{}, nil, nil, unsupported(stack, key, "build without image")
	case len(svc.EnvFiles) > 0:
		return swarm.ServiceSpec{}, nil, nil, unsupported(stack, key, "env_file")
	}

	specName := stack + "_" + key
	mounts, cfgs, vols, err := translateMounts(stack, dir, specName, proj, svc)
	if err != nil {
		return swarm.ServiceSpec{}, nil, nil, err
	}

	cs := &swarm.ContainerSpec{
		Image:           svc.Image,
		Labels:          svc.Labels,
		Command:         svc.Entrypoint,
		Args:            svc.Command,
		Hostname:        svc.Hostname,
		Dir:             svc.WorkingDir,
		User:            svc.User,
		Groups:          svc.GroupAdd,
		TTY:             svc.Tty,
		OpenStdin:       svc.StdinOpen,
		ReadOnly:        svc.ReadOnly,
		Mounts:          mounts,
		StopSignal:      svc.StopSignal,
		Healthcheck:     healthcheck(svc.HealthCheck),
		Hosts:           hosts(svc.ExtraHosts),
		Init:            svc.Init,
		Sysctls:         svc.Sysctls,
		CapabilityAdd:   svc.CapAdd,
		CapabilityDrop:  svc.CapDrop,
		Ulimits:         ulimits(svc.Ulimits),
		StopGracePeriod: (*time.Duration)(svc.StopGracePeriod),
	}
	if slices.ContainsFunc(svc.SecurityOpt, isNoNewPrivileges) {
		cs.Privileges = &swarm.Privileges{NoNewPrivileges: true}
	}
	rp, err := restartPolicy(svc.Restart)
	if err != nil {
		return swarm.ServiceSpec{}, nil, nil, fmt.Errorf("%s/%s: %w", stack, key, err)
	}

	role := Place(svc, cfg, dbProvider)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: specName,
			Labels: map[string]string{
				LabelNamespace: stack,
				LabelService:   stack,
				LabelRole:      string(role),
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: cs,
			RestartPolicy: rp,
			Networks:      nets.attachments(svc),
		},
	}
	return spec, cfgs, vols, nil
}

func isNoNewPrivileges(opt string) bool {
	opt = strings.ReplaceAll(opt, "=", ":")
	return opt == "no-new-privileges" || opt == "no-new-privileges:true"
}

// restartPolicy maps compose's restart: onto Swarm's. Unset means "no", as
// in compose.
func restartPolicy(restart string) (*swarm.RestartPolicy, error) {
	policy, arg, _ := strings.Cut(restart, ":")
	switch policy {
	case types.RestartPolicyAlways, types.RestartPolicyUnlessStopped:
		return &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny}, nil
	case types.RestartPolicyOnFailure:
		rp := &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionOnFailure}
		if arg != "" {
			n, err := strconv.ParseUint(arg, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("restart %q: %w", restart, err)
			}
			rp.MaxAttempts = &n
		}
		return rp, nil
	case "", types.RestartPolicyNo:
		return &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone}, nil
	}
	return nil, fmt.Errorf("unknown restart policy %q", restart)
}

func healthcheck(h *types.HealthCheckConfig) *container.HealthConfig {
	if h == nil {
		return nil
	}
	if h.Disable {
		return &container.HealthConfig{Test: []string{"NONE"}}
	}
	hc := &container.HealthConfig{Test: h.Test}
	if h.Interval != nil {
		hc.Interval = time.Duration(*h.Interval)
	}
	if h.Timeout != nil {
		hc.Timeout = time.Duration(*h.Timeout)
	}
	if h.StartPeriod != nil {
		hc.StartPeriod = time.Duration(*h.StartPeriod)
	}
	if h.StartInterval != nil {
		hc.StartInterval = time.Duration(*h.StartInterval)
	}
	if h.Retries != nil {
		hc.Retries = int(*h.Retries)
	}
	return hc
}

// hosts renders extra_hosts in Swarm's "IP host..." form.
func hosts(h types.HostsList) []string {
	var out []string
	for _, host := range slices.Sorted(maps.Keys(h)) {
		for _, ip := range h[host] {
			out = append(out, ip+" "+host)
		}
	}
	return out
}

func ulimits(u map[string]*types.UlimitsConfig) []*container.Ulimit {
	var out []*container.Ulimit
	for _, name := range slices.Sorted(maps.Keys(u)) {
		l := u[name]
		soft, hard := l.Soft, l.Hard
		if l.Single != 0 {
			soft, hard = l.Single, l.Single
		}
		out = append(out, &container.Ulimit{Name: name, Soft: int64(soft), Hard: int64(hard)})
	}
	return out
}
