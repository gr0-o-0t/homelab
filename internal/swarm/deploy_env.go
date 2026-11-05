package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"

	"github.com/groot/homelab/internal/envshim"
)

// keepSet names the env secrets and configs the deployed specs reference;
// prune removes the service's others.
type keepSet struct {
	secrets, configs map[string]bool
}

// withEnvSecret puts a container's environment in a content-addressed Swarm
// secret and wraps its command in the env shim (R15). The image is
// inspected for the entrypoint/command the shim must run when compose sets
// none. A container without environment is left as it is: Swarm refuses an
// empty secret, and there is nothing to pass.
func (d *Deployer) withEnvSecret(ctx context.Context, svc string, spec *swarm.ServiceSpec, env map[string]string, keep keepSet) error {
	if len(env) == 0 {
		return nil
	}
	ref := spec.TaskTemplate.ContainerSpec.Image
	img, _, err := d.API.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", ref, err)
	}
	var entry, cmd []string
	if img.Config != nil {
		entry, cmd = img.Config.Entrypoint, img.Config.Cmd
	}
	// The shim needs something to exec; caught here rather than as a task
	// that dies at start.
	if cs := spec.TaskTemplate.ContainerSpec; len(cs.Command)+len(cs.Args)+len(entry)+len(cmd) == 0 {
		return fmt.Errorf("%s: image %s has no entrypoint or command and the compose file sets none", spec.Name, ref)
	}
	content := envshim.Encode(env)
	name := envshim.SecretName(spec.Name, content)
	id, err := d.ensureSecret(ctx, svc, name, content)
	if err != nil {
		return err
	}
	WithEnv(spec, name, entry, cmd)
	for _, r := range spec.TaskTemplate.ContainerSpec.Secrets {
		if r.SecretName == name {
			r.SecretID = id
		}
	}
	keep.secrets[name] = true
	return nil
}

func (d *Deployer) ensureSecret(ctx context.Context, svc, name string, data []byte) (string, error) {
	list, err := d.API.SecretList(ctx, swarm.SecretListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return "", err
	}
	for i := range list {
		if list[i].Spec.Name == name {
			return list[i].ID, nil
		}
	}
	resp, err := d.API.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{LabelService: svc}},
		Data:        data,
	})
	if err != nil {
		return "", fmt.Errorf("creating secret %s: %w", name, err)
	}
	return resp.ID, nil
}

// withConfigs turns the container's bind-mounted service files into Swarm
// configs, which reach whichever node runs the task. A bind of a directory
// becomes one config per file under it (empty files skipped: Swarm refuses
// empty configs); a bound single file must not be empty.
func (d *Deployer) withConfigs(ctx context.Context, svc string, spec *swarm.ServiceSpec, all []FileConfig, keep keepSet) error {
	for _, fc := range all {
		if fc.Service != spec.Name {
			continue
		}
		src := filepath.Join(d.serviceDir(svc), filepath.FromSlash(fc.Source))
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if info.Size() == 0 {
				return fmt.Errorf("%s: %s is empty; Swarm configs cannot be", spec.Name, fc.Source)
			}
			if err := d.addConfig(ctx, svc, spec, src, fc.Target, fc.Mode, keep); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
			if err != nil || !e.Type().IsRegular() {
				return err
			}
			if i, err := e.Info(); err != nil || i.Size() == 0 {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			return d.addConfig(ctx, svc, spec, p, path.Join(fc.Target, filepath.ToSlash(rel)), fc.Mode, keep)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Deployer) addConfig(ctx context.Context, svc string, spec *swarm.ServiceSpec, file, target string, mode os.FileMode, keep keepSet) error {
	data, err := os.ReadFile(file) // #nosec G304 -- a file under the service directory
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	name := "hl_" + svc + "_cfg_" + hex.EncodeToString(sum[:])[:12]
	id, err := d.ensureConfig(ctx, svc, name, data)
	if err != nil {
		return err
	}
	cs := spec.TaskTemplate.ContainerSpec
	cs.Configs = append(cs.Configs, &swarm.ConfigReference{
		File:       &swarm.ConfigReferenceFileTarget{Name: target, UID: "0", GID: "0", Mode: mode},
		ConfigID:   id,
		ConfigName: name,
	})
	keep.configs[name] = true
	return nil
}

func (d *Deployer) ensureConfig(ctx context.Context, svc, name string, data []byte) (string, error) {
	list, err := d.API.ConfigList(ctx, swarm.ConfigListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return "", err
	}
	for i := range list {
		if list[i].Spec.Name == name {
			return list[i].ID, nil
		}
	}
	resp, err := d.API.ConfigCreate(ctx, swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: name, Labels: map[string]string{LabelService: svc}},
		Data:        data,
	})
	if err != nil {
		return "", fmt.Errorf("creating config %s: %w", name, err)
	}
	return resp.ID, nil
}

// prune removes the service's secrets and configs not in keep. Run after
// the services were updated, so none of them still references one.
func (d *Deployer) prune(ctx context.Context, svc string, keep keepSet) error {
	var errs []error
	secs, err := d.API.SecretList(ctx, swarm.SecretListOptions{Filters: byService(svc)})
	if err != nil {
		return err
	}
	for i := range secs {
		s := &secs[i]
		if !keep.secrets[s.Spec.Name] {
			if err := d.API.SecretRemove(ctx, s.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing secret %s: %w", s.Spec.Name, err))
			}
		}
	}
	cfgs, err := d.API.ConfigList(ctx, swarm.ConfigListOptions{Filters: byService(svc)})
	if err != nil {
		return err
	}
	for i := range cfgs {
		c := &cfgs[i]
		if !keep.configs[c.Spec.Name] {
			if err := d.API.ConfigRemove(ctx, c.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing config %s: %w", c.Spec.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}
