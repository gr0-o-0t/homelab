# Changelog

Notable changes per release. The release workflow publishes the section
matching the pushed tag as the GitHub release notes.

## v0.6.1-dev.1 — 2026-11-23

Development prerelease of v0.6.1 (does not move `:latest`). Install it with
`homelab upgrade --version v0.6.1-dev.1`.

### Added

- `rabbitmq`: shared message queue (AMQP), installed on demand like
  postgres, redis and garage; management UI at `rabbitmq.home.*` (login
  `admin`, generated password). A service declares `databases: - amqp:`
  and gets its own vhost and user, both named after it, with permissions
  on that vhost only; env keys `host port user password vhost dsn`
  (`amqp://<svc>:<pw>@homelab-rabbitmq:5672/<svc>`). The password is
  generated into the service's vault namespace (`AMQP_PASSWORD`).
  `homelab delete` removes the user and keeps the vhost and its queues.
  A custom service cannot be named `rabbitmq`, and a service named `admin`
  (the management login) cannot declare `amqp`.

### Fixed

- `stirlingpdf`: user-saved files, custom files and pipelines are on named
  volumes, so they survive task restarts; the image comes from the official
  registry, `docker.stirlingpdf.com`. Installed services keep their compose
  file: add the volumes to an installed Stirling PDF with
  `homelab edit stirlingpdf docker-compose.yml`.

## v0.6.0 — 2026-11-19

Minor release: the v0.6.0-dev.1 prerelease, verified on the live cluster
through an engine restart and a reboot of the core node. Upgrade with
`homelab upgrade`.

### Changed

- The leader agent runs as the Swarm service `homelab_agent` instead of the
  `homelab-agent` container, so it comes back after any engine restart or
  reboot of the core node by itself. A plain container on the overlay could
  fail its network attach at boot (a stale Swarm IPAM address), which the
  engine never retries, and that left the cluster without its CLI. Worker
  agents stay plain containers: they are not on the overlay.
- `homelab upgrade` updates the leader service (stop-first, rolled back by
  Swarm if the new agent exits within 30 s). The first upgrade from v0.5.x
  restarts the leader twice: the old swap, then the new agent converts
  itself into the service. The CLI waits up to 3 min for the leader.
- `agent` is a reserved service name.

### Recovery

- With the leader agent down: `docker service ps homelab_agent`,
  `docker service logs homelab_agent`, `docker service update --force
  homelab_agent` on the core node. See "Recovering a leader with no agent"
  in `docs/reference/cli.md`.

## v0.6.0-dev.1 — 2026-11-19

Development prerelease of v0.6.0 (does not move `:latest`). Install it with
`homelab upgrade --version v0.6.0-dev.1`.

### Changed

- The leader agent runs as the Swarm service `homelab_agent` instead of the
  `homelab-agent` container, so it comes back after any engine restart or
  reboot of the core node by itself. A plain container on the overlay could
  fail its network attach at boot (a stale Swarm IPAM address), which the
  engine never retries, and that left the cluster without its CLI. Worker
  agents stay plain containers: they are not on the overlay.
- `homelab upgrade` updates the leader service (stop-first, rolled back by
  Swarm if the new agent exits within 30 s). The first upgrade from v0.5.x
  restarts the leader twice: the old swap, then the new agent converts
  itself into the service. The CLI waits up to 3 min for the leader.
- `agent` is a reserved service name.

### Recovery

- With the leader agent down: `docker service ps homelab_agent`,
  `docker service logs homelab_agent`, `docker service update --force
  homelab_agent` on the core node. See "Recovering a leader with no agent"
  in `docs/reference/cli.md`.

## v0.5.8 — 2026-11-18

Patch release: the v0.5.8-dev.1 prerelease, verified on the live cluster,
plus a Hugging Face token for LocalAI. Upgrade with `homelab upgrade`.

### Added

- `localai`: OpenAI-compatible API for local LLMs, embeddings, images and
  audio (`localai.home.*`), CPU image on the core node. Threads default to
  16; at most two models stay loaded (least recently used unloads), idle
  models unload after 30m and a stuck backend is killed after 15m. Set
  `LOCALAI_API_KEY`, and optionally `HF_TOKEN` for private or gated Hugging
  Face models, with `homelab setup localai`.

