# Audit Quick Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the small, user-visible bugs and inconsistencies the 2026-11-13 v0.4.3→v0.5.0 audit and end-user review found, each shipped as a patch release once tested and reviewed.

**Architecture:** Independent fixes in existing code paths; no new subsystems.

**Tech Stack:** Go, cobra, Docker SDK, swarmtest fakes.

**Spec:** the owner's request (2026-11-13): "continue fixing/building the bugs/inconsistencies and missing features one by one and tag a patch release with each confirmed fix/feature", applied to the audit findings recorded in conversation (items quoted per task below).

## Global Constraints

- Every change follows existing patterns (CLAUDE.md); tests never reach Docker, the network or a tailnet; `make ci` passes after every task.
- New user-visible commands/flags need an action in `internal/actions/registry.go` or a reasoned parity exclusion.
- No output contract used by the front ends (`status --json`, `status <svc> --json`, `ls --json`, `backup-list --json`) loses a field; adding fields is fine.
- One CHANGELOG entry per task under its release section (`## v0.5.N — <date>`, Fixed/Changed).
- Commits: Sentry style, signed, `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A multi-container stack (immich: server + machine-learning) and a service with old + new tasks in `logs` and `status`.
2. Services with raw listen ports (gitea `2222:2222/tcp`), named HTTP ports (vaultwarden `vault:80`), and none.
3. Front ends (TUI/GUI) parse `--json` output: an error where `--json` is unsupported must not break any action they run.
4. `doctor` on the leader agent (it runs inside a container: no host files).

---

### Task 1: `doctor` — no false log-cap warning; honest summary  → v0.5.3

Audit: "`doctor` warns on every run ('no log cap in /etc/docker/daemon.json') and still prints 'All checks passed': it reads the file inside the agent container, where it is never mounted."
- Remove the daemon.json log-cap check (or replace it with a check the agent can actually make; `docker info` carries no log options, so removal is expected). Find it under internal/diagnostics (`checkLogRotation`).
- The summary counts warnings: "All checks passed." only with no warnings; otherwise "Passed with N warning(s)." (failures keep today's wording).
- Tests: no log-cap check registered; summary wording for 0/1/2 warnings.

### Task 2: `logs -n N` — merged by time, tail after merging  → v0.5.3

Audit: "`homelab logs gitea -n 5` returned 10 lines with Nov 12 and Nov 13 interleaved: `--tail` applies per task, not merged by time."
- Find how `logs <svc>` reads `docker service logs` (cmd/logs*.go, internal/swarm or internal/run). Request timestamps from Docker for ordering (`--timestamps`); merge the lines of all tasks of all containers of the stack by timestamp; apply `-n` after merging; print timestamps only when the user asked (`-t`). Keep `-f` streaming behaviour (no merge needed beyond arrival order once following; the initial backlog is merged and tailed).
- Tests with captured fake output of two tasks: `-n 5` prints exactly 5 lines, newest last, in time order; `-t` keeps timestamps; without `-t` they are stripped.

### Task 3: PORTS in `status` / `status <svc>`  → v0.5.4

Audit: "The PORTS column is always `–` … `\"Ports\": null` in JSON. Gitea's raw `2222/tcp` exposure is never displayed."
- Fill PORTS from what the service declares in config.yaml and is exposed on (exposure.yaml `ports:` filter honoured): raw listen ports as `2222/tcp`, HTTP ports as their subdomain (`vault`) or `https` for the default. `status --json` gets the same data (keep the existing field name `Ports`, now populated; document its shape in the JSON).
- `status <svc>` gains a "Ports" section listing each declared port: its name as `--ports=` accepts it, what it maps to (URL or `<host>:<listen>/<proto>`), and whether it is enabled.
- Tests: gitea (3000 + 2222:2222/tcp), vaultwarden (vault:80), grafana (not exposed).

### Task 4: `--json` where unsupported is an error  → v0.5.4

Audit: "`homelab doctor --json` and `homelab ext list --json` print human text, exit 0."
- `--json` is a root persistent flag. Commands that honour it keep working; every other command run with `--json` fails before doing anything: `error: --json is not supported by "homelab doctor"`. Implement once (a set of supporting commands, checked in the root PreRun before forwarding), not per command.
- Check every front-end action's argv (internal/actions) still parses and none passes `--json` to an unsupported command; the parity/ArgsParse tests must pass.
- Tests: doctor --json → error, nothing forwarded; status --json → forwarded.

### Task 5: front ends — confirmations and labels  → v0.5.5

Audit: "'Stop all' runs without confirmation; 'Down all' asks. 'Update all' and 'Restart all' also run unconfirmed. 'Compose config' shows plaintext secrets with no confirmation, in the harmless Inspect group. Stop uses a pause icon. TUI: `r` is restart on a service but restore at Global. 'Down all' help says 'Stop and remove every service's containers' (it scales to zero). `disable --stop` help says 'stop and remove'."
- internal/actions/registry.go: Stop all, Update all, Restart all get the same confirmation as Down all; "Compose config" gets a confirmation ("prints secrets") and moves out of the Inspect group if a better group exists; Stop's icon matches Down's (stop), not pause; fix the two help texts; give the Global "restore" action a key that is not `r` if keys are defined in the registry (otherwise in the TUI key map).
- Tests: the registry tests assert Danger/Confirm on those actions; key uniqueness per scope if a test exists or add one.

### Task 6: Docs — new-user path  → v0.5.6

Audit findings to fix in README.md, docs/cloudflare-setup.md, docs/tailscale-setup.md, docs/adding-a-service.md, CONTRIBUTING.md, docs/service-testing.md, docs/network-extensions-plan.md, and help texts:
- README: drop "`homelab upgrade` … does not exist yet"; replace the `you/homelab` clone URL with the release install (tarball + SHA256SUMS, as in the release notes) and `homelab upgrade` thereafter; regenerate the Included-services table from assets/services (name, subdomain from config.yaml ports, one-line purpose from the compose header comment) and drop minero from it (unsupported on Swarm); add a "Firewall" prerequisite (Swarm ports 2377/tcp, 7946/tcp+udp, 4789/udp on tailscale0; ufw lines) and the Quick-start order that actually works (install → `homelab init` prompts → `homelab setup` → add/up/enable).
- cloudflare-setup.md: a "Cloudflare Tunnel" section — create the tunnel, where CF_TUNNEL_TOKEN comes from, set every public hostname's service to `HTTP` `homelab-edge:80`.
- tailscale-setup.md: a complete ACL example that also allows the Swarm ports between cluster hosts.
- "keyring" wording that now means the leader's vault (cloudflare-setup.md:16, `add`/`restore` help, doctor's "set in keyring"); "minis" in user-facing docs/help → "the leader (core node)".
- adding-a-service.md: the port grammar (`ssh:22` is a subdomain, not a named port; how `--ports=` names are formed: `default` for a bare port, the subdomain, or the listen port) and the compose keys Swarm ignores (`ports:`, `deploy.resources`, `depends_on`, `shm_size`, `dns`, …).
- Mark docs/network-extensions-plan.md obsolete at the top; update service-testing.md and CONTRIBUTING.md (no tor/i2p/ygg steps; secrets in the leader's vault).
- Tests: none beyond existing doc/help tests; `make ci`.
