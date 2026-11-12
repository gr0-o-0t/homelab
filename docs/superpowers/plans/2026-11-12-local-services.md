# Custom Services with Local Images Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install a custom service from a folder on any PC, ship its locally built image to the leader and roll it on `update`, give another service read-only access to its database, and move taash onto the cluster that way.

**Architecture:** `pull_policy: never` marks a container's image local: the translator labels it and places it on core; the deployer never pulls it and stamps its store-independent identity into a container label, so a changed image rolls the tasks. The CLI's `up`/`update`/`add <dir>` do local pre-work (stream `docker save` / a tar of the folder over the existing remote stdin) and then forward, the same way `upgrade` already does. A `readonly: true` postgres declaration is provisioned as a SELECT-only role on another service's database.

**Tech Stack:** Go, cobra, Docker Engine SDK v28.5.2 (`ImageSave`/`ImageLoad`), compose-go, testify, `swarm/swarmtest` fake.

**Spec:** `docs/superpowers/specs/2026-11-12-local-services-design.md`

## Global Constraints

- Swarm only; secrets never in a spec, argv or label; tests never reach Docker or a tailnet (every new test package touching docker needs the `TestMain` DOCKER_HOST guard; existing packages have it).
- `up` never takes a different image than a running service was deployed with (R53); only `update` rolls onto a rebuilt local image.
- "Differs" for images always means the identity of Task 2 (layers + Created), never the image ID: minis runs the containerd image store, the laptop overlay2.
- New user-visible commands/flags need an action in `internal/actions/registry.go` or a reasoned entry in `cmd/actions_parity_test.go`; hidden plumbing needs neither.
- `add <dir>` refuses an existing service; files are validated like `files put` (no known secret value in a spec).
- Read-only access never grants CREATE or ownership.
- Ships as v0.5.0-rc.3.
- `make ci` (golangci-lint default + nogui, `go test -race`, both builds) passes after every task.

## Review Focus

1. A local image the leader lacks during **boot bring-up** (agent restart after a manual `docker image rm`): bring-up must log the "run `homelab up|update <svc>` from the PC that built it" error for that service and continue with the others — Task 2 test `TestDeploy_LocalImageMissingFails` + the agent already continues past an `Up` error.
2. `homelab up -a` / `update --group` from the laptop with several local-image services: each is shipped once, before the single forwarded command — Task 4 test `TestShip_AllServices`.
3. `add <dir>` with a symlink, a dotfile, `exposure.yaml`, or a tree over 10 MiB: refused before anything reaches the leader, and a leader-side validation failure leaves no `services/<svc>/` behind — Task 5 tests.
4. `update` run on the leader host itself (minis) for a local image rebuilt there: nothing shipped (identities equal), tasks roll because the label changed — Task 2 `TestUpdate_LocalImageRollsOnNewIdentity` covers the roll; Task 4 `TestShip_SkipsWhenIdentityMatches` covers the no-ship.
5. `up grafana` before taash exists, then after: first fails with the "up taash first" message and changes nothing in Postgres; second succeeds — Task 7 tests.

---

### Task 1: Local images in the translator and placement

