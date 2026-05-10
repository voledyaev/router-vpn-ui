package main

import (
	"fmt"
	"strings"
	"time"
)

// --- Constants ----------------------------------------------------------

const (
	remoteBaseDir       = "/opt/yonder"
	remoteBinPath       = "/opt/yonder/yonderd"
	remoteDataDir       = "/opt/yonder/data"
	remoteInitScript    = "/opt/etc/init.d/S99yonder"
	webUIPort           = 8080
	rebootWaitTimeout   = 240 * time.Second
	rebootPollInterval  = 5 * time.Second
	xkeenInstallURL     = "https://raw.githubusercontent.com/jameszeroX/XKeen/main/install.sh"
	geoIPURL            = "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat"
	geoSiteURL          = "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat"
	geoDatDir           = "/opt/etc/xray/dat"
	antiPoisoningDoH    = "https://cloudflare-dns.com/dns-query"
)

// entwareInstallerURLs holds the Entware installer tarball URLs per arch.
var entwareInstallerURLs = map[string]string{
	"aarch64": "https://bin.entware.net/aarch64-k3.10/installer/EN_aarch64-installer.tar.gz",
	"mipsel":  "https://bin.entware.net/mipselsf-k3.4/installer/EN_mipsel-installer.tar.gz",
	"armv7":   "https://bin.entware.net/armv7sf-k3.2/installer/EN_armv7-installer.tar.gz",
}

// xkeenInstallAnswers is piped to stdin during XKeen's interactive install.
// Order matches the prompts in install.sh and `xkeen -i`:
//
//	1 → install.sh: Stable XKeen (vs Beta)
//	1 → xkeen -i choice_add_proxy_cores: Xray only (vs Mihomo / both / skip)
//	1 → xkeen -i download_xray: pick the latest Xray release (item #1)
//	0 → xkeen -i choice_geosite: skip GeoSite download (we install our own)
//	0 → xkeen -i choice_geoip: skip GeoIP download (same)
//	0 → xkeen -i choice_update_cron: skip auto-update cron tasks
//	1 → xkeen -i autostart prompt: register S99xkeen for boot-time start
//	(extra zeros guard against future-added prompts)
const xkeenInstallAnswers = "1\n1\n1\n0\n0\n0\n1\n0\n0\n"

// requiredComponents is the set of Keenetic firmware components the
// installer assumes are present.
var requiredComponents = map[string]struct{}{
	"opkg":                        {}, // OPKG package manager
	"ext":                         {}, // ext2/3/4 filesystem support
	"opkg-kmod-netfilter":         {}, // iptables modules for VPN routing
	"opkg-kmod-netfilter-addons":  {},
	"dns-https":                   {}, // DoH support via `dns-proxy https upstream`
}

// minUSBFreeMB is the minimum free space on the USB drive before bootstrap.
// Entware base ~30 MB, our daemon ~10 MB, xray ~40 MB; rest is headroom.
const minUSBFreeMB = 200

// --- Pre-flight ---------------------------------------------------------

