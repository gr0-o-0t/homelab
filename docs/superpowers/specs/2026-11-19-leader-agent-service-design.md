# The leader agent as a Swarm service — design

Date: 2026-11-19. Status: draft for the owner's approval.

## Why

On 2026-11-18 the leader agent on minis stayed down after an engine restart
and the cluster lost its CLI, its edge and every way to diagnose itself
(`connect to the agent at homelab.<tailnet>:7443: … i/o timeout`).

The engine log on minis shows the chain:

1. 15:06 engine restart. The agent container (`docker run`, restart
   `unless-stopped`) is attached to the `home-services` overlay, so the engine
   restarts it only once Swarm is up, through a network attachment request to
   the manager. Swarm's IPAM still held the container's previous address
   (`could not allocate IP from IPAM: Address already in use`); the attach
   timed out (`attaching to network failed … context deadline exceeded`).
2. The engine does not retry a restart-policy container whose start failed at
   boot. A manual `docker start` at 18:42 failed the same way; the container
   was later removed by hand, and the 2026-11-19 reboot had nothing to start.
3. Every Swarm service on the same overlay came back by itself: Swarm
   allocates a task's network as part of scheduling it and retries a failed
   task forever.

A runtime `docker network connect` does not avoid this: the engine persists
it in the container's config and re-attaches it on every start, boot
included. Something must retry the agent's start, and the owner wants no
extra service for it. Swarm already is that something for every other
workload.

## Decisions taken with the owner

- The homelab must come back after any engine restart or reboot of the
  leader without anyone touching it.
- No extra service, sidecar or watchdog: the leader agent itself becomes the
  Swarm service, replacing its container.
- New nodes joining the cluster (workers) must be considered: what they run,
  how they upgrade, and that nothing of the leader's change leaks to them.

## Goals

1. The leader agent runs as the Swarm service `homelab_agent` (1 replica,
   pinned to the manager), not as a plain container.
2. After an engine restart or reboot of minis the agent, its edge and the
   remote CLI return with no manual step.
3. Existing clusters move to it by `homelab upgrade`, with no manual step and
   a rollback to the running container on failure.
4. Worker agents are unchanged: a plain container, no overlay, upgraded by
   the existing helper swap.
5. `init`, `join`, `upgrade`, `version --agent`, `caddy logs`, `backup`,
   `status`, the TUI and the GUI work as before.

## Non-goals

- Worker agents as Swarm services (a global service). They are not on the
  overlay, so they do not have the failure; changing them adds risk for
  nothing.
- More than one manager, or leader failover.
- Changing the agent's state layout, the vault, the tsnet identity or the
  remote CLI protocol.

## Anti-goals

- Two leader agents running at once, even briefly: both would bring up the
  same tsnet node (`homelab`) from the same state and contend for the vault.
  Every path below is stop-first.
- Any path that removes or recreates the `homelab-agent`, `homelab-shim` or
  `homelab-backups` volumes.

## Design

### 1. The service

`bootstrap.LeaderServiceSpec(img)` builds it from the same pieces as
`AgentSpec(img, true)` (one source of truth for mounts and env):

| Field | Value |
|---|---|
| Name | `homelab_agent` (constant `bootstrap.AgentService`) |
| Labels | `homelab.namespace=agent`, `homelab.service=agent`, `homelab.role=core` (the `cloudflared` pattern; `agent` becomes a reserved service name) |
| Image / Args | the agent image, `["agent"]` |
| Env | `XDG_DATA_HOME`, `XDG_CONFIG_HOME` as today |
| Mounts | bind `/var/run/docker.sock`; volumes `homelab-agent` → `/var/lib/homelab`, `homelab-shim` → `/hl`, `homelab-backups` → `/var/lib/homelab-backups` |
| Network | `home-services`, alias `homelab-edge` |
| Placement | `node.role==manager` **and** the core label (`swarm.Constraints(RoleCore, …)`): it can never be scheduled on a worker |
| Replicas | 1 |
| Restart policy | condition `any`, delay 5 s, no max attempts |
| Update / rollback | parallelism 1, order **stop-first**, monitor 30 s, failure action `rollback` |

Swarm's stop-first update with monitor and rollback replaces what
`RunReplace` does for the leader today (stop old, start new, watch it, put
the old one back on failure). A task that exits within the monitor window
rolls the service back to the previous image.

