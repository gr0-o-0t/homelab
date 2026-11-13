# Tailscale Setup

Every PC in the cluster is already on your tailnet with its own Tailscale. On
top of that, each homelab agent runs an embedded Tailscale node (tsnet) of its
own: the leader's is `homelab` — it carries Caddy, the `*.home.<domain>` sites
and the remote CLI on `:7443` — and each worker's is `homelab-<hostname>`. No
TUN device or Tailscale container is involved.

## 1. Create a tailnet (if you don't have one)

Sign up at [tailscale.com](https://tailscale.com). The free plan supports up to 100 devices — more than enough for a homelab.

Enable **MagicDNS** and **HTTPS certificates** (Admin Console → DNS): the CLI
reaches the leader at `https://homelab.<tailnet>.ts.net:7443`, served with the
node's tailnet certificate.

## 2. Generate an auth key

Go to **Admin Console → Settings → Keys → Generate auth key**.

Recommended settings:

| Option | Value | Reason |
|---|---|---|
| Reusable | Yes | Every agent (leader and workers) registers a node with it |
| Ephemeral | No | The nodes must persist across agent restarts |
| Pre-authorized | Yes | Skips manual device approval |
| Tags | `tag:homelab` | Enables ACL-based access control |

Give it to homelab as the `TS_AUTHKEY` secret: `homelab init` takes it from
the machine's keyring or prompts for it, and stores it in the leader's vault —
never in a file or the agent's environment. `homelab join` receives it from
the leader with the Swarm join token and seeds the worker agent's vault with
it, which is why the key must be reusable.

> **Note:** Auth keys expire. An agent whose node is already registered keeps
> working (its node state lives in the agent volume); a new key is only needed
> to register a node again — set it with `homelab setup`.

## 3. Configure ACLs (recommended)

Two kinds of tailnet nodes are involved, so use two tags:

- `tag:homelab` — the agents' tsnet nodes (`homelab`, `homelab-<host>`): the
  auth key from step 2 carries it. They serve the sites (443), the remote CLI
  (7443) and the raw ports services declare.
- `tag:homelab-host` — the PCs' own Tailscale nodes (tag each cluster PC with
  `sudo tailscale up --advertise-tags=tag:homelab-host`). Swarm traffic runs
  between these addresses.

A complete policy (adapt the owners and the raw ports):

```json
{
  "tagOwners": {
    "tag:homelab":      ["autogroup:admin"],
    "tag:homelab-host": ["autogroup:admin"]
  },
  "acls": [
    {
      // Clients reach the sites, the remote CLI and the raw ports services declare
      // (22 and 2222 are Forgejo's and Gitea's SSH; list what you use).
      "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["tag:homelab:443", "tag:homelab:7443", "tag:homelab:22", "tag:homelab:2222"]
    },
    {
      // Swarm between the cluster PCs, in both directions: management (2377),
      // gossip (7946, tcp + udp) and the VXLAN overlay (4789, udp).
      "action": "accept",
      "src": ["tag:homelab-host"],
      "dst": [
        "tag:homelab-host:2377",
        "tag:homelab-host:7946",
        "tag:homelab-host:4789"
      ]
    },
    {
      // The agents reach each other's API (the leader relays exec and upgrades to the workers).
      "action": "accept",
      "src": ["tag:homelab"],
      "dst": ["tag:homelab:7443"]
    }
  ]
}
```

(The admin console's policy editor accepts the `//` comments.) An ACL port
without a protocol covers tcp, udp and icmp, which is what 7946 (tcp + udp) and
4789 (udp) need. The leader's own `ufw` (or other host firewall) must allow the
same Swarm ports on `tailscale0`; see the README's Firewall section.

If your PCs are not tagged, use their user or `autogroup:member` as `src` and
`dst` of the Swarm rule instead; the requirement is only that the cluster hosts
reach each other on 2377/tcp, 7946/tcp+udp and 4789/udp.

## 4. Verify the node is registered

```bash
homelab status          # the Edge table: caddy running
homelab caddy status    # the agent's Caddy answers on its admin socket
```

The `homelab` machine appears in **Admin Console → Machines** with a
`100.x.x.x` address. The leader agent points the `*.home.<domain>` A record at
that address itself (see `docs/architecture.md`).

## 5. Approve the device (if not pre-authorized)

Go to **Admin Console → Machines** and approve `homelab` (and any
`homelab-<hostname>` worker node) if it appears as pending.
