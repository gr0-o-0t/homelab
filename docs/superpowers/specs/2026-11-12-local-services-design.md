# Custom services with locally built images — design

Date: 2026-11-12. Status: draft for the owner's approval (public exposure and the automated read-only role decided by the owner).

## Why

The cluster (v0.5.0-rc.2) only runs services from the embedded catalog, with
images from a registry. The owner's own game server, taash, is neither: its
service files live outside the catalog and its image (`taash-server:current`)
is built on the laptop by `make deploy-server` in the cards repo and never
pushed anywhere. Under Compose that worked because the laptop ran its own
homelab; under the cluster, `homelab up taash` on the laptop fails (the
cluster has no such service) and minis could not get the image anyway.

The owner's workflow should survive the move: build on the laptop, run one
command, the new version is live — and it keeps serving players while the
laptop is closed.

## Decisions taken with the owner

- The laptop's old Compose-era homelab (appflowy, bifrost, dbx, garage, its
  own postgres/redis/…, taash) is retired. Nothing of it is migrated.
- taash starts fresh: a new, empty `taash` database in the cluster's shared
  Postgres on minis. Only the owner and one friend use it today.
- taash runs on minis by default, so it is up while the laptop is off.
- No registry to run or maintain: homelab ships the image itself from the PC
  where it was built.
- Workflow: the cards repo's `make deploy-server` builds the image and runs
  `homelab update taash`; that ships the new build and rolls the service.

## Goals

1. Install a custom service from a folder on any PC: `homelab add <dir>`.
2. A service whose image is local (`pull_policy: never` in its compose file)
   never triggers a pull; `up`/`update` run from a PC that has the image ship
   it to the leader's engine.
3. `homelab update <svc>` after a rebuild under the same tag rolls the tasks
   onto the new image.
4. taash deployed on the cluster from the laptop with that workflow, exposed
   as before: on the tailnet (`https://taash.home.gr00t.fun`) **and publicly**
   through the Cloudflare Tunnel (`taash.gr00t.fun`, which the cards app
   uses as its `wss://` server). Owner's decision: keep it public.