### 2. Finding the leader agent's container

Several commands address the agent by its container name. On the leader that
name no longer exists: the task container is `homelab_agent.1.<task-id>`.
One helper resolves it everywhere:

`bootstrap.AgentContainerID(ctx, api) (string, error)`: the running
container labelled `com.docker.swarm.service.name=homelab_agent` on this
engine, else the container named `homelab-agent` (workers, and a leader not
yet converted).

| Consumer | Today | After |
|---|---|---|
| `cmd/version.go` `agentBootID` (upgrade proof of replacement) | inspect `homelab-agent` | `AgentContainerID`. A new task is a new container ID, so the upgrade check is unchanged. |
| `cmd/caddy.go` `agentLogs` | `docker logs homelab-agent` | leader: `docker service logs homelab_agent` (keeps logs across task restarts); worker: as today |
| `cmd/backup.go` `agentImage` | inspect `homelab-agent` | `AgentContainerID`, then its image |
| `bootstrap` seed / restart / import copy (`seedExec`, `restartAgent`, `CopyToContainer`) | container name | `AgentContainerID`; `restartAgent` on the leader is a service force-update |
| status core row, TUI, GUI, tailscale layer (`CoreStates("homelab-agent")`, `ContainerStatus`) | container state by name | state of the resolved container. The row key stays `homelab-agent` so `status --json` is unchanged for the front ends. |
| hints in errors (`see docker logs homelab-agent`) | fixed text | on the leader, `docker service logs homelab_agent` |

### 3. Boot: a leader agent in a plain container converts itself

When the agent starts with the leader role (engine `ControlAvailable`) and
`AgentContainerID` shows it is a plain container rather than a task of
`homelab_agent`, it converts itself before doing anything else. That covers
every way a plain leader container can exist: the upgrade from an older
version (section 4), an `init` by an older CLI, and the manual recovery
runbook.

The agent cannot stop itself and keep working, so it reuses the replace
helper. A new helper mode, `agent replace-helper` with a spec of kind
`to-service`:

1. waits `ReplaceDelay`, stops the old container, renames it
   `homelab-agent-old` (intact) — exactly as `RunReplace` does today;
2. creates `homelab_agent` from `LeaderServiceSpec(img)` (the image the
   container ran), or, if the service already exists, sets it to that image
   and replicas 1;
