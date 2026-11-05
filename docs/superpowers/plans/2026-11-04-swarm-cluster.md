# homelab on Docker Swarm Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Docker Compose backend with one `homelab` agent container per PC on top of Docker Swarm, with embedded Caddy + caddy-l4 + Tailscale (`tsnet`), controlled from any PC by the same binary.

**Architecture:** The leader agent (on the Swarm manager, minis) owns a state directory laid out exactly like today's config dir (`config.yaml`, `services/<svc>/…`, `caddy/sites`, `caddy/l4`), so `config`, `exposure`, `configgen` and `routing` keep working with `root = /var/lib/homelab/state`. Secrets move from the desktop keyring to an encrypted file vault behind the existing `secrets.Manager`. The CLI forwards every non-local command to the leader, which runs the existing cobra command against its state under a pty and streams it back; lifecycle commands drive Swarm through the Docker SDK instead of shelling `docker compose`.

**Tech Stack:** Go 1.25, Docker Engine SDK (`github.com/docker/docker` v28, swarm types), `github.com/compose-spec/compose-go/v2` (compose loader), Caddy v2 as a library + `github.com/mholt/caddy-l4` + `github.com/caddy-dns/cloudflare` + `github.com/tailscale/caddy-tailscale`, `tailscale.com/tsnet`, `github.com/creack/pty`, `github.com/gorilla/websocket`.

**Spec:** `docs/superpowers/specs/2026-11-04-swarm-cluster-design.md`

## Global Constraints

- Compose mode is removed, not kept alongside (spec: "No dual mode").
- One container per PC: the `homelab` agent; hosts need only Docker Engine (rootful) and their existing Tailscale.
- Secrets never in plain text on disk, in a stack/service spec, or in env visible via `docker service inspect`. Leader secrets: AES-256-GCM vault in the agent volume; key file `/var/lib/homelab/vault.key`, mode `0400`, root-owned. No unlock prompt; Swarm autolock off.
- Nothing permanent on laptop/desktop: data volumes, host bind paths, the docker socket and shared DB providers pin to `node.labels.homelab.role == core`.
- Overlay network `home-services`: driver `overlay`, `--attachable`, **not** `encrypted` (Docker's IPsec rule fails on minis' iptables-nft; spike S3), MTU `1230` (VXLAN inside WireGuard). Encryption between machines comes from the data path riding Tailscale: advertise and data-path addresses must be tailnet IPs (100.64.0.0/10).
- Swarm `--advertise-addr` / `--data-path-addr` = the host's tailnet IPv4 on every node; `init`/`join` refuse an address outside 100.64.0.0/10.
- Raw listen ports are cluster-unique per protocol; reserved: tcp 80, 443, 8081, 2019 (carried over from `internal/routing/routing.go`).
- Day-one layers: `ts`, `cf`. tor/i2p/ygg are disabled in the registry until a later plan; `enable --tor|--i2p|--ygg` fails with "not available in cluster mode yet".
- Tests never reach a real Docker daemon or tailnet: every new package that touches Docker takes an interface and has fakes; packages with a `TestMain` keep the `DOCKER_HOST` guard.
- Every new runnable command/flag gets an `internal/actions` entry or a commented exclusion in `cmd/actions_parity_test.go`.
- Agent and CLI share `cmd.Version`; a mismatched major.minor is refused.
- Agent image: `ghcr.io/<owner>/homelab:<version>` built from the static headless binary.

## Review Focus

1. **A worker vanishes mid-operation** (laptop lid closed while `up` deploys onto it): the command must still finish, the constraint drops so Swarm reschedules onto minis, and no route breaks — test in Task 9 (`TestRebalancer_LastWorkerDownDropsConstraint`, `TestRebalancer_WorkerDownDoesNotTouchPinned`) and Task 8 (`TestDeploy_StatelessOnMinisWhenNoWorker`).
2. **Two PCs mutate at once** (`enable` from laptop while `up` runs from desktop): leader serialises mutating commands; the second waits, never interleaves state writes — test in Task 13 (`TestRunner_MutatingCommandsSerialise`).
3. **Leader agent restarts** (minis reboot): routes, raw ports and cloudflared come back from state before anything else, without a login and without the CLI — test in Task 11 (`TestEdge_LoadsAllServicesOnStart`).
4. **Secret values with `=`, newlines, quotes or leading spaces** survive the env shim byte-for-byte — test in Task 7 (`TestEnvShim_RoundTripsAwkwardValues`).
5. **Version skew** (laptop CLI upgraded, minis agent not): clear refusal naming both versions — test in Task 13 (`TestClient_RefusesVersionMismatch`).

---

## Phase 0 — Spikes (throwaway; gate the rest)

### Task 0: Verify the three risky assumptions

Throwaway code under `spikes/` (deleted at the end of the task, findings kept). Each spike must end in a yes/no recorded in the spec's "Risks to verify early" section, with the working config snippet.

