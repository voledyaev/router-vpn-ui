package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// EntwareShell runs Linux commands on a router with Entware bootstrapped.
//
// SSH-ing as `admin` lands in Keenetic's CLI on port 22. We reach the Linux
// shell by wrapping each command as `exec sh -c '<cmd>'` — Keenetic's CLI
// `exec` builtin spawns the named binary (here /opt/bin/sh from the USB
// drive) and pipes stdin/stdout through.
//
// SFTP is denied for `admin` by Keenetic, so uploads go through a tar+base64
// stream chunked into `echo X >> file` to stay under the CLI's argv cap.
type EntwareShell struct {
	host     string
	user     string
	password string
	client   *ssh.Client
}

func newEntwareShell(host, user, password string) (*EntwareShell, error) {
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
	return &EntwareShell{host: host, user: user, password: password, client: client}, nil
}

// kquote quotes s for use as one argument to Keenetic CLI's `exec sh -c`.
//
// Keenetic's CLI argument parser only recognises double quotes (single
// quotes pass through literally and confuse `sh -c`). We escape only `\` and
// `"` so the inner shell's `$` and backtick semantics still work — needed so
// we can append `; echo MARKER=$?` to capture exit codes (Keenetic's `exec`
// always returns rc=0 to the SSH layer).
func kquote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// exitMarker is echoed by the inner shell after every command and the exit
// code parsed back out — Keenetic's `exec` builtin returns rc=0 to SSH
// regardless of the wrapped command's actual exit status.
const exitMarker = "__YONDER_EXIT__"

var exitMarkerRE = regexp.MustCompile(regexp.QuoteMeta(exitMarker) + `=(\d+)\s*$`)

// run executes a single shell command via `exec sh -c '...'`. Returns
// (rc, stdout, stderr).
func (e *EntwareShell) run(cmd string, check bool, timeout time.Duration) (int, string, string, error) {
	wrappedCmd := fmt.Sprintf("%s; echo %s=$?", cmd, exitMarker)
	wrapped := "exec sh -c " + kquote(wrappedCmd)

	session, err := e.client.NewSession()
	if err != nil {
		return -1, "", "", fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := runSessionWithTimeout(session, wrapped, timeout); err != nil {
		return -1, stdoutBuf.String(), stderrBuf.String(), err
	}

	outStr := ansiRE.ReplaceAllString(stdoutBuf.String(), "")
	errStr := ansiRE.ReplaceAllString(stderrBuf.String(), "")
	rc, cleanedOut := extractExitMarker(outStr)
	if check && rc != 0 {
		return rc, cleanedOut, errStr,
			fmt.Errorf("remote command failed (rc=%d): %s\n--stdout--\n%s\n--stderr--\n%s",
				rc, cmd, cleanedOut, errStr)
	}
	return rc, cleanedOut, errStr, nil
}

// extractExitMarker pulls the trailing `MARKER=N` out of out and returns
// (rc, cleaned). If the marker is missing the command is treated as failed
// — typically that means Keenetic CLI rejected the wrapped command before
// `exec` ran.
func extractExitMarker(out string) (int, string) {
	m := exitMarkerRE.FindStringSubmatchIndex(out)
	if m == nil {
		return 1, out
	}
	rc := 0
	fmt.Sscanf(out[m[2]:m[3]], "%d", &rc)
	return rc, strings.TrimRight(out[:m[0]], "\r\n\t ")
}

// runSessionWithTimeout calls session.Run(cmd) but enforces a wall-clock
// timeout by closing the session on expiry. SSH library has no native
// per-call timeout for Run.
func runSessionWithTimeout(session *ssh.Session, cmd string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = session.Close()
		return fmt.Errorf("command timed out after %s", timeout)
	}
}

// isAlive does a tiny `echo __OK__` round-trip to confirm the Entware shell
// is reachable. Used during reboot polling.
func (e *EntwareShell) isAlive() bool {
	rc, out, _, err := e.run("echo __OK__", false, 5*time.Second)
	return err == nil && rc == 0 && strings.Contains(out, "__OK__")
}

// uploadIgnore lists basenames that are skipped when packing a local
// directory for upload — historically Python dev junk that shouldn't end
// up on the router. Kept for the static/ folder upload during deploy.
var uploadIgnore = map[string]struct{}{
	"__pycache__":   {},
	".pytest_cache": {},
	".mypy_cache":   {},
	".ruff_cache":   {},
	".DS_Store":     {},
	".git":          {},
}

// chunkBytes is the size of each base64 chunk pushed into a remote staging
// file. Keenetic CLI's argv cap is somewhere between 8K and 12K bytes;
// 6000 leaves comfortable headroom for the `echo X >> file` boilerplate.
const chunkBytes = 6000

