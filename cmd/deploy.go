package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docker/docker/client"

	"github.com/groot/homelab/internal/swarm"
)

// newSwarmAPI connects to the Swarm manager. Tests swap in swarmtest.Fake.
var newSwarmAPI = func() (swarm.API, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// withDeployer runs fn with a Deployer for the config dir: the per-service
// lifecycle verbs run on Swarm, not docker compose. needSecrets is set for
// verbs that (re)start containers: they refuse to run without the secrets
// manager rather than deploy a service missing its secrets (R23). Taking a
// service down or pulling its images only warns.
func withDeployer(root string, needSecrets bool, fn func(context.Context, *swarm.Deployer) error) error {
	// openSecrets (setup_noninteractive.go) is secrets.Open: the keyring, or
	// the vault when the agent sets HOMELAB_VAULT/HOMELAB_VAULT_KEY (R21).
	sm, err := openSecrets()
	if err != nil {
		if needSecrets {
			return fmt.Errorf("opening the secrets manager (keyring or vault): %w — not deploying without the service's secrets", err)
		}
		fmt.Fprintf(os.Stderr, "warning: keyring unavailable (%v)\n", err)
		sm = nil
	}
	api, err := newSwarmAPI()
	if err != nil {
		return fmt.Errorf("connecting to docker: %w", err)
	}
	if c, ok := api.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	d := &swarm.Deployer{API: api, Root: root, SM: sm, ConfigFile: rootFlags.configFile}
	return fn(cmdContext(), d)
}

// deploy runs a Deployer verb that only stops, removes or pulls.
func deploy(root, name string, verb func(*swarm.Deployer, context.Context, string) error) error {
	return withDeployer(root, false, func(ctx context.Context, d *swarm.Deployer) error {
		return verb(d, ctx, name)
	})
}

// deployRunning runs a Deployer verb that leaves containers running (up,
// update, restart): it needs the secrets manager.
func deployRunning(root, name string, verb func(*swarm.Deployer, context.Context, string) error) error {
	return withDeployer(root, true, func(ctx context.Context, d *swarm.Deployer) error {
		return verb(d, ctx, name)
	})
}