- [ ] **S1 Embedded edge.** A 60-line Go `main` that imports `caddy/v2/modules/standard`, caddy-l4, caddy-dns/cloudflare and caddy-tailscale, adapts an in-memory Caddyfile with one site bound via `bind tailscale/homelab` and one layer4 server listening on `tailscale/homelab:2222` proxying to a local `nc -l`, and serves both on a throwaway tailnet node (ephemeral auth key). Yes = `curl https://homelab.<tailnet>.ts.net` and `ssh -p 2222` both reach through. If caddy-l4 cannot listen on a `tailscale/` network address, record the fallback: the agent opens `tsnet.Server.Listen("tcp", ":2222")` itself and hands the listener to Caddy via `caddy.RegisterNetwork("hl-ts", …)`.
- [ ] **S2 Swarm naming.** On a local single-node swarm: create overlay `home-services` (attachable, encrypted, MTU 1230); `docker stack deploy` two stacks where stack A's service `postgres` has network alias `homelab-postgres`; from stack B and from a plain `docker run --network home-services` container, resolve and connect to `homelab-postgres:5432` and `postgres`. Yes = both names resolve from both.
- [ ] **S3 Swarm over tailnet.** minis `docker swarm init --advertise-addr <ts ip> --data-path-addr <ts ip>`; laptop joins with its ts IP; run `nginx` pinned to the laptop on `home-services`; `curl` it from a container on minis; then suspend the laptop 2 minutes and resume. Yes = traffic works, node goes `Down` then `Ready`, no manual step. Leave the swarm afterwards (`docker swarm leave --force` on both).
- [ ] **Record findings** in the spec, delete `spikes/`, commit: `docs(spec): Record swarm/edge spike findings`.

If any spike says no, stop and bring the finding back to the owner before Phase 1.

---

## Phase 1 — Secrets and state

### Task 1: Vault keyring backend

**Files:**
- Create: `internal/vault/vault.go`, `internal/vault/vault_test.go`
- Modify: `internal/secrets/keyring.go` (add `OpenVault`)

**Interfaces:**
- Produces: `vault.Open(path, keyFile string) (*vault.Vault, error)` — creates both files if absent (key: 32 random bytes, mode 0400; data: mode 0600). `*Vault` implements `github.com/99designs/keyring.Keyring` (`Get`, `GetMetadata`, `Set`, `Remove`, `Keys`). Whole-file AES-256-GCM, random 12-byte nonce per write, atomic write (temp + rename), in-process mutex.
- Produces: `secrets.OpenVault(path, keyFile string) (*secrets.Manager, error)` — a `Manager` over the vault (same namespaces `root:`/`svc:` as today).

- [ ] **Step 1: Write failing tests** `TestVault_RoundTrip` (Set/Get/Keys/Remove across a reopen), `TestVault_CiphertextHasNoPlaintext` (file bytes do not contain the value `s3cr3t-value`), `TestVault_KeyFileMode0400`, `TestVault_WrongKeyFails` (reopen with a different key file → error, not empty vault), `TestOpenVault_ManagerNamespaces` (`m.Set(secrets.RootKey("CF"),…)` then `Get` works).
- [ ] **Step 2:** `go test ./internal/vault/ ./internal/secrets/` → FAIL (undefined).
- [ ] **Step 3: Implement** `internal/vault/vault.go` and `secrets.OpenVault`.
- [ ] **Step 4:** same command → PASS.
- [ ] **Step 5: Commit** `feat(vault): Add an encrypted file keyring for the agent`.

### Task 2: Agent state root and path resolution

**Files:**
- Create: `internal/agentstate/agentstate.go`, `_test.go`

**Interfaces:**
- Produces: `const agentstate.Root = "/var/lib/homelab"`; `agentstate.Paths{StateDir, VaultFile, KeyFile, ClusterFile string}` from `agentstate.At(root string) Paths` (`state/`, `vault.bin`, `vault.key`, `cluster.yaml`).
- Produces: `agentstate.Cluster{Secret string /* hex 32 bytes */; LeaderAddr string; TSHostname string}` with `LoadCluster(p Paths) (Cluster, error)` / `SaveCluster(p Paths, c Cluster) error` (cluster.yaml holds only non-secret fields; `Secret` lives in the vault under `root:CLUSTER_SECRET` — Save writes it there).
- `StateDir` is used as the config dir (`--config-dir`) for every existing package.

- [ ] **Step 1: Failing tests** `TestAt_Layout`, `TestCluster_SecretNotInYAML` (cluster.yaml bytes lack the secret), `TestCluster_RoundTrip`.
- [ ] **Step 2–4:** run `go test ./internal/agentstate/` FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(agentstate): Add the agent's state layout`.

---

## Phase 2 — Swarm backend

### Task 3: Catalog markers for placement and cache volumes

**Files:**
- Modify: `internal/config/config.go` (service schema), `internal/config/config_test.go`
- Modify: `assets/services/immich/config.yaml`, catalog test in `assets/` (whichever file holds `TestCatalogServices_*`)

**Interfaces:**
- Produces on the service config type: `Placement map[string]string \`yaml:"placement,omitempty"\`` — compose service name → `core|worker|any`; and `CacheVolumes []string \`yaml:"cache_volumes,omitempty"\`` — compose volume keys that are re-creatable.
- immich: `cache_volumes: [model-cache]`, `placement: {immich-machine-learning: worker}` (use the compose keys as they appear in `assets/services/immich/docker-compose.yml`).

