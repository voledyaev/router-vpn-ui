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
	"strings"
	"time"
)

// XKeenBin is the path to the xkeen wrapper script installed by XKeen's
// own installer into Entware.
const XKeenBin = "/opt/sbin/xkeen"

// xkeen -start/-restart can take a while: it sets up iptables, may load
// kernel modules on the first run, and waits for xray to come up.
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
	code, stderr, err := run(xkeenTimeout, XKeenBin, arg)
	if err != nil {
		return false, err.Error()
	}
	if code != 0 {
		if msg := lastMeaningfulLine(stderr); msg != "" {
			return false, msg
		}
		return false, fmt.Sprintf("xkeen exit %d", code)
	}
	return true, ""
}

// IsRunning is a best-effort check: is xray actually running right now?
// Uses pidof from busybox-ash; pgrep would require the procps-ng opkg.
func IsRunning() bool {
	code, _, err := run(5*time.Second, "pidof", "xray")
	return err == nil && code == 0
}

// run executes cmd with stdio redirected and returns (exitCode, stderr, err).
//
// Why we discard stdout: xkeen forks xray as a daemon, and xray inherits
// xkeen's stdout. If we capture stdout via a pipe, exec.Cmd blocks until
// xray exits — long after xkeen itself returned. Discarding stdout (which
// xray then inherits) avoids the deadlock.
func run(timeout time.Duration, name string, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return -1, stderr.String(), fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), stderr.String(), nil
		}
		return -1, stderr.String(), err
	}
	return 0, stderr.String(), nil
}

// lastMeaningfulLine picks the last non-empty line from xkeen's stderr,
// stripping ANSI color codes that xkeen uses for progress output.
func lastMeaningfulLine(text string) string {
	text = stripANSI(text)
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// stripANSI removes CSI SGR sequences (the most common ANSI escape kind:
// ESC [ ... m). Sufficient for xkeen's colored output; we don't try to
// handle every escape kind a terminal might emit.
func stripANSI(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] >= 0x20 && s[j] < 0x40) {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				i = j
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}