5. The cards repo's Grafana dashboard works in the cluster's Grafana (already
   installed): homelab creates the read-only Postgres role it reads through
   (owner's decision: automate it), and the dashboard, its provider and its
   data source are pushed from the laptop.

## Non-goals

- Running or integrating a container registry (gitea's included).
- Shipping images to worker nodes. A local-image service defaults to the core
  node; the laptop has the image anyway because it builds it. Distribution to
  other workers can come later if a local service should move around.
- `build:` in compose files (homelab does not build images).
- Migrating any data from the laptop's old homelab.
- Tor/I2P/Yggdrasil (a separate piece of work, next).

## Anti-goals

- An image or a service file silently replaced on the leader by a mistyped
  command: `add <dir>` refuses an existing service (changes go through
  `homelab edit`), and image shipping only ever loads what the service
  declares.
- Secrets in the uploaded files or in the image stream's metadata: the same
  validation as `edit` (no known secret value in a spec) applies.
- A `homelab up` that quietly upgrades: `up` still never takes a different
  image than the one a running service was deployed with (R53). Shipping on
  `up` only happens when the leader lacks the image entirely (first deploy).

## Design

### 1. `homelab add <dir>`

- `add` keeps its catalog behaviour for a bare name. An argument that is a
  path (contains `/` or names an existing directory) is a local folder.
- The CLI side (it runs on the PC, not forwarded) reads the folder:
  `docker-compose.yml` and `config.yaml` are required; other regular files
  and directories (bind-mounted config) come along; dotfiles, `exposure.yaml`
  and anything over a size cap (builder's call; ~10 MiB total) are refused.
  The service name is the folder's base name, validated like every name.
- It streams the files as a tar on stdin to a new hidden, forwarded leader
  command (e.g. `files install <svc>`), alongside `files get|put`.
- The leader refuses an existing service, writes the tree to
  `state/services/<svc>/` atomically (temp dir + rename), and validates as
  `files put` does (compose loads and translates; no secret value in a
  spec). On a validation failure nothing is left behind.
- Then, as for a catalog service: `homelab setup <svc>` (if it declares
  secrets), `homelab up <svc>`, `homelab enable <svc>`.

### 2. Local images

- A compose service with `pull_policy: never` has a **local image**. Today the
  translator ignores `pull_policy`; it becomes part of the stack (a label on
  the spec, e.g. `homelab.image-local=true`).
- The leader never pulls a local image: `up`/`update`/`pull` and the boot
  bring-up skip the pull for it and require it in the leader's engine; a
  missing one fails with "image <ref> is local: run `homelab up|update <svc>`
  from the PC that built it".
- Placement: a container with a local image is placed `core` unless its
  `config.yaml` `placement:` says otherwise (the image is then the owner's
  business on that node).

### 3. Shipping an image from the PC

- `up` and `update` stay forwarded, but the CLI first asks the leader which
  images the service deploys and which are local, with the leader engine's
  image ID for each (extend `homelab images <svc>` with `--json` carrying
  `ref`, `local`, `id`).
- For each local image: if this PC's engine has the ref and its image ID
  differs from the leader's (or the leader has none), the CLI streams
  `docker save <ref>` (Docker SDK `ImageSave`) to a new hidden forwarded
  leader command (e.g. `images load`) that feeds it to the leader engine's
  `ImageLoad` and prints the loaded ref and its identity. The identities
  must match after the load; otherwise fail.
- **Image identity is not the image ID.** minis runs Docker's containerd
  image store (`io.containerd.snapshotter.v1`), the laptop the classic
  overlay2 store; the same image has a different ID in each. Compare a
  store-independent identity instead: the image's `RootFS.Layers` (diff
  IDs) plus its `Created` time, hashed (builder's call on the exact form,
  under that constraint). "Differs" in this document always means this
  identity.
- `up` ships only when the leader has no image for the ref (first deploy);
  `update` ships whenever the IDs differ. On the leader itself (PC == leader
  engine) there is nothing to ship.
- If the PC does not have the image either: no shipping; the forwarded
  command fails on the leader as above if the leader lacks it too.
- Progress: one line per image ("shipping taash-server:current (54 MB) to
  the leader…"). The stream is large; it goes through the existing stdin
  pump, which frames it.

### 4. Rolling a rebuilt local image

- A locally built image has no registry digest, so the spec keeps the bare
  tag and an update with the same tag would change nothing in Swarm.
- `update` (and only `update`) of a service with a local image whose image ID
  on the leader differs from what its running tasks use bumps
  `TaskTemplate.ForceUpdate`, so Swarm replaces the tasks; the new tasks start
  from the tag, which now points at the shipped image. (How the running
  task's image ID is found — task container inspect, or a label recording the
  deployed ID — is the builder's call.)
- Swarm tries to pull a bare tag at every task start and falls back to the
  local image when the pull fails; verify that a task of a local image starts
  cleanly on the core node (and log noise from the failed pull is acceptable).
- Risk accepted (designer's call): a local image whose name also exists on a
  public registry (`someuser/app:tag` on Docker Hub) would be replaced by the
  registry's at task start. taash (`taash-server`, i.e. `library/`, official
  images only) is safe. `add`/`up` print a warning for a local image whose
  reference has a namespace or a registry host; docs say to give local
  images an unqualified name.

### 5. Files into a bind-mounted directory (Grafana dashboard)

- `homelab edit`'s leader half (`files put`) accepts a **new** file inside a
  directory the compose file bind-mounts (e.g. grafana's
  `provisioning/dashboards/taash/taash-beta.json`), creating subdirectories
  within it, and redeploys the service as it does for an edited file. A path
  outside every bind-mounted directory is still refused. (The builder check
  found `isConfig`/`stageFile` in `internal/swarm/configs.go` may already
  allow this; then this item is tests plus `--from`.)
- A non-interactive form for scripts: `homelab files put grafana <path> <
  file` is already forwarded but hidden; expose it as `homelab edit <svc>
  <path> --from <file>` (no editor) so the cards Makefile can call it.

### 6. Read-only database access for another service

What the old setup did by hand (a `taash_ro` role with SELECT on taash's
tables, `ALTER DEFAULT PRIVILEGES FOR ROLE taash`, its password in Grafana as
`TAASH_RO_PASSWORD`) becomes a `databases:` declaration on the **consumer**:

```yaml
# grafana's config.yaml
databases:
  - postgres:
      database: taash        # another service's database
      user: taash_ro
      readonly: true
      env:
        host: TAASH_PG_HOST
        port: TAASH_PG_PORT
        user: TAASH_RO_USER
        password: TAASH_RO_PASSWORD
        database: TAASH_PG_DB
```

- `readonly: true` means: the database must already exist (owned by its own
  service's role); the provisioner creates or updates the login role with a
  generated password, kept in the consumer's secrets namespace like any
  generated database password; grants CONNECT, USAGE on schema public,
  SELECT on all tables and sequences, and default SELECT privileges for
  tables the owner role creates later. Never CREATE, never ownership.
- Run at `up` of the consumer (as provisioning is today). If the database
  does not exist yet: fail with "database taash does not exist yet — `homelab
  up taash` first"; re-running `up grafana` after that succeeds.
- `homelab delete` of the consumer drops the role (as its other roles);
  `delete` of the owner service is unaffected (its database is kept, as
  today).
- `env:` injection, backups (the read-only role is not dumped separately)
  and `setup` re-sync of the password (`ALTER USER … PASSWORD`) work as for
  a normal declaration.

One-time wiring of Grafana in the cluster (designer's call: documented
steps, not automated, because it edits the catalog Grafana's own files):
`homelab edit grafana config.yaml` adds the declaration above, and `homelab
edit grafana docker-compose.yml` passes the injected env to the container
(`TAASH_RO_PASSWORD: ${TAASH_RO_PASSWORD}` etc.). The cards README lists
these steps.

### 7. Public exposure of taash

- `homelab enable taash --cf` (after `up`) writes the Cloudflare layer's
  site and creates the proxied CNAME `taash.gr00t.fun` → the cluster's
  tunnel through the Cloudflare API, as `enable --cf` already does.
- The existing `taash.gr00t.fun` record points at the laptop's old tunnel
  if that was a different tunnel; `enable --cf` never overwrites a record
  that is not its tunnel's. Builder: check which tunnel the record targets;
  if it is not the cluster's, the owner deletes it in the Cloudflare
  dashboard once (a documented step), then reruns `enable --cf`.
- Verify the game's WebSocket works end to end through cloudflared →
  `homelab-edge:80` → taash (connect from off the tailnet), and through the
  tailnet URL.

### 8. taash itself (the owner's other repo)

- The cards repo gets the service folder, e.g. `deploy/homelab/taash/`, with
  `docker-compose.yml` and `config.yaml` taken from the laptop's old
  `~/.config/homelab/services/taash` (ports rewritten to the list form
  `- 8080`; `placement:` not needed, the local image defaults to core).
- First deploy from the laptop: `homelab add deploy/homelab/taash`,
  `homelab up taash`, `homelab enable taash`.
- `make deploy-server`: build the image, then `homelab update taash`.
- `make grafana-dashboard`: push the provider
  (`provisioning/dashboards/taash.yaml`), the data source
  (`provisioning/datasources/taash.yaml`, reading `$TAASH_RO_PASSWORD`) and
  the dashboard JSON with `homelab edit grafana <path> --from <file>`; the
  cards repo keeps those files (taken from the laptop's old
  `services/grafana/provisioning/`).
- First deploy also: `homelab enable taash --cf` (section 7), and the
  one-time Grafana wiring (section 6).
- The compose file sets `read_only`, `user`, `cap_drop: [ALL]` and
  `security_opt: [no-new-privileges:true]`; `translateService` already
  carries all four into the Swarm spec. A test with taash's compose file
  pins that.
- The cards repo changes are in scope (the owner agreed to the one-line
  Makefile change): `deploy/homelab/taash/`, the `deploy-server` and
  `grafana-dashboard` targets, and `packages/server/grafana/README.md`,
  committed in that repo.

## Constraints

- Everything in CLAUDE.md's key design decisions holds: Swarm only, secrets
  never in a spec/argv/label, tests never reach Docker or a tailnet (fakes in
  `swarm/swarmtest`), front ends run the CLI (the actions registry and its
  parity test need entries for new commands/flags).
- Version skew: the new hidden leader commands are refused by an older agent
  with the usual version message; no compatibility shims.

## Release

Designer's call: this ships as v0.5.0-rc.3 (CLI and agent must share
major.minor, so both PCs and the leader upgrade together); v0.5.0 final
follows once taash runs on the cluster.

## Left to the builder

- Command and flag names for the hidden plumbing (`files install`,
  `images load`, `images --json`).
- The size cap for `add <dir>`, and how the deployed image ID is tracked.
- Whether `pull` of a local-image service says "skipped (local image)".
- Docs: CLAUDE.md (commands, Services under Swarm), docs/adding-a-service.md
  (a custom service with a local image), CHANGELOG.