- [ ] **Step 1: Failing tests** `TestServiceConfig_PlacementAndCache` (parses both keys), `TestServiceConfig_PlacementRejectsUnknown` (`placement: {x: gpu}` → error naming `gpu`), catalog test `TestCatalogServices_PlacementKeysExist` (every placement/cache key names a service/volume in that service's compose file).
- [ ] **Step 2–4:** `go test ./internal/config/ ./assets/...` FAIL → implement + edit immich → PASS.
- [ ] **Step 5: Commit** `feat(config): Declare per-container placement and cache volumes`.

### Task 4: Placement rules

**Files:**
- Create: `internal/swarm/placement.go`, `placement_test.go`

**Interfaces:**
- Consumes: Task 3 fields; compose-go `types.ServiceConfig`.
Swarm has no soft affinity (`spread` balances, it does not prefer), so "prefer workers, fall back to minis" is a constraint the agent switches: Task 8 applies it, Task 9 flips it on node events.

- Produces: `type Role string` (`RoleCore="core"`, `RoleWorker="worker"`, `RoleAny="any"`); `func Place(svc types.ServiceConfig, cfg *config.ServiceConfig, isDBProvider bool) Role`:
  - `RoleCore` if: any named volume not in `CacheVolumes`; any bind mount (`type: bind`); a mount of `/var/run/docker.sock`; `isDBProvider`; or override `core`. Data wins over an override `worker`.
  - `RoleAny` for override `any`.
  - otherwise `RoleWorker` (stateless default and override `worker`).
- Produces: `func Constraints(r Role, workerReady bool) []string` — core → `["node.labels.homelab.role==core"]`; worker → `["node.labels.homelab.role==worker"]` when `workerReady`, else `nil` (runs on minis); any → `nil`.

- [ ] **Step 1: Failing table tests** `TestPlace`: named volume → core; cache-only volume → worker; bind `./config` → core; docker.sock → core; db provider → core; override core on stateless → core; override worker on a data-volume service → core; override any → any. `TestConstraints`: the four role/readiness combinations above.
- [ ] **Step 2–4:** `go test ./internal/swarm/ -run TestPlace` FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(swarm): Decide per-container placement`.

### Task 5: Docker Swarm API seam and fake

**Files:**
- Create: `internal/swarm/api.go`, `internal/swarm/fake_test.go` (exported fake in `internal/swarm/swarmtest/fake.go` so other packages' tests can use it)

**Interfaces:**
- Produces: `type API interface` — the subset used: `ServiceList, ServiceInspectWithRaw, ServiceCreate, ServiceUpdate, ServiceRemove, TaskList, NodeList, NodeUpdate, NetworkList, NetworkCreate, SecretList, SecretCreate, SecretRemove, ConfigList, ConfigCreate, ConfigRemove, VolumeList, ImageInspectWithRaw, ImagePull, SwarmInit, SwarmJoin, SwarmInspect, Events, ContainerList, ContainerExecCreate, ContainerExecAttach` with the Docker SDK v28 signatures (`*client.Client` satisfies it; assert with `var _ API = (*client.Client)(nil)`).
- Produces: `swarmtest.Fake` — in-memory services/nodes/secrets/configs/networks; records calls; `Fake.Nodes` settable.

- [ ] **Step 1:** compile-time assertion test `TestClientSatisfiesAPI`; `TestFake_ServiceCreateThenList`.
- [ ] **Step 2–4:** FAIL → implement → PASS (`go test ./internal/swarm/...`).
- [ ] **Step 5: Commit** `feat(swarm): Add the Swarm API seam and fake`.

### Task 6: Compose → ServiceSpec translation

**Files:**
- Create: `internal/swarm/translate.go`, `translate_test.go`, `internal/swarm/testdata/*.yml`

**Interfaces:**
- Consumes: `Place` (Task 4).
- Produces: `type Stack struct{ Name string; Services []swarm.ServiceSpec; Volumes []string /* external named volumes to ensure */; Configs []FileConfig }`, `type FileConfig struct{ Service, Target, Source string /* path under the service dir */; Mode os.FileMode }`, and `func Translate(svcDir string, env map[string]string, cfg *config.ServiceConfig, dbProvider bool) (Stack, error)`:
  - load `docker-compose.yml` with compose-go, interpolating `env`;
  - service spec name `<svc>_<compose service>`; labels `com.docker.stack.namespace=<svc>`, `homelab.service=<svc>`;
  - networks: every service attaches to `home-services` with aliases `[<compose key>, <container_name if set>]`; compose networks marked `internal: true` become per-stack overlay `<svc>_internal` (`internal`, MTU 1230, not encrypted — see Global Constraints);
  - named volumes → `mount.TypeVolume` with source = compose `name:` if set (keeps `gitea_data` etc.), else `<svc>_<key>`;
  - bind mounts of files/dirs under the service dir → `FileConfig` entries (Task 16 turns them into Swarm configs); bind mounts outside it (`/mnt/media`, `/etc/localtime`, docker.sock) stay binds;
  - `restart:` → `RestartPolicy` (`always|unless-stopped` → any, `on-failure` → on-failure, `no` → none); `healthcheck`, `user`, `command`, `entrypoint`, `cap_add`, `labels`, `ports` (ignored: exposure is the edge's job), `depends_on` (ignored);
  - `privileged` or `devices` present → error `ErrUnsupported` naming the service (minero).
  - Env is **not** put in the spec; Task 7 adds it.

- [ ] **Step 1: Failing tests** with testdata copies of gitea, immich and glance compose files: `TestTranslate_GiteaAliasesAndVolume` (alias `gitea` on `home-services`, mount source `gitea_data`, no `Env`), `TestTranslate_InternalNetwork`, `TestTranslate_BindUnderServiceDirBecomesConfig` (glance `./config/glance.yml` → FileConfig), `TestTranslate_HostBindStays` (`/etc/localtime`), `TestTranslate_PrivilegedUnsupported`, `TestTranslate_EveryCatalogServiceTranslates` (loops `assets/services/*`, all but minero succeed with a dummy env containing every `${VAR}`).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(swarm): Translate catalog compose files into Swarm services`.

### Task 7: Environment through a secret and an entrypoint shim

**Files:**
- Create: `internal/envshim/envshim.go`, `_test.go`; `cmd/envshim.go` (hidden `homelab envshim -- <argv>`)
- Modify: `internal/swarm/translate.go` (apply shim)

**Interfaces:**
- Produces: `envshim.Encode(env map[string]string) []byte` / `envshim.Decode([]byte) (map[string]string, error)` — length-prefixed `key\x00value\x00` records (no escaping ambiguity).
- Produces: `swarm.WithEnv(spec *swarm.ServiceSpec, secretName string, origEntrypoint, origCmd []string)` — sets `ContainerSpec.Command = ["/hl/homelab","envshim","--", <orig entrypoint+cmd>...]`, `Args = nil`, mounts volume `homelab-shim` read-only at `/hl`, adds `SecretReference{SecretName: secretName, File: {Name: "/run/secrets/homelab-env", Mode: 0444, UID: "0", GID: "0"}}`.
- `homelab envshim -- argv…`: read `/run/secrets/homelab-env`, `Decode`, merge over `os.Environ()`, `syscall.Exec(argv[0] resolved via PATH, argv, env)`.
- Secret naming: `hl_<svc>_env_<first 12 hex of sha256(content)>` (content-addressed so updates roll; old ones removed after update).
- Entrypoint/cmd of an image comes from `ImageInspectWithRaw` after `ImagePull` on the leader (Task 8 calls it). Precedence (ruling R17): compose `entrypoint`/`command` already in the spec (`ContainerSpec.Command`/`Args` set by Translate) win; the image's Entrypoint/Cmd only fill whichever compose left unset. Test `TestWithEnv_ComposeCommandBeatsImage`.
- The secret's content is exactly `Stack.Env[spec.Name]` (ruling R15: the container's resolved compose `environment:`), encoded with `Encode`.

- [ ] **Step 1: Failing tests** `TestEnvShim_RoundTripsAwkwardValues` (values `a=b`, `line1\nline2`, `"q"`, `  lead`, empty), `TestWithEnv_SpecHasNoPlainEnv` (spec JSON lacks every value), `TestWithEnv_CommandWrapsOriginal`, `TestEnvshimCmd_ExecsWithMergedEnv` (exec a test helper binary via `os.Args[0]` re-exec pattern; assert it sees the decoded vars).
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(envshim): Pass service env through a Swarm secret`.

### Task 8: Deployer — the lifecycle verbs on Swarm

**Files:**
- Create: `internal/swarm/deploy.go`, `deploy_test.go`
- Modify: `cmd/up.go`, `cmd/down.go`, `cmd/start.go`, `cmd/stop.go`, `cmd/restart.go`, `cmd/pull.go`, `cmd/update.go`, `cmd/delete.go`, `cmd/service_deps.go` (replace `run.Commander.DockerComposeEnv` calls)

**Interfaces:**
- Consumes: `API` (Task 5), `Translate`/`WithEnv` (Tasks 6–7), `config.BuildEnv` (unchanged signature, `sm` from `secrets.OpenVault`).
- Produces: `type Deployer struct{ API API; Root string; SM *secrets.Manager }` with
  `EnsureNetwork(ctx) error` (Global Constraints values),
  `Up(ctx, svc string) error` (pull images, translate with `secretValues` = the values BuildEnv took from the vault (ruling R16; extend BuildEnv or add a sibling that also reports which keys are secret-sourced), create/update services, env secret per container from `Stack.Env[spec.Name]` (R15), replicas 1, ensure networks (`Stack.Networks`)+volumes+secrets+configs, prune stale secrets),
  `Down(ctx, svc string) error` (replicas 0; spec and volumes kept),
  `Restart(ctx, svc string) error` (`ForceUpdate++`),
  `Pull(ctx, svc string) error`, `Update(ctx, svc string) error` (pull + Up),
  `Remove(ctx, svc string) error` (services, its secrets/configs; volumes kept),
  `Status(ctx, svc string) ([]TaskStatus, error)` with `TaskStatus{Service, Node, State string}`.
- `down` and `stop` both map to `Down` (Swarm has no stopped-but-kept containers); `start` maps to `Up`.

- `Up` labels each spec `homelab.role=<Role>` and sets `Constraints(role, anyWorkerReady)` from `NodeList`.

- [ ] **Step 1: Failing tests** against `swarmtest.Fake`: `TestDeploy_UpCreatesThenUpdates`, `TestDeploy_DownScalesToZeroKeepsVolumes`, `TestDeploy_EnsureNetworkValues` (not encrypted, attachable, mtu 1230), `TestDeploy_StatelessOnMinisWhenNoWorker` and `TestDeploy_StatelessConstrainedToWorkerWhenReady`, `TestDeploy_StaleEnvSecretRemoved`, `TestDeploy_RemoveKeepsVolumes`.
- [ ] **Step 2–4:** `go test ./internal/swarm/` FAIL → implement → PASS.
- [ ] **Step 5:** switch the cmd files to `Deployer`; `go test ./cmd/...` PASS (update cmd tests that asserted compose argv to assert Fake calls).
- [ ] **Step 6: Commit** `feat(swarm): Run the lifecycle verbs on Swarm`.

### Task 9: Shared-database provisioning on Swarm, and the rebalancer

**Files:**
- Modify: `internal/db/provisioner.go` (`containerName` → resolver), `internal/db/*_test.go`
- Create: `internal/swarm/rebalance.go`, `rebalance_test.go`

**Interfaces:**
- Produces: `Provisioner.ContainerFor func(dbType config.DBType) (string, error)` — default resolves the running task container of service `<provider>_<provider>` on the local node via `ContainerList` filter `label=com.docker.swarm.service.name=…`; `EnsureRunning` calls `Deployer.Up(provider)`. `docker exec` shell-out stays (leader runs on minis where providers are pinned).
- Produces: `type Rebalancer struct{ API API; Debounce time.Duration /* default 2m */ }`, `Run(ctx) error` — on any node event for a `homelab.role=worker` node, recompute `anyWorkerReady`; when it changes and stays changed for `Debounce`, update every service labelled `homelab.role=worker` with `Constraints(RoleWorker, anyWorkerReady)`. Going down (`Ready→Down`) uses a 20 s debounce instead, so work returns to minis quickly; coming up uses `Debounce`.

- [ ] **Step 1: Failing tests** `TestProvisioner_ResolvesSwarmTaskContainer`, `TestRebalancer_DebouncesFlapping` (Ready/Down/Ready within debounce → no update), `TestRebalancer_LastWorkerDownDropsConstraint`, `TestRebalancer_WorkerDownDoesNotTouchPinned`, `TestRebalancer_SecondWorkerReadyIsNoOp`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(swarm): Provision shared databases and rebalance onto workers`.

