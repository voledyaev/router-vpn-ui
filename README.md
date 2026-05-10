# yonder

A self-hosted web UI on a Keenetic router that turns a VLESS subscription into a transparent VPN for every device on the LAN. Pick a country, flip a switch, all your phones / laptops / TVs go through the chosen exit point — no per-device clients.

> **Tested on:** Keenetic Giga (KN-1012), KeeneticOS 5.0.x, aarch64. Should work on any modern aarch64 Keenetic with USB and the OPKG component (see [Compatibility](#compatibility) for caveats). Older mipsel / armv7 routers are out of scope today but tracked in [ARCHITECTURE.md](./ARCHITECTURE.md).

---

## Quick start

The whole flow is **3 steps** and ~5 minutes wall-clock (most of it the router rebooting once during Entware bootstrap).

### Step 1 — Prepare the router (one-time, manual)

Things that can't be automated remotely — they're either physical or live in Keenetic's authenticated web UI:

1. **Plug in a USB drive** formatted as **ext4**, with at least **200 MB free**. The installer will refuse a non-ext4 drive.
2. **Open the Keenetic web UI** (`http://192.168.1.1`) and install these firmware components under System → Components:
   - **Open Packages support** (`opkg`)
   - **Ext file system** (`ext`)
   - **Netfilter modules** (`opkg-kmod-netfilter`, `opkg-kmod-netfilter-addons`)
   - **DNS-over-HTTPS** (`dns-https`) — required for the DNS-poisoning bypass; the installer registers Cloudflare's DoH endpoint with Keenetic's DNS-proxy
3. **Enable SSH** — two things, both needed:
   - **Start the SSH server.** It's off by default after factory reset. In the web UI search bar (top of the admin page) search for "SSH" and toggle it on, **or** open `http://192.168.1.1/webcli/parse` and run `service ssh` followed by `system configuration save`. Verify with `nc -zv 192.168.1.1 22` — should print `succeeded`.
   - **Give `admin` CLI access.** System → Users → admin → set a strong password and check the **CLI access** label (it's on by default but factory reset sometimes drops it).

That's it for manual setup. The installer's pre-flight check verifies all of this and fails with a specific message if anything is missing. (See [docs/keenetic-notes.md § Enabling SSH](./docs/keenetic-notes.md) for the gotcha that `ip ssh` doesn't do what its name suggests.)

### Step 2 — Run the installer (one command)

From a Mac (Apple Silicon — M1/M2/M3/M4) on the same LAN as the router:

```sh
curl -fsSL https://github.com/voledyaev/yonder/releases/latest/download/yonder-darwin-arm64 -o yonder
chmod +x yonder
xattr -d com.apple.quarantine yonder   # macOS marks downloaded binaries; remove the gate
./yonder admin@192.168.1.1
```

That's it — one binary, no Go / Python / Homebrew required on your machine. The installer asks for the SSH password once and reuses it. The first run takes ~5 minutes because it bootstraps Entware (downloads ~3 MB, reboots the router once, ~3 minutes downtime). Subsequent runs (e.g. after a release update) take ~10 seconds — they only redeploy the daemon binary.

What happens, in order:

| | Step | Notes |
|---|---|---|
| 1 | Pre-flight checks | Firmware components present, USB ext4 free space, router has internet. |
| 2 | Entware bootstrap | One-time. Sets `opkg disk <UUID>:/`, downloads the Entware tarball, reboots the router. |
| 3 | XKeen + Xray | Runs the upstream `jameszeroX/XKeen` installer with default answers piped via stdin. ~30 MB download. |
| 4 | geoip + geosite | Downloads v2fly's `.dat` files (xray needs them when rules reference `geoip:foo`). |
| 5 | Move HTTPS admin to :8443 | `xkeen` proxies all ports by default; admin on :443 conflicts. After install, web UI lives at **`https://<router>:8443/`**. HTTP on :80 is unchanged. |
| 6 | Deploy `yonderd` | Pushes the embedded daemon binary to `/opt/yonder/yonderd`, drops the `S99yonder` init script, opens firewall port 8080. |
| 7 | Configure DoH upstream | Adds Cloudflare's DoH endpoint (`https://cloudflare-dns.com/dns-query`) to Keenetic's built-in DNS-proxy via `dns-proxy https upstream`. Defeats ISP-level DNS poisoning of services like Meta / X / LinkedIn; encrypts all client lookups. |
| 8 | Start daemon | Waits for it to bind. |

### Step 3 — Connect

Open **`http://192.168.1.1:8080/`** in any browser on the LAN.

1. Paste your **VLESS subscription URL** (the standard base64-encoded list of `vless://` lines that most providers serve at a per-user URL).
2. Optionally paste a **routing-rules URL** — a JSON file in [Xray's native routing format](./docs/rules-format.md). The bundled default is "everything through VPN, only local networks direct" — fine to start without one.
3. Hit **Connect**. You'll see the country list pop up; pick one.
4. Flip the **VPN** switch on. Every device on the LAN now exits through the chosen server.

---

## Using the UI

| Action | What it does |
|---|---|
| **Pick a different country tile** | Updates the active outbound and reapplies — usually 1–3 seconds. |
| **VPN toggle off** | Stops `xkeen`. iptables tproxy rules are removed; LAN goes direct again. |
| **VPN toggle on** | Restarts `xkeen` with the current config. |
| **Refresh subscription** | Re-fetches the subscription URL and updates the server list. Useful when the provider rotates nodes. |
| **Replace…** | Onboarding all over again — paste a new URL. |
| **Set rules URL** | Validates by fetching the URL once and confirming it parses. Stored rules supersede the bundled default. |
| **Refresh rules** | Re-pulls the rules URL and re-applies. |
| **Reset to default** | Drops the user-supplied rules URL; falls back to the conservative bundled default. |

The UI polls the backend every 10 seconds, so changes you make from another device (a partner toggling on a phone) show up automatically.

---

## What's running on the router

```
/opt/
├── yonder/
│   ├── yonderd            our daemon — static Go binary (~6 MB, no runtime deps)
│   └── data/
│       ├── state.json     persistent state — atomic writes
│       └── yonderd.log    stdout/stderr of the daemon
├── etc/
│   ├── init.d/
│   │   ├── S99yonder      our init script: starts yonderd on :8080
│   │   └── S99xkeen       XKeen's init script: tproxy iptables + xray daemon
│   └── xray/configs/      six split JSON files merged by xray
│       ├── 01_log.json      ┐
│       ├── 02_dns.json      │
│       ├── 03_inbounds.json ├ XKeen's defaults (we don't touch these)
│       ├── 06_policy.json   ┘
│       ├── 04_outbounds.json  ← we own this — current VLESS server config
│       └── 05_routing.json    ← we own this — your custom rules JSON, or the bundled default
└── sbin/
    ├── xkeen              XKeen wrapper (start/stop/restart, iptables setup)
    └── xray               Xray-core binary
```

Two persistent processes: `xray` (the proxy) and `yonderd` (the UI). XKeen sets up iptables rules that REDIRECT TCP and TPROXY UDP from LAN clients to xray on port 1181; xray reads `04_outbounds.json` + `05_routing.json` to decide whether to forward traffic through the VLESS tunnel or send it direct.

The Go daemon has a small **watchdog** goroutine that polls `pidof xray` every 30 s. If the process died while `vpn_on=true`, it calls `xkeen -restart`. Combined with XKeen's iptables rules (which stay in place when xray dies), this means traffic *fails closed* during a crash — packets to the proxy port find nothing listening and get refused, rather than falling through to a direct route. That's the kill-switch property of the design.

For the deeper architecture, see [ARCHITECTURE.md](./ARCHITECTURE.md).

---

## Notes & gotchas

**HTTPS admin moves to :8443.** This is the most surprising thing the installer does. After install, the Keenetic web UI is at `https://<router>:8443/` — `http://<router>/` (port 80) still works unchanged. The reason is that XKeen tproxies every outbound port by default, and an admin service binding 443 conflicts with that.

**macOS Gatekeeper.** The release binary is unsigned. macOS adds the `com.apple.quarantine` xattr to anything downloaded via curl — Finder/double-click will refuse to launch it. The `xattr -d com.apple.quarantine yonder` line above removes the gate. (If you forget, you'll get "yonder cannot be opened because the developer cannot be verified" from a double-click; running from Terminal still works.)

**Devices with their own VPN client bypass us.** If your laptop is running Shadowrocket / WireGuard / similar, that client wraps the traffic before it reaches the router. The router only sees the encrypted tunnel destined for *that* VPN's exit point and routes that one connection — through *our* VPN, ironically — but nothing is decrypted at the router level, so the on-device VPN's exit IP is what shows up. Disable the on-device client for testing the router VPN on that device.

**Country code unknown? `??`** The subscription parser detects country from the leading flag emoji in each `vless://...#name` fragment. If your provider doesn't include a flag, you'll see `??` for those servers. The country code is purely cosmetic — server selection still works.

**Rules use Xray's native JSON format.** No proprietary DSL — same shape as XKeen's `05_routing.json`. The validator accepts the full `{"routing": {"rules": [...]}}` form, the `{"rules": [...]}` shorthand, or a bare `[...]` array. See [docs/rules-format.md](./docs/rules-format.md) for fields, examples, and a migration cheat sheet for users coming from Shadowrocket / Clash / Surge.

**Updates.** Re-download the binary from the latest release and re-run `./yonder admin@<router>` — the installer is idempotent. It detects what's already in place and only redeploys the changed bits (typically just the `/opt/yonder/yonderd` binary, ~10 seconds end-to-end). State (subscription, picked country, rules) is preserved.

**Local trust model.** The web UI is unauthenticated and bound to all interfaces. Anyone on the LAN can flip the VPN. This is intentional for home use — consider locking it down before exposing to untrusted networks.

**DNS bypass uses Keenetic's native DoH.** Many ISPs in censorship regions return poisoned answers (e.g. `127.0.0.1` for `instagram.com`) — and the VPN tunnel can't help if the wrong IP is given to the client *before* the connection starts. The installer fixes this by registering a Cloudflare DoH upstream with Keenetic's built-in DNS-proxy. All client DNS lookups are then forwarded encrypted to Cloudflare, ISP DNS is bypassed for everything, and there's no per-domain list to maintain. Requires KeeneticOS 4.x+ (any modern Keenetic). To switch resolvers (e.g. to Google), edit `antiPoisoningDoH` in `cmd/installer/steps.go` and rebuild.

**Devices that hardcode their own DNS still use it.** Smart TVs, Chromecasts, etc. that ship with hardcoded `8.8.8.8` or vendor-private DoH bypass our setup. To force them through Keenetic's DoH-equipped DNS-proxy too, SSH into the router and run `dns-proxy / intercept enable / system configuration save` — this DNATs every outbound DNS request to the local proxy. Off by default because some devices break when their preferred resolver is hijacked.

**What we deliberately do NOT do.** A few approaches we tried and abandoned, documented so future contributors don't repeat them:

- **xray DNS-via-VPN.** Routing DoH through the VLESS proxy creates a circular dependency (proxy needs DNS, DNS needs proxy) that hard-locks small routers — including ignoring the physical reset button. Even pinning DoH IPs to `direct` instead of `proxy` to break the cycle left the test router unresponsive ~3 min after every boot. xray's DNS module is too heavy for this hardware. We use Keenetic's built-in DoH instead and keep DNS entirely out of xray's data path.
- **Per-domain DNS overrides** (`ip name-server 1.1.1.1 instagram.com` for each poisoned domain). Works, but requires maintaining a hardcoded list as RKN expands its blocklist. Native DoH covers everything.
- **`opkg dns-override`.** The Keenetic-documented way to free port 53 for an opkg-installed resolver; in our test it left the LAN unable to talk to the router itself, requiring a factory reset.
- **Keenetic `ip policy xkeen`.** Would silence xkeen's "policy not found" warning, but the policy needs per-MAC client assignment via web UI. Whole-LAN VPN doesn't benefit; the warning is harmless.

---

## Uninstall

```sh
./yonder --uninstall admin@192.168.1.1
```

Removes our app, init script, firewall rule, and Keenetic DoH upstream. Leaves XKeen / Xray / Entware in place — Entware survives a fresh re-install and removing it isn't worth the risk of breaking other tooling that may depend on it. To fully start over: format the USB drive externally and re-run install.

---

## Build from source

If you'd rather build the installer yourself instead of downloading a release binary:

```sh
git clone https://github.com/voledyaev/yonder.git
cd yonder
./scripts/build.sh         # cross-compiles yonderd-linux-arm64 → embed → yonder for host
./yonder admin@192.168.1.1
```

Requires Go 1.25+ (see `go.mod`). The build script cross-compiles the router daemon into the installer's `//go:embed` slot before building the installer, so the resulting `./yonder` binary is fully self-contained.

---

## Compatibility

**Keenetic models.** The installer detects router architecture (`aarch64` / `mipsel` / `armv7`) from `show version` and knows the right Entware tarball for each. Release builds currently include only the aarch64 daemon binary — modern Keenetic (KN-10xx/11xx/19xx) is aarch64 and fully tested. mipsel / armv7 support is a matrix-build away (see [ARCHITECTURE.md](./ARCHITECTURE.md)); file an issue if you have an older router and want them re-enabled.

**KeeneticOS version.** Tested on 5.0.x. The `opkg disk <UUID>:/ <URL>` trick the installer uses for Entware bootstrap is documented for 5.x ([Keenetic support article 18482](https://support.keenetic.com/hero/kn-1012/en/18482.html)). Earlier 4.x may need manual Entware setup before running our installer.

**Other routers (OpenWrt, Asus-Merlin, etc.).** Out of scope for this installer. The daemon itself is portable Go — the parts that aren't are everything around it: bootstrap (Keenetic-specific via `opkg disk`), XKeen integration (Keenetic-specific iptables work), and the Keenetic CLI driver in `cmd/installer/`. Adapting to OpenWrt would be a significant rewrite of `cmd/installer/` against `opkg` directly + OpenWrt's iptables/`nftables`. PRs welcome.

---

## Project docs

- [ARCHITECTURE.md](./ARCHITECTURE.md) — components, data flow, design decisions
- [docs/keenetic-notes.md](./docs/keenetic-notes.md) — Keenetic CLI quirks discovered while building this
- [docs/vless-format.md](./docs/vless-format.md) — subscription / VLESS link parsing reference
- [docs/rules-format.md](./docs/rules-format.md) — accepted custom-rules JSON format