3. waits for a task of the service to run without restarting for the settle
   window (`waitStable` on the task's container), within `WaitBack`;
4. on success removes `homelab-agent-old` and itself; on failure removes the
   service (only if step 2 created it; an existing one is scaled to 0),
   renames the old container back and starts it, and stays exited for
   `docker logs homelab-agent-replace`, as today.

Guards, each with a test:

- A worker never converts: the role check runs first, and the helper
  refuses when the engine is not a manager.
- Never two leaders: the plain container is stopped before the service
  exists or is scaled up; the service is removed or scaled to 0 before the
  old container restarts.
- A plain leader container that finds `homelab_agent` already running a task
  does not start its edge or tsnet node. It logs that the service is the
  leader agent and removes itself through the helper (steps 1 and 4 only).
- Conversion runs at most once per container: the helper is labelled, and a
  leftover helper or `homelab-agent-old` blocks a new attempt with the same
  message `upgrade` prints today.

### 4. `init`, `upgrade` and `join`

**`init`** (new clusters): after `EnsureNetwork` it creates the service
instead of the container, waits for its task container, then seeds it through
`AgentContainerID` (the same exec of `agent-seed` with secrets on stdin) and
restarts it with a force-update. `removeAgentAndState` on failure removes the
service instead of the container. The leftover check in `preflight` also
refuses an existing `homelab_agent` service.

**`upgrade`** (`bootstrap.Upgrade`): pulling on the workers and replacing
them one by one is unchanged. The leader's step becomes:

- leader already a service: `ServiceUpdate` of `homelab_agent` to the new
  image (stop-first, monitor, rollback). It prints `LeaderReplaceMarker`
  first; the session ends when Swarm stops the current task, and the PC
  reconnects and checks the version and the changed container ID as today.
- leader still a plain container (the first upgrade onto this design, run by
  the old code): the old code does its usual container swap to the new
  image, and the new agent converts itself at boot (section 3). That costs
  one extra restart, once. The PC's reconnect window covers a swap plus a
  conversion; check this with a test of the timing budget, and widen the
  60 s window if it does not.

`agent self-replace` stays refused on the leader. It is the worker path only.

**`join`** (new nodes), the owner's question: a joining PC gets a worker
agent exactly as today. `StartAgent(img, false)` creates a plain container
with no networks, restart `unless-stopped`. Its boot never attaches to an
overlay, so the engine's start at boot cannot fail the way the leader's did.
Specifically:

- Placement: the service's `node.role==manager` + core constraint means
  Swarm never schedules the leader agent onto a joined node, whatever its
  labels.
- Role: a worker's agent sees `ControlAvailable=false` and runs `runWorker`.
  It never reaches the conversion in section 3, and the helper refuses on a
  non-manager as a second guard.
- Upgrade: workers keep the helper container swap and are upgraded first, as
  today. Their `version --agent` resolves to the `homelab-agent` container
  through `AgentContainerID`.
- Endpoint: workers and clients reach the leader at `homelab.<tailnet>:7443`.
  The tsnet identity lives in the `homelab-agent` volume, which the service
  mounts, so the endpoint does not change.
- Reboot of a worker: unchanged. Its agent container and its Swarm tasks
  come back on their own.
- Test: `AgentSpec(img, false)` has no `EndpointsConfig` (pins the property
  that keeps workers safe).

### 5. Everything that walks Swarm services

The agent service carries the reserved namespace `agent`, so
per-service code paths never match it: `down --all`, `status`, the
rebalancer (core-pinned anyway), `backup`'s scale-down, `prune`. Add `agent`
to the reserved service names next to `cloudflared`, and add a test that
`down --all` and the rebalancer leave `homelab_agent` alone.

### 6. Docs

- `docs/reference/internals.md` § The agent: the leader runs as
  `homelab_agent`; workers as the `homelab-agent` container.
- `docs/reference/cli.md` § Cluster: upgrade of the leader by service update;
  "Recovering a leader with no agent" rewritten. Primary path
  `docker service ps homelab_agent` / `docker service logs homelab_agent` /
  `docker service update --force homelab_agent`. If the service is gone, the
  documented `docker create … agent` still works, and the agent converts
  itself.
- `docs/migrating-to-swarm.md` rollback: `docker service rm homelab_agent`
  next to `docker rm -f homelab-agent`.

## Constraints

- Swarm only; the Docker SDK is read-only except in `internal/swarm` and
  `internal/bootstrap`, as today.
- Tests never reach a real daemon: everything above goes through
  `swarm.API` with `swarmtest.Fake`. The fake needs service tasks to back
  containers (task label on the created container) for
  `AgentContainerID` and the conversion tests.
- No secret in the service spec, args or labels. Seeding stays an exec with
  stdin.

## Verification

- Unit: `LeaderServiceSpec` fields; `AgentContainerID` (service task, plain
  container, none); conversion success / failure / rollback / service already
  exists / worker refuses; `Upgrade` leader-as-service path and the marker;
  `init` creates the service; `down --all` skips it; worker `AgentSpec` has no
  network.
- Live on minis + leno, in this order:
  1. `homelab upgrade --image homelab:dev`: the leader converts, and
     `docker service ps homelab_agent` shows one running task;
  2. `rc-service docker restart` on minis: the agent returns with no manual
     step, and `homelab status` works from leno;
  3. reboot minis: same;
  4. `docker kill` the agent's task container: Swarm restarts it;
  5. on leno, `homelab upgrade --image homelab:dev` again: the worker swaps
     and the leader is updated through the service.

## Release

Minor version (v0.6.0): the leader's runtime form changes and the first
upgrade runs the one-time conversion. The changelog names the one extra
leader restart and the new recovery commands.

## Left to the builder

- Whether `restartAgent` on the leader force-updates the service or kills the
  task container (both restart it; force-update leaves a trace in
  `service ps`).
- The exact settle and monitor values. Keep today's `Settle` / `WaitBack`
  unless a live test shows a cold leader boot (Caddy, tsnet, certificates)
  needs longer than 30 s to count as stable.