---

## Phase 3 — Edge

### Task 10: Disable unsupported layers; keep exposure rendering

**Files:**
- Modify: `internal/network/layers/*.go` (registry), `cmd/enable.go`, `cmd/disable.go`, tests

**Interfaces:**
- Produces: `layers.New(...)` registers `ts` and `cf` only; `enable --tor|--i2p|--ygg` → error `"<layer> is not available in cluster mode yet"`.
- ts layer's site address gains `bind tailscale/homelab` (or the S1 fallback network) — `HostTemplate` gets a `Bind string` field emitted as a `bind` line inside the block.
- cf sites stay `http://<host>.{$DOMAIN}` on `:80`, bound to the overlay interface address (`bind {$HL_OVERLAY_IP}`, set by the agent from its own `home-services` endpoint).
- l4 listen addresses: `tailscale/homelab:<port>` (or the S1 fallback).

- [ ] **Step 1: Failing tests** `TestEnable_TorRefusedInClusterMode`, `TestRender_TailnetSiteHasBind`, `TestRenderL4_ListensOnTailnet`.
- [ ] **Step 2–4:** FAIL → implement → PASS; update goldens with `-update` and review the diff.
- [ ] **Step 5: Commit** `feat(routing): Bind tailnet sites and raw ports to the agent's tailnet node`.

