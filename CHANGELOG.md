# Changelog

Notable changes per release. The release workflow publishes the section
matching the pushed tag as the GitHub release notes.

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
