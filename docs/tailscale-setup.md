# Tailscale Setup

## 1. Create a tailnet (if you don't have one)

Sign up at [tailscale.com](https://tailscale.com). The free plan supports up to 100 devices — more than enough for a homelab.

## 2. Generate an auth key

Go to **Admin Console → Settings → Keys → Generate auth key**.

Recommended settings:

| Option | Value | Reason |
|---|---|---|
| Reusable | Yes | Allows container restarts without generating a new key |
| Ephemeral | No | The node should persist across restarts |
| Pre-authorized | Yes | Skips manual device approval |
| Tags | `tag:homelab` | Enables ACL-based access control |

Paste the key as `TS_AUTHKEY` in your `.env`.

> **Note:** Auth keys expire. Generate a new key and update `.env` if the container fails to authenticate after a long period.

## 3. Configure ACLs (recommended)

In the Tailscale admin console, edit your ACL policy to restrict which tailnet devices can reach the Caddy node. Example policy:

```json
{
  "tagOwners": {
    "tag:homelab": ["autogroup:admin"]
  },
  "acls": [
    {
      "action": "accept",
      "src": ["autogroup:member"],
      "dst": ["tag:homelab:443", "tag:homelab:80"]
    }
  ]
}
```

This allows any authenticated tailnet member to reach port 443 on homelab-tagged devices, and nothing else.

## 4. TUN device on the host

The Tailscale container runs with `TS_USERSPACE: "false"` (kernel-mode networking, required for the shared `network_mode: service:tailscale` trick). This needs `/dev/net/tun` on the host:

```bash
# Check if the device exists
ls -la /dev/net/tun

# If missing, load the module (Ubuntu/Debian)
sudo modprobe tun

# Make it persistent across reboots
echo "tun" | sudo tee /etc/modules-load.d/tun.conf
```

### WSL2

WSL2 kernels typically support TUN. If `/dev/net/tun` is missing:

```bash
sudo mkdir -p /dev/net
sudo mknod /dev/net/tun c 10 200
sudo chmod 0666 /dev/net/tun
```

Add to `/etc/wsl.conf` to persist:

```ini
[boot]
command = mkdir -p /dev/net && mknod -m 0666 /dev/net/tun c 10 200 || true
```

## 5. Verify the node is registered

```bash
homelab status
```

You should see your `TS_HOSTNAME` node (e.g. `caddy-home`) listed with a `100.x.x.x` Tailscale IP. That IP is what you'll use as the value for the Cloudflare A record.

## 6. Approve the device (if not pre-authorized)

Go to **Admin Console → Machines** and approve `caddy-home` if it appears as pending.
