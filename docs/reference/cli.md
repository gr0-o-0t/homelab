# CLI reference

Moved out of CLAUDE.md. Command behaviour in detail; read the section for the command you are changing.

Every command except the local ones (`init`, `join`, `login`, `logout`, `edit`,
`version`, `completion`, `backup --fetch`, and the bare `homelab` dashboard/GUI)
is forwarded to the leader agent and runs there against its state dir.

## Cluster

```bash
homelab init [--import <dir>] [--image <ref>]      # first PC (minis): swarm init on its tailnet IP, start the leader agent; prints the join command
homelab join [--endpoint <url>] [--secret-stdin]   # another PC: swarm join as a worker, start its agent
homelab login [--endpoint <url>] [--secret-stdin]  # use this PC as a client only (no agent, no swarm)
homelab logout                                     # forget the endpoint and the cluster secret here
homelab upgrade [--version X] [--image REF]        # this PC's CLI to the latest release (or X), then the agents: pull on every node, workers one by one, the leader last
```

`init`/`join` advertise the host's Tailscale IPv4 and refuse an address outside
100.64.0.0/10. `init --import ~/.config/homelab` brings a Compose-era config dir
(root and service config, exposure, routing, desired state, keyring secrets)
into the new leader and reuses its named volumes.

`upgrade` on a PC first upgrades this PC's CLI (`cmd/selfupdate.go`): the
target is the latest GitHub release (`internal/release`), or `--version`'s.
When the CLI is not the target it downloads that release's archive for this
build, checks it against `SHA256SUMS` and renames the binary over the
running executable (a temp file in the same directory, `os.Executable` +
`EvalSymlinks`). API calls time out after 30 s; the download has no overall
client timeout (dial/TLS/header timeouts, 15 min per download). Release
binaries are linux-only: off Linux the CLI step refuses (rebuild from
source, then `--version`/`--image` for the agents). A directory the user
cannot write: `sudo -- <exe> upgrade --cli-only --version <tag>` (`doas`
when there is no `sudo`; hidden, local, refused in the agent: replace the
binary and exit) — only that step runs as root — then this process execs the
new binary as `upgrade --version <tag>` with `HOMELAB_UPGRADE_SELF=<tag>` in
its environment (every root flag set kept, wherever it stood) so the agent
rollout runs as the invoking user, with their client.yaml and keyring; that
exec'd leg never runs a CLI step (a binary not on the tag fails: "not
retrying"). Already root or neither `sudo` nor `doas`: an error naming the
path and the manual command (as root); a rename refused with EPERM (sticky
dir) also takes that path. `sudo homelab upgrade` swaps the binary but root
has no client.yaml: the rollout's error then says to run it as yourself. A
bare upgrade whose CLI is not behind the latest release (never a downgrade)
keeps the CLI and still forwards the rollout to the CLI's version (unless
the leader runs a newer release): the leader leaves every agent already on
it alone, so a worker that missed an upgrade is caught up. A dev CLI build
refuses a bare upgrade (`--version` or `--image`). `--image` leaves the CLI
alone and rolls that image to the agents only.
`--image` with an unchanged version label (a rebuilt dev image of the same commit) replaces nothing: agents on that version are left alone.
A bare upgrade from a release CLI rolls a dev-build cluster to that release (the dev agents are not "newer").

