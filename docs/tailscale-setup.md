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

In the Tailscale admin console, edit your ACL policy to restrict which tailnet devices can reach the homelab nodes. Example policy:

```json
{
  "tagOwners": {
    "tag:homelab": ["autogroup:admin"]
  },
  "acls": [
    {
      "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["tag:homelab:443", "tag:homelab:7443"]
    }
  ]
}
```

This allows any authenticated tailnet member to reach the sites (443) and the
remote CLI (7443, which also needs the cluster secret) on homelab-tagged nodes.
Add the raw ports your services declare (e.g. `tag:homelab:22` for Gitea SSH).

The Swarm traffic between PCs (2377, 7946, 4789) runs between the hosts' own
Tailscale addresses, so those hosts must be able to reach each other on those
ports as well.

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