### Fixed

- `filestash`: the local backend shows the core node's home folders at
  `/home` (`HOME_PATH`, default `/home`), writable as uid 1000. Installed
  services keep their files: add the mount to an installed Filestash with
  `homelab edit filestash docker-compose.yml`.

## v0.5.8-dev.1 — 2026-11-18

Development prerelease of v0.5.8 (does not move `:latest`).

### Added

- `localai`: OpenAI-compatible API for local LLMs, embeddings, images and
  audio (`localai.home.*`), CPU image on the core node. Threads default to
  16; at most two models stay loaded (least recently used unloads), idle
  models unload after 30m and a stuck backend is killed after 15m. Set
  `LOCALAI_API_KEY` with `homelab setup localai`.

### Fixed

- `filestash`: the local backend shows the core node's home folders at
  `/home` (`HOME_PATH`, default `/home`), writable as uid 1000. Installed
  services keep their files: add the mount to an installed Filestash with
  `homelab edit filestash docker-compose.yml`.

## v0.5.7 — 2026-11-18

Patch release: the v0.5.7-dev.1 prerelease, verified on the live cluster,
unchanged. Upgrade with `homelab upgrade`.

### Added

- `deploy.mode: global` in a catalog compose file runs the container on every
  node, joining nodes included, unpinned (its host paths and volumes are each
  node's own). `down`/`stop` remove it, keeping its volumes; `up`/`start`, and
  `restart` of a stopped service, create it again; a backup leaves it running.
- `security_opt: apparmor:unconfined` is honoured (Swarm AppArmor disabled).
- `netdata`: a parent on the core node (dashboard and history,
  `netdata.home.*`) and an agent on every node streaming to it with a
  homelab-generated key. No host networking or `pid: host` under Swarm: host
  NIC charts and host process attribution are missing.
- `beszel-hub` gains a `beszel-agent` on every node, registering with the hub
  by universal token. Set `BESZEL_KEY` and `BESZEL_TOKEN` from the hub with
  `homelab setup beszel-hub` after its first start, then `homelab up
  beszel-hub`; stop any hand-run beszel-agent first.
- `filestash`: web file manager (S3, SFTP, WebDAV, FTP) with Collabora office
  editing on the same hostname.

### Fixed

- State files, the vault included, are written atomically and privately.
- A late `init`/`join` failure removes the agent it started, so a retry works.
- `delete` keeps a service's postgres owner role as NOLOGIN instead of failing,
  and cleans up a service whose folder was removed by hand; extension names
  are quoted.
- A service whose name maps to an installed one's database password key is
  refused (`new`, `add`, `files install`).
- `doctor`, `status --check`, `enable` without `config.yaml` and a failed
  `disable` reload exit non-zero.
- `restore` of an uninstalled service is refused before the prompt; a failed
  database dump removes its partial file.
- Log and job output read lines up to 1 MiB; the TUI log view no longer sticks
  on "refreshing"; setup form loads time out.

### Internal

- Dependency bumps (Go modules, Docker base image, cloudflared, CI actions);
  CI lints with Go 1.27; brittle tests and dead helpers removed; reference
  detail moved from CLAUDE.md to `docs/reference/`.

## v0.5.7-dev.1 — 2026-11-18

Development prerelease of v0.5.7 (does not move `:latest`).

### Added

- `deploy.mode: global` in a catalog compose file runs the container on every
  node, joining nodes included, unpinned (its host paths and volumes are each
  node's own). `down`/`stop` remove it, keeping its volumes; `up`/`start`, and
  `restart` of a stopped service, create it again; a backup leaves it running.
- `security_opt: apparmor:unconfined` is honoured (Swarm AppArmor disabled).
- `netdata`: a parent on the core node (dashboard and history,
  `netdata.home.*`) and an agent on every node streaming to it with a
  homelab-generated key. No host networking or `pid: host` under Swarm: host
  NIC charts and host process attribution are missing.
- `beszel-hub` gains a `beszel-agent` on every node, registering with the hub
  by universal token. Set `BESZEL_KEY` and `BESZEL_TOKEN` from the hub with
  `homelab setup beszel-hub` after its first start, then `homelab up
  beszel-hub`; stop any hand-run beszel-agent first.
- `filestash`: web file manager (S3, SFTP, WebDAV, FTP) with Collabora office
  editing on the same hostname.

### Fixed

- State files, the vault included, are written atomically and privately.
- A late `init`/`join` failure removes the agent it started, so a retry works.
- `delete` keeps a service's postgres owner role as NOLOGIN instead of failing,
  and cleans up a service whose folder was removed by hand; extension names
  are quoted.
- A service whose name maps to an installed one's database password key is
  refused (`new`, `add`, `files install`).
- `doctor`, `status --check`, `enable` without `config.yaml` and a failed
  `disable` reload exit non-zero.
- `restore` of an uninstalled service is refused before the prompt; a failed
  database dump removes its partial file.
- Log and job output read lines up to 1 MiB; the TUI log view no longer sticks
  on "refreshing"; setup form loads time out.

### Internal

- Dependency bumps (Go modules, Docker base image, cloudflared, CI actions);
  CI lints with Go 1.27; brittle tests and dead helpers removed; reference
  detail moved from CLAUDE.md to `docs/reference/`.

## v0.5.6 — 2026-11-13

Patch release.

### Fixed

- `logs -f -n N` follows every service, also one whose backlog could not be
  read (a task of its history on a node that is down): the error is reported
  and the live tail goes on, so the TUI/GUI log viewer no longer dies. A
  service with an empty backlog follows from when the backlog read began, not
  from "now", so lines logged in between are not lost.
- `backup <service> --json` is refused; only `backup --list` reads `--json`.
- `doctor --all` closes with "Passed with N warnings." instead of "All
  services healthy." when services passed with warnings.

### Docs

- README: install from the release tarball (checked against `SHA256SUMS`) and
  keep current with `homelab update` / `homelab upgrade`, instead of a clone of
  a placeholder URL; a Firewall prerequisite (Swarm ports on `tailscale0`, with
  the `ufw` lines); the quick-start order that works; the Included-services
  table regenerated from the catalog (real subdomains and purposes; minero
  left out, it is unsupported on Swarm; shared providers marked).
- Cloudflare guide: how to create the tunnel, where `CF_TUNNEL_TOKEN` comes
  from, and that every public hostname's service is `HTTP` `homelab-edge:80`.
- Tailscale guide: a complete ACL policy that also allows the Swarm ports
  between the cluster hosts.
- Adding a service: the port grammar (`ssh:22` is a subdomain, not a named
  port; the `--ports=` names) and the compose keys Swarm ignores.
- Secrets live in the leader's vault: the wording in `restore`'s help,
  `setup`'s output and `doctor`'s checks no longer says "keyring"; "minis" in
  the docs is "the leader (core node)".
- `docs/network-extensions-plan.md` is marked obsolete; the service-testing
  guide and CONTRIBUTING drop the Tor/I2P/Ygg steps and the `caddy.conf` files.

## v0.5.5 — 2026-11-13

Patch release.

### Fixed

- The front ends confirm "Stop all", "Restart all" and "Update all" the way
  "Down all" already did, and "Compose config" (which prints secrets in plain
  text) now asks first and sits in Configure instead of Inspect.
- The TUI's Global restore moved from `r` (restart on a service) to `B`.

### Changed

- Stop shows the stop icon, like Down, instead of pause.
- "Down all" and `disable --stop` said they remove containers; they scale
  the service to zero, keeping config, volumes and exposure.

## v0.5.4 — 2026-11-13

Patch release.

### Fixed

- `homelab status` fills the PORTS column, which showed `–` for every
  service (it listed host-published ports, and Swarm services publish
  none). It now lists the ports a service is exposed on, honouring
  exposure.yaml's `ports:` selection: a raw listen port as `2222/tcp`
  (tailnet only), an HTTP port as its subdomain (`vault`) or `https` for
  the default. `homelab status <service>` gains a Ports section with every
  declared port: the name `enable --ports=` takes, where it answers
  (`https://gitea.home.<domain>`, `gitea.home.<domain>:2222/tcp`) and
  whether it is enabled. `status --json` and `status <service> --json`
  carry the same as each service's `ports` (name, label, raw, port, listen,
  protocols, target, enabled, note).
- `--json` on a command that does not support it (`doctor`, `ext list`, ...)
  now fails with `error: --json is not supported by "homelab doctor"` before
  anything is forwarded, instead of printing human text and exiting 0. The
  commands that honour it (`status`, `ls`, `setup`, `images`, `backup --list`,
  `backup-list`) are unchanged.

## v0.5.3 — 2026-11-13

Patch release.

### Fixed

- `homelab doctor` no longer warns "no log cap in /etc/docker/daemon.json" on
  every run: the agent runs in a container where the host's file is never
  mounted, so the check could not tell. It also ends with "Passed with N
  warning(s)." instead of "All checks passed." when anything warned.
- `homelab logs <service> -n N` prints the last N lines of the whole stack,
  in time order. Docker applied the tail per task and printed one task after
  another, so a service with an old and a new task gave twice the lines,
  interleaved across days. The lines of every task and container are now
  merged by their timestamps before the tail; the timestamps are shown only
  with `-t`. `-f` prints those lines, then follows. Without `-n`, logs
  still stream as Docker gives them, one task after another.

## v0.5.2 — 2026-11-13

### Added

- `homelab update` with no service checks for a newer homelab release and
  reports this CLI's and the leader agent's versions. `homelab update
  <service>` still pulls and redeploys a service.
- `homelab upgrade` upgrades this PC's CLI to the latest release (or
  `--version`'s), verified against the release's `SHA256SUMS`, then the
  agents. A CLI binary you cannot write is replaced through `sudo`; the
  agents are then upgraded as you. It never downgrades: with no newer
  release it only brings agents behind this CLI up to it. `--image` still
  rolls an image to the agents only.

### Build

- Dependabot proposes weekly, grouped updates for Go modules, GitHub Actions,
  the agent image base, cloudflared and catalog images. The cloudflared image
  now comes from `assets/cloudflared/docker-compose.yml`.

## v0.5.1 — 2026-11-13

Patch release.

### Fixed

- Stopped and replaced task containers piled up: Swarm kept five finished
  tasks per service. The leader now keeps one, and Swarm removes the older
  ones.

## v0.5.0 — 2026-11-12

The homelab moves from one Docker Compose host to a Docker Swarm cluster run
by a `homelab agent` container on every PC: one `*.home.<domain>` for the
whole cluster, control from any PC, stateless work on worker PCs while they
are on. Upgrading a v0.4.x install is a one-time cutover:
`docs/migrating-to-swarm.md` (backup with v0.4.3 first; the rollback never
touches data volumes). From a v0.5.0 release candidate: install this CLI and
run `homelab upgrade`.

This is the three release candidates below, verified on a live two-PC
cluster, plus:

### Fixed

- The desktop (GUI) amd64 build needed glibc 2.43 and did not start on
  Linux Mint 22 or other distributions older than the CI runner. It is built
  on Ubuntu 24.04 (glibc 2.39; the prebuilt Dear ImGui library needs 2.38),
  and the release fails if it ever needs more than glibc 2.39.

### Highlights (details in the release candidates)

- `homelab init` / `join` / `login` / `upgrade`: the leader agent runs Caddy
  (with raw TCP/UDP ports, e.g. gitea ssh on 2222) and Tailscale in-process;
  every other command is forwarded to it over the tailnet.
- Secrets live in an encrypted vault in the leader agent and reach a service
  as a Swarm secret, never in its spec.
- Placement per container: data and shared databases stay on the core node,
  stateless containers move to worker PCs and back.
- Custom services from a folder (`homelab add <dir>`) with locally built
  images shipped from the PC that built them; `homelab edit --from`;
  read-only access to another service's database (`readonly: true`).
- Removed: the Compose backend, the core stack, session mode; Tor, I2P and
  Yggdrasil are not served in cluster mode yet.

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
  service's database, with the consumer's generated password. A postgres
  restore leaves grants out: run `homelab up <consumer>` afterwards to
  re-grant its read access.

### Changed

- `homelab join` and `homelab init` (with or without `--import`) remove an
  idle Compose-era `home-services` bridge network, and refuse one with
  containers.

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
