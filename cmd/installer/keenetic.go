package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ansiRE strips ANSI control sequences and carriage returns from Keenetic
// CLI output, which uses them heavily for progress and re-drawing.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\x1b\[K|\x1b\][^\x07]*\x07|\r`)

// promptRE matches Keenetic's structured-CLI prompt at end-of-buffer.
// Examples: `(config)>`, `(dns-proxy)>`, `(KN-1012)>`.
var promptRE = regexp.MustCompile(`\([\w\-]*\)>\s*$`)

// KeeneticCLI drives the Keenetic structured CLI (`(config)>` prompt) over
// an interactive SSH shell. Used during install for the things only the
// Keenetic CLI knows how to do: setting `opkg disk`, configuring the
// dns-proxy, moving HTTPS admin off port 443, rebooting.
type KeeneticCLI struct {
	host    string
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
}

func newKeeneticCLI(host, user, password string) (*KeeneticCLI, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}
	if err := session.RequestPty("xterm", 2000, 200, ssh.TerminalModes{}); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutR, stdoutW := io.Pipe()
	go func() {
		// Wire the session's stdout to our pipe. Keenetic merges stderr
		// into the PTY anyway, so we don't need to read it separately.
		session.Stdout = stdoutW
		session.Stderr = stdoutW
		_ = session.Shell()
		_ = session.Wait()
		stdoutW.Close()
	}()
	// session.Stdout/Stderr must be set before Shell(); the goroutine above
	// races with the test below. Synchronize by using a small helper.
	c := &KeeneticCLI{
		host:    host,
		client:  client,
		session: session,
		stdin:   stdin,
		stdout:  stdoutR,
	}
	if _, err := c.waitForPrompt(30 * time.Second); err != nil {
		c.Close()
		return nil, fmt.Errorf("wait for prompt: %w", err)
	}
	return c, nil
}

// waitForPrompt reads from the shell until promptRE matches, then returns
// the accumulated ANSI-cleaned output. Used after every command.
func (c *KeeneticCLI) waitForPrompt(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var buf strings.Builder
	tmp := make([]byte, 65536)
	for {
		if time.Now().After(deadline) {
			return buf.String(), fmt.Errorf("timed out (last output: %q)", trimSnippet(buf.String(), 200))
		}
		// SSH stdout doesn't expose a deadline directly; rely on the
		// outer loop's clock check and a short read window per iter.
		n, err := readWithTimeout(c.stdout, tmp, 500*time.Millisecond)
		if n > 0 {
			buf.Write(tmp[:n])
			clean := ansiRE.ReplaceAllString(buf.String(), "")
			if promptRE.MatchString(clean) {
				return clean, nil
			}
		}
		if err == io.EOF {
			return buf.String(), io.EOF
		}
	}
}

// cmd sends a command (newline appended) and waits for the next prompt.
// Returns ANSI-cleaned output.
func (c *KeeneticCLI) cmd(command string, timeout time.Duration) (string, error) {
	if _, err := c.stdin.Write([]byte(command + "\n")); err != nil {
		return "", fmt.Errorf("send cmd: %w", err)
	}
	return c.waitForPrompt(timeout)
}

// detectArch parses `show version` for the `arch:` field.
func (c *KeeneticCLI) detectArch() (string, error) {
	out, err := c.cmd("show version", 30*time.Second)
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`\barch:\s*(\S+)`).FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("could not parse arch from `show version`:\n%s", out)
	}
	arch := m[1]
	if strings.HasPrefix(arch, "armv7") {
		return "armv7", nil
	}
	return arch, nil
}

// installedComponents parses the `components: a, b, c, ...` block from
// `show version` into a set. The components list spans multiple indented
// continuation lines.
func (c *KeeneticCLI) installedComponents() (map[string]struct{}, error) {
	out, err := c.cmd("show version", 30*time.Second)
	if err != nil {
		return nil, err
	}
	var parts []string
	inSection := false
	for _, line := range strings.Split(out, "\n") {
		stripped := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(stripped, "components:") {
			inSection = true
			parts = append(parts, strings.TrimPrefix(stripped, "components:"))
			continue
		}
		if inSection {
			// Continuation lines start with at least 16 leading spaces.
			if strings.HasPrefix(line, strings.Repeat(" ", 16)) {
				parts = append(parts, stripped)
			} else {
				break
			}
		}
	}
	joined := strings.Join(parts, " ")
	comps := make(map[string]struct{})
	for _, c := range strings.Split(joined, ",") {
		if s := strings.TrimSpace(c); s != "" {
			comps[s] = struct{}{}
		}
	}
	return comps, nil
}

