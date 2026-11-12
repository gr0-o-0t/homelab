# Changelog

Notable changes per release. The release workflow publishes the section
matching the pushed tag as the GitHub release notes.

## v0.5.0-rc.3 — 2026-11-12

Release candidate. Run `homelab upgrade` from a PC running this CLI (CLI and
agent must share major.minor).

### Added

- Custom services from a folder: `homelab add <dir>` installs one from a folder
  on the PC (refuses an existing service, symlinks, dotfiles, files over
  500 KiB, folders over 10 MiB, and the shared provider names).
- Locally built images: a compose service with `pull_policy: never` is never
  pulled, runs on the core node by default, and `homelab up`/`update` from the PC
  that built the image ship it to the leader (`up` only when the leader lacks it,
  `update` when it differs). `update` rolls the tasks onto a rebuilt image under
  the same tag. `homelab pull` says "skipped (local image)". Use an
  unqualified image name; a manual `docker build` on the leader goes live at
  the next task restart.
- `homelab edit <svc> <path> --from <file>` updates a file without an editor.
- `homelab images <svc>... --json` (also `-a`/`--group`).
- `readonly: true` on a postgres `databases:` entry: a read-only role on another
  service's database, with the consumer's generated password.

### Changed

- `homelab join` removes an idle Compose-era `home-services` bridge network
  (it refuses one with containers); `init` does the same without `--import`.

### Fixed

- After `join` on a PC that had run the v0.4.x Compose stack, tasks scheduled
  there attached to its leftover `home-services` bridge instead of the overlay
  and were rejected (immich-machine-learning never ran on the laptop).

### Build

- `make lint` runs golangci-lint (default and `nogui` builds); `make ci` uses it.
- CI no longer runs on tag pushes (it ran once on main already).

## v0.5.0-rc.2 — 2026-11-11

Release candidate. Upgrade an rc.1 cluster with `homelab upgrade` from a PC
running this CLI; a v0.4.x install follows `docs/migrating-to-swarm.md` as for
rc.1.

### Fixed

- A healthcheck that needs the service's secret environment never passed: the
  environment reaches only the main process (through the env shim, never the
  service spec), and Docker runs healthchecks with the spec's. The shared
  redis (`redis-cli ping` without its password) was restarted every minute,
  so immich and searxng could not start. Healthchecks now run through the
  shim too; `homelab up <svc>` redeploys a running service with the fixed
  check.

## v0.5.0-rc.1 — 2026-11-11

Release candidate. The homelab moves from one Docker Compose host to a Docker
Swarm cluster run by a `homelab agent` container on every PC. Upgrading an
existing v0.4.x install is a one-time cutover: follow
`docs/migrating-to-swarm.md` (backup with v0.4.3 first; rollback to v0.4.3 is
documented from every step and never touches data volumes).

### Added

- `homelab init` (first PC: Swarm on its tailnet IP, the leader agent),
  `homelab join` (another PC as a worker), `homelab login`/`logout` (a PC as a
  client only). `init --import` brings a v0.4.x config dir, its exposure and
  its keyring secrets into the cluster and reuses the named volumes.
- The leader agent runs Caddy, caddy-l4 and Tailscale (tsnet) in-process: one
  `*.home.<domain>` for the whole cluster, HTTPS sites and raw TCP/UDP ports
  (e.g. gitea ssh on 2222) on the tailnet, cloudflared as a Swarm service.
- Control from any PC: every command except the local ones is forwarded to the
  leader over the tailnet (cluster-secret auth); the TUI and GUI show the
  cluster from any PC.
- Placement per container: data, host paths and shared databases stay on the
  core node; stateless containers prefer worker PCs while they are on and move
  back when they leave.
- `homelab edit <svc> <file>` edits a service's compose file, config.yaml or
  bind-mounted files from any PC, validated before saving.
- `homelab upgrade` rolls a new agent image: workers one by one, the leader
  last, with automatic rollback. The agent image is published as
  `ghcr.io/gr0-o-0t/homelab:<tag>`.
- `backup --fetch` copies a backup to the PC; backups live in their own
  `homelab-backups` volume and keep running if the client disconnects.
