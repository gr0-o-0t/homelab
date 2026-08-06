package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/groot/homelab/internal/config"
	"github.com/groot/homelab/internal/db"
	"github.com/groot/homelab/internal/run"
	"github.com/groot/homelab/internal/tui/styles"
)

// Shared-database dependency startup. A service that declares a shared
// postgres/mariadb needs that container up and provisioned before its own
// compose run, which is a different concern from running the service itself.

// sharedDBStartTimeout bounds how long we wait for an auto-started shared
// database to become healthy. Postgres recovery after an unclean shutdown is the
// slow case; a minute is generous without hanging a script forever.
const sharedDBStartTimeout = 90 * time.Second

// ensureDBDependencies makes sure every shared database a service declares is
// actually usable before that service starts.
//
// A declared dependency is a dependency: if the shared container is not running
// we start it and wait for it to report healthy, rather than failing and telling
// the user to run a command we could have run ourselves. Compose cannot express
// this — the shared databases live in their own compose projects, so
// `depends_on` cannot reach them.

// ensureDBDependencies makes sure every shared database a service declares is
// actually usable before that service starts.
//
// A declared dependency is a dependency: if the shared container is not running
// we start it and wait for it to report healthy, rather than failing and telling
// the user to run a command we could have run ourselves. Compose cannot express
// this — the shared databases live in their own compose projects, so
// `depends_on` cannot reach them.
func ensureDBDependencies(ctx context.Context, root, name string) error {
	svcCfg, err := config.Load(config.ServiceConfigFile(root, name))
	if err != nil {
		return err
	}
	if svcCfg == nil || svcCfg.Databases.Kind == 0 {
		return nil
	}

	svcDB, err := svcCfg.ServiceDatabases()
	if err != nil {
		return fmt.Errorf("reading database declarations: %w", err)
	}
	if len(svcDB) == 0 {
		return nil
	}

	// Sorted so the startup order (and any output) is deterministic.
	types := make([]config.DBType, 0, len(svcDB.DBTypeSet()))
	for dbType := range svcDB.DBTypeSet() {
		types = append(types, dbType)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	p := db.New(root, nil) // nil SM — status checks need no secrets
	for _, dbType := range types {
		if err := p.EnsureRunning(ctx, dbType); err == nil {
			continue
		}
		if err := startSharedDB(ctx, root, dbType, p); err != nil {
			return err
		}
	}
	return nil
}

// startSharedDB brings up the shared instance for dbType and waits for it to be
// healthy. Installation is left to the user: the shared databases need a root
// password in the keyring before they will initialise, so silently installing
// one would just produce a container that crash-loops.

// startSharedDB brings up the shared instance for dbType and waits for it to be
// healthy. Installation is left to the user: the shared databases need a root
// password in the keyring before they will initialise, so silently installing
// one would just produce a container that crash-loops.
func startSharedDB(ctx context.Context, root string, dbType config.DBType, p *db.Provisioner) error {
	shared := config.SharedDBName(dbType)
	composeFile := run.ServiceComposeFile(root, shared)
	if _, err := os.Stat(composeFile); err != nil {
		return fmt.Errorf("%s is required by this service but is not installed\n"+
			"  Install: homelab add %s && homelab setup %s", shared, shared, shared)
	}

	fmt.Printf("%s Starting %s (required by this service)…\n",
		styles.Primary.Render("→"), styles.Bold.Render(shared))

	if err := run.Default().DockerComposeEnv(
		composeFile,
		buildEnv(root, shared),
		"up", "-d",
	); err != nil {
		return fmt.Errorf("starting %s: %w", shared, err)
	}

	if err := p.WaitHealthy(ctx, dbType, sharedDBStartTimeout); err != nil {
		return err
	}
	fmt.Printf("  %s %s is ready\n", styles.Success.Render("✓"), shared)
	return nil
}
