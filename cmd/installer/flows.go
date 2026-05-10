package main

import (
	"fmt"
	"net"
	"time"
)

func doProbe(host, user, password string) {
	fmt.Printf("\nProbing %s@%s...\n", user, host)
	if isEntwareReady(host, user, password) {
		ok("Entware ready (exec sh works)")
		shell, err := newEntwareShell(host, user, password)
		if err != nil {
			fail(err.Error())
		}
		defer shell.Close()
		_, out, _, _ := shell.run(
			"uname -a; echo --; command -v python3 xkeen iptables; "+
				"echo --; test -d /opt/etc/init.d && echo init.d=ok",
			false, 15*time.Second)
		fmt.Printf("\n%s\n\n", out)
		return
	}

	warn("Entware NOT ready (exec sh did not return our marker)")
	info("collecting diagnostic info via Keenetic CLI...")
	cli, err := newKeeneticCLI(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	defer cli.Close()
	arch, err := cli.detectArch()
	if err != nil {
		warn(err.Error())
	} else {
		ok("architecture: " + arch)
	}
	drives, err := cli.listUSBDrives()
	if err != nil {
		warn(err.Error())
	} else if len(drives) == 0 {
		warn("no ext4 USB drive detected")
	} else {
		for _, d := range drives {
			ok(fmt.Sprintf("USB drive: uuid=%s fstype=%s", d.UUID, d.FSType))
		}
	}
}

func doInstall(host, user, password string) {
	fmt.Printf("\n[1/4] Inspecting %s@%s...\n", user, host)

	if isEntwareReady(host, user, password) {
		ok("Entware already up")
	} else {
		bootstrap(host, user, password)
	}

	shell, err := newEntwareShell(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	defer shell.Close()
	cli, err := newKeeneticCLI(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	defer cli.Close()

	fmt.Println("\n[3/4] Installing dependencies and deploying app...")
	installXKeen(shell, cli)
	deployBinary(shell)
	installInitScript(shell)
	openFirewallPort(shell)
	configureDNSUpstream(cli)

	fmt.Println("\n[4/4] Starting daemon...")
	startDaemon(shell)

	fmt.Println("\n  ✓ Done.")
	fmt.Printf("\n  Open http://%s:%d/ on any device on your LAN.\n\n", host, webUIPort)
}

// bootstrap runs the one-time Entware install path (pre-flight + opkg disk +
// reboot + wait). Called only when isEntwareReady returns false.
func bootstrap(host, user, password string) {
	cli, err := newKeeneticCLI(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	arch, err := cli.detectArch()
	if err != nil {
		cli.Close()
		fail(err.Error())
	}
	ok("router architecture: " + arch)

	preflightCheckComponents(cli)

	drives, err := cli.listUSBDrives()
	if err != nil {
		cli.Close()
		fail(err.Error())
	}
	if len(drives) == 0 {
		cli.Close()
		fail("no ext4 USB drive found. Plug one in and try again.")
	}
	if len(drives) > 1 {
		warn(fmt.Sprintf("multiple USB drives detected; using first: %s", drives[0].UUID))
	}
	driveID := drives[0].UUID
	if driveID == "" {
		driveID = drives[0].Name
	}
	if driveID == "" {
		cli.Close()
		fail(fmt.Sprintf("could not determine USB drive identifier: %+v", drives[0]))
	}
	ok("USB drive: " + driveID)

	preflightCheckDiskSpace(drives)
	preflightCheckInternet(cli)

	fmt.Println("\n[2/4] Bootstrapping Entware on USB drive...")
	warn("This will REBOOT the router. Existing connections will drop.")
	if !confirm("Proceed?", false) {
		cli.Close()
		fail("aborted by user")
	}
	if err := cli.bootstrapEntware(driveID, arch); err != nil {
		cli.Close()
		fail(err.Error())
	}
	cli.Close()

	info(fmt.Sprintf("waiting for SSH back up (up to %s)", rebootWaitTimeout))
	time.Sleep(10 * time.Second) // let it actually go down
	if err := waitForSSHUp(host, rebootWaitTimeout); err != nil {
		fail(err.Error())
	}
	info("waiting for /opt to mount and Entware to be reachable")
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if isEntwareReady(host, user, password) {
			ok("Entware ready")
			return
		}
		time.Sleep(5 * time.Second)
	}
	fail("router rebooted but Entware shell never became reachable (waited 120s). " +
		"The USB drive may not have a working Entware installation yet. " +
		"Re-running the installer will retry the bootstrap.")
}

func doUninstall(host, user, password string) {
	fmt.Printf("\nConnecting to %s@%s...\n", user, host)
	if !isEntwareReady(host, user, password) {
		fail("Entware shell not reachable — nothing to uninstall.")
	}
	shell, err := newEntwareShell(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	defer shell.Close()
	cli, err := newKeeneticCLI(host, user, password)
	if err != nil {
		fail(err.Error())
	}
	defer cli.Close()

	stopDaemon(shell)
	removeInitScript(shell)
	removeApp(shell)
	closeFirewallPort(shell)
	removeDNSUpstream(cli)
	ok("uninstalled")
	fmt.Println("\n  Note: Entware itself is left in place.")
}

// dialTCP is referenced by waitForSSHUp in entware.go.
func dialTCP(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}