### Task 11: Embedded Caddy + tsnet

**Files:**
- Create: `internal/edge/edge.go`, `internal/edge/caddyfile.go`, `edge_test.go`
- Modify: `internal/caddy/manager.go` (`Reload` → `edge.Reload`), `internal/caddy/rollback.go` (keep Snapshot; `ErrInvalidConfig` now from adapt/load)
- Modify: `go.mod` (caddy, caddy-l4, caddy-dns/cloudflare, caddy-tailscale, tsnet)

**Interfaces:**
- Produces: `edge.Assemble(root string) (string, error)` — the full Caddyfile text: today's `assets/caddy/Caddyfile` global block (embedded) with `layer4 { <every caddy/l4/*.conf> }` inlined and every `caddy/sites/*.conf` appended (no `import` globs; no bind mounts).
- Produces: `type Edge struct{ Root string; TSAuthKey, TSHostname, TSStateDir string }`, `Start(ctx) error` (start tsnet; `Reload`), `Reload() error` (Assemble → caddyfile adapter → `caddy.Load(json, false)`; on adapt/load error wrap `caddy.ErrInvalidConfig` and keep the running config), `Stop() error`.
- Env placeholders (`{$DOMAIN}`, `{$CLOUDFLARE_API_TOKEN}`) are resolved by setting process env from `BuildEnv(root, "")` before adapting; the token never touches disk.