func preflightCheckComponents(cli *KeeneticCLI) {
	have, err := cli.installedComponents()
	if err != nil {
		fail(fmt.Sprintf("read components: %v", err))
	}
	var missing []string
	for c := range requiredComponents {
		if _, ok := have[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		fail("missing required Keenetic firmware components: " +
			strings.Join(missing, ", ") +
			"\n  Open the Keenetic web UI → System → Components and install " +
			"them, then re-run.")
	}
	ok(fmt.Sprintf("firmware components: all required present (%d)", len(requiredComponents)))
}

func preflightCheckInternet(cli *KeeneticCLI) {
	if cli.pingHost("bin.entware.net", 1) {
		ok("router can reach bin.entware.net")
		return
	}
	fail("router cannot reach bin.entware.net (used to download Entware).\n" +
		"  Check the router's WAN connection. If it's working in general but DNS fails, " +
		"also check that DNS is configured.")
}

func preflightCheckDiskSpace(drives []usbDrive) {
	if len(drives) == 0 {
		return
	}
	d := drives[0]
	var freeBytes int64
	if _, err := fmt.Sscanf(d.Free, "%d", &freeBytes); err != nil {
		warn(fmt.Sprintf("could not parse free space from drive entry: %+v", d))
		return
	}
	freeMB := freeBytes / (1024 * 1024)
	if freeMB < minUSBFreeMB {
		fail(fmt.Sprintf("USB drive has only %d MB free; need at least %d MB for "+
			"Entware + yonderd + Xray + headroom.", freeMB, minUSBFreeMB))
	}
	ok(fmt.Sprintf("USB drive free: %d MB", freeMB))
}

// --- Install steps ------------------------------------------------------

func xkeenFullyInstalled(shell *EntwareShell) bool {
	// XKeen installs xray at /opt/sbin/xray (not /opt/bin/xray as the
	// upstream README implies).
	rc, _, _, _ := shell.run(
		"test -x /opt/sbin/xkeen && test -x /opt/sbin/xray "+
			"&& test -f /opt/etc/init.d/S99xkeen",
		false, 10*time.Second)
	return rc == 0
}

func installXKeen(shell *EntwareShell, cli *KeeneticCLI) {
	if xkeenFullyInstalled(shell) {
		ok("XKeen + Xray already installed")
		ensureXKeenRuntimeDeps(shell)
		ensureGeoDats(shell)
		if cli != nil {
			freePort443(cli)
		}
		return
	}

	info("cleaning up any previously-failed XKeen install fragments")
	_, _, _, _ = shell.run(
		"rm -rf /opt/sbin/xkeen /opt/sbin/_xkeen /opt/sbin/.xkeen "+
			"/tmp/xkeen.tar.gz /tmp/xray.tar.gz /opt/etc/init.d/S99xkeen",
		false, 30*time.Second)

	info("installing curl + tar (needed by XKeen installer)")
	if _, _, _, err := shell.run("opkg install curl tar", true, 120*time.Second); err != nil {
		fail(err.Error())
	}

	info("downloading + running XKeen installer (~30 MB Xray binary, 3-5 min)")
	script := fmt.Sprintf("cd /tmp || exit 1\nprintf %q | sh -c \"$(curl -fsSL %s)\"\n",
		xkeenInstallAnswers, xkeenInstallURL)

	stream := func(chunk string) {
		for _, line := range strings.Split(chunk, "\n") {
			cleaned := strings.TrimRight(ansiRE.ReplaceAllString(line, ""), " \r\n\t")
			if cleaned != "" && !strings.Contains(cleaned, "YONDER_EXIT") {
				fmt.Printf("      | %s\n", cleaned)
			}
		}
	}

	rc, _, _, _ := shell.runScript(script, false, 600*time.Second, stream)
	if rc != 0 {
		warn(fmt.Sprintf("XKeen installer exited with rc=%d", rc))
	}

	if !xkeenFullyInstalled(shell) {
		fail("XKeen install verification failed: missing one of " +
			"/opt/sbin/xkeen, /opt/sbin/xray, /opt/etc/init.d/S99xkeen.\n" +
			"  Inspect the installer output above. Common causes:\n" +
			"    - GitHub unreachable from the router (try again later)\n" +
			"    - prompts have changed in a new XKeen release " +
			"(file an issue with the output above)")
	}
	ok("XKeen + Xray installed")
	ensureXKeenRuntimeDeps(shell)
	ensureGeoDats(shell)
	if cli != nil {
		freePort443(cli)
	}
}

// ensureXKeenRuntimeDeps installs/creates things XKeen needs at runtime
// but doesn't bring on its own:
//   - findutils: XKeen's S99xkeen uses `find` heavily; busybox-ash on
//     Entware doesn't ship it.
//   - /opt/etc/ndm/{netfilter.d,ifstatechanged.d,fs.d}: XKeen drops hook
//     scripts here so its iptables rules survive Keenetic firewall reloads.
//   - /opt/etc/{passwd,group,shadow}: XKeen's S99xkeen runs `adduser` for
//     the unprivileged xkeen user. Entware doesn't seed those, so the very
//     first xkeen -start otherwise fails silently with
//     `adduser: /opt/etc/passwd: No such file or directory`.
func ensureXKeenRuntimeDeps(shell *EntwareShell) {
	rc, _, _, _ := shell.run("test -x /opt/bin/find", false, 5*time.Second)
	if rc != 0 {
		info("installing findutils (XKeen's S99xkeen needs `find`)")
		if _, _, _, err := shell.run("opkg install findutils", true, 120*time.Second); err != nil {
			fail(err.Error())
		}
	}
	info("ensuring /opt/etc/ndm hook directories exist")
	_, _, _, _ = shell.run(
		"mkdir -p /opt/etc/ndm/netfilter.d /opt/etc/ndm/ifstatechanged.d /opt/etc/ndm/fs.d",
		true, 10*time.Second)
	rc, _, _, _ = shell.run("test -f /opt/etc/passwd", false, 5*time.Second)
	if rc != 0 {
		info("seeding /opt/etc/{passwd,group,shadow} for adduser")
		_, _, _, _ = shell.run(
			`printf 'root:x:0:0:root:/opt/root:/opt/bin/sh\n' > /opt/etc/passwd && `+
				`printf 'root:x:0:\n' > /opt/etc/group && `+
				`touch /opt/etc/shadow && chmod 600 /opt/etc/shadow`,
			true, 10*time.Second)
	}
}

// freePort443 moves Keenetic's HTTPS admin from 443 to 8443 so xkeen's
// tproxy doesn't conflict. After this `https://router/` becomes
// `https://router:8443/` (HTTP on :80 unchanged).
func freePort443(cli *KeeneticCLI) {
	out, err := cli.cmd("show running-config", 30*time.Second)
	if err != nil {
		warn(fmt.Sprintf("show running-config: %v", err))
		return
	}
	if strings.Contains(out, "ip http ssl port 8443") {
		ok("Keenetic HTTPS admin already on port 8443")
		return
	}
	if !strings.Contains(out, "ip http ssl port 443") &&
		!strings.Contains(out, "ip http ssl enable") {
		// SSL admin not enabled at all — nothing to free.
		return
	}
	info("moving Keenetic HTTPS admin from 443 to 8443 (avoids VPN tproxy conflict)")
	if _, err := cli.cmd("ip http ssl port 8443", 15*time.Second); err != nil {
		warn(err.Error())
		return
	}
	if _, err := cli.cmd("system configuration save", 30*time.Second); err != nil {
		warn(err.Error())
		return
	}
	ok("HTTPS admin → 8443 (use https://<router>:8443/ from now on)")
}

func dohUpstreamPresent(cli *KeeneticCLI) bool {
	out, err := cli.cmd("show running-config", 30*time.Second)
	if err != nil {
		return false
	}
	needle := "https upstream " + antiPoisoningDoH
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), needle) {
			return true
		}
	}
	return false
}

