# Migrating minis from Compose (v0.4.3) to the Swarm cluster

A one-time, all-at-once cutover: back up with the old binary, stop the Compose
stacks, create the cluster with `homelab init --import`, bring the services back
up, verify, then join the laptop. Downtime is expected (plan about an hour, more
if the backup of immich is large).

Conventions:

- **[laptop]** / **[minis]** say where a command runs. Both login shells are
  fish; every command below works in fish (command substitution is `(…)`).
- On minis, `homelab` is the **old** v0.4.3 binary until step 6 replaces it;
  `homelab-new` (in `/usr/local/bin`) is the new one. On the laptop, `homelab`
  becomes the new one in 0.1.
- Data lives in Docker named volumes under `/var/lib/docker`. Nothing in this
  runbook removes a data volume; that is what makes the rollback cheap.
- `sudo` below may be `doas` on minis (Alpine), whichever it has.
- **Stop** means: do not go on to the next step. Investigate, and if it cannot
  be fixed in place, follow the [rollback](#rollback), which works from any
  step.

---

## 0. Preconditions

### 0.1 Build the image and the binary [laptop]

```fish
cd ~/src/local/homelab
git switch feat/swarm-cluster; git status --short   # clean tree, the commit you mean to deploy
type -a homelab                                     # today: ~/go/bin/homelab, v0.4.3 — note every path listed
mkdir -p ~/homelab-rollback; cp ~/go/bin/homelab ~/homelab-rollback/homelab-v0.4.3   # off PATH
make image                                          # → image homelab:dev + dist/image/linux_amd64/homelab
make install                                        # go install → (go env GOBIN) or (go env GOPATH)/bin, i.e. ~/go/bin/homelab
type -a homelab                                     # exactly one entry, ~/go/bin/homelab
homelab version
docker image inspect homelab:dev --format '{{index .Config.Labels "org.opencontainers.image.version"}}'
```

Success: `type -a` lists one `homelab`, and both versions print the same
`v0.0.0-<date>-<commit>` (not `v0.4.3`). If another `homelab` is listed before
`~/go/bin`, replace or remove it. `make image` fails with "the image binary is
not statically linked" if cgo leaked into the build — rerun from a clean shell.

### 0.2 Copy the image and the new binary to minis [laptop]

The binary goes to `/usr/local/bin`, not `~/bin`: minis' home is ecryptfs, and
an ssh session without the desktop login may write into the *unmounted* home.

```fish
docker save homelab:dev | ssh minis docker load
cat dist/image/linux_amd64/homelab | ssh minis 'cat > /tmp/homelab-new'
ssh -t minis 'sudo install -m 0755 /tmp/homelab-new /usr/local/bin/homelab-new && rm /tmp/homelab-new'
ssh minis 'homelab-new version'
```

Success: `Loaded image: homelab:dev`, and the same version as in 0.1 (a fish
greeting line before it is harmless: it goes to ssh's stdout, never into
`docker load`'s or `cat`'s stdin). Do not use `scp`: it can choke on the
remote shell's greeting.

### 0.3 Host checks [minis]

```fish
type -a homelab                                        # every path of the old binary — WRITE THEM DOWN (step 6, rollback)
homelab version                                        # v0.4.3 — the old binary, used for steps 1–4
docker info --format '{{.ServerVersion}} swarm={{.Swarm.LocalNodeState}}'   # 29.x swarm=inactive
docker volume ls --filter name=homelab-agent           # nothing (a leftover makes init refuse)
tailscale ip -4                                        # a 100.x address
tailscale status --json | grep MagicDNSSuffix          # non-empty: MagicDNS is on
df -h /var                                             # room for the backup (immich uploads dominate)
```

**Boot order.** Docker and tailscaled must start at boot without a login
(step 7.6), and dockerd must start *after* Tailscale: Swarm binds its control
and data path to the tailscale0 address, and a dockerd that starts first comes
up with a broken swarm. On Alpine/OpenRC:

```fish
rc-update show default | grep -E 'docker|tailscale'    # both in the default runlevel
rc-service -l | grep -i tailscale                      # the service's exact name, usually `tailscale`
grep rc_after /etc/conf.d/docker                       # already set?
echo 'rc_after="tailscale"' | sudo tee -a /etc/conf.d/docker   # use the name printed above; merge into an existing rc_after instead
```

(On systemd: `systemctl is-enabled docker tailscaled`, and a drop-in with
`After=tailscaled.service` / `Wants=tailscaled.service` for docker.)

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
service secret from it. Run this step and steps 1–6 in a terminal of the
logged-in desktop session, where the home is mounted and the keyring unlocked.

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

### 0.7 Record what is there now

[laptop]:

```fish
dig +short gitea.home.gr00t.fun @1.1.1.1      # the old Caddy sidecar's 100.x IP — write it down (rollback)
git -C ~/dev/cards remote -v                  # ssh://git@gitea.home.gr00t.fun:2222/groot/cards.git
```

[minis] — services exposed through the Cloudflare Tunnel (`--cf`):

```fish
grep -l cf ~/.config/homelab/services/*/exposure.yaml
```

Today every service is tailnet-only, so this prints nothing (fish may say "No
matches for wildcard" when no service has an exposure.yaml — also "none").
Any service it lists needs step 6.1.

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
stopped for its own snapshot and started again. If a service fails: stop, fix
it, rerun — the backup is the safety net for the data volumes.

This backup can be restored **only with v0.4.3** (see
[limitations](#known-limitations-and-follow-ups)).

## 2. Copy the config dir and the old binary aside [minis]

Before anything stops (step 4 rewrites `state/desired.yaml` to "stopped"):

```fish
tar -C ~/.config -czf /var/backups/homelab-v043/config-homelab.tgz homelab
command -v homelab > /var/backups/homelab-v043/old-bin-path.txt     # the path from 0.3 (first on PATH)
cp (command -v homelab) /var/backups/homelab-v043/homelab-v0.4.3
tar -tzf /var/backups/homelab-v043/config-homelab.tgz | head
cat ~/.config/homelab/state/desired.yaml      # which services were running — keep this
```

The tarball holds no secrets (they are in the keyring). The copy is outside the
login-mounted home, so it survives anything that happens to it.

## 3. Update four service files, and check every data mount [laptop → minis]

The importer copies each service's `docker-compose.yml` and `config.yaml` **as
they are on minis**; it does not refresh them from the catalog. Exactly four
files changed for Swarm since v0.4.3 and must be updated:

| file | why it matters |
|---|---|
| `redis/docker-compose.yml` | the old one puts `REDIS_PASSWORD` on the command line; Swarm would show it in the spec, so `up redis` refuses it ("secret value in service spec") |
| `gitea/docker-compose.yml` | `GITEA__server__SSH_PORT: "2222"`, so clone URLs carry the port |
| `gitea/config.yaml` | the `2222:2222/tcp` listen port (gitea's SSH on the tailnet) |
| `immich/config.yaml` | `cache_volumes: [immich-model-cache]` + `placement:`, or immich-machine-learning is pinned to minis and never moves to the laptop |

**Nothing else is taken from the catalog.** A different compose file elsewhere
can mount a different volume and bring a service up on an empty one.

### 3.1 Compare [laptop → minis]

[laptop] — send the whole current catalog (small) to minis:

```fish
cd ~/src/local/homelab
tar -C assets/services -cf - . | ssh minis 'rm -rf /tmp/hl-catalog && mkdir -p /tmp/hl-catalog && tar -C /tmp/hl-catalog -xf -'
```

[minis] — every installed service, derived from the config dir:

```fish
for s in (ls ~/.config/homelab/services)
    echo "=== $s"
    diff -u ~/.config/homelab/services/$s/docker-compose.yml /tmp/hl-catalog/$s/docker-compose.yml
    diff -u ~/.config/homelab/services/$s/config.yaml /tmp/hl-catalog/$s/config.yaml
end
```

Expected differences: the four files above (redis's `command:`/`environment:`,
gitea's `SSH_PORT` and port, immich's `cache_volumes`/`placement`), plus your
own variable values in `config.yaml` files. **Any other difference** — above
all in `volumes:`, `image:` or `container_name:` — **stop**: keep minis' file
and decide before going on (the import will use minis' file as it is).

### 3.2 Update the four files [minis]

Compose files — copy them only if 3.1 showed nothing in them but the known
change:

```fish
cp /tmp/hl-catalog/redis/docker-compose.yml ~/.config/homelab/services/redis/
cp /tmp/hl-catalog/gitea/docker-compose.yml ~/.config/homelab/services/gitea/
```

`config.yaml` files — never copy them whole (they hold your variable values);
add the missing keys:

```fish
grep -E 'cache_volumes|placement' ~/.config/homelab/services/immich/config.yaml   # nothing? then:
printf '\ncache_volumes: [immich-model-cache]\nplacement:\n  immich-machine-learning: worker\n' >> ~/.config/homelab/services/immich/config.yaml
grep -n -A4 '^ports:' ~/.config/homelab/services/gitea/config.yaml               # must list 2222:2222/tcp
```

If gitea's `ports:` lacks `- 2222:2222/tcp`, add that line under `- 3000` with
`$EDITOR` (a tcp+udp `2222` is rewritten to tcp-only by the importer and
reported). Rerun the 3.1 loop: only your values should differ now. v0.4.3 runs
these files too, so the rollback does not need them reverted; the tarball from
step 2 has the originals.

### 3.3 Data-mount check [minis]

While the Compose stacks still run, list every mount of every Compose
container:

```fish
for c in (docker ps --format '{{.Names}}' --filter label=com.docker.compose.project.working_dir)
    docker inspect $c --format '{{.Name}}  ({{index .Config.Labels "com.docker.compose.project.working_dir"}}){{range .Mounts}}{{"\n"}}    {{.Type}} {{if .Name}}{{.Name}}{{else}}{{.Source}}{{end}} -> {{.Destination}}{{end}}'
end
```

Check, for each service container (under `~/.config/homelab/services/`):

- Every `volume` mount with a readable name must be declared in that service's
  `docker-compose.yml` (its `name:` under top-level `volumes:`, or
  `<service>_<key>` without one). Swarm reuses volumes by that name; one not
  declared would not be reattached. On 2026-11-11 minis showed:
  `homelab_postgres_data` (→ `/var/lib/postgresql`), `homelab_redis_data`,
  `immich_uploads`, `immich_model-cache`, gitea's data and config volumes,
  `vaultwarden_data`, `homelab_grafana_data`, `beszel-hub_data`,
  `searxng_config`.
- A `volume` whose name is 64 hex characters is **anonymous**. On a data path
  that means **stop**: Swarm would give the task a new, empty anonymous
  volume. The one expected exception is searxng's `/var/cache/searxng` — a
  cache, safe to lose.
- `bind` mounts from the home (glance's `services/glance/config`, grafana's
  `services/grafana/provisioning`) are expected: the importer copies those
  files into the agent and Swarm delivers them as configs.
- Core containers (`tailscale`, `caddy`, …, under `~/.config/homelab/core`)
  are not migrated; ignore their mounts.

## 4. Stop the Compose stacks [minis]

```fish
homelab session status          # v0.4.3 session mode: is the unit installed/active?
homelab session uninstall       # only if it is installed — otherwise skip
homelab down -a                 # every service: containers stopped and removed
homelab down                    # no service = the core stack (tailscale + caddy + enabled extensions)
```

`homelab session status` runs `systemctl --user`; on OpenRC it fails, which
means session mode was never installed — skip `uninstall`.

If `homelab down` (core) fails, stop the core directly:
`docker compose -f ~/.config/homelab/core/docker-compose.yml down`. That only
stops the core's default services; extension containers of other profiles
(`cloudflared`, `tor`, `i2p`, `yggdrasil`) may remain — the next check lists
them; `docker rm -f` each.

Check:

```fish
docker ps -a --filter label=com.docker.compose.project.working_dir --format '{{.Names}}  {{.Label "com.docker.compose.project.working_dir"}}'
docker network inspect home-services --format '{{range .Containers}}{{.Name}} {{end}}'
docker ps --filter name=tv-kiosk
```

Success: the first command lists nothing under `~/.config/homelab` (other
projects, if any, are fine); the second prints an empty line; `tv-kiosk` is
still `Up` (it is not on `home-services`; leave it alone). A homelab container
that remains: `docker rm -f <name>`.

## 5. Create the cluster with the import [minis, logged-in desktop session]

```fish
homelab-new init --import ~/.config/homelab --image homelab:dev
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
- `Imported N services: …`, then `Changed on the way:` (e.g. gitea's 2222
  tcp-only) and `Secrets this machine's keyring did not have` with the
  `homelab setup <svc>` to run for each.

If it fails after `swarm init`, it leaves the swarm again, and before the agent
restarted it also removes the agent and the state volume it created ("Import
failed: removed the agent …"), so a retry starts clean; fix the cause and
rerun. If it refuses on a leftover, the message names the exact `docker rm -f`
/ `docker volume rm` / `docker swarm leave --force` to run.

### 5.1 Wait for the agent [minis]

```fish
homelab-new status
```

The first call may fail for up to a minute while the agent's tailnet node and
Caddy start. Success: the edge rows show Caddy running, every imported service
is listed (not running yet), with NETWORK `ts` and its URL. If it keeps
failing: `docker logs --tail 100 homelab-agent`; stop.

Any secret reported missing in step 5: `homelab-new setup <service>` now.

## 6. Start the services, retire the old binary [minis]

The agent brings up, at start, every service recorded as running — but step 4's
`down -a` recorded every service as **stopped**, so nothing is up yet. Start
them all (shared databases first, automatically):

```fish
homelab-new up -a
homelab-new status
```

Success: every service `running`, each with its URL. A service that was stopped
on purpose before the cutover (see the `desired.yaml` saved in step 2) can be
stopped again with `homelab-new down <svc>`.

Exposure: the importer brings each service's `exposure.yaml` along and the
agent loads the routes; no `enable` is needed. If a service shows no NETWORK in
`status` although it was on the tailnet before:
`homelab-new enable <svc>` (add `--name=…` / `--ports=…` if it had them).

If `up` fails for a service: `homelab-new logs <svc>` and
`homelab-new doctor <svc>`; stop. "secret value in service spec" means a
compose file still interpolates a secret into the spec (step 3).

**Replace the old binary now.** Left on PATH, a v0.4.3 `homelab up` would start
Compose containers beside the Swarm tasks, on the same volumes. Replace
exactly the path(s) recorded in 0.3 / step 2:

```fish
cat /var/backups/homelab-v043/old-bin-path.txt
sudo install -m 0755 /usr/local/bin/homelab-new (cat /var/backups/homelab-v043/old-bin-path.txt)
type -a homelab        # every path listed must be the new binary (repeat the install for any other)
homelab version        # the new version from 0.1 — NOT v0.4.3
```

Success: `homelab version` prints the new version. Do not go on otherwise.
From here on, minis' `homelab` is the new CLI (v0.4.3 stays in
`/var/backups/homelab-v043/homelab-v0.4.3` for the rollback).

### 6.1 Cloudflare Tunnel origin (skip if 0.7 listed no `--cf` service)

v0.4.3's cloudflared sent public traffic to `http://tailscale:80` (Caddy in the
sidecar's namespace). In cluster mode cloudflared reaches the agent's Caddy as
`http://homelab-edge:80`. The public hostnames' origin lives in Cloudflare, so
change it there:

1. Cloudflare Zero Trust → Networks → Tunnels → your tunnel → Public Hostname.
2. For every hostname: Service type `HTTP`, URL `homelab-edge:80`. Save.

Verify [laptop]:

```fish
homelab cf status
homelab status --json | jq -r '.services[] | select(.layers | index("cf")) | .addresses.cf[]?.URL'
```

Then, from a device **off the tailnet** (phone on mobile data, Tailscale off),
open each URL printed. Success: `cf status` shows the tunnel connected, and
each URL serves its page. If not: `homelab cf logs`; stop (or roll this layer
back by pointing the hostnames at `http://tailscale:80` again during a
rollback).

## 7. Verify

Each check says what to look at when it fails. Anything that fails and cannot
be fixed in place: **stop** and decide on the rollback before step 8.

### 7.1 This PC as a client [laptop]

```fish
homelab login        # endpoint: accept the default https://homelab.<tailnet>:7443; paste the cluster secret
homelab status
```

Success: the same table as on minis. (Step 8's `join` also makes the laptop a
client; logging in first lets you verify before it is a worker.) If `login`
cannot connect: `tailscale status | grep homelab` (the node must be online and
named exactly `homelab`, see 0.4).

### 7.2 Every service answers [laptop]

The hosts come from the cluster, not from memory:

```fish
for u in (homelab status --json | jq -r '.services[].addresses.ts[]?.URL')
    printf '%-40s ' $u; curl -s -o /dev/null -w '%{http_code}\n' --max-time 15 $u/
end
```

Success: 200, 30x or 401 for each. Then open each in a browser and log in:
vaultwarden must show your vault, immich your library, gitea your repos —
proof that the old volumes were reattached.

If it fails:
- `000` (DNS/TLS/connection): 7.3 first, then `homelab logs -n 200` (the
  leader agent: Caddy, certificates).
- `502`: the route is there but the service is not: `homelab status <svc>`,
  `homelab logs <svc>`.
- Empty data (a fresh setup page, an empty vault): **stop immediately — do not
  log in or create anything**. A volume was not reattached; `homelab down
  <svc>`, compare `docker volume ls` with 3.3, and roll back if the original
  volume cannot be found or wired back.

### 7.3 DNS points at the agent [laptop]

```fish
dig +short gitea.home.gr00t.fun @1.1.1.1
tailscale ip -4 homelab
```

Success: the same 100.x address (no longer the old one from 0.7). If not:
`homelab logs -n 200` for the Cloudflare DNS update (token, DOMAIN), and wait
for the record's TTL.

### 7.4 Gitea over SSH on 2222 [laptop]

```fish
ssh -T -p 2222 git@gitea.home.gr00t.fun
git -C ~/dev/cards push
```

Success: the Gitea greeting, then the push succeeds (or "Everything
up-to-date"). No host-key warning: the key lives in the reattached gitea volume.

If it fails:
- Connection refused / timeout: is the raw port routed?
  `homelab status gitea` (PORTS) and, on minis,
  `docker exec homelab-agent cat /var/lib/homelab/state/services/gitea/exposure.yaml`.
  If that lists explicit `ports:` without the ssh one, enable both port names
  (`default` is the bare `3000`, `2222` the listen port):
  `homelab enable gitea --ports=default,2222`.
- A **host-key changed** warning: stop — gitea is not on its old data volume
  (see 7.2's empty-data case).

### 7.5 No secret in any service spec [minis]

```fish
for s in (docker service ls --format '{{.Name}}')
    echo "=== $s"
    docker service inspect $s --format '{{json .Spec.TaskTemplate.ContainerSpec.Env}}'
end
docker service ls -q | xargs docker service inspect | grep -i -E 'pass|token|secret'
```

Success: no env holds a password or token value (only plain settings and
whatever the compose file sets literally); the `grep` lines are Swarm secret *references*
(`SecretName`/`SecretID`, `hl_<service>_env_…` names), never values. `homelab config
<svc>`, by contrast, prints the resolved compose file **with** secrets — don't
paste it anywhere.

If a value shows up (deploys refuse a spec containing a known secret, so this
means one the vault does not know about): stop. `homelab down <svc>`, move the
value out of the compose file with `homelab edit <svc> docker-compose.yml`
(into a declared secret, `homelab setup <svc>`), and rotate that credential,
since it has been in the spec.

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

If it fails, on minis (console or ssh):

```fish
docker info --format '{{.Swarm.LocalNodeState}}'    # must be "active"
docker logs --tail 100 homelab-agent
```

`pending`/`error`, or Swarm errors about the advertise/data-path address in the
Docker log, mean dockerd started before tailscale0 had its address: check the
`rc_after` from 0.3, then `sudo rc-service docker restart` and recheck. If the
swarm does not become `active` and the services do not return, roll back.

### 7.7 Go / no-go for joining the laptop

Go on to step 8 only if all of these hold:

- [ ] 7.2: every URL answers and shows its **old** data;
- [ ] 7.3: DNS points at the `homelab` node;
- [ ] 7.4: the push over :2222 works with no host-key warning;
- [ ] 7.5: no secret value in any spec;
- [ ] 7.6: everything came back after a reboot without login;
- [ ] 6: `homelab version` on minis is the new one; 6.1 done if needed.

Otherwise fix it or roll back now — a rollback before the laptop joins is the
simplest one.

## 8. Join the laptop as a worker [laptop]

The ufw rules of 0.6 must be in place. Pre-pull immich's machine-learning image
so the first move is quick — its tag follows `IMMICH_VERSION`, so take the
reference the stack actually deploys:

```fish
homelab images -q immich                     # e.g. ghcr.io/immich-app/immich-machine-learning:release
docker pull (homelab images -q immich | grep machine-learning)
homelab join --image homelab:dev             # paste the cluster secret
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

Rolling back from steps 1–4 (before `init`), most commands below fail with
"No such container", "not found" or "This node is not part of a swarm": that is
expected — run the rest.

On the **laptop**, if it joined:

```fish
docker rm -f homelab-agent
docker swarm leave --force
homelab logout
```

On **minis**:

```fish
homelab-new logout             # drops client.yaml and CLUSTER_SECRET (the new binary; after step 6 also `homelab logout`)
docker rm -f homelab-agent homelab-agent-replace homelab-agent-old   # the last two usually don't exist
docker service ls --filter label=homelab.service                     # the homelab stacks
docker service rm (docker service ls -q)                             # every Swarm service (only homelab uses Swarm here)
while count (docker ps -aq --filter label=com.docker.swarm.service.id) >/dev/null; sleep 2; end   # wait for the tasks to go
docker swarm leave --force
docker network rm home-services            # the overlay; "not found" is fine (leaving the swarm removes it)
docker network create home-services        # the Compose-era bridge v0.4.3's stacks expect (external)
docker ps -a --filter label=com.docker.swarm.service.id              # must print only the header
```

Put v0.4.3 back at the recorded path (if step 6 replaced it):

```fish
sudo install -m 0755 /var/backups/homelab-v043/homelab-v0.4.3 (cat /var/backups/homelab-v043/old-bin-path.txt)
type -a homelab; homelab version            # v0.4.3 at every path listed
```

On the laptop, likewise: `cp ~/homelab-rollback/homelab-v0.4.3 ~/go/bin/homelab`
if you want v0.4.3 back there.

Restore the config dir only if step 3's edits or anything else in it is
unwanted (v0.4.3 runs the updated files fine):

```fish
mv ~/.config/homelab ~/.config/homelab.swarm-attempt
tar -C ~/.config -xzf /var/backups/homelab-v043/config-homelab.tgz
```

Bring the Compose stacks back with v0.4.3 — only once the last check above
printed no Swarm task container:

```fish
homelab up                  # the core stack (tailscale sidecar + caddy); `homelab update` if its images are gone
homelab up -a               # every installed service
homelab session install     # only if session mode was installed before step 4
```

Point the wildcard DNS record back: in the Cloudflare dashboard set
`*.home.gr00t.fun` A → the old IP from step 0.7 (DNS only, not proxied). If 6.1
was done, set every tunnel public hostname back to `http://tailscale:80`. Then
verify as in 7.2/7.4 (with v0.4.3's `homelab status`).

Leftover volumes `homelab-agent`, `homelab-shim` and `homelab-backups`:
`homelab-agent` holds **every secret** (the vault and its key) — do not copy or
keep it around once the rollback is final; `homelab-backups` holds any
cluster-era backups. Remove them once the rollback is final:
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
- The importer does not refresh service files from the catalog, and does not
  check data mounts (step 3 does both by hand).
- The Cloudflare Tunnel origin is not switched automatically (step 6.1).