- [ ] **Step 1: Failing tests** `TestAssemble_InlinesL4AndSites` (temp root with one sites and one l4 file → text contains both, no `import /homelab`), `TestEdge_InvalidConfigKeepsPrevious` (Reload with a broken site → `ErrInvalidConfig`, `caddy.ActiveContext` config unchanged; run Caddy with admin off and no tsnet: a test constructor `newForTest` that skips tsnet and binds `localhost:0`), `TestEdge_LoadsAllServicesOnStart`.
- [ ] **Step 2–4:** `go test ./internal/edge/ ./internal/caddy/` FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(edge): Run Caddy, caddy-l4 and Tailscale inside the agent`.

### Task 12: cloudflared as a Swarm service

**Files:**
- Create: `internal/swarm/cloudflared.go`, `_test.go`
- Modify: `cmd/tunnel.go`, `internal/network/cf/layer.go` (no container start; Deployer)

**Interfaces:**
- Produces: `Deployer.EnsureCloudflared(ctx, token string) error` — service `homelab_cloudflared`, image `cloudflare/cloudflared:<pinned tag>`, pinned core, on `home-services`, `tunnel --no-autoupdate run`, token via env shim secret (Task 7), origin `http://homelab-edge:80` (the leader agent's alias on the overlay).
- `RemoveCloudflared(ctx) error` when the token is unset.

