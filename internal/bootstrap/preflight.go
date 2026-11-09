package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/jsonmessage"

	"github.com/groot/homelab/internal/swarm"
)

// ResolveImage is the agent image: flag when given, else the release image
// of version. A development build (dev, or a Go pseudo-version) has no
// published image, so it needs --image.
func ResolveImage(flag, version string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if version == "" || version == "dev" || strings.HasPrefix(version, "v0.0.0-") {
		return "", fmt.Errorf("this homelab is a development build (version %q): no agent image is published for it — "+
			"pass --image <ref> (e.g. a locally built image)", version)
	}
	return DefaultImage(version), nil
}

// ValidateEndpoint refuses an agent endpoint that is not an https:// URL with
// a host: the cluster secret's token must never travel in the clear.
func ValidateEndpoint(endpoint string) error {
	if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("agent endpoint %q must be an https:// URL", endpoint)
	}
	return nil
}

// EnsureImage makes img available on the engine, pulling it when missing.
// The pull's progress stream is decoded, so a failure inside it (auth, not
// found, rate limit) is an error rather than a silently empty pull.
func EnsureImage(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	if _, _, err := api.ImageInspectWithRaw(ctx, img); err == nil {
		return nil
	}
	_, _ = fmt.Fprintf(out, "Pulling %s …\n", img)
	rc, err := api.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", img, err)
	}
	defer func() { _ = rc.Close() }()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("pulling %s: %w", img, err)
	}
	return nil
}

// checkEngine refuses, before anything is asked or changed, an engine
// already in a swarm or already running an agent container.
func checkEngine(ctx context.Context, api swarm.API) error {
	info, err := api.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking Docker for its swarm state: %w", err)
	}
	switch info.Swarm.LocalNodeState {
	case "", dswarm.LocalNodeStateInactive:
	default:
		return fmt.Errorf("this Docker engine is already in a swarm (state %q) — to start over, run "+
			"`docker swarm leave --force` (on a manager this destroys that swarm), then retry", info.Swarm.LocalNodeState)
	}
	list, err := api.ContainerList(ctx, container.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("name", AgentContainer))})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	for i := range list {
		for _, n := range list[i].Names {
			if strings.TrimPrefix(n, "/") == AgentContainer {
				return fmt.Errorf("a %s container already exists — remove it (`docker rm -f %s`) and retry",
					AgentContainer, AgentContainer)
			}
		}
	}
	return nil
}

// preflight is checkEngine, then the agent image made available: init and
// join only mutate the swarm once it passes.
func preflight(ctx context.Context, api swarm.API, img string, out io.Writer) error {
	if err := checkEngine(ctx, api); err != nil {
		return err
	}
	return EnsureImage(ctx, api, img, out)
}

// leaveOnError takes the engine back out of the swarm when a later step of
// init or join failed, so a retry starts clean. The original error is kept.
func leaveOnError(ctx context.Context, api swarm.API, out io.Writer, err *error) {
	if *err == nil {
		return
	}
	if lerr := api.SwarmLeave(context.WithoutCancel(ctx), true); lerr != nil {
		_, _ = fmt.Fprintf(out, "warning: could not leave the half-set-up swarm (%v) — run `docker swarm leave --force`\n", lerr)
		return
	}
	_, _ = fmt.Fprintln(out, "Left the swarm again so a retry starts clean.")
}
