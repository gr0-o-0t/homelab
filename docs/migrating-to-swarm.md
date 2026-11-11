# Migrating minis from Compose (v0.4.3) to the Swarm cluster

A one-time, all-at-once cutover: back up with the old binary, stop the Compose
stacks, create the cluster with `homelab init --import`, bring the services back
up, verify, then join the laptop. Downtime is expected (plan about an hour, more
if the backup of immich is large).

Conventions:

- **[laptop]** / **[minis]** say where a command runs. Both login shells are
  fish; every command below works in fish (command substitution is `(…)`).
- `homelab` on minis is the **old** v0.4.3 binary until step 6;
  `~/bin/homelab-new` is the new one. On the laptop, `homelab` becomes the new
  one in step 1.
- Data lives in Docker named volumes under `/var/lib/docker`. Nothing in this
  runbook removes a data volume; that is what makes the rollback cheap.
- `sudo` below may be `doas` on minis (Alpine), whichever it has.

Commands that fail say so; each step lists what success looks like and what to
do if it does not. The [rollback](#rollback) works from any step.

---

## 0. Preconditions

### 0.1 Build the image and the binary [laptop]

```fish
cd ~/src/local/homelab
git switch feat/swarm-cluster; git status --short   # clean tree, the commit you mean to deploy
make image                                          # → image homelab:dev + dist/image/linux_amd64/homelab
make install                                        # this PC's CLI, same commit/version (replaces the old v0.4.3 here)
homelab version
docker image inspect homelab:dev --format '{{index .Config.Labels "org.opencontainers.image.version"}}'
```

Success: both print the same `v0.0.0-<date>-<commit>` version. `make image`
fails with "the image binary is not statically linked" if cgo leaked into the
build — rerun from a clean shell (`CGO_ENABLED` is set by the target).

### 0.2 Copy the image and the new binary to minis [laptop]

```fish
docker save homelab:dev | ssh minis docker load
cat dist/image/linux_amd64/homelab | ssh minis 'mkdir -p ~/bin && cat > ~/bin/homelab-new && chmod +x ~/bin/homelab-new'
ssh minis '~/bin/homelab-new version'
```

Success: `Loaded image: homelab:dev`, and the same version as in 0.1 (a fish
greeting line before it is harmless: it goes to ssh's stdout, never into
`docker load`'s or `cat`'s stdin). Do not use `scp` for the binary: it can
choke on the remote shell's greeting.

### 0.3 Host checks [minis]

```fish
homelab version                                        # v0.4.3 — the old binary, used for steps 1–4
docker info --format '{{.ServerVersion}} swarm={{.Swarm.LocalNodeState}}'   # 29.x swarm=inactive
docker volume ls --filter name=homelab-agent           # nothing (a leftover makes init refuse)
tailscale ip -4                                        # a 100.x address
tailscale status --json | grep MagicDNSSuffix          # non-empty: MagicDNS is on
df -h /var                                             # room for the backup (immich uploads dominate)
```

Docker and tailscaled must start at boot without a login (step 7.6 depends on
it): on OpenRC `rc-update show default | grep -E 'docker|tailscale'`, on
systemd `systemctl is-enabled docker tailscaled`.

If minis runs a host firewall, allow Swarm on the tailnet interface:
2377/tcp, 7946/tcp, 7946/udp, 4789/udp on `tailscale0`. With ufw, `homelab init`
prints the exact `sudo ufw allow in on tailscale0 to any port … proto …` lines.

### 0.4 Tailscale [laptop, admin console]

- Create a **reusable** auth key (Settings → Keys; see
  [tailscale-setup.md](tailscale-setup.md)). Every `homelab join` hands the
  leader's key to that PC's agent, so a one-off key breaks the second node.
- The leader's tailnet node is named **`homelab`**. In the admin console's
  Machines list, make sure no other machine already has that name (the old
  Compose sidecar was `caddy-home` unless `TS_HOSTNAME` said otherwise — check).
  If the name is taken, the agent's node becomes `homelab-1` and the CLI's
  default endpoint `https://homelab.<tailnet>:7443` points at the wrong
  machine. Rename or remove the clash first.
- ACLs must let the laptop reach `homelab` on 443, 7443 and 2222, and let minis
  and the laptop reach each other on 2377/tcp, 7946/tcp+udp and 4789/udp. The
  default allow-all policy does.

### 0.5 Secrets in minis' keyring [minis, logged-in desktop session]

`init` reads `TS_AUTHKEY`, `CLOUDFLARE_API_TOKEN` and `CF_TUNNEL_TOKEN` from this
keyring (prompting only for a missing one), and `--import` reads every declared
service secret from it. Run this step and step 5 in a terminal of the
logged-in session, where the keyring is unlocked.

```fish
homelab setup --json        # root: TS_AUTHKEY, CLOUDFLARE_API_TOKEN … show "set": true
```

The stored `TS_AUTHKEY` is the one the Compose sidecar used, probably one-off
or expired. Replace it with the reusable key from 0.4 (`init` does not prompt
when the keyring has a value):

```fish
read -s -P 'reusable TS_AUTHKEY: ' k; printf '{"TS_AUTHKEY":"%s"}' $k | homelab setup --secrets-stdin; set -e k
```

Success: no error; `homelab setup --json` still shows it set.

### 0.6 Firewall on the laptop [laptop]

```fish
sudo ufw allow in on tailscale0 to any port 7946 proto tcp
sudo ufw allow in on tailscale0 to any port 7946 proto udp
sudo ufw allow in on tailscale0 to any port 4789 proto udp
sudo ufw status | grep -E '7946|4789'
```

Without these, Swarm's gossip probes to the laptop are lost ("Suspect … no acks
received" in the Docker log) and overlay traffic to its tasks does not arrive.

### 0.7 Record what is there now [laptop]

```fish
dig +short gitea.home.gr00t.fun @1.1.1.1      # the old Caddy sidecar's 100.x IP — write it down (rollback)
git -C ~/dev/cards remote -v                  # ssh://git@gitea.home.gr00t.fun:2222/groot/cards.git
```

---

## 1. Back up with the old binary [minis]

v0.4.3 writes `<--out>/<timestamp>/` from your user (the directory is
bind-mounted into its helper container), so the destination must be writable
by you and outside the ecryptfs home:

```fish
sudo mkdir -p /var/backups/homelab-v043
sudo chown (id -u):(id -g) /var/backups/homelab-v043
homelab backup --all --out /var/backups/homelab-v043
homelab backup --list --out /var/backups/homelab-v043
```

Success: one `✓`-style line per service with volumes/databases, a final
manifest, and the listing shows the new timestamped directory. Each service is
stopped for its own snapshot and started again. If a service fails, fix it (or
note it) before going on: the backup is the safety net for the data volumes,
even though no step below deletes them.

This backup can be restored **only with v0.4.3** (see
[limitations](#known-limitations-and-follow-ups)).

## 2. Copy the config dir aside [minis]

Before anything stops (step 4 rewrites `state/desired.yaml` to "stopped"):

```fish
tar -C ~/.config -czf /var/backups/homelab-v043/config-homelab.tgz homelab
cp (command -v homelab) /var/backups/homelab-v043/homelab-v0.4.3
tar -tzf /var/backups/homelab-v043/config-homelab.tgz | head
cat ~/.config/homelab/state/desired.yaml      # which services were running — keep this
```

The tarball holds no secrets (they are in the keyring). The copy is outside the
login-mounted home, so it survives anything that happens to it.

## 3. Bring the catalog files up to date [laptop → minis]

The importer copies each service's `docker-compose.yml` and `config.yaml` **as
they are on minis**; it does not refresh them from the catalog. Three of the
installed services changed for Swarm since v0.4.3:

| file | why it matters |
|---|---|
| `redis/docker-compose.yml` | the old one puts `REDIS_PASSWORD` on the command line; Swarm would show it in the spec, so `up redis` refuses it ("secret value in service spec") |
| `immich/config.yaml` | needs `cache_volumes: [immich-model-cache]` (+ `placement:`), or immich-machine-learning is pinned to minis and never moves to the laptop |
| `gitea/docker-compose.yml`, `gitea/config.yaml` | `GITEA__server__SSH_PORT: "2222"` (clone URLs) and the `2222:2222/tcp` port |

Send the current catalog entries of the installed services to minis and compare
[laptop]:

```fish
cd ~/src/local/homelab
tar -C assets/services -cf - beszel-hub gitea glance grafana immich postgres redis searxng vaultwarden | ssh minis 'rm -rf /tmp/hl-catalog && mkdir -p /tmp/hl-catalog && tar -C /tmp/hl-catalog -xf -'
```

[minis]:

```fish
for s in beszel-hub gitea glance grafana immich postgres redis searxng vaultwarden
    diff -u ~/.config/homelab/services/$s/docker-compose.yml /tmp/hl-catalog/$s/docker-compose.yml
    diff -u ~/.config/homelab/services/$s/config.yaml /tmp/hl-catalog/$s/config.yaml
end
```

Then, for each difference:

- **docker-compose.yml**: take the catalog's file unless the difference is a
  customization of yours. At least:
  ```fish
  cp /tmp/hl-catalog/redis/docker-compose.yml ~/.config/homelab/services/redis/
  cp /tmp/hl-catalog/gitea/docker-compose.yml ~/.config/homelab/services/gitea/
  ```
- **config.yaml**: never copy it whole — it holds your variable values
  (`homelab setup` writes them there). Add only the missing keys, with
  `$EDITOR`. For immich, if `grep -E 'cache_volumes|placement' ~/.config/homelab/services/immich/config.yaml`
  prints nothing:
  ```fish
  printf '\ncache_volumes: [immich-model-cache]\nplacement:\n  immich-machine-learning: worker\n' >> ~/.config/homelab/services/immich/config.yaml
  ```
  For gitea, make sure `ports:` lists `2222:2222/tcp` (a tcp+udp `2222` is
  rewritten to tcp-only by the importer and reported).

Success: rerunning the `diff` loop shows only your own customizations and
config values. (v0.4.3 still runs these files, so the [rollback](#rollback)
does not need them reverted; the tarball from step 2 has the originals.)

## 4. Stop the Compose stacks [minis]

```fish
homelab session status          # v0.4.3 session mode: is the unit installed/active?
homelab session uninstall       # only if it is installed — otherwise skip
homelab down -a                 # every service: containers stopped and removed
homelab down                    # no service = the core stack (tailscale + caddy)
```

If `homelab down` (core) fails, stop the core directly:
`docker compose -f ~/.config/homelab/core/docker-compose.yml down`.

Check:

```fish
docker ps -a --filter label=com.docker.compose.project.working_dir --format '{{.Names}}  {{.Label "com.docker.compose.project.working_dir"}}'
docker network inspect home-services --format '{{range .Containers}}{{.Name}} {{end}}'
docker ps --filter name=tv-kiosk
```

Success: the first command lists nothing under `~/.config/homelab` (other
projects, if any, are fine); the second prints an empty line; `tv-kiosk` is
still `Up`. If a homelab container remains, `docker rm -f <name>` it. If
`tv-kiosk` (or anything else not homelab) is attached to the `home-services`
bridge, `init` refuses to remove the bridge: detach it with
`docker network disconnect home-services tv-kiosk` and reattach it to another
network afterwards if it needs one. Do not stop `tv-kiosk`.

## 5. Create the cluster with the import [minis, logged-in desktop session]

```fish
~/bin/homelab-new init --import ~/.config/homelab --image homelab:dev
```

What happens, in order: it reads the tailnet IP and MagicDNS name, refuses if
Docker is already in a swarm, a `homelab-agent` container or volume exists, a
Compose container under `~/.config/homelab` remains or the `home-services`
bridge has containers; builds the import (config files, bind-mounted files,
exposure, routing, desired state, secrets from the keyring); prompts only for a
root secret missing from the keyring (`CF_TUNNEL_TOKEN`: press Enter to skip);
then `swarm init` on the tailnet IP, labels the node `core`, removes the old
`home-services` bridge, creates the overlay, starts the leader agent, seeds and
restarts it.

It prints, at the end:

- `Swarm created; this node (…) is the manager, advertising 100.x.x.x.`
- the ufw lines, if ufw is enabled here;
- `homelab join --endpoint https://homelab.<tailnet>:7443`;
- **the cluster secret, once.** Store it in your password manager now. (It is
  also in minis' keyring as the root secret `CLUSTER_SECRET`, and `init` made
  this PC a client: `~/.config/homelab/client.yaml`.)
- `Imported 9 services: …`, then `Changed on the way:` (e.g. gitea's 2222
  tcp-only) and `Secrets this machine's keyring did not have` with the
  `homelab setup <svc>` to run for each.

If it fails before the agent restarted, it removes the agent and the state
volume it created ("Import failed: removed the agent …") and leaves the swarm,
so a retry starts clean; fix the cause and rerun. If it refuses on a leftover,
the message names the exact `docker rm -f` / `docker volume rm` /
`docker swarm leave --force` to run.

### 5.1 Wait for the agent [minis]

```fish
~/bin/homelab-new status
```

The first call may fail for up to a minute while the agent's tailnet node and
Caddy start. Success: the edge rows show Caddy running, every imported service
is listed (not running yet), with NETWORK `ts` and its URL. If it keeps failing:
`docker logs --tail 100 homelab-agent`.

Any secret reported missing in step 5: `~/bin/homelab-new setup <service>` now.

## 6. Start the services [minis]

The agent brings up, at start, every service recorded as running — but step 4's
`down -a` recorded every service as **stopped**, so nothing is up yet. Start
them all (shared databases first, automatically):

```fish
~/bin/homelab-new up -a
~/bin/homelab-new status
```

Success: every service `running`, each with its URL. A service that was stopped
on purpose before the cutover (see the `desired.yaml` saved in step 2) can be
stopped again with `~/bin/homelab-new down <svc>`.

Exposure: the importer brings each service's `exposure.yaml` along and the
agent loads the routes; no `enable` is needed. If a service shows no NETWORK in
`status` although it was on the tailnet before:
`~/bin/homelab-new enable <svc>` (add `--name=…` / `--ports=…` if it had them).

If `up` fails for a service: `~/bin/homelab-new logs <svc>` and
`~/bin/homelab-new doctor <svc>`. "secret value in service spec" means a
compose file still interpolates a secret into the spec (step 3).

Once happy, make the new binary the default on minis:

```fish
mv ~/bin/homelab-new ~/bin/homelab        # or wherever `command -v homelab` pointed; v0.4.3 is in /var/backups/homelab-v043
```

(The rest of this runbook writes `homelab`; on minis before this `mv` it is
`~/bin/homelab-new`.)

## 7. Verify

### 7.1 This PC as a client [laptop]

```fish
homelab login        # endpoint: accept the default https://homelab.<tailnet>:7443; paste the cluster secret
homelab status
```

Success: the same table as on minis. (Step 8's `join` also makes the laptop a
client; logging in first lets you verify before it is a worker.)

### 7.2 Every service answers [laptop]

```fish
for h in beszel-hub gitea glance grafana immich searxng vault
    printf '%-12s ' $h; curl -s -o /dev/null -w '%{http_code}\n' --max-time 15 https://$h.home.gr00t.fun/
end
```

Success: 200, 30x or 401 for each, never `000` (no DNS/TLS/connection) or `502`
(Caddy up, service not). Then open each in a browser and log in: vaultwarden
must show your vault, immich your library, gitea your repos — the old volumes
were reattached. (`vault` is vaultwarden's subdomain; use your `--name` if you
set one.)

### 7.3 DNS points at the agent [laptop]

```fish
dig +short gitea.home.gr00t.fun @1.1.1.1
tailscale ip -4 homelab
```

Success: the same 100.x address (no longer the old one from 0.7). If not:
`homelab logs -n 200` (the leader agent's log) for the Cloudflare update.

### 7.4 Gitea over SSH on 2222 [laptop]

```fish
ssh -T -p 2222 git@gitea.home.gr00t.fun
git -C ~/dev/cards push
```

Success: the Gitea greeting, then the push succeeds (or "Everything
up-to-date"). No host-key warning: the key lives in the reattached gitea volume.

### 7.5 No secret in the service spec [minis]

```fish
docker service ls --filter label=homelab.service
docker service inspect gitea_gitea --format '{{json .Spec.TaskTemplate.ContainerSpec.Env}}'
docker service inspect gitea_gitea | grep -i -E 'pass|token|secret'
```

Success: the env holds no password or token value; the only secret-related
lines are Swarm secret *references* (names/IDs). `homelab config gitea`, by
contrast, prints the resolved compose file **with** secrets — don't paste it
anywhere.

### 7.6 Reboot without logging in [minis]

```fish
sudo reboot
```

Do **not** log in to minis. From the laptop, after 2–3 minutes:

```fish
homelab status
curl -s -o /dev/null -w '%{http_code}\n' https://gitea.home.gr00t.fun/
```

Success: every service running again and answering, with no login on minis
(the agent restarts with Docker; Swarm restarts the services). Only then log in.

## 8. Join the laptop as a worker [laptop]

The ufw rules of 0.6 must be in place. Pre-pulling immich's ML image avoids a
long first move:

```fish
docker pull ghcr.io/immich-app/immich-machine-learning:release
homelab join --image homelab:dev       # paste the cluster secret
```

Success: it joins the swarm on the laptop's tailnet IP and starts
`homelab-agent` here (a `homelab-<host>` node appears in `tailscale status`).

### 8.1 immich-machine-learning moves to the laptop

```fish
homelab status immich       # NODE column of immich-machine-learning
```

Success: within about 2 minutes (the rebalancer waits 2 min after a worker
comes up, to ignore flapping) immich-machine-learning runs on the laptop;
immich-server and the databases stay on minis. Run an immich "Smart Search" to
see it work across the overlay. If it stays on minis, check `immich/config.yaml`
has `cache_volumes` (step 3; fix with `homelab edit immich config.yaml`) and
`docker node ls` on minis shows the laptop `Ready`.

### 8.2 Suspend the laptop

```fish
systemctl suspend
```

Watch on minis (its console, or ssh from another device):
`homelab status immich` or `docker service ps immich_immich-machine-learning`.
Success: once Swarm marks the laptop `Down` (`docker node ls`; tens of seconds),
immich-machine-learning is back on minis about 20 s later. After waking the
laptop, it moves back after the 2-minute debounce.

### 8.3 The desktop (when it is on)

Same as 0.1 (or `docker save homelab:dev | ssh desktop docker load`), 0.6 and 8
on the desktop.

### 8.4 Optional: exercise upgrade [laptop]

```fish
homelab upgrade --image homelab:dev
```

Replaces the laptop's agent, then the leader's, with the same image; the CLI
prints "agent restarting" and reconnects. Note that `homelab:dev` is used as it
is on each node (no re-pull): rebuild/reload it on every node before a real dev
upgrade.

### 8.5 First cluster-era backup [laptop]

```fish
homelab backup --all
homelab backup --list
```

---

## Rollback

From any step. Data volumes were never removed, so v0.4.3 reattaches them by
name; secrets are still in minis' keyring (the import only copied them).

On the **laptop**, if it joined:

```fish
docker rm -f homelab-agent
docker swarm leave --force
homelab logout
```

On **minis**:

```fish
docker rm -f homelab-agent homelab-agent-replace homelab-agent-old   # the last two usually don't exist
docker service ls --filter label=homelab.service                     # the homelab stacks
docker service rm (docker service ls -q)                             # every Swarm service (only homelab uses Swarm here)
docker swarm leave --force
docker network rm home-services            # the overlay; "not found" is fine (leaving the swarm removes it)
docker network create home-services        # the Compose-era bridge v0.4.3's stacks expect (external)
~/bin/homelab-new logout     # or ~/bin/homelab logout after step 6: drops client.yaml + CLUSTER_SECRET
```

Reinstall v0.4.3 where `homelab` was (if step 6's `mv` happened):

```fish
cp /var/backups/homelab-v043/homelab-v0.4.3 ~/bin/homelab
homelab version                             # v0.4.3
```

Restore the config dir only if step 3's edits or anything else in it is
unwanted (v0.4.3 runs the updated files fine):

```fish
mv ~/.config/homelab ~/.config/homelab.swarm-attempt
tar -C ~/.config -xzf /var/backups/homelab-v043/config-homelab.tgz
```

Bring the Compose stacks back with v0.4.3:

```fish
homelab up                  # the core stack (tailscale sidecar + caddy); `homelab update` if its images are gone
homelab up -a               # every installed service
homelab session install     # only if session mode was installed before step 4
```

Point the wildcard DNS record back: in the Cloudflare dashboard set
`*.home.gr00t.fun` A → the old IP from step 0.7 (DNS only, not proxied). Then
verify as in 7.2/7.4.

Leftover volumes `homelab-agent`, `homelab-shim` and `homelab-backups` are
harmless; remove them only once the rollback is final (`homelab-backups` holds
any cluster-era backups):
`docker volume rm homelab-agent homelab-shim homelab-backups`.
The old `caddy-home` Tailscale machine: keep it until the rollback window is
over (the rollback reuses it), then remove it in the admin console.

---

## Known limitations and follow-ups

- Tor, I2P and Yggdrasil are not served in cluster mode: `enable --tor|--i2p|--ygg`
  fails, and an imported layer is kept in `exposure.yaml` but reported "not
  served" ([Network exposure](../CLAUDE.md#network-exposure)).
- A service recorded as stopped is imported stopped and stays so until `up`
  (step 6); `down -a` before the import records every service stopped.
- `minero` (privileged, devices) cannot run under Swarm
  ([Services under Swarm](../CLAUDE.md#services-under-swarm-internalswarm)).
- Compose-era backups (step 1) are restorable only with v0.4.3; new backups
  live in the leader's `homelab-backups` volume
  ([Backup, restore, prune](../CLAUDE.md#backup-restore-prune)).
- Raw listen ports are tcp only after the import (gitea's 2222).
- The importer does not refresh service files from the catalog (step 3 does it
  by hand).