- [ ] **Step 1: Failing tests** `TestCloudflared_TokenNotInSpec`, `TestCloudflared_PinnedCore`, `TestCloudflared_RemovedWithoutToken`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(cf): Run cloudflared as a Swarm service`.

---

## Phase 4 — Agent, remote CLI, cluster bootstrap

### Task 13: Remote runner (agent side) and client (CLI side)

**Files:**
- Create: `internal/remote/server.go`, `internal/remote/client.go`, `internal/remote/auth.go`, `internal/remote/*_test.go`
- Modify: `cmd/root.go` (dispatch), `cmd/version.go`

**Interfaces:**
- Protocol: HTTPS over the tailnet (tsnet listener `:7443`, cert from tsnet `LocalClient().GetCertificate`), `GET /v1/run` upgraded to websocket. First client frame JSON `{"argv":[…],"tty":bool,"rows":n,"cols":n,"version":"x.y.z","term":"…"}`; then binary frames stdin→, ←stdout; text frames `{"resize":[r,c]}` and final `←{"exit":code}`.
- Auth: header `Authorization: Bearer <hex(HMAC-SHA256(clusterSecret, "homelab-remote-v1"))>`, constant-time compare; 401 otherwise.
- Server runs `os.Args[0] --config-dir <StateDir> --no-color=<!tty> <argv…>` under `creack/pty` when `tty`, plain pipes otherwise; env `HOMELAB_AGENT=1`.
- Mutating commands (`add setup up down start stop restart pull update delete enable disable reload backup restore prune edit`) take one server-wide lock; read-only ones don't.
- `remote.LocalOnly = map[string]bool{"init","join","login","logout","version","completion","agent","envshim","edit"}` — everything else is forwarded by `cmd/root.go`'s `PersistentPreRunE` when not `HOMELAB_AGENT=1`.
- Client endpoint from `~/.config/homelab/client.yaml` (`endpoint: https://homelab.<tailnet>.ts.net:7443`), secret from the local keyring (`secrets.Open()`, key `root:CLUSTER_SECRET`).
- `version` checks: client sends its version; server refuses major.minor mismatch with exit 2 and message `homelab CLI <c> and agent <a> differ — run \`homelab upgrade\``.

- [ ] **Step 1: Failing tests** (httptest server + in-process client): `TestRemote_RejectsBadToken`, `TestRemote_StreamsOutputAndExitCode` (argv runs a re-exec helper printing and exiting 3), `TestRunner_MutatingCommandsSerialise` (two concurrent `up` → non-overlapping intervals), `TestClient_RefusesVersionMismatch`, `TestRoot_ForwardsNonLocalCommands` / `TestRoot_RunsLocalOnlyCommandsLocally`.
- [ ] **Step 2–4:** `go test ./internal/remote/ ./cmd/...` FAIL → implement → PASS.
- [ ] **Step 5:** add actions-parity exclusions for `agent`, `envshim`; actions for `login`, `join`, `init`.
- [ ] **Step 6: Commit** `feat(remote): Run homelab commands on the leader from any PC`.

### Task 14: `homelab agent`

**Files:**
- Create: `cmd/agent.go`, `internal/agent/agent.go`, `_test.go`

**Interfaces:**
- Consumes: `agentstate` (2), `secrets.OpenVault` (1), `Deployer`/`Rebalancer` (8, 9), `Edge` (11), `remote.Server` (13).
- Produces: `homelab agent [--root /var/lib/homelab]` (hidden). Role from `SwarmInspect`/node info: **leader** (manager) → open vault, `EnsureNetwork`, ensure `homelab-shim` volume holds the current binary (copy `os.Args[0]` to `/hl/homelab`, mode 0555), `Edge.Start` (tsnet hostname `homelab`), ensure the Cloudflare DNS record `*.<HOME_SUBDOMAIN>.<DOMAIN>` is an A record (DNS-only) to the tsnet node's IPv4, updating it through the existing cf API client when it differs, `EnsureCloudflared` if token set, bring up desired services (reuse `internal/session` desired-state file semantics: services recorded running → `Deployer.Up`; core-pinned providers first), start `Rebalancer`, serve `remote` on tsnet `:7443`. **worker** → ensure `homelab-shim` volume, tsnet hostname `homelab-<node hostname>`, serve `remote` restricted to `exec`/`logs` for local containers.
- SIGTERM: stop serving, `Edge.Stop`; never scales services down (Swarm keeps them).

- [ ] **Step 1: Failing tests** with Fake + edge test constructor: `TestAgent_WildcardRecordFollowsTailnetIP`, `TestAgent_LeaderStartupOrder` (network → shim → edge → DNS record → cloudflared → providers → others → rebalancer → serve; assert call order), `TestAgent_WorkerServesOnlyLocalExec`, `TestAgent_ShimVolumeHoldsBinary`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(agent): Add the per-PC agent`.

### Task 15: `init`, `join`, `login`, `exec`/`logs` relay

**Files:**
- Create: `cmd/init.go`, `cmd/join.go`, `cmd/login.go`, `internal/bootstrap/bootstrap.go`, `_test.go`
- Modify: `cmd/exec.go`, `cmd/logs.go`

**Interfaces:**
- `homelab init [--import <old-config-dir>]` (on minis, as a user who can run docker): find host tailnet IPv4 (`tailscale ip -4`; error if absent) → `SwarmInit{AdvertiseAddr, DataPathAddr}` → label node `homelab.role=core` → generate cluster secret → `docker run -d --name homelab-agent --restart unless-stopped -v /var/run/docker.sock:/var/run/docker.sock -v homelab-agent:/var/lib/homelab --network home-services --network-alias homelab-edge ghcr.io/<owner>/homelab:<Version> agent` (via SDK) → write the secret into the agent's vault through a one-shot `docker exec homelab-agent homelab agent-seed` reading stdin → store secret in local keyring + write `client.yaml` → print the join command. `--import` handled by Task 17.
- `homelab join` (on a worker): prompt for the cluster secret (or `--secret-stdin`), call leader `remote` with argv `["cluster","join-token"]` (hidden leader command returning worker token, manager addr, and a Tailscale auth key from the vault's `root:TS_AUTHKEY` for the worker's own tsnet node — the key must be reusable; `join` fails with that message if Tailscale rejects it) → `SwarmJoin` with own tailnet IP → leader labels it `homelab.role=worker` (hidden `cluster label <node>`) → start local agent container (no `--network`, no alias) → save client.yaml + secret.
- `homelab login`: prompt secret + endpoint → keyring + client.yaml (for a PC that is a client only).
- `exec`/`logs`: on the leader, find the task's node; if not local, forward the same argv to `homelab-<node>` agent's remote endpoint and pipe it through.

- [ ] **Step 1: Failing tests** with Fake + fake tailscale-ip runner: `TestInit_SwarmInitOnTailnetIP`, `TestInit_SecretNeverInRunArgs` (agent `docker run` spec contains no secret), `TestJoin_LabelsWorker`, `TestExec_ForwardsToTaskNode`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(cluster): Add init, join and login`.

### Task 16: `homelab edit` and bind-config files as Swarm configs

**Files:**
- Create: `cmd/edit.go`, `internal/swarm/configs.go`, tests

**Interfaces:**
- Consumes: `Stack.Configs` (Task 6).
- Produces: `Deployer` creates `ConfigSpec` per `FileConfig`, name `hl_<svc>_<sha12(content)>`, mounted at `Target` with `Mode`; directories are expanded to one config per file at the matching target path; stale configs removed after update.
- `homelab edit <svc> <path>` (local-only): remote `["files","get",svc,path]` → temp file → `$EDITOR` (fallback `vi`) → if changed, remote `["files","put",svc,path]` with stdin → leader writes into `state/services/<svc>/<path>` and runs `Deployer.Up(svc)`. `files get|put` are hidden leader commands; `put` refuses paths escaping the service dir.

- [ ] **Step 1: Failing tests** `TestConfigs_DirectoryExpandsPerFile`, `TestConfigs_ContentAddressedRollover`, `TestFilesPut_RejectsTraversal` (`../../config.yaml`), `TestEdit_UnchangedFileDoesNotPut`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(edit): Edit service config files from any PC`.

---

## Phase 5 — Cutover

### Task 17: Import from a Compose-era config dir

**Files:**
- Create: `internal/importer/importer.go`, `_test.go`
- Modify: `cmd/init.go`

**Interfaces:**
- Produces: `importer.Import(oldDir string, keyring *secrets.Manager, dst agentstate.Paths, vault *secrets.Manager) (Report, error)`: copies `config.yaml`, `services/*/{config.yaml,docker-compose.yml,exposure.yaml,caddy.routes.conf}` and every bind-mounted file/dir under each service dir; copies `redis-dbs.yaml` and `state/desired.yaml`; for every declared root/service secret and generated secret, `keyring.Get` → `vault.Set` (missing ones listed in `Report.MissingSecrets`, not fatal); runs `routing.Migrate` first if legacy `conf.d*` files exist; re-renders sites/l4 via `routing.Sync` for every service with exposure; adds `/tcp` to listen-port declarations that carry no protocol (logs each change in `Report.Changed`).
- `init --import` runs on minis in the logged-in session (keyring open), copies into the new agent's volume via `docker cp` of a tar stream, then restarts the agent.

- [ ] **Step 1: Failing tests** with a temp old dir mirroring minis (gitea with `listen: 2222` mapping form, glance with `./config`, immich): `TestImport_CopiesServicesAndFiles`, `TestImport_MovesSecretsToVault`, `TestImport_ReportsMissingSecrets`, `TestImport_AddsTCPToRawPorts`, `TestImport_RendersExposure`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(import): Import a Compose-era homelab into the cluster`.

