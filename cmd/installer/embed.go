package main

import _ "embed"

// yonderdBinary is the router daemon binary, cross-compiled for the router's
// architecture and placed in cmd/installer/embed/ by scripts/build.sh (and by
// the GitHub Actions release workflow) before the installer is built. The
// installer scp's this to /opt/yonder/yonderd during the deploy phase.
//
// Currently only linux/arm64 is supported (Keenetic KN-1012 and other modern
// aarch64 Keenetic devices). To support mipsel/armv7, add additional embeds
// and pick the right one based on `uname -m` over SSH at install time.
//
//go:embed embed/yonderd-linux-arm64
var yonderdBinary []byte

// initScript is the Entware /opt/etc/init.d/S99yonder shell script that
// supervises yonderd. Embedded so the installer is self-contained.
//
//go:embed embed/S99yonder
var initScript []byte