**Files:**
- Modify: `internal/swarm/translate.go` (container labels ~`:225-230`), `internal/swarm/placement.go:26-50`
- Test: `internal/swarm/translate_test.go`, `internal/swarm/placement_test.go`; fixture `internal/swarm/testdata/taash/{docker-compose.yml,config.yaml}` (copy of the laptop's `~/.config/homelab/services/taash/docker-compose.yml`, config ports as `- 8080`)

**Interfaces:**
- Produces: `const LabelImageLocal = "homelab.image-local"` (container label, value `"true"`); `func IsLocalImage(cs *swarm.ContainerSpec) bool`; `Place(svc, cfg, isDBProvider)` returns `RoleCore` for a `pull_policy: never` service unless `cfg.Placement[svc.Name] != ""` (then the existing override logic).

- [ ] **Step 1: Write failing tests**
  - `TestTranslate_PullPolicyNeverLabelsLocal`: translate the taash fixture → the `taash_taash` spec's `ContainerSpec.Labels[LabelImageLocal] == "true"`; a catalog service (e.g. glance testdata) has no such label.
  - `TestTranslate_TaashHardeningCarried`: same spec has `ReadOnly == true`, `User == "65532:65532"`, `CapabilityDrop == []string{"ALL"}`, `Privileges.NoNewPrivileges == true`.
  - `TestPlace_LocalImageCore`: stateless svc with `PullPolicy: "never"` → `RoleCore`; with `cfg.Placement["taash"]="worker"` → `RoleWorker`; with `"any"` → `RoleAny`.
- [ ] **Step 2:** `go test ./internal/swarm/ -run 'PullPolicy|TaashHardening|LocalImageCore'` → FAIL.
- [ ] **Step 3:** Implement: in `translateService` add the label when `svc.PullPolicy == types.PullPolicyNever`; in `Place`, before the override switch, return `RoleCore` for that case when `cfg.Placement[svc.Name] == ""`.
- [ ] **Step 4:** Same command → PASS; `go test ./internal/swarm/...` PASS.
- [ ] **Step 5:** Commit `feat(swarm): Mark pull_policy never images local and place them on core`.

### Task 2: Deployer — never pull local images, roll on a new identity

**Files:**
- Create: `internal/swarm/image.go`
- Modify: `internal/swarm/deploy.go` (`deploy` loop `:142-190`, `Pull` `:377`, `ensureImage` `:442`)
- Test: `internal/swarm/image_test.go`, `internal/swarm/deploy_test.go`

**Interfaces:**
- Consumes: `LabelImageLocal`, `IsLocalImage` (Task 1); fake `SetImage(ref, image.InspectResponse)`.
- Produces:
  - `func ImageIdentity(img image.InspectResponse) string` — `"sha256:" + hex(sha256(strings.Join(img.RootFS.Layers, "\n") + "\n" + img.Created))`; `""` when `RootFS.Layers` is empty.
  - `const LabelImageIdentity = "homelab.image-identity"` (container label).
  - `var ErrLocalImageMissing` with message format `image %s is local: run `homelab up|update %s` from the PC that built it`.

- [ ] **Step 1: Write failing tests**
  - `TestImageIdentity`: same layers+Created, different `ID` → equal identities; one layer changed → different; no layers → `""`.
  - `TestDeploy_LocalImageNeverPulled`: fake has the image; `Up` and `Update` of the taash fixture → `CallLog` has no `ImagePull`; created spec's `ContainerSpec.Labels[LabelImageIdentity] == ImageIdentity(img)`; `Image` stays `taash-server:current` (no `@`).
  - `TestDeploy_LocalImageMissingFails`: fake lacks it → `Up` error `errors.Is(err, ErrLocalImageMissing)`, message contains `homelab up|update taash`; no `ImagePull`, no `ServiceCreate`.
  - `TestUpdate_LocalImageRollsOnNewIdentity`: deploy, then `SetImage` with a new layer, `Update` → the service's container label changes to the new identity.
  - `TestUp_LocalImageKeepsDeployedIdentity`: deploy, change the image, `Up` → label keeps the old identity (R53).
  - `TestPull_SkipsLocalImage`: `Pull` → no `ImagePull` for the local ref, prints `skipped (local image)` (Deployer gets an `Out io.Writer` field if it has none; otherwise return the note through the existing output path — implementer's choice, assert on whichever).
- [ ] **Step 2:** `go test ./internal/swarm/ -run 'ImageIdentity|LocalImage|SkipsLocal'` → FAIL.
- [ ] **Step 3:** Implement in `image.go` + `deploy.go`: for a local-image spec, inspect instead of pull/ensure (missing → `ErrLocalImageMissing`), set the identity label; on update of an existing service under `!refresh`, copy the running spec's identity label (like `keepPinned`). Changing the container label is what makes Swarm roll — no `ForceUpdate`.
- [ ] **Step 4:** PASS; `go test ./internal/swarm/... ./internal/agent/...` PASS.
- [ ] **Step 5:** Commit `feat(swarm): Never pull local images; roll them on a new identity`.

### Task 3: Leader plumbing — `images --json` and hidden `images load`

**Files:**
- Modify: `internal/swarm/api.go` (add `ImageLoad(ctx context.Context, input io.Reader, opts ...client.ImageLoadOption) (image.LoadResponse, error)`), `internal/swarm/swarmtest/fake.go` (record `"ImageLoad"`, parse nothing: the fake's load sets an image from a test hook `SetLoadResult(ref string, img image.InspectResponse)`), `cmd/images.go`
- Test: `cmd/images_test.go`

**Interfaces:**
- Consumes: `ImageIdentity`, `LabelImageLocal` semantics (Task 1–2).
- Produces:
  - `homelab images <svc>... --json` → `[{"service":"taash","container":"taash","ref":"taash-server:current","local":true,"identity":"sha256:…"}]` (`identity` from the leader engine, `""` if absent). Type `imageInfo struct{Service, Container, Ref string; Local bool; Identity string}` with those JSON tags. With `--json`, `images` accepts several services and `-a/--all`, `--group` through the lifecycle commands' existing `resolveTargets`; without `--json` it keeps today's single-service behaviour.
  - Hidden `homelab images load` (stdin: a `docker save` tar) → leader `ImageLoad`, then prints one JSON line `{"ref":…,"identity":…}` per ref named on its argv: `homelab images load <ref>...`. Forwarded, Mutating (add `"images load"` handling so it takes the lock: `images` itself stays lock-free).
- [ ] **Step 1: Write failing tests** `TestImages_JSON` (taash fixture + fake image → expected JSON), `TestImagesLoad_PrintsIdentity` (stdin bytes reach the fake's `ImageLoad`; output identity matches `SetLoadResult`), `TestImagesLoad_RefNotLoaded` (ref absent after load → non-zero exit, message names the ref).
- [ ] **Step 2:** `go test ./cmd/ -run 'Images'` → FAIL.
- [ ] **Step 3:** Implement; `--json` is a user-visible flag → add `"images --json"` to the `service.images` action's `Commands`.
- [ ] **Step 4:** PASS; `go test ./cmd/ ./internal/swarm/...` PASS.
- [ ] **Step 5:** Commit `feat(cmd): Report image identities and load images on the leader`.

### Task 4: CLI ships local images on `up` / `update`

**Files:**
- Create: `cmd/ship.go`
- Modify: `cmd/remote.go:55-57` (hook next to `forwardUpgrade`), `cmd/deploy.go:16` (local image saver var)
- Test: `cmd/ship_test.go`

**Interfaces:**
- Consumes: `images --json` and `images load` (Task 3); `editLeader`-style leader call with stdin (`cmd/edit.go:24`).
- Produces:
  - `var shipLeader = func(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (int, error)` (same shape as `editLeader`; tests swap it).
  - `var localImages = func() (imageSaver, error)` where `type imageSaver interface{ ImageInspectWithRaw(...); ImageSave(ctx context.Context, refs []string, opts ...client.ImageSaveOption) (io.ReadCloser, error) }` (real: `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`).
  - `func forwardDeploy(argv []string) (int, error)`, for `up` and `update` only: call the leader once with `images <same service args and -a/--group flags> --json`, then ship each distinct local ref once: `up` only when leader `identity == ""`; `update` when it differs from this PC's `ImageIdentity`; skip when this PC lacks the image. Print `shipping <ref> (<size>) to the leader…`. Then `forwardRun(argv)`.
- [ ] **Step 1: Write failing tests** (fake leader via `shipLeader`, fake local engine via `localImages`):
  - `TestShip_UpShipsWhenLeaderLacks`: leader identity `""`, PC has it → one `images load taash-server:current` call with the save stream as stdin, then the forwarded `up taash`.
  - `TestShip_UpSkipsWhenLeaderHas`: leader identity ≠ PC's → no load (R53), forwarded.
  - `TestShip_UpdateShipsWhenDiffers` / `TestShip_SkipsWhenIdentityMatches`.
  - `TestShip_SkipsWhenPCLacksImage`: no load, still forwarded.
  - `TestShip_AllServices`: `up -a` with two services sharing one local ref → one load.
  - `TestShip_LoadFailureStops`: load exits 1 → `up` not forwarded, error names the ref.
  - `TestShip_IdentityMismatchAfterLoad`: load reports a different identity → error, not forwarded.
- [ ] **Step 2:** `go test ./cmd/ -run Ship` → FAIL.
- [ ] **Step 3:** Implement `cmd/ship.go`; wire in `rootPreRun` (`if c == upCmd || c == serviceUpdateCmd { run = forwardDeploy }`).
- [ ] **Step 4:** PASS; `go test ./cmd/` PASS.
- [ ] **Step 5:** Commit `feat(cmd): Ship locally built images to the leader on up and update`.

### Task 5: `homelab add <dir>` and hidden `files install`

**Files:**
- Modify: `cmd/add.go`, `cmd/files.go`, `cmd/remote.go` (hook), `internal/swarm/configs.go` (reuse `checkSpec`/`stack` for a staged tree)
- Test: `cmd/add_dir_test.go`, `cmd/files_internal_test.go`

**Interfaces:**
- Produces:
  - `func isLocalDirArg(arg string) bool` — contains `/` or names an existing directory.
  - `func forwardAddDir(argv []string) (int, error)` — validates the folder locally, tars it (gzip not needed), sends to `files install <svc>` via `shipLeader`'s shape (`addLeader` var), prints the leader's output.
  - Limits: `maxAddTree = 10 << 20` bytes total; refused entries: symlinks, any path component starting with `.`, `exposure.yaml`, non-regular files; `docker-compose.yml` and `config.yaml` required. Service name = `filepath.Base(abs)`, `config.ValidServiceName`.
  - Hidden `homelab files install <svc>` (stdin tar): refuses an existing `services/<svc>`; extracts into `services/.install-<svc>-*` under the state dir (same filesystem), rejects tar entries with `..`, absolute paths, links; runs the same checks as `WriteConfigFile` for spec files plus `d.stack(svc)` on the staged dir; `os.Rename` into place; any failure removes the temp dir. Prints `installed <svc>: run homelab setup <svc> (if it has secrets), homelab up <svc>, homelab enable <svc>`, and a warning `warning: local image <ref> has a namespace or registry host; a registry image of that name would replace it` when a `pull_policy: never` image ref, normalized, is not `docker.io/library/…`.
- [ ] **Step 1: Write failing tests**: `TestAddDir_ForwardsTar` (folder → one `files install taash` call; tar contains exactly the expected files), `TestAddDir_Refuses` (table: symlink, `.env`, `exposure.yaml`, >10 MiB, missing compose → error, no leader call), `TestFilesInstall_Installs` (taash fixture tar → files under `services/taash/`), `TestFilesInstall_RefusesExisting`, `TestFilesInstall_BadComposeLeavesNothing`, `TestFilesInstall_TarTraversalRefused`, `TestFilesInstall_WarnsQualifiedLocalImage`.
- [ ] **Step 2:** `go test ./cmd/ -run 'AddDir|FilesInstall'` → FAIL.
- [ ] **Step 3:** Implement; `add` with a bare name keeps the catalog path untouched.
- [ ] **Step 4:** PASS; `go test ./cmd/` PASS.
- [ ] **Step 5:** Commit `feat(cmd): Install a custom service from a folder with homelab add <dir>`.

### Task 6: `homelab edit <svc> <path> --from <file>`

**Files:**
- Modify: `cmd/edit.go` (flag + branch in RunE), `internal/actions/registry.go` (`service.edit` Commands add `"edit --from"`)
- Test: `cmd/edit_test.go`, `internal/swarm/configs_test.go`

**Interfaces:**
- Produces: `--from <file>`: read the local file (`os.ReadFile`, ≤ `swarm.MaxConfigSize`), skip `files get` and the editor, send it with `files put`; print `<svc>/<path> updated`.
- [ ] **Step 1: Write failing tests**: `TestEdit_FromFileSkipsEditor` (via `stubEdit`: one `files put` call with the file's bytes, editor never runs), `TestEdit_FromMissingFile` (error, no leader call), `TestWriteConfigFile_NewFileUnderBindDir` (grafana fixture: `provisioning/dashboards/taash/taash-beta.json` created with its subdirs), `TestWriteConfigFile_OutsideBindDirRefused` (`ErrNotConfig`).
- [ ] **Step 2:** `go test ./cmd/ ./internal/swarm/ -run 'Edit_From|NewFileUnderBindDir|OutsideBindDir'` → FAIL (the configs tests may already pass — keep them as pins).
- [ ] **Step 3:** Implement the flag.
- [ ] **Step 4:** PASS, incl. `go test ./cmd/ -run Parity`.
- [ ] **Step 5:** Commit `feat(cmd): Add edit --from for scripted file updates`.

### Task 7: Read-only access to another service's postgres database

**Files:**
- Modify: `internal/config/config.go` (`ServiceDBDecl` `:271`: `ReadOnly bool \`yaml:"readonly,omitempty"\``; validation), `internal/db/provisioner.go` (`Provision`/`Deprovision`), `cmd/service_deps.go:227` (`releaseDBDependencies`), `internal/backup/plan.go:109-118`
- Test: `internal/config/config_test.go`, `internal/db/provisioner_test.go`, `cmd/service_deps_internal_test.go`, `internal/backup/plan_test.go`

**Interfaces:**
- Produces:
  - Validation: `readonly` only with type `postgres`; requires `database` and `user`; refused with `superuser` or `extensions`. Error text names the service and the field.
  - `func (p *Provisioner) provisionPostgresReadOnly(ctx context.Context, svcName string, decl config.ServiceDBDecl, password string) error`: database missing (`SELECT 1 FROM pg_database WHERE datname=…`) → `fmt.Errorf("database %s does not exist yet — `homelab up %s` first", decl.Database, decl.Database)` (catalog and custom services name their database after themselves); then CREATE/ALTER role `WITH LOGIN PASSWORD` (same re-sync as today), then in that database: `GRANT CONNECT ON DATABASE`, `GRANT USAGE ON SCHEMA public`, `GRANT SELECT ON ALL TABLES IN SCHEMA public`, `GRANT SELECT ON ALL SEQUENCES IN SCHEMA public`, and `ALTER DEFAULT PRIVILEGES FOR ROLE <db owner> IN SCHEMA public GRANT SELECT ON TABLES` (owner from `pg_database.datdba`). Password: the consumer's existing `ensurePassword(svcName)` (`DB_PASSWORD_<SVC>`, root namespace).
  - Deprovision of a read-only decl: in the database `DROP OWNED BY <role>` then `DROP ROLE IF EXISTS <role>`; `releaseDBDependencies` calls it for read-only postgres decls (other postgres decls unchanged).
  - Backup plan: a read-only decl adds no `DatabaseTarget`.
- [ ] **Step 1: Write failing tests**: config validation table; `TestProvision_ReadOnlyGrants` (recorded SQL contains each GRANT and the ALTER DEFAULT PRIVILEGES with the owner from the fake `datdba` result, and no `CREATE DATABASE`/`OWNER TO`); `TestProvision_ReadOnlyMissingDatabase` (error text, no role created); `TestDelete_DropsReadOnlyRole`; `TestBackupPlan_SkipsReadOnly`; `TestBuildEnv_ReadOnlyInjects` (grafana with the spec's declaration → `TAASH_RO_PASSWORD` is the `DB_PASSWORD_GRAFANA` value, marked secret; `TAASH_RO_USER=taash_ro`).
- [ ] **Step 2:** run those tests → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS; `go test ./internal/... ./cmd/` PASS.
- [ ] **Step 5:** Commit `feat(db): Read-only access to another service's postgres database`.

### Task 8: Docs, changelog, release candidate

**Files:**
- Modify: `CLAUDE.md` (Service lifecycle: `add <dir>`, `edit --from`; Services under Swarm: local images; Shared services: `readonly`), `docs/adding-a-service.md` (a custom service with a local image, unqualified image names), `CHANGELOG.md` (`## v0.5.0-rc.3 — <date>`: Added `add <dir>`, local images, `edit --from`, `readonly`; Fixed `join` removing a stale Compose-era bridge; Build: golangci-lint in `make lint`, CI not on tags).
- [ ] **Step 1:** Write the docs.
- [ ] **Step 2:** `make ci` → passes.
- [ ] **Step 3:** Commit `docs: Document custom services with local images; v0.5.0-rc.3 changelog`.
- [ ] **Step 4:** With the owner's go-ahead: merge to main, signed tag `v0.5.0-rc.3`, push (CI once on main, release on the tag).

### Task 9: Rollout of taash (live, with the owner)

Not code in this repo; done in the session after the release.

- [ ] Owner installs rc.3 on the laptop and minis; `homelab upgrade` (laptop worker, then leader).
- [ ] Cards repo (`/home/groot/dev/cards`): `deploy/homelab/taash/{docker-compose.yml,config.yaml}` from the laptop's old service dir (ports `- 8080`, no exposure.yaml); Grafana files under `deploy/homelab/grafana/provisioning/{dashboards/taash.yaml,datasources/taash.yaml}` + the dashboard JSON; Makefile `deploy-server` → `homelab update taash`, `grafana-dashboard` → three `homelab edit grafana … --from …`; README updated. Commit in that repo (owner's identity, signed).
- [ ] `make server-image`, `homelab add deploy/homelab/taash`, `homelab up taash` (ships the image), `homelab status taash` (node minis, healthy).
- [ ] Grafana one-time wiring (`homelab edit grafana config.yaml` / `docker-compose.yml`), `homelab up grafana`, `make grafana-dashboard`; the dashboard loads (panels may be empty on a fresh DB).
- [ ] `homelab enable taash --cf`; if the `taash.gr00t.fun` record targets another tunnel, owner deletes it in Cloudflare, rerun. Verify a WebSocket connects off-tailnet and on the tailnet URL; owner plays a game in the app.
- [ ] Laptop's old Compose homelab: stop and remove its containers (owner confirms the list first); its volumes stay until the owner says otherwise.
- [ ] Rebuild once and `make deploy-server` → task rolls onto the new image.