// uploadB64Chunked streams a base64 payload into remoteB64Path in
// <chunkBytes pieces. First chunk uses `>`, the rest `>>`.
func (e *EntwareShell) uploadB64Chunked(b64, remoteB64Path string) error {
	for i := 0; i < len(b64); i += chunkBytes {
		end := i + chunkBytes
		if end > len(b64) {
			end = len(b64)
		}
		chunk := b64[i:end]
		redir := ">"
		if i > 0 {
			redir = ">>"
		}
		// echo CHUNK appends a newline; base64 -d ignores whitespace.
		cmd := fmt.Sprintf("echo %s %s %s", chunk, redir, remoteB64Path)
		if _, _, _, err := e.run(cmd, true, 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// uploadDirectory tars+gzips a local directory and unpacks it on the router
// at remoteDir. The dir is wiped first to keep deploys idempotent.
func (e *EntwareShell) uploadDirectory(localDir, remoteDir string) error {
	st, err := os.Stat(localDir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("missing local dir: %s", localDir)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := filepath.Walk(localDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == localDir {
			return nil
		}
		rel, _ := filepath.Rel(localDir, path)
		// Skip ignored basenames anywhere in the path.
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if _, skip := uploadIgnore[part]; skip {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(part, ".pyc") {
				return nil
			}
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			f.Close()
			if copyErr != nil {
				return copyErr
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())
	const staging = "/tmp/__yonder_upload.b64"
	if _, _, _, err := e.run(fmt.Sprintf("rm -rf %s && mkdir -p %s && rm -f %s",
		remoteDir, remoteDir, staging), true, 30*time.Second); err != nil {
		return err
	}
	if err := e.uploadB64Chunked(b64, staging); err != nil {
		return err
	}
	_, _, _, err = e.run(fmt.Sprintf("base64 -d %s | tar xzf - -C %s && rm -f %s",
		staging, remoteDir, staging), true, 60*time.Second)
	return err
}

// uploadBytes writes the given content to remotePath with the given mode.
// Same chunked-base64 scheme as uploadDirectory; used for single files
// (the yonderd binary, the init script).
func (e *EntwareShell) uploadBytes(content []byte, remotePath string, mode os.FileMode) error {
	b64 := base64.StdEncoding.EncodeToString(content)
	parent := filepath.Dir(remotePath)
	staging := remotePath + ".b64.tmp"
	if _, _, _, err := e.run(fmt.Sprintf("mkdir -p %s && rm -f %s", parent, staging),
		true, 15*time.Second); err != nil {
		return err
	}
	if err := e.uploadB64Chunked(b64, staging); err != nil {
		return err
	}
	cmd := fmt.Sprintf("base64 -d %s > %s && rm -f %s && chmod %o %s",
		staging, remotePath, staging, mode.Perm(), remotePath)
	_, _, _, err := e.run(cmd, true, 60*time.Second)
	return err
}

// runScript pipes a multi-line shell script over stdin into `exec sh`, used
// for cases that need pipes, $(...) substitution, or stdin input to a child
// (e.g. the XKeen installer expects answers on stdin).
//
// onOutput, if non-nil, is called with each stdout chunk as it arrives —
// makes long-running installers visible to the user rather than silent.
func (e *EntwareShell) runScript(script string, check bool, timeout time.Duration,
	onOutput func(string)) (int, string, string, error) {

	full := fmt.Sprintf("trap 'echo %s=$?' EXIT\n%s", exitMarker, script)
	if !strings.HasSuffix(full, "\n") {
		full += "\n"
	}

	session, err := e.client.NewSession()
	if err != nil {
		return -1, "", "", fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	session.Stdin = strings.NewReader(full)
	var outBuf, errBuf bytes.Buffer

	if onOutput == nil {
		session.Stdout = &outBuf
	} else {
		stdoutPipe, err := session.StdoutPipe()
		if err != nil {
			return -1, "", "", err
		}
		go func() {
			tmp := make([]byte, 4096)
			for {
				n, err := stdoutPipe.Read(tmp)
				if n > 0 {
					chunk := tmp[:n]
					outBuf.Write(chunk)
					onOutput(string(chunk))
				}
				if err != nil {
					return
				}
			}
		}()
	}
	session.Stderr = &errBuf

	if err := runSessionWithTimeout(session, "exec sh", timeout); err != nil {
		return -1, outBuf.String(), errBuf.String(), err
	}

	outStr := ansiRE.ReplaceAllString(outBuf.String(), "")
	errStr := ansiRE.ReplaceAllString(errBuf.String(), "")
	rc, cleaned := extractExitMarker(outStr)
	if check && rc != 0 {
		return rc, cleaned, errStr,
			fmt.Errorf("remote script failed (rc=%d):\n--script--\n%s\n--stdout--\n%s\n--stderr--\n%s",
				rc, script, cleaned, errStr)
	}
	return rc, cleaned, errStr, nil
}

func (e *EntwareShell) Close() { _ = e.client.Close() }

// isEntwareReady is the oracle for "Entware bootstrap done": connects as
// admin and tries `exec sh -c 'echo __ENTWARE_OK__'`. If we see the marker,
// /opt/bin/sh exists and we can run Linux commands.
func isEntwareReady(host, user, password string) bool {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return false
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()
	var out bytes.Buffer
	session.Stdout = &out
	if err := runSessionWithTimeout(session, `exec sh -c "echo __ENTWARE_OK__"`, 5*time.Second); err != nil {
		return false
	}
	return strings.Contains(out.String(), "__ENTWARE_OK__")
}

// waitForSSHUp polls TCP :22 until it accepts a connection or times out.
// Used after rebooting the router.
func waitForSSHUp(host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := dialTCP(host+":22", 3*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(rebootPollInterval)
	}
	return fmt.Errorf("router did not come back on %s:22 within %s", host, timeout)
}
