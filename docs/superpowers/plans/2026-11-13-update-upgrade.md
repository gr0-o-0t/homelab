# Task History, Release Check, Self-Upgrade and Dependabot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep at most one dead task container per service; make `homelab update` (no service) report a newer homelab release; make `homelab upgrade` (no flags) upgrade this PC's CLI and then the agents to the latest release; let Dependabot bump dependencies.

**Architecture:** Swarm's `Orchestration.TaskHistoryRetentionLimit` set to 1 by `init` and ensured by the leader agent at boot. A small `internal/release` package talks to the GitHub Releases API (latest = newest non-prerelease) and downloads + verifies release archives. `homelab upgrade` gains a client-side CLI self-replace step before the existing agent rollout; it re-executes under `sudo` when the binary is not writable. cloudflared's image moves from a Go constant into an embedded compose file so Dependabot's docker-compose ecosystem can bump it.

**Tech Stack:** Go, cobra, net/http (+ httptest), archive/tar + compress/gzip, crypto/sha256, Docker SDK (SwarmInspect/SwarmUpdate), GitHub Dependabot.

**Spec:** the owner's instructions in conversation (2026-11-13): "task history set to 1, the homelab update command should fetch if there are any update. and the homelab upgrade should upgrade it to the next version. both the cli (with root, ask for root if not given) and the agent containers. add github actions to bump dependency versions when they are available. do not invent a new way to do things. follow best practices and conventions already followed by most users in the github."

## Global Constraints

- Release source: `https://api.github.com/repos/gr0-o-0t/homelab/releases/latest` (GitHub excludes prereleases and drafts from "latest"). Assets: `homelab_<tag>_linux_<arch>.tar.gz` with `<arch>` ∈ `amd64` (GUI build), `amd64_headless`, `arm64` (headless), each holding `homelab_<tag>_linux_<arch>/homelab`, plus `SHA256SUMS` (`sha256sum` format). The CLI picks the asset matching its own build: GUI build → `amd64`; `-tags nogui` → `amd64_headless` on amd64, `arm64` on arm64.
- Every download is verified against `SHA256SUMS` before use; a mismatch or a missing entry aborts with nothing replaced.
- Binary replacement is atomic: write the new binary to a temp file in the same directory as the running executable (`os.Executable` + `filepath.EvalSymlinks`), chmod 0755, `os.Rename` over it.
- Not writable → re-exec the same argv under `sudo` (`sudo -- <exe> <args…>`), printing `homelab: <path> is not writable; re-running with sudo…`. Already root, or `sudo` missing → plain error naming the path. Never write a password anywhere; sudo prompts on the terminal.
- No GitHub token; unauthenticated API with a `User-Agent: homelab/<version>` header; network errors are reported, never retried in a loop. Tests use `httptest`; tests never reach the network, Docker or a tailnet.
- Version comparison: semver (`golang.org/x/mod/semver`, already in go.sum via deps — check; else compare with the existing version helpers).
- R53 is unchanged: `homelab update <svc>` keeps its meaning (pull + roll a service).
- Conventional, not invented: Dependabot (`.github/dependabot.yml`), weekly, grouped minor/patch updates per ecosystem.
- New user-visible commands/flags need an action in `internal/actions/registry.go` or a reasoned parity exclusion.
- `make ci` passes after every task. Commits: Sentry style, signed, `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A dev build (`v0.0.0-…` or `-dev`) or a newer-than-latest CLI: `update` must not claim a downgrade is an update; `upgrade` without `--version` must refuse to "upgrade" to an older release.
2. `upgrade` run under sudo re-exec must still reach the cluster: the CLI reads `client.yaml` and the cluster secret from the invoking user's config/keyring — under sudo `HOME`/`XDG_CONFIG_HOME`/D-Bus differ. Preserve the user's environment for the agent step (e.g. sudo only replaces the binary — `homelab upgrade --cli-only` hidden step run under sudo — and the agent rollout continues in the original unprivileged process with the new binary).
3. A truncated download, a wrong checksum, or an archive whose entry path escapes (`../`) never replaces the binary.
4. The leader with an existing swarm at limit 5: boot lowers it to 1 once and does not rewrite the spec on every boot when already 1.
5. A client-only PC (no Docker) can still run `update` (release check + leader versions) and `upgrade` (CLI + agents via the leader).

---

### Task 1: Swarm task history limit 1  → release v0.5.1

**Files:** `internal/bootstrap/bootstrap.go` (`InitRequest.Spec.Orchestration.TaskHistoryRetentionLimit`), `internal/agent/agent.go` (leader boot: ensure), `internal/swarm/api.go` + `swarmtest/fake.go` (`SwarmUpdate` if missing), tests; CLAUDE.md (one line under "The agent"), CHANGELOG `## v0.5.1`.

- [ ] Tests: `TestInit_TaskHistoryLimitOne` (InitRequest carries 1); `TestLeaderBoot_LowersTaskHistory` (fake swarm at 5 → one SwarmUpdate with 1, version index passed); `TestLeaderBoot_TaskHistoryAlreadyOne` (no SwarmUpdate). A failed update is logged, not fatal.
- [ ] Implement, `make ci`, commit `fix(swarm): Keep one finished task per service`.

