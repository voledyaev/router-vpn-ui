# Custom routing rules format

yonder accepts routing rules in **Xray's native JSON format** — exactly what XKeen ships in `/opt/etc/xray/configs/05_routing.json`. No custom DSL, no proprietary format.

This means you can:

- Copy rules from any other Xray-based tool
- Use community-maintained rule sets that target Xray
- Reference Xray's own [routing documentation](https://xtls.github.io/en/config/routing.html) for any field

The app fetches the URL once when you set it (and on each manual refresh) and validates the structure before applying.

## Accepted shapes

The validator accepts three nested levels for convenience — pick whichever feels most natural:

### 1. Full XKeen-compatible config — drop-in for `05_routing.json`

```json
{
  "routing": {
    "domainStrategy": "AsIs",
    "rules": [
      { "type": "field", "outboundTag": "direct", "ip": ["10.0.0.0/8"] },
      { "type": "field", "outboundTag": "proxy",  "domain": ["geosite:google"] }
    ]
  }
}
```

### 2. Just the `rules` key

```json
{
  "rules": [
    { "outboundTag": "direct", "ip": ["10.0.0.0/8"] }
  ]
}
```

### 3. Bare array

```json
[
  { "outboundTag": "direct", "ip": ["10.0.0.0/8"] }
]
```

The `domainStrategy` field (when present) is ignored — XKeen's `02_dns.json` controls that. The `type: "field"` is auto-filled if you omit it; everything else passes through to xray as-is.

## What each rule needs

Every rule must have:

- **`outboundTag`** — one of:
  - `direct` — bypass the VPN, send straight to the internet
  - `proxy` — send through the active VLESS server
  - `block` — drop the connection (xray's `blackhole` outbound)
- **At least one match field** — typically `domain` or `ip`, but xray supports more (`port`, `network`, `source`, `protocol`, …)

## Match field reference (most common)

| Field | What matches | Example values |
|---|---|---|
| `"domain": [...]` | Hostname patterns. Prefixes change the match mode. | `"domain:example.com"` (suffix match), `"full:example.com"` (exact), `"regexp:^api\\.[a-z]+$"`, `"geosite:google"` |
| `"ip": [...]` | IPv4/IPv6 addresses or CIDR. `geoip:` prefix resolves against `geoip.dat`. | `"10.0.0.0/8"`, `"2001:db8::/32"`, `"geoip:cn"`, `"geoip:private"` |
| `"port": "..."` | TCP/UDP destination port(s). | `"443"`, `"80,443,8443"`, `"1000-2000"` |
| `"network": "..."` | Protocol family. | `"tcp"`, `"udp"`, `"tcp,udp"` |

A rule with `domain` AND `ip` matches when *either* matches — xray treats it as an OR.

## Order matters

xray evaluates rules top-to-bottom; the first match wins. So put the most specific rules (LAN, important services that must stay direct) at the top, and the broader catch-alls below.

If no rule matches, traffic falls through to the *first* outbound in `04_outbounds.json` — which is `proxy` in our setup. That's why "everything through VPN" is the default behavior even with a tiny rule set.

## Hosting

Anywhere that returns the JSON over HTTPS as plain text:

- A GitHub gist with a `.json` file (use the **Raw** URL)
- A self-hosted file
- A static-site CDN

The fetch must complete in under 30 seconds and the body must be under 1 MB — that's plenty for thousands of rules in practice.

## Migrating from Shadowrocket / Surge / Clash

This app no longer parses any provider-specific formats. If you have rules in:

- **Shadowrocket `.conf`** — see https://github.com/Loyalsoldier/v2ray-rules-dat or convert by hand. The pattern `DOMAIN-SUFFIX,foo,DIRECT` becomes `{"outboundTag": "direct", "domain": ["domain:foo"]}`. `IP-CIDR,1.2.3.0/24,PROXY` becomes `{"outboundTag": "proxy", "ip": ["1.2.3.0/24"]}`. Coalesce adjacent rules with the same `(outboundTag, match-type)` into a single rule with multiple values for compactness.
- **Surge** — similar to Shadowrocket, similar conversion.
- **Clash YAML** — Clash's `rules:` list maps cleanly; the proxy group names just need to be remapped to xray's `direct`/`proxy`/`block` outbound tags.

A community-maintained Xray rule set (often used as a reference) is at [Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat).
