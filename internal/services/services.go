// Package services controls the xray daemon via the xkeen wrapper.
//
// Abstracts away the underlying service manager so the rest of the app
// just calls Restart() and gets a sensible result regardless of whether
// xkeen is present (router) or not (local dev machine).
package services

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// XKeenBin is the path to the xkeen wrapper script installed by XKeen's
// own installer into Entware.
const XKeenBin = "/opt/sbin/xkeen"

// xkeen -start/-restart can take a while: it sets up iptables, may load
// kernel modules on first run, and may do a synchronous probe of the
// configured proxy server. 90s is the budget after which we assume xkeen
// has hung and kill it.
const xkeenTimeout = 90 * time.Second

// xkeenAvailable returns true when xkeen looks installed. We check for a
// regular file (not a directory) so a stray symlink to a missing target
// is treated as "not installed".
func xkeenAvailable() bool {
	matches, err := filepath.Glob(XKeenBin)
	return err == nil && len(matches) == 1
}

// Restart, Stop, Start return (ok, human-readable-message). When xkeen is
// not installed, all three return (true, "skipped …") so the daemon can
// run on a dev machine without faking out service control.
func Restart() (bool, string) { return runXKeen("-restart") }
func Stop() (bool, string)    { return runXKeen("-stop") }
func Start() (bool, string)   { return runXKeen("-start") }

func runXKeen(arg string) (bool, string) {
	if !xkeenAvailable() {
		return true, "xkeen not installed; skipped (config still written)"
	}
	code, err := run(xkeenTimeout, XKeenBin, arg)
	if err != nil {
		return false, err.Error()
	}
	if code != 0 {
		return false, fmt.Sprintf("xkeen exit %d", code)
	}
	return true, ""
}

// IsRunning is a best-effort check: is xray actually running right now?
// Uses pidof from busybox-ash; pgrep would require the procps-ng opkg.
func IsRunning() bool {
	code, err := run(5*time.Second, "pidof", "xray")
	return err == nil && code == 0
}

// run executes cmd with stdio fully discarded and returns (exitCode, err).
//
// Stdio handling is load-bearing here. `xkeen -restart` forks `xray` as a
// daemon — xray inherits xkeen's stdout/stderr file descriptors. If we
// capture either into a Go io.Writer (string buffer, etc), Go internally
// wires a pipe and a goroutine that drains the pipe until EOF. EOF only
// happens when *all* write ends close — but xray is a long-running daemon
// that keeps its inherited fds open forever. cmd.Wait() therefore blocks
// indefinitely, leaking the apply worker.
//
// Even more subtle: when the context timeout fires, CommandContext sends
// SIGKILL to xkeen (our direct child) but NOT to xray (which is already
// orphaned to init). So even after timeout, the pipe stays open, cmd.Wait
// stays blocked, and the goroutine that called run() is stuck. The next
// call to Restart never gets to run.
//
// Both stdout and stderr must therefore be discarded to actual file
// descriptors (Go's nil-Writer behavior → child fd dup'd to /dev/null),
// not to in-process buffers. That breaks the inheritance chain.
func run(timeout time.Duration, name string, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil // → /dev/null in child; no inherited pipe.
	cmd.Stderr = nil // ditto. Diagnostic detail is lost, but daemon-leak protection wins.

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return -1, fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

