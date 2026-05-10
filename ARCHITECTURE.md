# Architecture

## Overview

```
┌─ Mac (one-time, for install/uninstall) ────────────────┐
│  yonder — single static Go binary. Orchestrates SSH    │
│  into Keenetic CLI (Entware bootstrap) AND Entware     │
│  shell (deploy). Embeds the yonderd router daemon.     │
└────────────────┬───────────────────────────────────────┘
                 │ SSH :22 (Keenetic CLI + `exec sh`)
                 ▼
┌─ User devices on LAN ──────────────────────────────────┐
│  Phones, laptops, TV — all DHCP clients of the router  │
└────────────────┬───────────────────────────────────────┘
                 │ HTTP :8080  (admin UI, any device)
                 │ all traffic (transparently routed by XKeen)
                 ▼
┌─ Keenetic Giga KN-1012 ────────────────────────────────┐
│                                                        │
│  /opt/  (Entware on USB ext4 drive)                    │
│    ├─ etc/init.d/                                      │
│    │   ├─ S99xkeen           ← XKeen's autostart       │
│    │   └─ S99yonder          ← our autostart           │
│    ├─ etc/xray/configs/      ← split JSON merged by    │
│    │                            xray; we own 04 + 05   │
│    ├─ sbin/                                            │
│    │   ├─ xkeen              ← XKeen wrapper script    │
│    │   └─ xray               ← Xray-core binary        │
│    └─ yonder/                                          │
│        ├─ yonderd            ← daemon (static Go ELF)  │
│        └─ data/                                        │
│            ├─ state.json     ← saved settings          │
│            └─ yonderd.log    ← stdout/stderr           │
│                                                        │
│  XKeen → intercepts LAN traffic via iptables/tproxy →  │
│          forwards through xray → out via active VLESS  │
└────────────────────────────────────────────────────────┘
                 │
                 ▼
   VLESS provider (e.g. provider.example) — multiple
   country endpoints; user picks one at a time
```

## Components

### Daemon — `cmd/router-app` (`yonderd`)

- **Static Go binary** built with `CGO_ENABLED=0`, ~6 MB stripped. No runtime deps, no libc requirement — drops onto Entware aarch64 with zero extra packages.
- `net/http` stdlib server. Method-aware routing (Go 1.22+ `mux.HandleFunc("POST /api/...")`).
- Static UI (`cmd/router-app/static/index.html` + `app.js` + `style.css`) embedded via `//go:embed`. Served from root path with `Cache-Control: no-cache`.
- Configured entirely via env vars set by the init script:
  - `RVU_BASE_DIR` — where state.json lives (default `/opt/yonder/data`)
  - `RVU_LISTEN` — listen addr (default `:8080`)
  - `RVU_XRAY_CONFIGS_DIR` — overridable for local dev (default `/opt/etc/xray/configs`)
