// Package session runs the homelab stack as part of the user's login session
// rather than from Docker's restart policies.
//
// Docker starts `restart: always` containers at boot, before the user logs in.
// When the home directory is encrypted (ecryptfs, fscrypt) or otherwise
// mounted late, every bind mount into the config dir then points at an empty
// placeholder Docker created in the unmounted directory: single-file mounts
// fail ("mounting a directory onto a file"), directory mounts silently serve
// nothing. Session mode turns the restart policies off and lets a systemd user
// service — started at login, stopped at logout — bring the stack up from a
// recorded desired state and restart crashed containers itself.
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// State is what a lifecycle command asks of a target.
type State string

const (
	Running State = "running"
	Stopped State = "stopped"
	// Removed drops the target from the file: it is not installed any more.
	Removed State = "removed"
)

// Desired is what should be running: the file the lifecycle commands write
// and the supervisor restores at login.
type Desired struct {
	Core     State            `yaml:"core,omitempty"`
	Services map[string]State `yaml:"services,omitempty"`
}

// StateDir is <config-dir>/state.
func StateDir(root string) string { return filepath.Join(root, "state") }

// DesiredFile is <config-dir>/state/desired.yaml.
func DesiredFile(root string) string { return filepath.Join(StateDir(root), "desired.yaml") }

// LoadDesired reads the desired state. exists is false when there is no file
// yet; d is then empty, never nil.
func LoadDesired(root string) (d *Desired, exists bool, err error) {
	d = &Desired{Services: map[string]State{}}
	data, err := os.ReadFile(DesiredFile(root))
	if errors.Is(err, os.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if err := yaml.Unmarshal(data, d); err != nil {
		return d, true, fmt.Errorf("%s: %w", DesiredFile(root), err)
	}
	if d.Services == nil {
		d.Services = map[string]State{}
	}
	return d, true, nil
}

const desiredHeader = `# Written by homelab: what should be running. Lifecycle commands (up, start,
# restart, update → running; stop, down → stopped; delete, prune → removed)
# update it, and the session supervisor (homelab session) restores it at login.
`

// Save writes the file atomically (temp file + rename): the supervisor reads it
// on every container event.
func (d *Desired) Save(root string) error {
	if err := os.MkdirAll(StateDir(root), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	return writeAtomic(DesiredFile(root), append([]byte(desiredHeader), data...))
}

// SetCore records the core stack's desired state.
func (d *Desired) SetCore(s State) {
	if s == Removed {
		s = Stopped
	}
	d.Core = s
}

// SetService records one service's desired state; Removed forgets it.
func (d *Desired) SetService(name string, s State) {
	if d.Services == nil {
		d.Services = map[string]State{}
	}
	if s == Removed {
		delete(d.Services, name)
		return
	}
	d.Services[name] = s
}

// CoreRunning reports whether the core stack should be up.
func (d *Desired) CoreRunning() bool { return d.Core == Running }

// ServiceRunning reports whether a service should be up. A service the file
// does not mention should not.
func (d *Desired) ServiceRunning(name string) bool { return d.Services[name] == Running }

// RunningServices lists the services that should be up, shared database
// services first (everything else may depend on them), then by name.
func (d *Desired) RunningServices() []string {
	var out []string
	for n, s := range d.Services {
		if s == Running {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := SharedServices[out[i]], SharedServices[out[j]]
		if si != sj {
			return si
		}
		return out[i] < out[j]
	})
	return out
}

// SharedServices are the shared database / object store services other
// services depend on.
var SharedServices = map[string]bool{"postgres": true, "mariadb": true, "redis": true, "garage": true}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
