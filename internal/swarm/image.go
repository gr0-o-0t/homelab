package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/swarm"
)

// LabelImageIdentity is the container label holding the ImageIdentity of a
// local image a service was deployed with. A rebuilt image under the same
// tag changes it, and a changed container label is what rolls the tasks.
const LabelImageIdentity = "homelab.image-identity"

// ErrLocalImageMissing means a service's image is built locally
// (pull_policy: never) and this engine does not have it: no registry can
// supply it, so it must be shipped from the PC that built it.
var ErrLocalImageMissing = errors.New("local image missing")

// localImageMissingError names the image and the service to ship it with.
type localImageMissingError struct{ ref, svc string }

func (e localImageMissingError) Error() string {
	return fmt.Sprintf("image %s is local: run `homelab up|update %s` from the PC that built it", e.ref, e.svc)
}

func (localImageMissingError) Is(target error) bool { return target == ErrLocalImageMissing }

// ImageIdentity names an image by its content: its layers and creation
// time, hashed. Unlike the image ID it is the same on an engine using the
// containerd image store and one using overlay2, so it says whether two
// engines hold the same image. It is "" for an image with no layers.
func ImageIdentity(img image.InspectResponse) string {
	if len(img.RootFS.Layers) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(img.RootFS.Layers, "\n") + "\n" + img.Created))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// withLocalImage checks that this engine has cs's local image, which is
// never pulled, and labels cs with the image's identity.
func (d *Deployer) withLocalImage(ctx context.Context, svc string, cs *swarm.ContainerSpec) error {
	img, _, err := d.API.ImageInspectWithRaw(ctx, cs.Image)
	switch {
	case errdefs.IsNotFound(err):
		return localImageMissingError{ref: cs.Image, svc: svc}
	case err != nil:
		return fmt.Errorf("inspecting %s: %w", cs.Image, err)
	}
	cs.Labels[LabelImageIdentity] = ImageIdentity(img)
	return nil
}

// keepIdentity keeps the image identity the running service was deployed
// with, as keepPinned keeps its digest: up must not roll the tasks onto a
// local image rebuilt since (R53). The labels are copied, not changed.
func keepIdentity(next, now map[string]string) map[string]string {
	id, ok := now[LabelImageIdentity]
	if !ok || next[LabelImageIdentity] == "" {
		return next
	}
	out := maps.Clone(next)
	out[LabelImageIdentity] = id
	return out
}
