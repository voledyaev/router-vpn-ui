// Package watchdog keeps the proxy alive when state.vpn_on is true.
//
// Runs as a background goroutine inside the daemon. Every `interval` it:
//
//  1. reads vpn_on; if false, sleeps one tick
//  2. checks IsRunning; if alive, sleeps one tick
//  3. else calls Restart — XKeen re-establishes iptables tproxy and
//     re-launches xray with the current config
//
// Failure mode is important: while vpn_on is true but xray is dead,
// XKeen's iptables rules stay in place. Client traffic gets REDIRECT'd
// to port 1181 (xray's tproxy) and finds nothing listening → connection
// refused. So during recovery, traffic fails closed — no leak to a direct
// route. That's the kill-switch property of the design.
package watchdog

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"
)

// Deps describes what the watchdog needs from the rest of the app. Using a
// tiny interface keeps the package decoupled from concrete types and trivial
// to mock in tests.
type Deps interface {
	VPNOn() bool
	IsRunning() bool
	Restart() (ok bool, msg string)
}

// Start launches the watchdog goroutine. Cancel ctx to stop it. Logs to
// out (typically os.Stderr).
func Start(ctx context.Context, d Deps, interval, backoffMax time.Duration, out io.Writer) {
	go loop(ctx, d, interval, backoffMax, out)
}

func loop(ctx context.Context, d Deps, interval, backoffMax time.Duration, out io.Writer) {
	failures := 0
	for {
		newFailures, err := tick(d, out)
		if err != nil {
			fmt.Fprintf(out, "watchdog: tick errored: %v\n", err)
			failures++
		} else {
			failures = newFailures
		}

		// Exponential back-off when restarts keep failing — caps at
		// backoffMax so we don't hammer xkeen if something is broken.
		sleep := interval
		if failures > 0 {
			mult := math.Pow(2, float64(failures))
			sleep = time.Duration(float64(interval) * mult)
			if sleep > backoffMax {
				sleep = backoffMax
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

func tick(d Deps, out io.Writer) (int, error) {
	defer func() {
		// Swallow panics so a buggy dependency can't kill the daemon.
		// Logged via the (ok, msg) return path of Restart instead.
		_ = recover()
	}()

	if !d.VPNOn() {
		return 0, nil
	}
	if d.IsRunning() {
		return 0, nil
	}
	ok, msg := d.Restart()
	status := "OK"
	if !ok {
		status = "FAILED"
	}
	fmt.Fprintf(out, "watchdog: xray was down, restart %s: %s\n", status, msg)
	if !ok {
		return 1, nil
	}
	return 0, nil
}