// configureDNSUpstream registers Cloudflare DoH with Keenetic's DNS-proxy.
// KeeneticOS 4.x+ handles encrypted resolution itself. This sidesteps every
// problem with our earlier attempts to route DoH through xray (which
// deadlocked the router on this hardware — see project history).
func configureDNSUpstream(cli *KeeneticCLI) {
	if dohUpstreamPresent(cli) {
		ok("DoH upstream already registered with Keenetic")
		return
	}
	info(fmt.Sprintf("registering Cloudflare DoH (%s) — DNS becomes encrypted and ISP-poisoning-proof",
		antiPoisoningDoH))
	_, _ = cli.cmd("dns-proxy", 10*time.Second)
	_, _ = cli.cmd("https upstream "+antiPoisoningDoH, 10*time.Second)
	_, _ = cli.cmd("exit", 10*time.Second)
	_, _ = cli.cmd("system configuration save", 30*time.Second)
	ok("DoH upstream configured")
}

func removeDNSUpstream(cli *KeeneticCLI) {
	if !dohUpstreamPresent(cli) {
		return
	}
	info("removing DoH upstream from Keenetic")
	_, _ = cli.cmd("dns-proxy", 10*time.Second)
	_, _ = cli.cmd("no https upstream "+antiPoisoningDoH, 10*time.Second)
	_, _ = cli.cmd("exit", 10*time.Second)
	_, _ = cli.cmd("system configuration save", 30*time.Second)
}

// ensureGeoDats downloads v2fly's geoip.dat + geosite.dat into Xray's data
// dir. User-supplied rule sets routinely reference `geoip:cn` / `geosite:google`
// and Xray refuses to start if the .dat file is missing. Our bundled
// default rule set doesn't need them but we install proactively.
func ensureGeoDats(shell *EntwareShell) {
	rc, _, _, _ := shell.run(
		fmt.Sprintf("test -s %s/geoip.dat && test -s %s/geosite.dat", geoDatDir, geoDatDir),
		false, 5*time.Second)
	if rc == 0 {
		ok("geoip.dat + geosite.dat already present")
		return
	}
	info("downloading geoip.dat + geosite.dat (~10 MB) from v2fly releases")
	if _, _, _, err := shell.run("mkdir -p "+geoDatDir, true, 10*time.Second); err != nil {
		fail(err.Error())
	}
	for _, dl := range []struct{ url, dest string }{
		{geoIPURL, geoDatDir + "/geoip.dat"},
		{geoSiteURL, geoDatDir + "/geosite.dat"},
	} {
		cmd := fmt.Sprintf("curl -fL --connect-timeout 15 -m 120 -o %s %s", dl.dest, dl.url)
		if _, _, _, err := shell.run(cmd, true, 180*time.Second); err != nil {
			fail(err.Error())
		}
	}
	ok("geofiles installed")
}