The agent step (forwarded; Mutating and Detached) resolves the image on the PC
(`ghcr.io/gr0-o-0t/homelab:<tag>`, or `--image`),
asks the leader `version --agent` (version + agent container ID)
and sends `upgrade --image REF [--version V]` to the leader, which
(`bootstrap.Upgrade`): makes the image available on its engine; asks every
Ready worker's agent `version --agent` and has it pull (`agent self-replace
--pull-only`) — a worker already on the target is left alone, a Down node or
an unreachable agent is **skipped**, a failed pull halts before anything is
replaced; then replaces the workers one by one through `agent self-replace
--image REF`, accepting a worker only when a *different* agent container
(new ID) has answered steadily for 10 s within 60 s, on the expected version
(`--version`, else the image's `org.opencontainers.image.version` label) — a
failure, rollback or mismatch halts before the leader; then — unless the
leader agent itself already runs the expected version (`Upgrade.Current`:
it prints "leader: already on vX", and "Everything is on vX: nothing
replaced." when no worker changed either, and ends without the marker) —
prints
`bootstrap.LeaderReplaceMarker` and replaces its own agent, ending the
session. Only after seeing that marker does the PC print "agent restarting",
reconnect for up to 60 s and check the leader the same way (a connection lost
before it is reported; the detached rollout continues on the leader).
Worker agents serve `exec`, `version` and `agent self-replace` only. `logs`
resolves there too but refuses a service ("runs on the leader"): service logs
are read cluster-wide on the leader through `docker service logs` (R39).

Replacement (`bootstrap.SelfReplace`, `RunReplace`): the agent inspects its
own `homelab-agent` container (spec without what the old image itself sets:
env, labels, entrypoint, working dir, user, healthcheck) and starts a helper
container `homelab-agent-replace` (new image, docker socket only, no
AutoRemove, no restart policy, label `homelab.agent-replace`, spec in
`HOMELAB_REPLACE_SPEC`) running `agent replace-helper`. After 3 s the helper
stops the old agent and renames it `homelab-agent-old` (kept intact), creates
and starts the new one (identical config, host config — mounts, restart
policy — and networks + aliases, new image), and waits up to 60 s for it to
run 10 s with no restart. Only then is the old container removed and the
helper removes itself. On any failure the new container is removed, the old
one renamed back, started and checked running; the exited helper stays so
`docker logs homelab-agent-replace` says why, and the next upgrade removes it
(it refuses while a helper runs or a `homelab-agent-old` exists). No volume
is ever removed or created. A node left with a lone `homelab-agent-old` and
no `homelab-agent` (the helper killed mid-swap) has no agent: check that no
`homelab-agent` exists, then `docker rename homelab-agent-old homelab-agent
&& docker start homelab-agent`, and rerun the upgrade. `agent self-replace`
runs remotely only on a worker agent (`ServerConfig.AllowInternal`) and
refuses on the leader. The agent accepts `upgrade` and `version` from a CLI
of another major.minor (`remote.AcrossVersions`), and a worker agent also
`agent self-replace` (R55: the leader must be able to upgrade a worker that
missed an upgrade); every other command is refused.

`EnsureImage` (init, join, upgrade) pulls only when the reference is not
already on the engine: it never refreshes an existing local tag, so a moving
tag (`:dev`, `:latest`) is used as it is there — rebuild/pull it on each node
yourself, or upgrade to an immutable version tag.

## Service lifecycle

```bash
homelab add [name]                                 # install a bundled service from the catalog (no name → list catalog)
homelab add <dir>                                  # install a custom service from a folder on this PC (a path, or a bare name that is no catalog service; `./<name>` forces the folder)
homelab new [name]                                 # scaffold wizard (TUI) or --container/--port flags
homelab setup [service]                            # configure vars and secrets interactively (no arg → root wizard)
homelab up <service...>                            # deploy on Swarm, shared databases first (-a/--all, --group)
homelab down <service...>                          # scale to zero; config, volumes and exposure kept (-a, --group)
homelab start <service...>                         # same as up on Swarm (-a, --group)
homelab stop <service...>                          # same as down on Swarm (-a, --group)
homelab restart <service...>                       # force-update every Swarm service of the stack (-a, --group)
homelab pull <service...>                          # pull images only (-a, --group)
homelab update <service...>                        # pull + redeploy, tags resolved to digests (-a, --group)
homelab update                                     # no service: report whether a newer homelab release exists (CLI + leader versions; runs on this PC)
homelab reload [service]                           # load the routes into the agent's Caddy, or re-render one service's
homelab edit <service> <path>                      # edit docker-compose.yml, config.yaml or a bind-mounted file in $EDITOR; validated, saved on the leader, applied as `up` does if the service runs
homelab edit <service> <path> --from <file>        # same without an editor: the file's content replaces/creates <path> (scripts; max 500 KiB)
homelab delete <service>... (alias: rm)            # Swarm services + exposure + config; type the name or -y
```

`delete` also cleans up a service whose `services/<name>/` folder was
removed by hand, as long as the desired state still lists it; a name nothing
knows is still refused.

The lifecycle verbs take several services, continue past a failing one and
report every failure at the end. With no service they refuse ("name a service,
or use -a/--all"): there is no core stack to act on. Exposure is configuration,
not container state: `down` leaves routes in place (they answer 502 while the
service is down) and `up` brings it back exactly as it was. Only `disable` and
`delete` remove exposure.

`up` (and `start`, `restart`, `edit`'s apply, a shared provider brought up
for a consumer) never upgrades an image (R53): it pulls an image only when
the engine does not have it, never asks the registry for a newer digest, keeps
a running service on the digest it was deployed with, and pins a new one to
the local image's digest. `update` is the deliberate upgrade: pull, tags
resolved to the registry's current digests, tasks rolled.

## Observation and diagnostics

```bash
homelab                                            # interactive TUI (service browser)
homelab --gui                                      # desktop GUI (giu; absent from -tags nogui builds)
homelab ls [-q]                                    # list installed services (table, --json, -q names)
homelab status [service]   (alias: ps)             # edge + services overview, or per-service detail (each task and its node, each declared port); --json for front ends
homelab logs [service]                             # a service's logs from every node, key-prefixed (-f, -n, -t, --since, -c, --tui); no service: the leader agent's
homelab caddy status                               # whether the agent's Caddy answers on its admin socket
homelab caddy logs                                 # follow the leader agent's logs (Caddy logs there)
homelab doctor [service] [--fix] [--all]           # health checks (a service: its tasks running, newest task's image and health); --fix creates the overlay network, sites dir
homelab validate                                   # assemble and adapt the Caddy config without loading it
homelab config <service>                           # the compose file as resolved to deploy (contains secrets)
homelab images <service>... [-q] [--json]          # images the stack deploys (--json: ref, local, identity; -a/--group; the identity only for local images)
homelab version                                    # print the homelab version
```

`doctor` (with or without `--all`) and `status --check` exit non-zero when a
check fails; warnings (`!`) do not fail them. `enable` without a
`config.yaml` and `disable` whose edge reload fails are errors too (a
stopped agent is not: the change applies when it starts).

## Container access

```bash
homelab exec <service> <command> [args...]         # relayed to the node running the task; flags after the service go to the command (-c, -u, -w, -e, -T)
```

`exec` and `logs` pick one container of a multi-container stack with
`-c <compose key or container_name>`.

## Backup, restore, prune

```bash
homelab backup [service]                           # volumes + DB dumps + config (--all, --group)
homelab backup [service] --out <dir>               # destination inside the homelab-backups volume (default its top level)
homelab backup [service] --live                    # don't stop the service (risks torn files)
homelab backup --list [--json]                     # existing backups on the leader, newest first
homelab backup --fetch <name> <local-dir>          # copy a backup from the leader to this PC (<local-dir>/<name>)

