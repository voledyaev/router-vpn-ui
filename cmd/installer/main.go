// yonder is the Mac-side installer that brings up the yonder stack on a
// Keenetic router over SSH. End-to-end flow: pre-flight checks, Entware
// bootstrap (one-time, requires a reboot), XKeen install, deploy of the
// embedded yonderd binary, Cloudflare DoH wiring, daemon start.
//
// Usage:
//
//	yonder admin@192.168.1.1            install / update
//	yonder --uninstall admin@192.168.1.1
//	yonder --probe admin@192.168.1.1    inspect router state, no changes
//
// Architecture (KeeneticOS 5.x):
//
//   - SSH as admin (tag cli) lands in Keenetic's structured CLI on port 22.
//   - That CLI has an `exec` builtin that launches Linux processes — combined
//     with `ssh -t ... exec sh -c '<cmd>'` we run arbitrary Linux commands
//     once Entware is on disk.
//   - Entware bootstrap is fully scriptable: `opkg disk <UUID>:/ <URL>`
//     tells Keenetic to download+inflate the Entware tarball; one reboot
//     later /opt is populated and `exec sh` works.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"
)

func main() {
	uninstall := flag.Bool("uninstall", false, "uninstall instead of install")
	probe := flag.Bool("probe", false, "connect and report router state without making changes")
	passwordEnv := flag.String("password-env", "", "read password from this env var instead of prompting")
	autoYes := flag.Bool("yes", false, "auto-confirm destructive prompts (e.g. reboot)")
	flag.BoolVar(autoYes, "y", false, "shorthand for --yes")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := flag.Arg(0)
	user, host, err := parseTarget(target)
	if err != nil {
		fail(err.Error())
	}

	autoYesFlag = *autoYes

	var password string
	if *passwordEnv != "" {
		password = os.Getenv(*passwordEnv)
		if password == "" {
			fail(fmt.Sprintf("env var %s is empty", *passwordEnv))
		}
	} else {
		password = promptPassword(target)
	}

	switch {
	case *probe:
		doProbe(host, user, password)
	case *uninstall:
		doUninstall(host, user, password)
	default:
		doInstall(host, user, password)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  yonder [--uninstall|--probe] [-y] [--password-env VAR] user@host")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  yonder admin@192.168.1.1")
	fmt.Fprintln(os.Stderr, "  yonder --uninstall admin@192.168.1.1")
}

func parseTarget(target string) (user, host string, err error) {
	at := strings.Index(target, "@")
	if at == -1 {
		return "", "", fmt.Errorf("target must look like user@host (got %q)", target)
	}
	user, host = target[:at], target[at+1:]
	if user == "" || host == "" {
		return "", "", fmt.Errorf("invalid target: %q", target)
	}
	return user, host, nil
}

func promptPassword(target string) string {
	fmt.Fprintf(os.Stderr, "SSH password for %s: ", target)
	bytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fail(fmt.Sprintf("read password: %v", err))
	}
	pw := strings.TrimSpace(string(bytes))
	if pw == "" {
		fail("password is empty")
	}
	return pw
}
