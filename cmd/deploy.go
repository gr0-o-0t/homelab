package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docker/docker/client"

	"github.com/groot/homelab/internal/secrets"
	"github.com/groot/homelab/internal/swarm"
)

// newSwarmAPI connects to the Swarm manager. Tests swap in swarmtest.Fake.
var newSwarmAPI = func() (swarm.API, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// withDeployer runs fn with a Deployer for the config dir: the per-service
// lifecycle verbs run on Swarm, not docker compose.
func withDeployer(root string, fn func(context.Context, *swarm.Deployer) error) error {
	api, err := newSwarmAPI()
	if err != nil {
		return fmt.Errorf("connecting to docker: %w", err)
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	// Same tolerance as buildEnv: without a keyring the service still
	// deploys, just without its secrets.
	sm, err := secrets.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keyring unavailable (%v)\n", err)
		sm = nil
	}
	d := &swarm.Deployer{API: api, Root: root, SM: sm, ConfigFile: rootFlags.configFile}
	return fn(cmdContext(), d)
}

// deploy runs one Deployer verb for a service.
func deploy(root, name string, verb func(*swarm.Deployer, context.Context, string) error) error {
	return withDeployer(root, func(ctx context.Context, d *swarm.Deployer) error {
		return verb(d, ctx, name)
	})
}
