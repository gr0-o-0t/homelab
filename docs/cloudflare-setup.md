# Cloudflare Setup

## 1. Create a scoped API token

Go to **Cloudflare → My Profile → API Tokens → Create Token**.

Use the "Edit zone DNS" template, then restrict it:

| Field | Value |
|---|---|
| Permissions | Zone – Zone – Read |
| Permissions | Zone – DNS – Edit |
| Zone Resources | Include – Specific zone – `example.com` |

Copy the token, then store it with `homelab setup` — it goes into the leader's
vault (`CLOUDFLARE_API_TOKEN` is a root secret), not a file on disk.

> **When you change `DOMAIN`, re-scope the token too.** Zone Resources still
> point at the old zone, so Caddy's DNS-01 challenge fails with
> `expected 1 zone, got 0 for <newdomain>.` — the token is valid, it just
> cannot see the new zone. Same trap if the zone is still pending nameserver
> delegation.

> **Do not** use the Global API Key. A scoped token limits blast radius if it ever leaks.

## 2. Add the wildcard A record

The leader agent creates and updates this record itself, with the same API
token, each time it starts: `*.<HOME_SUBDOMAIN>.<DOMAIN>` → the IPv4 of its
tailnet node `homelab` (see the Tailscale admin console). Its log says so:

```bash
homelab logs | grep DNS      # agent: DNS *.home.example.com → 100.x.x.x
```

Tailscale IPs are stable — they only change if the node is removed and
re-added to the tailnet, and the agent then updates the record. For
reference, or to create it by hand:

In Cloudflare DNS for your domain, add:

| Type | Name | Value | Proxy |
|---|---|---|---|
| `A` | `*.home` | `100.x.x.x` (the `homelab` node's Tailscale IP) | DNS only ⚠️ |

> **Critical:** Set proxy to **DNS only** (grey cloud). Enabling the orange cloud routes traffic through Cloudflare's public network, bypassing Tailscale entirely.

If you also want the apex (`home.example.com`) to work, add a second record:

| Type | Name | Value | Proxy |
|---|---|---|---|
| `A` | `home` | `100.x.x.x` | DNS only |

### Why A record, not CNAME?

A CNAME pointing to `<ts-hostname>.<tailnet>.ts.net` seems natural, but public recursive resolvers (including DNS-over-HTTPS providers used by browsers) query Tailscale's authoritative DNS for that hostname and get NXDOMAIN — because ts.net hostnames are only resolvable via Tailscale's internal DNS (`100.100.100.100`), not the public internet. A direct A record to the Tailscale IP avoids this chain entirely.

## 3. Verify DNS propagation

```bash
# Should return your Tailscale IP (100.x.x.x)
dig immich.home.example.com A +short

# Test via a specific public resolver
dig immich.home.example.com A +short @1.1.1.1
```

Both queries should return the same `100.x.x.x` address. If you see a public IP instead, you accidentally enabled Cloudflare proxy — switch it back to DNS only.

> The Tailscale IP will only be reachable from inside your tailnet. Querying from a device not on the tailnet will resolve the IP correctly but the connection will time out — that is expected and correct.

## 4. Browser DNS-over-HTTPS (DoH) note

Browsers with Secure DNS (DoH) enabled use their own resolver, which bypasses your system's DNS. This works fine with an A record since the A record resolves publicly to the Tailscale IP. However, if your browser shows `ERR_DNS_SECURE_RESOLVER_HOSTNAME_RESOLUTION_FAILED`, check the Secure DNS setting:

- **Brave:** `brave://settings/security` → Use secure DNS → set to "With your current service provider"
- **Chrome:** `chrome://settings/security` → same option
- **Firefox:** Settings → Network Settings → DNS over HTTPS → set to "Default protection" or off

This is only needed if the browser's DoH provider has not yet cached the updated record.

## 5. Cloudflare Tunnel (optional, for public services)

Only needed to expose a service on the public internet with `homelab enable
<service> --cf`. The tailnet-only setup above does not use it.

1. In the Cloudflare dashboard open **Zero Trust → Networks → Tunnels →
   Create a tunnel**, choose the **Cloudflared** connector type and name it
   (e.g. `homelab`). Do not run the install command it shows: homelab runs
   cloudflared itself, as a Swarm service pinned to the core node.
2. Copy the **tunnel token** from that install command (the long string after
   `--token`) into the root secret `CF_TUNNEL_TOKEN`, and set the var
   `CF_TUNNEL_NAME` to the tunnel's name (the root wizard asks for both):

   ```bash
   homelab setup                                  # root wizard, or:
   echo '{"CF_TUNNEL_TOKEN":"<token>"}' | homelab setup --secrets-stdin
   homelab setup --set CF_TUNNEL_NAME=<tunnel name>
   ```

3. Turn the extension on; the leader deploys cloudflared:

   ```bash
   homelab ext enable cf
   homelab cf status
   ```

4. The tunnel's ingress is managed in the dashboard, not by homelab. In the
   tunnel's **Public Hostname** settings add either one rule per public host,
   or ONE wildcard rule added once: hostname `*.<DOMAIN>`, service type
   **HTTP**, URL `homelab-edge:80` (the leader's plain-HTTP listener on the
   overlay; its catch-all closes hosts no service answers). Without a rule the
   CNAME exists but the host answers 502/404.
5. `homelab enable <service> --cf` only creates the proxied CNAME
   `<service>.<DOMAIN>` → `<tunnel-id>.cfargotunnel.com` through the Cloudflare
   API (it needs the `CLOUDFLARE_API_TOKEN` from step 1, with DNS – Edit); it
   does not touch the tunnel's public hostnames.

```bash
homelab enable paperless --cf     # public, in addition to the private tailnet route
homelab disable paperless --cf    # removes the public route and its CNAME
```