func deployBinary(shell *EntwareShell) {
	if len(yonderdBinary) == 0 {
		fail("yonderd binary not embedded in this installer — was it built with scripts/build.sh?")
	}
	// Stop a previously-running daemon first: a running binary on Linux is
	// "text file busy" and base64 -d cannot overwrite it. Idempotent — fine
	// on first install when the init script doesn't exist yet.
	info("stopping any running yonder daemon before replacing the binary")
	_, _, _, _ = shell.run(
		fmt.Sprintf("test -x %s && %s stop || true", remoteInitScript, remoteInitScript),
		false, 15*time.Second)
	info(fmt.Sprintf("uploading yonderd → %s (%.1f MB)", remoteBinPath, float64(len(yonderdBinary))/1024/1024))
	if _, _, _, err := shell.run("mkdir -p "+remoteBaseDir+" "+remoteDataDir,
		true, 10*time.Second); err != nil {
		fail(err.Error())
	}
	if err := shell.uploadBytes(yonderdBinary, remoteBinPath, 0o755); err != nil {
		fail(fmt.Sprintf("upload yonderd: %v", err))
	}
	ok("yonderd uploaded")
}

func installInitScript(shell *EntwareShell) {
	info("installing init script → " + remoteInitScript)
	if _, _, _, err := shell.run("mkdir -p /opt/etc/init.d", true, 10*time.Second); err != nil {
		fail(err.Error())
	}
	if err := shell.uploadBytes(initScript, remoteInitScript, 0o755); err != nil {
		fail(fmt.Sprintf("upload init script: %v", err))
	}
	ok("init script installed")
}

func openFirewallPort(shell *EntwareShell) {
	info(fmt.Sprintf("opening LAN-side TCP port %d", webUIPort))
	rule := fmt.Sprintf("-p tcp --dport %d -m comment --comment 'yonder' -j ACCEPT", webUIPort)
	// Idempotent: try to delete first (ignored if absent), then insert.
	cmd := fmt.Sprintf("iptables -D INPUT %s 2>/dev/null; iptables -I INPUT %s", rule, rule)
	_, _, _, _ = shell.run(cmd, false, 10*time.Second)
	ok("firewall rule added")
}

func startDaemon(shell *EntwareShell) {
	info("starting yonder daemon")
	_, _, _, _ = shell.run(remoteInitScript+" restart", false, 20*time.Second)
	time.Sleep(1500 * time.Millisecond)
	cmd := fmt.Sprintf(
		"netstat -lnt 2>/dev/null | grep ':%d ' || ss -lnt 2>/dev/null | grep ':%d ' || true",
		webUIPort, webUIPort)
	_, out, _, _ := shell.run(cmd, false, 10*time.Second)
	if strings.TrimSpace(out) == "" {
		warn(fmt.Sprintf("daemon does not appear to be listening on port %d", webUIPort))
		warn(fmt.Sprintf("check the log: ssh %s@%s 'exec sh -c \"cat %s/yonderd.log\"'",
			shell.user, shell.host, remoteDataDir))
		return
	}
	ok(fmt.Sprintf("daemon listening: %s", strings.TrimSpace(out)))
}

// --- Uninstall steps ----------------------------------------------------

func stopDaemon(shell *EntwareShell) {
	info("stopping daemon")
	_, _, _, _ = shell.run(
		fmt.Sprintf("test -x %s && %s stop || true", remoteInitScript, remoteInitScript),
		false, 15*time.Second)
}

func removeInitScript(shell *EntwareShell) {
	info("removing init script")
	_, _, _, _ = shell.run("rm -f "+remoteInitScript, false, 10*time.Second)
}

func removeApp(shell *EntwareShell) {
	info("removing " + remoteBaseDir)
	_, _, _, _ = shell.run("rm -rf "+remoteBaseDir, false, 10*time.Second)
}

func closeFirewallPort(shell *EntwareShell) {
	info("removing firewall rule")
	rule := fmt.Sprintf("-p tcp --dport %d -m comment --comment 'yonder' -j ACCEPT", webUIPort)
	_, _, _, _ = shell.run("iptables -D INPUT "+rule+" 2>/dev/null; true", false, 10*time.Second)
}
