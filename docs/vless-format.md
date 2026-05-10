# VLESS subscription format

## Subscription URL

A subscription URL returns a single text body which is **base64-encoded** when fetched. After base64-decoding, the body is a list of `vless://` URIs, one per line (LF-separated).

Verified format from `https://provider.example/sub/<token>` (May 2026):

```
$ curl -sS https://provider.example/sub/<token> | base64 -d | head -3
vless://aaaaaaaa-...@pl.example:8443?security=reality&type=tcp&...&pbk=...&sid=...#🇵🇱⚡Польша
vless://bbbbbbbb-...@es.example:8443?security=reality&type=tcp&...&pbk=...&sid=...#🇪🇸⚡Испания
vless://cccccccc-...@de.example:8443?security=reality&type=tcp&...&pbk=...&sid=...#🇩🇪⚡Германия
```

This matches the **standard V2RayN / V2RayNG / Shadowrocket** subscription convention. Most VLESS providers follow it, so the parser is generic.

**Fallbacks the parser should handle:**

- The body is *not* base64 (some providers return raw `vless://` lines). Detect by trying base64 decode and falling back to the raw text if the result has no `vless://` lines.
- Trailing whitespace or `\r\n` line endings.
- Padding-less base64 (need to add `=` padding before decoding).

## VLESS URI structure

```
vless://<UUID>@<HOST>:<PORT>?<query>#<fragment>
```

| Component | Example | Notes |
|---|---|---|
| `UUID` | `aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee` | Auth identifier |
| `HOST` | `pl.example` | DNS or IP |
| `PORT` | `8443` | Usually 443 / 8443 |
| `query` | `security=reality&type=tcp&...` | Transport + crypto params |
| `fragment` | `🇵🇱⚡Польша` (URL-encoded) | Human-readable label |

### Query parameters (Reality protocol — most common today)

| Param | Example | Required | Notes |
|---|---|---|---|
| `security` | `reality` | yes | Other values: `tls`, `none` |
| `type` | `tcp` | yes | Other: `ws`, `grpc`, `xhttp` |
| `flow` | `xtls-rprx-vision` | for reality | Empty for non-reality |
| `sni` | `rbc.ru` | for reality/tls | Server Name Indication / camouflage host |
| `pbk` | `9Y-_jCI3Z1x6...` | for reality | Reality public key |
| `sid` | `d2c6d9f6e6e12bfe` | for reality | Reality short ID |
| `fp` | `chrome` | for reality | Browser fingerprint |
| `headerType` | (empty) | optional | tcp obfuscation header type |
| `host` | (empty) | for ws/grpc | HTTP Host header / SNI for h2 |
| `path` | `/` | for ws/grpc | URL path |

### Fragment (server label)

URL-encoded UTF-8 string. Convention: starts with country flag emoji, then optional separator (`⚡`, `-`, ` `), then country name (often in the provider's locale rather than English).

**Country detection:**

1. Look for a flag emoji at the start (`U+1F1E6..U+1F1FF` regional indicator pairs)
2. Map flag to ISO-3166 alpha-2 (e.g. 🇵🇱 → `PL`)
3. If no flag: try matching the rest of the fragment against a known country-name list (multilingual)
4. Fallback: `??` (still display the raw fragment as `name`)

This gives us a stable `country` code for grouping/sorting in the UI plus the original `name` for display.

## Internal representation

After parsing, each server is stored as a flat dict:

```jsonc
{
    "id": "pl.example:8443",        // host:port — stable, used as key
    "country": "PL",                // ISO-3166 alpha-2
    "name": "🇵🇱⚡Польша",           // raw fragment
    "host": "pl.example",
    "port": 8443,
    "uuid": "aaaaaaaa-...",
    "security": "reality",
    "type": "tcp",
    "flow": "xtls-rprx-vision",
    "sni": "rbc.ru",
    "pbk": "9Y-_jCI3Z1x6...",
    "sid": "d2c6d9f6e6e12bfe",
    "fp": "chrome",
    # transport-specific extras only when relevant
    "host_header": null,
    "path": null,
}
```

## Mapping to Xray outbound

Reality VLESS server → Xray `outbound`:

```json
{
  "tag": "proxy",
  "protocol": "vless",
  "settings": {
    "vnext": [{
      "address": "<host>",
      "port": <port>,
      "users": [{
        "id": "<uuid>",
        "encryption": "none",
        "flow": "<flow>"
      }]
    }]
  },
  "streamSettings": {
    "network": "<type>",
    "security": "reality",
    "realitySettings": {
      "serverName": "<sni>",
      "fingerprint": "<fp>",
      "publicKey": "<pbk>",
      "shortId": "<sid>",
      "spiderX": ""
    }
  }
}
```

For non-reality (plain TLS): `streamSettings.security = "tls"`, `tlsSettings.serverName = sni`. For ws/grpc: extra `wsSettings` / `grpcSettings` blocks.