homelab restore <backup-dir> [service]             # replace volumes + databases (--create-volumes on a fresh cluster)
homelab restore <backup-dir> --with-config         # also overwrite config.yaml / compose

homelab prune [service...]                         # remove Swarm services, then volumes and images
homelab prune [service] --keep-volumes             # reclaim images only, no data loss
homelab prune --dangling                           # unreferenced images + build cache only
```

Backups run on the leader, where every data volume is pinned, and land in the
`homelab-backups` volume. Each writes a timestamped directory with a
`manifest.json` listing every volume, dump and config file. Each service is
scaled to zero for its own snapshot unless `--live` is given; `backup` and
`restore` keep running on the leader if the client disconnects.

Secrets are deliberately **not** in backups; they stay in the leader's vault.
After restoring onto a new cluster run `homelab setup <service>`, which
re-enters them and re-syncs the database role password (`ALTER USER … PASSWORD`).
A postgres restore leaves out the dump's grants (`--no-privileges`): after a
restore, `homelab up <consumer>` re-grants a consumer's read-only access.

Redis is never dumped: in this catalog it is only ever a cache or job queue, and
a stale restored queue is worse than an empty one.

`restore` and `prune` are destructive and both confirm first. `prune` requires
typing the service name when volumes will be deleted, and refuses outright
without a TTY unless `--yes` is passed. It removes images on the leader only.

## Network exposure

```bash
homelab enable <service>                           # private tailnet only (default)
homelab enable <service> --cf                      # + Cloudflare Tunnel (public)
homelab enable <service> --all                     # all available layers
homelab enable <service> --name=<custom>           # custom display name/subdomain
homelab enable <service> --ports=web,ssh           # expose specific named ports only

homelab disable <service>                          # remove private tailnet route
homelab disable <service> --cf                     # remove Cloudflare Tunnel
homelab disable <service> -a                       # remove every layer, private included
homelab disable <service> -a --stop                # …and take the service down
```

Day-one layers are the tailnet (`ts`) and Cloudflare (`cf`). `--tor`, `--i2p`
and `--ygg` are accepted but fail with "not available in cluster mode yet"; the
layer packages stay for a later plan.

## Configuration

```bash
homelab setup                                      # interactive root config wizard (config.yaml + vault)
homelab setup <service>                            # per-service config wizard
homelab setup [service] --json                     # declared vars + secrets (set: bool, never values)
homelab setup [service] --set KEY=VALUE            # write vars without prompting (repeatable)
homelab setup [service] --secrets-stdin            # secrets as {"NAME":"value"} JSON on stdin
```

## Shell completion

```bash
homelab completion bash|zsh|fish|powershell        # generate shell completion scripts
```

## Network extensions (`homelab ext`)

```bash
homelab ext enable <ext>                           # turn an extension on (config.yaml) and deploy it
homelab ext disable <ext>                          # remove it and turn it off
homelab ext list                                   # list extensions and their enabled/disabled status
homelab ext status [ext]                           # state of all or one extension
homelab ext logs [ext]                             # stream logs for all or one extension
```

The only extension in cluster mode is `cf`: cloudflared, a Swarm service
pinned to the core node. Its own command:

```bash
homelab cf status                                  # tunnel status
homelab cf logs                                    # docker service logs of cloudflared
homelab cf route add <service>                     # proxied CNAME → <tunnel>.cfargotunnel.com via the API (enable --cf does it)
homelab cf route rm <service>                      # remove that CNAME (disable --cf does it)
```