- `exec`/`logs` reach a container on any node; `-c` picks one container of a
  multi-container stack; `logs` shows the whole stack.

### Changed

- Secrets live in an encrypted vault in the leader agent; a service's
  environment reaches it as a Swarm secret, never in the service spec.
- Lifecycle verbs need a service or `-a`/`--all` (there is no core stack).
  `up` never re-pulls an image it already has; `update` pulls and re-resolves.
- `restore --config` is now `restore --with-config`.
- Day-one exposure layers are the tailnet and Cloudflare; `--tor`, `--i2p` and
  `--ygg` are refused in cluster mode for now.

### Removed

- The Docker Compose backend, the core stack (tailscale/caddy/tor/i2p/ygg
  containers), session mode, `homelab migrate`, `port`, `ts`, `tor`, `i2p`,
  `ygg`, `ext start|stop`, `--no-record`, and `--build` on `up`/`restart`.

## v0.4.3 — 2026-10-17

### Fixed

- Session mode did not start at login on an encrypted home. The systemd user
  manager starts before the home is mounted (at boot with lingering, and
  pam_systemd can run before pam_ecryptfs), so it never saw the unit in
  `~/.config/systemd/user`. `homelab session install` now also writes
  `~/.config/autostart/homelab-session.desktop`, which the desktop session
  runs after login to load and start the unit. Run `homelab session install`
  once to add it.
- The first `session install` recorded only the containers running at that
  moment, so after a reboot that left the stack down nothing was restored. A
  container whose restart policy would have started it at boot now counts as
  desired running too.
- Shared databases started as a dependency (postgres for bifrost) were
  recorded as not desired, so the supervisor would not restart them after a
  crash. Starting one as a dependency now records it running.

## v0.4.2 — 2026-10-14

### Fixed

- Bifrost 2.0 refuses to create the first admin account without a setup
  token. The bifrost service now declares a `BIFROST_SETUP_TOKEN` secret and
  passes it to the container. Set it with `homelab setup bifrost`, then
  `homelab update bifrost`, and enter the same token in the dashboard's
  **Setup token** field.

### Build

- The headless release builds (`linux_amd64_headless`, `linux_arm64`) are
  static binaries and run on any Linux, Alpine/musl included. The release now
  fails if either one is not statically linked.

## v0.4.1 — 2026-10-08

### Fixed

- `enable --cf` left a service unreachable: it wrote the Caddy site but no
  DNS record, and `cf route add` could never create one for a token-based
  tunnel (it needed the cert.pem of an interactive `cloudflared tunnel
  login`). Both now create a proxied CNAME to the tunnel through the
  Cloudflare API with `CLOUDFLARE_API_TOKEN`, taking the tunnel id from
  `CF_TUNNEL_TOKEN`; `disable --cf` removes it. Existing records that are not
  the tunnel's are never overwritten.

## v0.4.0 — 2026-10-08

### Highlights

- **The stack runs as a login session.** On laptops with an encrypted home,
  Docker used to start homelab at boot, before the home was mounted — Caddy,
  Tor, Yggdrasil and services failed to start or ran without their config.
  `homelab session install` hands start-up to a systemd user service instead:
  restored at login, supervised while you are logged in, stopped at logout.
- **Release archives.** Each platform ships as
  `homelab_<tag>_linux_<arch>.tar.gz` (binary, LICENSE, README, CHANGELOG)
  with a `SHA256SUMS` file; archives are byte-reproducible.

### Added

- **Session mode** (`homelab session install|uninstall|status`): a systemd
  user service that starts the stack at login and stops it at logout instead
  of Docker starting it at boot. Fixes stacks whose config dir lives on an
  encrypted or late-mounted home (ecryptfs, fscrypt, systemd-homed), where
  boot-started containers got empty placeholder bind mounts or failed with
  "mounting a directory onto a file". The supervisor sets every homelab
  container's restart policy to `no`, restores the desired state at login
  (core first, then services, shared databases first), restarts crashed
  containers with backoff (1s → 60s, gives up after 5 failures in 10 minutes)
  and stops everything at logout without changing the desired state.
