# CLAUDE.md

## What this is

A self-hosted infrastructure stack on Docker Swarm, managed by a Go CLI (`homelab`). One `homelab agent` container runs on every PC. The agent on the Swarm manager (the **leader**, host `minis`) holds all state and runs the edge: an embedded Caddy (with caddy-l4) on its own Tailscale node (tsnet). Services are reachable only inside the tailnet at `https://<service>.home.<domain>`; TLS is a wildcard cert via Cloudflare DNS-01. The CLI on any PC forwards its argv to the leader agent over the tailnet (`:7443`, HMAC of the cluster secret).

## Building

```bash
make build           # default build, includes the desktop GUI (cgo + OpenGL/X11 headers)
make build-headless  # -tags nogui: pure Go; servers, the agent, cross-compiling
make test            # go test ./...
make lint            # golangci-lint (default + nogui build tags)
make ci              # lint + test-race + build
make catalog         # export assets/services/ -> services/ (gitignored, browsing only)
make image           # local agent image homelab:dev from the static headless binary
make release         # dist/: tarballs + SHA256SUMS + the image build context
go test ./internal/scaffold/...   # a single package
```

Test a local agent image with `init|join|upgrade --image homelab:dev`.

## Reference docs: read before changing the matching area

The detail that used to live in this file is in `docs/reference/`. Read only the section you need.

| Changing | Read |
|---|---|
| Any CLI command's behaviour or flags (`init`/`join`/`upgrade`, lifecycle verbs, `backup`/`restore`/`prune`, `enable`/`disable`, `setup`, `ext`, `cf`) | `docs/reference/cli.md` |
| Upgrade rollout, agent self-replacement, `EnsureImage` | `docs/reference/cli.md` § Cluster |
| Agent boot order, state dir layout, vault | `docs/reference/internals.md` § The agent, § State |
| Remote CLI protocol, locking, version checks | `docs/reference/internals.md` § Remote CLI |
| Compose → Swarm translation, placement, local images, `add <dir>` | `docs/reference/internals.md` § Services under Swarm |
| Caddy assembly, routing, exposure state, port declarations | `docs/reference/internals.md` § Edge, § Service Routing, § Embedded Catalog |
| Shared postgres/mariadb/redis/garage provisioning | `docs/reference/internals.md` § Shared services |
| Desired state, groups, the action registry | `docs/reference/internals.md` (same-named sections) |
| Full package table | `docs/reference/internals.md` § Go package layout |

Other docs: `docs/architecture.md` (design, network diagrams), `docs/adding-a-service.md`, `docs/migrating-to-swarm.md`, `docs/tailscale-setup.md`, `docs/cloudflare-setup.md`, `docs/superpowers/specs/` (design specs).

## Package map (short)

- `cmd/`: Cobra commands; forwarding to the agent (`remote.go`), exec/logs relay (`relay.go`), CLI self-update (`selfupdate.go`).
- `internal/agent`, `internal/bootstrap`: the agent process (leader or worker); `init`/`join`/`upgrade` and self-replacement.
- `internal/remote`: remote CLI protocol, client and server.
- `internal/swarm`: Compose → Swarm, Deployer, placement, rebalancer, cloudflared. `internal/envshim`: env through a Swarm secret.
- `internal/edge`, `internal/caddy`, `internal/configgen`, `internal/routing`, `internal/exposure`: the edge and how routes are rendered and loaded.
- `internal/config`, `internal/secrets`, `internal/vault`, `internal/desired`, `internal/agentstate`: config, secrets and state.
- `internal/db`, `internal/backup`: shared database provisioning; backup/restore.
- `internal/service`, `internal/docker` (read-only SDK), `internal/run` (docker CLI shell-outs), `internal/diagnostics`.
- `internal/actions`, `internal/clistate`, `internal/tui/*`, `internal/gui`: front ends.
- `assets/`: embedded Caddyfile global block and the service catalog.

## Rules that hold everywhere

- **Swarm only.** No Compose mode. Lifecycle goes through the Swarm API (`internal/swarm`); the Docker SDK is read-only; the docker CLI is only for DB dumps, volume helpers and `docker service logs`.
- **`minis` is the single manager.** All state, data volumes, host bind paths, the docker socket and shared databases pin to the core node.
- **No secrets on disk in plain text.** Secrets live in the agent's vault; a service's env reaches tasks as a Swarm secret via envshim, never in the spec, argv or labels. Secrets are not in backups.
- **`up`/`start`/`restart` never upgrade an image (R53).** Only `update` pulls newer digests.
- **Exposure is stored state** (`services/<svc>/exposure.yaml`), written only by `enable`/`disable`; nothing parses generated Caddy files back.
- **Services never bundle their own database, cache or object store**; they declare `databases:` and use the shared instance (a catalog test enforces this).
- **The catalog's only copy is `assets/services/`.** Root `services/` is a gitignored export.
- **Tests never reach the real Docker daemon or a tailnet.** Docker-touching packages take an interface with fakes (`swarm/swarmtest`); every test package that can shell out to docker needs a `TestMain` pointing `DOCKER_HOST` at a nonexistent socket.
- **Front ends run the CLI.** The TUI and GUI exec this binary for every action and read state via `status --json`; they never reimplement an action. A new CLI command or flag needs an entry in `internal/actions` (`cmd/actions_parity_test.go` fails otherwise).
- **Unsupported in services:** privileged containers, devices, `network_mode`, `env_file`.

## Adding a service

`homelab new myapp` → `homelab setup myapp` → `homelab up myapp` → `homelab enable myapp`, then add `assets/services/myapp/`. See `docs/adding-a-service.md`.
