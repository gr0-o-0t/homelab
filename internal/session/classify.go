package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/groot/homelab/internal/docker"
	"gopkg.in/yaml.v3"
)

// Project is a homelab compose project: the core stack or one service.
type Project struct {
	Core bool
	Name string // the service; "" for the core stack
}

func (p Project) String() string {
	if p.Core {
		return "core"
	}
	return p.Name
}

// Scope recognises the containers homelab manages: those whose compose
// working directory is <config-dir>/core or <config-dir>/services/<name>.
// Containers of other compose projects on the same daemon are never touched.
type Scope struct {
	roots []string // the config dir, cleaned, plus its symlink-resolved form
}

// NewScope builds the Scope for a config dir.
func NewScope(root string) Scope {
	var roots []string
	add := func(p string) {
		p = filepath.Clean(p)
		for _, r := range roots {
			if r == p {
				return
			}
		}
		roots = append(roots, p)
	}
	if abs, err := filepath.Abs(root); err == nil {
		add(abs)
	} else {
		add(root)
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			add(abs)
		}
	}
	return Scope{roots: roots}
}

// Classify returns the project a container (by its labels) belongs to. ok is
// false for containers outside the config dir and for one-off `compose run`
// containers, which nothing should start or restart.
func (s Scope) Classify(labels map[string]string) (p Project, ok bool) {
	if strings.EqualFold(labels[docker.LabelOneOff], "true") {
		return Project{}, false
	}
	wd := labels[docker.LabelWorkingDir]
	if wd == "" {
		return Project{}, false
	}
	wd = filepath.Clean(wd)
	for _, r := range s.roots {
		rel, err := filepath.Rel(r, wd)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case len(parts) == 1 && parts[0] == "core":
			return Project{Core: true}, true
		case len(parts) == 2 && parts[0] == "services" && parts[1] != "" && parts[1] != "..":
			return Project{Name: parts[1]}, true
		}
	}
	return Project{}, false
}

// Managed filters cs to the containers homelab manages.
func (s Scope) Managed(cs []docker.ContainerInfo) []docker.ContainerInfo {
	var out []docker.ContainerInfo
	for i := range cs {
		if _, ok := s.Classify(cs[i].Labels); ok {
			out = append(out, cs[i])
		}
	}
	return out
}

// Docker's names for a running container and the restart policy compose
// services usually declare.
const (
	stateRunning = "running"
	policyAlways = "always"
)

// BootStarting reports whether Docker would start a container with this
// restart policy by itself when the daemon starts.
func BootStarting(policy string) bool {
	switch policy {
	case "", "no":
		return false
	}
	return true
}

// ShouldRestart reports whether a container that died on its own should be
// started again, given the restart policy its compose file declared: the
// supervisor does what Docker would have done.
func ShouldRestart(original, exitCode string) bool {
	switch {
	case original == policyAlways, original == "unless-stopped":
		return true
	case strings.HasPrefix(original, "on-failure"):
		return exitCode != "" && exitCode != "0"
	}
	return false
}

// NeedsRebind reports whether a running container must be restarted because
// it was started before the session began and bind-mounts something under
// home: its mounts may be the placeholder directories Docker created while
// home was not mounted yet.
func NeedsRebind(c docker.ContainerInfo, cutoff time.Time, home string) bool {
	if c.State != stateRunning || c.StartedAt.IsZero() || !c.StartedAt.Before(cutoff) || home == "" {
		return false
	}
	home = filepath.Clean(home)
	for _, src := range c.BindSources {
		src = filepath.Clean(src)
		if src == home || strings.HasPrefix(src, home+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Bootstrap infers a desired state from the containers running now: a
// project with a running container should be running, everything else not.
// Used once, when there is no desired.yaml yet.
func Bootstrap(s Scope, cs []docker.ContainerInfo) *Desired {
	d := &Desired{Core: Stopped, Services: map[string]State{}}
	for i := range cs {
		p, ok := s.Classify(cs[i].Labels)
		if !ok || cs[i].State != stateRunning {
			continue
		}
		if p.Core {
			d.Core = Running
		} else {
			d.Services[p.Name] = Running
		}
	}
	return d
}

// ShutdownPlan splits the running managed containers into the services' and
// the core's: services stop first so nothing loses its proxy or database
// while still serving.
func ShutdownPlan(s Scope, cs []docker.ContainerInfo) (services, core []string) {
	var shared []string
	for i := range cs {
		p, ok := s.Classify(cs[i].Labels)
		if !ok || cs[i].State != stateRunning {
			continue
		}
		switch {
		case p.Core:
			core = append(core, cs[i].Name)
		case SharedServices[p.Name]:
			shared = append(shared, cs[i].Name)
		default:
			services = append(services, cs[i].Name)
		}
	}
	// Shared databases go after the services using them.
	return append(services, shared...), core
}

// ── restart policy record ────────────────────────────────────────────────────

// PolicyFile is <config-dir>/state/restart-policies.yaml: the restart policy
// each container had before session mode set it to "no", so the supervisor
// can honour it and `session uninstall` can put it back.
func PolicyFile(root string) string { return filepath.Join(StateDir(root), "restart-policies.yaml") }

// Policies maps container name → its compose-declared restart policy.
type Policies map[string]string

// LoadPolicies reads the record; a missing file is an empty record.
func LoadPolicies(root string) (Policies, error) {
	p := Policies{}
	data, err := os.ReadFile(PolicyFile(root))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var f struct {
		Containers Policies `yaml:"containers"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return p, err
	}
	if f.Containers != nil {
		p = f.Containers
	}
	return p, nil
}

// Save writes the record atomically.
func (p Policies) Save(root string) error {
	if err := os.MkdirAll(StateDir(root), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(struct {
		Containers Policies `yaml:"containers"`
	}{p})
	if err != nil {
		return err
	}
	header := "# Written by homelab session: each container's restart policy before session\n" +
		"# mode set it to \"no\". `homelab session uninstall` puts these back.\n"
	return writeAtomic(PolicyFile(root), append([]byte(header), data...))
}

// Observe notes a container's current policy and reports whether it must be
// set to "no". A boot-starting policy is the compose-declared one (compose
// just created the container, or session mode was never on) and replaces
// the record; a "no" with no record was "no" all along.
func (p Policies) Observe(name, current string) (fix bool) {
	if BootStarting(current) {
		p[name] = current
		return true
	}
	if _, ok := p[name]; !ok {
		p[name] = "no"
	}
	return false
}

// Original is the compose-declared policy of a container ("no" if unknown).
func (p Policies) Original(name string) string {
	if v, ok := p[name]; ok && v != "" {
		return v
	}
	return "no"
}