// pingHost uses Keenetic CLI's bundled `tools ping`.
func (c *KeeneticCLI) pingHost(target string, count int) bool {
	out, err := c.cmd(fmt.Sprintf("tools ping %s count %d", target, count), 30*time.Second)
	if err != nil {
		return false
	}
	return strings.Contains(out, "received") && strings.Contains(out, "0% packet loss")
}

// usbDrive captures the per-drive fields parsed from Keenetic's `ls` output.
type usbDrive struct {
	Name, FSType, UUID, Storage, Mounted, Free, Total string
}

// listUSBDrives parses top-level `ls` for mounted USB ext partitions.
// Captures the fields used by bootstrap (uuid) and the pre-flight space
// check (free/total).
func (c *KeeneticCLI) listUSBDrives() ([]usbDrive, error) {
	out, err := c.cmd("ls", 30*time.Second)
	if err != nil {
		return nil, err
	}
	keys := []string{"name", "fstype", "uuid", "storage", "mounted", "free", "total"}
	blockSplit := regexp.MustCompile(`\n\s*entry,\s*type\s*=`)
	blocks := blockSplit.Split(out, -1)
	var drives []usbDrive
	for _, block := range blocks {
		d := map[string]string{}
		for _, key := range keys {
			// Require a non-whitespace value on the same line — otherwise
			// the regex would fall through past an empty `label:` line and
			// pick up a later field's value.
			re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s+(\S(?:.*\S)?)\s*$`)
			m := re.FindStringSubmatch(block)
			if m == nil {
				continue
			}
			val := strings.TrimSpace(m[1])
			if key == "name" || key == "uuid" {
				val = strings.TrimRight(val, ":")
			}
			d[key] = val
		}
		if d["storage"] == "usb" &&
			strings.HasPrefix(d["fstype"], "ext") &&
			d["mounted"] == "yes" {
			drives = append(drives, usbDrive{
				Name: d["name"], FSType: d["fstype"], UUID: d["uuid"],
				Storage: d["storage"], Mounted: d["mounted"],
				Free: d["free"], Total: d["total"],
			})
		}
	}
	return drives, nil
}

// bootstrapEntware triggers Entware download and reboots the router.
// The caller is responsible for waiting for SSH back up afterwards.
func (c *KeeneticCLI) bootstrapEntware(driveID, arch string) error {
	url, ok := entwareInstallerURLs[arch]
	if !ok {
		return fmt.Errorf("no Entware installer URL known for arch=%q", arch)
	}
	info(fmt.Sprintf("triggering Entware download from %s", url))
	out, err := c.cmd(fmt.Sprintf("opkg disk %s:/ %s", driveID, url), 180*time.Second)
	if err != nil {
		return err
	}
	if strings.Contains(out, "Disk is unchanged") {
		// Factory reset can preserve the opkg-disk UUID setting without
		// actually having Entware on the drive. Clear and retry so the
		// firmware re-downloads the tarball.
		info("opkg disk already set (stale config) — clearing and retrying")
		if _, err := c.cmd("no opkg disk", 30*time.Second); err != nil {
			return err
		}
		out, err = c.cmd(fmt.Sprintf("opkg disk %s:/ %s", driveID, url), 180*time.Second)
		if err != nil {
			return err
		}
	}
	if !strings.Contains(out, "Disk is set to") {
		warn(fmt.Sprintf("unexpected `opkg disk` response:\n%s", strings.TrimSpace(out)))
	}
	info("saving running configuration")
	if _, err := c.cmd("system configuration save", 60*time.Second); err != nil {
		return err
	}
	info("rebooting router (~3 minutes for /opt to mount and Entware to come up)")
	// Best-effort send; the SSH session will likely die mid-write.
	_, _ = c.stdin.Write([]byte("system reboot\n"))
	return nil
}

func (c *KeeneticCLI) Close() {
	_, _ = c.stdin.Write([]byte("exit\n"))
	_ = c.stdin.Close()
	_ = c.session.Close()
	_ = c.client.Close()
}

// readWithTimeout does one io.Reader.Read with at most `timeout` of wait by
// spawning a goroutine and racing it against a timer. Returns (0, nil) when
// the timer fires with no data — caller is expected to loop.
func readWithTimeout(r io.Reader, buf []byte, timeout time.Duration) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(timeout):
		return 0, nil
	}
}

func trimSnippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