### Task 18: Backup and restore on Swarm

**Files:**
- Modify: `internal/backup/*.go`, `cmd/backup.go`, `cmd/restore.go`, tests

**Interfaces:**
- Stop/start around a snapshot becomes `Deployer.Down`/`Up`; volume tar runs in a helper container pinned to core via a one-shot Swarm-less `docker run` on minis (leader is on minis); DB dumps via `Provisioner.ContainerFor`. Manifest format unchanged (`ManifestVersion` stays 1). `--out` is a path inside the agent container mapped to `/var/lib/homelab/backups` by default; add `homelab backup --fetch <name> <local-dir>` streaming a tar to the CLI host via the remote channel.

- [ ] **Step 1: Failing tests** `TestBackup_ScalesDownAndUp`, `TestBackup_ManifestUnchanged`, `TestBackupFetch_StreamsTar`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `feat(backup): Back up and restore on Swarm`.

### Task 19: Remove the Compose era

**Files:**
- Delete: `internal/session/`, `cmd/session.go`, `cmd/migrate.go`, `internal/routing/migrate.go` (after Task 17 inlines what it needs), `assets/core/` (compose, `Dockerfile.caddy`), `assets/caddy/conf.d*/`, compose code in `internal/run` (keep `Commander.Run/Output*` used by db/backup), `internal/docker/compose.go`
- Modify: `assets/assets.go` (CoreFS = embedded Caddyfile global block only), `cmd/actions_parity_test.go`, `internal/actions`, `CLAUDE.md`, `docs/architecture.md`, `docs/adding-a-service.md`, `docs/tailscale-setup.md`

- [ ] **Step 1:** delete/modify; `go build ./... && go build -tags nogui ./...` → succeeds.
- [ ] **Step 2:** `go test ./...` → PASS; `make lint` → clean.
- [ ] **Step 3:** CLAUDE.md: replace Compose/session/core sections with agent/Swarm/remote/edge sections (commands: `init`, `join`, `login`, `edit`, `upgrade`; architecture diagram from the spec).
- [ ] **Step 4: Commit** `refactor!: Remove the Docker Compose backend and session mode`.

### Task 20: Agent image, release, `homelab upgrade`

**Files:**
- Create: `Dockerfile` (`FROM gcr.io/distroless/static-debian12` — the root variant, since the agent needs the docker socket; `COPY` the static headless binary to `/usr/local/bin/homelab`; `ENTRYPOINT ["/usr/local/bin/homelab"]`, `CMD ["agent"]`), `cmd/upgrade.go`, tests
- Modify: `.github/workflows/release.yml` (build + push `ghcr.io/<owner>/homelab:<version>` and `:latest`, amd64 + arm64, from the headless static binaries), `Makefile` (`image` target)

**Interfaces:**
- `homelab upgrade [--version X]` (forwarded to leader): for each worker node then the leader: pull image, recreate `homelab-agent` with the same mounts/flags and new tag (worker agents via their remote endpoint running hidden `agent self-replace`; leader last, which ends the session — client prints "agent restarting" and reconnects within 60 s, then verifies `version`).

- [ ] **Step 1: Failing tests** `TestUpgrade_WorkersBeforeLeader`, `TestUpgrade_PreservesMountsAndAlias`.
- [ ] **Step 2–4:** FAIL → implement → PASS.
- [ ] **Step 5:** `docker build -t homelab:test .` locally → succeeds; `docker run --rm homelab:test version` prints the version.
- [ ] **Step 6: Commit** `build: Publish the agent image and add homelab upgrade`.

### Task 21: Cutover runbook and live verification on minis

**Files:**
- Create: `docs/migrating-to-swarm.md`

- [ ] **Step 1:** write the runbook from the spec's Migration section with exact commands (v0.4.3 `homelab backup --all --out /mnt/…`, copy `~/.config/homelab` aside, `homelab down -a`, core down, install new binary, `homelab init --import ~/.config/homelab`, `homelab up -a`, checks, `homelab join` on laptop and desktop) and the rollback commands.
- [ ] **Step 2 (owner present):** run it on minis. Verify: every enabled service answers `https://<svc>.home.gr00t.fun`; `git -C ~/dev/cards push` over `ssh://git@gitea.home.gr00t.fun:2222/groot/cards.git` succeeds; `docker service inspect` of gitea shows no secret values; reboot minis without logging in → services and routes return; laptop `homelab join` → immich-ml moves to the laptop within debounce; laptop suspended → immich-ml back on minis.
- [ ] **Step 3: Commit** `docs: Add the Swarm migration runbook`.