### Task 2: `internal/release` — latest release, download, verify

**Files:** create `internal/release/release.go`, `internal/release/release_test.go`.

**Produces:** `type Release struct{ Tag string; Assets map[string]string /* name → browser_download_url */ }`; `func Latest(ctx context.Context, c *http.Client, api string) (Release, error)` (api = base URL, default `https://api.github.com/repos/gr0-o-0t/homelab`); `func ByTag(ctx, c, api, tag string) (Release, error)`; `func AssetName(tag string) string` (build-flavour aware via a build-tagged const); `func FetchBinary(ctx, c, rel Release) ([]byte, error)` (downloads the archive + SHA256SUMS, verifies, extracts exactly `homelab_<tag>_linux_<arch>/homelab`, size cap 200 MiB); `func Newer(current, latest string) bool` (semver; dev/invalid current → false with a reason the caller can print).

- [ ] Tests with httptest: latest parses tag + assets; checksum mismatch → error; missing SHA256SUMS entry → error; archive with `../` entry → error; wrong entry name → error; Newer table (v0.5.0<v0.5.1, rc<final, dev → false, equal → false).
- [ ] Implement, `make ci`, commit `feat(release): Find, download and verify homelab releases`.

### Task 3: `homelab update` with no service reports updates

**Files:** `cmd/update.go` (no-target branch, runs locally — not forwarded: add the no-arg form to the local path the way `backup --fetch` is local), tests, actions registry entry ("Check for updates", Global scope).

- Output: `homelab CLI v0.5.0 (this PC), agents: minis v0.5.0, leno v0.5.0` (agents from the leader's `status --json` nodes or `version --agent`; unreachable leader → say so and continue), then either `homelab v0.5.1 is available: run homelab upgrade` (+ release URL) or `homelab is up to date (v0.5.0)`. Exit 0 in both; non-zero only when GitHub cannot be reached. With `-a`/`--group`/services: unchanged behaviour (forwarded).
- [ ] Tests: available / up to date / dev build / leader unreachable; service form still forwarded.
- [ ] Implement, `make ci`, commit `feat(cmd): Check for a newer homelab release with update`.

### Task 4: `homelab upgrade` upgrades this PC's CLI, then the agents  → with Tasks 2–3 and 5: release v0.5.2

**Files:** `cmd/upgrade.go` (`forwardUpgrade`), new `cmd/selfupdate.go`, tests; CLAUDE.md Cluster section; README install/upgrade section.

- No `--version`/`--image`: target = latest release (refuse if not newer than the CLI: "already on vX; agents: …" and still upgrade agents that are behind the CLI). `--version vX`: that release. `--image`: unchanged (agents only, dev).
- CLI step (skipped when the CLI already is the target): FetchBinary → replace self (Global Constraints). Not writable → run `sudo -- <exe> upgrade --cli-only --version <tag>` (hidden flag: replace the binary and exit), then continue **in the original process** by exec'ing the new binary with `upgrade --version <tag>` (now a no-op CLI step) so the agent rollout runs as the invoking user with their keyring/client.yaml.
- Agent step: the existing rollout with image `ghcr.io/gr0-o-0t/homelab:<tag>`.
- [ ] Tests (fake release server, fake exec/sudo hooks, `fakeAgent`): writable binary replaced then agents forwarded with `--image ghcr…:<tag>`; unwritable → sudo command line built exactly, then exec of the new binary; checksum failure → nothing replaced, agents untouched; target not newer → CLI untouched, agents behind are still upgraded; `--image` path unchanged.
- [ ] Implement, `make ci`, commit `feat(cmd): Upgrade this PC's CLI before the agents`.

### Task 5: Dependabot; cloudflared image from an embedded compose file

**Files:** create `.github/dependabot.yml`; create `assets/cloudflared/docker-compose.yml` (one service, `image: cloudflare/cloudflared:<current>`); `assets/assets.go` (embed it); `internal/swarm/cloudflared.go` (read the image from it, keep `CloudflaredImage` as the parsed value or a func); tests; docs (CLAUDE.md Build section: Dependabot).

- dependabot.yml: `version: 2`; ecosystems `gomod` (/), `github-actions` (/), `docker` (/ — the Dockerfile's `docker:<ver>-cli@sha256` base), `docker-compose` (directories `/assets/cloudflared` and `/assets/services/*` via `directories:`); `schedule: weekly`; `groups:` one group per ecosystem for `minor` and `patch` updates; `open-pull-requests-limit: 5`.
- [ ] Tests: the embedded compose parses and yields exactly one image equal to today's `cloudflare/cloudflared:2026.10.7`; a catalog test still passes.
- [ ] Implement, `make ci`, commit `build: Let Dependabot bump modules, actions and images`.
- [ ] CHANGELOG `## v0.5.2`; with the owner's go-ahead implicit in the request: merge, tag v0.5.2, push; owner runs `homelab upgrade` to confirm live.