- Two background goroutines started at boot:
  - **applyLoop** — serializes xkeen restarts; see [Async apply](#async-apply).
  - **watchdog** — keeps xray alive while VPN is on; see [Watchdog](#watchdog--internalwatchdog).

### Domain packages — `internal/`

| Package | What it does |
|---|---|
| `internal/state` | Thread-safe JSON state with atomic writes. `Update(fn func(*Data))` for atomic read-modify-write. Schema is a typed Go struct. |
| `internal/vless` | Subscription parser. Handles base64-wrapped and plaintext bodies. Country detection from flag emoji + multilingual name aliases. |
| `internal/xray` | Generates `04_outbounds.json` + `05_routing.json` to slot into XKeen's configs dir. Atomic file writes. |
| `internal/services` | Wraps `xkeen -start/-stop/-restart`. Detects xkeen availability via `/opt/sbin/xkeen` presence — on dev machines becomes a safe no-op. |
| `internal/watchdog` | Goroutine that restarts xray if it dies while VPN is on. Exponential back-off on repeated failures. |

### State — `data/state.json`

Single source of truth for runtime config. Atomic write via `os.Rename()` from a `.tmp` sibling. Schema lives in `internal/state.Data`:

```jsonc
{
  "version": 1,
  "subscription_url": "https://provider.example/connection/subs/UUID",
  "subscription_fetched_at": "2026-05-15T12:00:00Z",
  "servers": [
    {"id": "pl.example:8443", "country": "PL", "name": "🇵🇱 Польша",
     "host": "pl.example", "port": 8443, "uuid": "...",
     "params": {"security": "reality", "type": "tcp",
                "flow": "xtls-rprx-vision", "sni": "example.com",
                "fp": "chrome", "pbk": "...", "sid": "..."}}
  ],
  "active_server_id": "pl.example:8443",
  "vpn_on": true,
  "rules_url": "https://gist.../xray-routing.json",
  "rules_fetched_at": "2026-05-15T12:05:00Z",
  "rules": [...],         // []json.RawMessage — preserved bit-for-bit
  "rules_warnings": [],
  "rules_skipped_count": 0,
  "last_error": ""
}
```

`rules` is stored as `[]json.RawMessage` so user-supplied JSON survives save → reload without re-shaping. `rules_warnings` and `last_error` surface to the UI for transient failures.

### XKeen integration — `internal/xray`

- Reads active server + rules from `state.Snapshot()`
- Writes `04_outbounds.json` (proxy / direct / block) and `05_routing.json` (rules + `domainStrategy: AsIs`) — both atomic via `.tmp` + rename
- Calls `services.Restart()` (or `Stop()` if `vpn_on=false`)
- The other four XKeen config files (`01_log`, `02_dns`, `03_inbounds`, `06_policy`) are left at XKeen's tested defaults — we never touch tproxy / iptables setup ourselves.

### Rules pipeline — `cmd/router-app/rules.go`

- Input: JSON in Xray's native routing format
  (`{"routing": {"rules": [...]}}`, `{"rules": [...]}`, or a bare array)
- Validation: each rule must have a recognised `outboundTag` (direct / proxy
  / block) and at least one match field (domain / ip / port / network / …).
  Each validated rule re-marshalled to `json.RawMessage` with `type: "field"`
  auto-filled if missing.
- Output: rules go into `state.Rules`; `xray.WriteXKeenSplit` splices them
  into `05_routing.json` on every reapply.
- If no URL set: bundled default in `internal/xray.defaultRules()` — only
  RFC 1918 / link-local / multicast direct, everything else falls through
  to `proxy`.
- See [docs/rules-format.md](./docs/rules-format.md) for accepted shapes
  and a Shadowrocket / Clash / Surge migration cheat sheet.

### Frontend — `cmd/router-app/static/`

- **Vanilla HTML + Alpine.js** (single CDN script tag, no build).
- Reactive state via `x-data`. Polls `/api/state` every 10s and after each action.
- Two screens via `x-if`:
  - **Onboarding** (no subscription set): paste subscription URL; optional rules URL; "Connect".
  - **Main**: status badge, country tiles, on/off toggle, "Refresh subscription/rules" buttons.
- No router/SPA framework — direct fetch + state.

### Watchdog — `internal/watchdog`

Goroutine launched from `main.go` on startup:

```
every 30s:
  if !state.VPNOn:            continue
  if services.IsRunning():    continue
  ok, msg := services.Restart()
  on repeated failure: exponential back-off up to 5 min
```

While `vpn_on=true` but xray is dead, XKeen's iptables rules stay in place:
client traffic gets REDIRECT'd to port 1181 (xray's tproxy) and finds nothing
listening → connection refused. So during recovery the LAN **fails closed**
(no leak to a direct route). That's the kill-switch property of the design.

### Installer — `cmd/installer` (`yonder`)

- Single static Go binary for macOS arm64. Embeds the cross-compiled `yonderd-linux-arm64` daemon AND the `S99yonder` init script via `//go:embed`.
- `golang.org/x/crypto/ssh` for SSH — no system `ssh` shelling required.
- Two SSH drivers:
  - **`KeeneticCLI`** — interactive PTY shell session. Used for things only the Keenetic CLI knows: `show version`, `opkg disk`, `dns-proxy`, `system reboot`. Matches a prompt regex (`(name)>`) to know when each command finishes.
  - **`EntwareShell`** — per-command exec sessions. Wraps each command as `exec sh -c '<cmd>; echo MARKER=$?'` to escape the Keenetic CLI layer and capture real exit codes (Keenetic's `exec` always returns rc=0 to SSH). Chunked base64 upload (no SFTP for `admin`).
- Top-level flows: `doInstall`, `doUninstall`, `doProbe` in `flows.go`.

### Init script — embedded `S99yonder`

- Standard Entware init.d entry: `start | stop | restart | status`.
- Launches `/opt/yonder/yonderd` as daemon, PID in `/var/run/yonder.pid`, stdout/stderr to `/opt/yonder/data/yonderd.log`.
- Sets the env vars the daemon reads (`RVU_BASE_DIR`, `RVU_LISTEN`, `RVU_XRAY_CONFIGS_DIR`).

## Data flow examples

All mutation endpoints follow the same shape: update state synchronously, ack the browser immediately, then drive xkeen asynchronously through a single worker goroutine. See [Async apply](#async-apply) below for why.

### Switching country

```
user clicks 🇩🇪 in UI
  → POST /api/server  {"id": "de.example:8443"}
  → Handler.postServer
     → state.Update(d.ActiveServerID = "de.example:8443")  // atomic, persisted
     → writeJSON(state.Snapshot())                          // ack browser
     → requestApply()                                       // non-blocking signal
  → UI re-renders status badge instantly

(meanwhile, in the applyLoop goroutine:)
  → regenerateAndRestart()
     → xray.WriteXKeenSplit(active, rules, cfgDir)
     → services.Restart()       (only if vpn_on=true)
  → state.Update(d.LastError = "" or failure msg)
```

### Refreshing rules

```
user clicks "Refresh rules"
  → POST /api/rules/refresh
  → Handler.postRulesRefresh
     → fetchAndValidateRules(rules_url)
        → httpClient.Do(GET) with size cap (1 MiB) → bytes
        → parseXrayRules(text)  → []json.RawMessage | error
     → state.Update(d.Rules = rules, d.RulesFetchedAt = now)
     → writeJSON(state.Snapshot())     // ack browser immediately
     → requestApply()                  // worker re-reads state and applies
```

### Async apply

`xkeen -restart` takes up to 90 seconds and re-installs LAN-side iptables tproxy rules during that window. If the HTTP response from `/api/toggle` is held open across the restart, the in-flight TCP connection gets torn down as a side effect — the browser hangs on `await fetch()` forever and the UI stays disabled.

Fix: respond to the user **before** kicking off xkeen. A single goroutine (`applyLoop`) reads from a buffered-1 channel; handlers `requestApply()` to nudge it. Concurrent requests coalesce harmlessly because the worker re-reads `state.Snapshot()` at each iteration — final intent always wins, regardless of how many in-flight signals were dropped by the non-blocking send.

Failures surface via `state.LastError`, picked up by the UI's 10s polling refresh.

### DNS-poisoning bypass (one-time, at install)

```
cmd/installer/steps.go: configureDNSUpstream
  → cli.cmd("dns-proxy")
  → cli.cmd("https upstream https://cloudflare-dns.com/dns-query")
  → cli.cmd("exit")
  → cli.cmd("system configuration save")

at runtime, every LAN client lookup:
  client → router :53 (Keenetic ndnproxy)
                       → encrypted DoH HTTPS to Cloudflare
                       → unpoisoned answer
                       → returned to client
```

## Design decisions

| Decision | Why |
|---|---|
| Go stdlib only on the router (no chi/gin) | Single binary, tiny attack surface; one HTTP server's worth of code is well within net/http's comfort zone. |
| Static linking (`CGO_ENABLED=0`) | Drops onto Entware without caring about libc flavor; one binary, one file. |
| Typed Go struct for state, no unknown-field passthrough | Cleaner than Python's `dict[str, Any]`; loss of forward-compat field preservation is irrelevant for a single-user hobby project. |
| `json.RawMessage` for user rules | Lets us validate without re-shaping; user-supplied JSON survives save/load bit-for-bit. |
| Async apply via single-worker goroutine | Decouples HTTP response from `xkeen -restart` (up to 90s with mid-flight iptables changes that tear down LAN TCP). Coalesces rapid toggles via buffered-1 channel; final intent always wins because the worker re-reads state on each iteration. |
| JSON file state, no DB | Trivial schema; atomic-write good enough; greppable. |
| Alpine.js, no build step | No Node.js on router; CDN script is enough for this UI. |
| Single static installer binary with embedded daemon | User downloads one file from Releases — no Python, no uv, no system deps, no `git clone`. |
| No auth | LAN-only trust model. Auth would add complexity and break the "just open a URL" UX. Documented in README. |
| Generic VLESS subscription parser | Works for any provider. Tested format is base64(`vless://...\n` ×N). |
| Xray-native JSON for custom rules | No proprietary DSL — users can copy rules from any Xray-based tool, reference upstream Xray docs, and the same JSON drops straight into XKeen's `05_routing.json` if they ever want to bypass our app. |
| `exec sh -c` wrap with exit-marker | Keenetic's CLI `exec` builtin always returns rc=0 to SSH. Appending `; echo MARKER=$?` and parsing it back is the only reliable way to get real exit codes for the chained installer steps. |
| Chunked base64 upload (no SFTP) | Keenetic denies SFTP for the `admin` user. tar.gz → base64 → `echo X >> file` chunks under the CLI's argv cap is the only path that works. |
| DNS-poisoning bypass via Keenetic native DoH | KeeneticOS 4.x+ ships with `dns-proxy https upstream`. Registering Cloudflare covers every domain RKN poisons (no hardcoded list) and encrypts the queries. Routing DoH through xray instead deadlocked the test router (xray DNS module is too heavy for this hardware); using a separate opkg resolver duplicates a feature the firmware already has. |

## Out of scope (for now)

- Multiple simultaneous outbounds / load balancing across countries
- Per-device routing policies (e.g. only iPhone goes through VPN)
- HTTPS for the admin UI / authentication
- Mobile app
- Other router architectures — currently only Keenetic aarch64 (KN-1012 + modern Keenetic). Adding mipsel / armv7 means matrix-building yonderd and selecting via `uname -m` in the installer; tracked but unscheduled.
- Other router platforms (OpenWrt, Asus-Merlin) — `cmd/installer` is Keenetic-CLI specific; would need a separate installer driver.
