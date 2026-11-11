package swarm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/swarm"
)

const (
	// CloudflaredImage is pinned to a concrete release (cloudflared prints
	// 2026.10.7 as its version; the tag was confirmed with `docker manifest
	// inspect`): the image carries no version label, and :latest would roll the
	// public tunnel on whichever node pulls next.
	CloudflaredImage = "cloudflare/cloudflared:2026.10.7"

	// CloudflaredService is the Swarm service name; its homelab.service label
	// is CloudflaredLabel.
	CloudflaredService = "homelab_cloudflared"
	CloudflaredLabel   = "cloudflared"

	// CloudflaredOrigin is where the tunnel's ingress (configured in the
	// Cloudflare dashboard) must point: the agent's plain-HTTP listener, which
	// answers on the overlay under the alias homelab-edge. Caddy's cf sites
	// listen on :80 inside the agent (R25).
	CloudflaredOrigin = "http://homelab-edge:80"
)

// EnsureCloudflared runs cloudflared as a core-pinned Swarm service on
// home-services. The token reaches it only through the env shim secret
// (TUNNEL_TOKEN), never the spec. Idempotent: the secret is content-addressed,
// so the same token and image leave the running service alone; a changed token
// rolls it. An empty token removes the service.
func (d *Deployer) EnsureCloudflared(ctx context.Context, token string) error {
	if token == "" {
		return d.RemoveCloudflared(ctx)
	}
	if err := d.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := d.ensureImage(ctx, CloudflaredImage, map[string]bool{}); err != nil {
		return err
	}
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name: CloudflaredService,
			Labels: map[string]string{
				LabelNamespace: CloudflaredLabel,
				LabelService:   CloudflaredLabel,
				LabelRole:      string(RoleCore),
			},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: CloudflaredImage,
				Args:  []string{"tunnel", "--no-autoupdate", "run"},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
			Networks:      []swarm.NetworkAttachmentConfig{{Target: SharedNetwork}},
			Placement:     &swarm.Placement{Constraints: Constraints(RoleCore, false)},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
	}
	keep := keepSet{secrets: map[string]bool{}, configs: map[string]bool{}}
	if err := d.withEnvSecret(ctx, CloudflaredLabel, &spec, map[string]string{"TUNNEL_TOKEN": token}, keep); err != nil {
		return err
	}

	list, err := d.services(ctx, CloudflaredLabel)
	if err != nil {
		return err
	}
	switch i := slices.IndexFunc(list, func(s swarm.Service) bool { return s.Spec.Name == CloudflaredService }); {
	case i < 0:
		if _, err := d.API.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{QueryRegistry: true}); err != nil {
			return fmt.Errorf("creating %s: %w", CloudflaredService, err)
		}
	case cloudflaredCurrent(&list[i].Spec, &spec):
		// Nothing changed; updating would roll the tunnel for no reason.
	default:
		err := d.update(ctx, list[i].ID, swarm.ServiceUpdateOptions{QueryRegistry: true},
			func(now *swarm.ServiceSpec) swarm.ServiceSpec {
				next := spec
				next.TaskTemplate.ForceUpdate = now.TaskTemplate.ForceUpdate
				return next
			})
		if err != nil {
			return fmt.Errorf("updating %s: %w", CloudflaredService, err)
		}
	}
	return d.prune(ctx, CloudflaredLabel, keep)
}

// cloudflaredCurrent reports whether the running spec already is the wanted
// one: same image (Swarm appends the resolved digest to it), same env secret
// (its name carries the token's hash), and at least one replica, so a service
// scaled down by hand is brought back.
func cloudflaredCurrent(have, want *swarm.ServiceSpec) bool {
	hc, wc := have.TaskTemplate.ContainerSpec, want.TaskTemplate.ContainerSpec
	if hc == nil || have.Mode.Replicated == nil || have.Mode.Replicated.Replicas == nil || *have.Mode.Replicated.Replicas == 0 {
		return false
	}
	img, _, _ := strings.Cut(hc.Image, "@")
	return img == wc.Image &&
		len(hc.Secrets) == 1 && len(wc.Secrets) == 1 && hc.Secrets[0].SecretName == wc.Secrets[0].SecretName
}

// RemoveCloudflared removes the service and its env secrets. Not being
// deployed is not an error.
func (d *Deployer) RemoveCloudflared(ctx context.Context) error {
	list, err := d.services(ctx, CloudflaredLabel)
	if err != nil {
		return err
	}
	for i := range list {
		if err := d.API.ServiceRemove(ctx, list[i].ID); err != nil {
			return fmt.Errorf("removing %s: %w", list[i].Spec.Name, err)
		}
	}
	return d.prune(ctx, CloudflaredLabel, keepSet{})
}
