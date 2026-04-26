# Cloudflare Setup

## 1. Create a scoped API token

Go to **Cloudflare → My Profile → API Tokens → Create Token**.

Use the "Edit zone DNS" template, then restrict it:

| Field | Value |
|---|---|
| Permissions | Zone – Zone – Read |
| Permissions | Zone – DNS – Edit |
| Zone Resources | Include – Specific zone – `example.com` |

Copy the token and paste it as `CLOUDFLARE_API_TOKEN` in your `.env`.

> **Do not** use the Global API Key. A scoped token limits blast radius if it ever leaks.

## 2. Add the wildcard A record

Start the core stack and find Caddy's Tailscale IP:

```bash
homelab core start
homelab ts status
```

Look for the node named after your `TS_HOSTNAME` (e.g. `caddy-home`) and note its `100.x.x.x` address. Tailscale IPs are stable — they only change if the node is removed and re-added to the tailnet.

In Cloudflare DNS for your domain, add:

| Type | Name | Value | Proxy |
|---|---|---|---|
| `A` | `*.home` | `100.x.x.x` (your Caddy node's Tailscale IP) | DNS only ⚠️ |

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