- **Desired state** (`<config-dir>/state/desired.yaml`): `up`, `start`,
  `restart` and `update` record "running"; `stop`, `down` and `disable --stop`
  "stopped"; `delete` and `prune` forget the service. Single, batch, `--group`
  and core forms alike. Seeded from the running containers the first time.
- `homelab status` shows a session mode line, with a warning when the config
  dir is on a late-mounted home and containers still start at boot.

### Changed

- With session mode installed, homelab containers have restart policy `no`;
  the supervisor does the restarting. `homelab session uninstall` restores the
  recorded policies.
- Release assets are tar.gz archives instead of bare binaries; see the
  install snippet in the release notes.

### Fixed

- `up` waits for a shared database to be healthy even when it is already
  running, instead of provisioning against one that is still starting.
- CI pins the Go toolchain to 1.26.3: Go 1.25.0's linker panicked building the
  GUI's cgo bindings under `-race`.

## v0.3.0 — 2026-10-07

### Highlights

- **Desktop GUI** (`homelab --gui`), now part of the default build: services,
  catalog, network layers, backups, health and settings, with a Ctrl+K command
  palette, per-service config forms and streamed command output.
- **Every CLI action in the TUI and GUI.** Both front ends render from one
  action registry; a parity test fails if the CLI gains a command they cannot
  reach.
- **Docker-like CLI**: lifecycle commands take several services, `-a/--all`,
  keep going past a failing service; `logs` and `exec` behave like their
  `docker compose` counterparts; new `ls`, `ext enable|disable`.
- **Shared backing services with per-service isolation**: one Postgres, one
  password-protected Redis with a database number per service, and Garage as
  the shared S3 store with a bucket and key per service.

### Breaking changes

- `down` no longer removes network exposure; `up` brings a service back as it
  was. Use `disable` to remove exposure.
- `disable -a` removes every layer but no longer stops the service; add
  `--stop`.
- `logs <svc>` prints and exits like `docker compose logs`; the full-screen
  viewer is `logs --tui`.
- Removed: `homelab service …`, `homelab tor|i2p|ygg enable|disable` (use
  `homelab enable <svc> --tor` etc.).
- Caddy routing is generated from each service's `ports:`; hand-written
  `caddy.conf` / `caddy.cf.conf` files are no longer supported. Existing
  installs are migrated automatically to `caddy/sites/<svc>.conf` and
  `services/<svc>/exposure.yaml` on `homelab update`.
- The default build needs cgo and the OpenGL/X11 headers; the resulting binary
  needs libGL/libX11 at runtime. Servers should use the `_headless` (amd64) or
  `arm64` release binaries, or `make build-headless`.
- Redis now requires a password, and each consumer gets its own database
  number. AppFlowy uses Garage instead of a bundled MinIO, NetBox uses the
  shared Redis, and Nextcloud uses the shared Postgres.

### Security

- Tor clients can no longer reach Cloudflare- or I2P-only sites through a
  forged Host header; unmatched hosts on the tunnel listeners are closed, and
  private hostnames are no longer revealed by redirects.
- Mesh-only (Yggdrasil) services no longer answer on the tailnet IP.
- Catalog services no longer publish ports on every host interface.
- Database passwords are kept off process arguments; the log viewer no longer
  writes secrets to a temp file; service names are validated before reaching
  paths or Caddy config.

### Fixes

- `restore` verifies an archive before replacing a volume; `backup --all`
  always writes its manifest; `prune` asks for the typed name whenever volumes
  are removed; `setup` no longer overwrites config.yaml after a parse error.
- A failed `enable` rolls back everything it wrote; an invalid Caddy config is
  restored instead of left on disk.
- `delete` asks for confirmation and aborts if the containers cannot be taken
  down; `doctor --fix` no longer re-exposes deliberately disabled services.
- Thirty-plus catalog services that failed on first start, could not route or
  ignored their configuration now work.
- i2p and yggdrasil apply exposure changes in place instead of restarting
  (mesh peerings and eepsite tunnels survive); core container logs are capped.
- `homelab update` refreshes the core files it owns without touching user or
  state files.
