package spread

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	// used instead of just importing "context" for compatibility
	// with go1.6 which is used in the xenial autopkgtests
	"golang.org/x/net/context"

	"net"
	"regexp"
	"strconv"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

var sshDial = ssh.Dial

type Client struct {
	server Server
	sshc   *ssh.Client
	config *ssh.ClientConfig
	addr   string
	job    string

	warnTimeout time.Duration
	killTimeout time.Duration
}

func getSSHKeySigner(sshKey string, sshKeyPass string) (ssh.Signer, error) {
	// Create the Signer for this private key.
	// It is not supported the
	var signer ssh.Signer
	var err error

	if sshKeyPass != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(sshKey), []byte(sshKeyPass))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(sshKey))
	}

	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %v", err)
	}
	return signer, nil
}

func Dial(server Server, username, password string, sshKey string, sshKeyPass string) (*Client, error) {
	auth := ssh.Password(password)
	config := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{auth},
		Timeout:         10 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	// When the sshKey is set, it is used for the authentication
	if sshKey != "" {
		signer, err := getSSHKeySigner(sshKey, sshKeyPass)
		if err != nil {
			return nil, fmt.Errorf("Unable to parse ssh key: %v", err)
		}
		config.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
		config.HostKeyAlgorithms = []string{ssh.KeyAlgoRSA, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}
	}

	addr := server.Address()
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	sshc, err := sshDial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %v", server, err)
	}
	client := &Client{
		server: server,
		sshc:   sshc,
		config: config,
		addr:   addr,
	}
	client.SetWarnTimeout(0)
	client.SetKillTimeout(0)
	client.SetJob("")
	return client, nil
}

func (c *Client) SetJob(job string) {
	if job == "" {
		c.job = c.server.String()
	} else {
		c.job = fmt.Sprintf("%s (%s)", c.server.Label(), job)
	}
}

func (c *Client) ResetJob() {
	c.SetJob("")
}

func (c *Client) dialOnReboot(prevBootID string) error {
	// First wait until SSH isn't working anymore.
	timeout := time.After(c.killTimeout)
	relog := time.NewTicker(c.warnTimeout)
	defer relog.Stop()
	retry := time.NewTicker(200 * time.Millisecond)
	defer retry.Stop()

	waitConfig := *c.config
	waitConfig.Timeout = 5 * time.Second

	for {
		// Try to establish a TCP connection with timeout
		conn, err := net.DialTimeout("tcp", c.addr, 10*time.Second)
		if err != nil {
			time.Sleep(1 * time.Second)
			// still rebooting
		} else {
			// Set a 10-second deadline to ensure the SSH handshake doesn't block indefinitely.
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			// Try to establish an SSH connection over the TCP socket.
			clientConn, chans, reqs, err := ssh.NewClientConn(conn, c.addr, &waitConfig)
			if err != nil {
				// SSH handshake failed — likely still rebooting — close the TCP connection and retry.
				time.Sleep(500 * time.Millisecond)
				conn.Close()
			} else {
				// Successfully connected via SSH; create an SSH client.
				sshc := ssh.NewClient(clientConn, chans, reqs)

				// once successfully connected, check boot_id to
				// see if the reboot actually happened
				c.sshc.Close()
				c.sshc = sshc

				// Try to get the boot_id to detect if reboot is complete
				conn.SetDeadline(time.Now().Add(15 * time.Second))
				curBootID, err := c.getBootID()

				// Remove the deadline set for the handshake
				conn.SetDeadline(time.Time{})

				if err == nil {
					if curBootID != prevBootID {
						printf("Connected after reboot to %s", c.job)
						return nil
					}
				} else {
					// ssh still not ready to retrieve bootId
					time.Sleep(200 * time.Millisecond)
				}
			}
		}

		// Use multiple selects to ensure that the channels get
		// checked in the right order. If a single select is used
		// and all channels have data golang will pick a random
		// channel. This means that on timeout there is a 1/2 chance
		// that there is also a retry and ssh.Dial() is run again
		// which needs to timeout first before the channels are
		// checked again.
		select {
		case <-timeout:
			return fmt.Errorf("kill-timeout reached after %s reboot request", c.job)
		default:
		}
		select {
		case <-relog.C:
			printf("Reboot on %s is taking a while...", c.job)
		default:
		}
		select {
		case <-retry.C:
		}
	}

	return nil
}

func (c *Client) Close() error {
	return c.sshc.Close()
}

func (c *Client) Server() Server {
	return c.server
}

func (c *Client) SetWarnTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultWarnTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.warnTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) SetKillTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultKillTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.killTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) WriteFile(path string, data []byte) error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	errch := make(chan error, 2)
	go func() {
		_, err := stdin.Write(data)
		if err != nil {
			errch <- err
		}
		errch <- stdin.Close()
	}()

	debugf("Writing to %s on %s:\n-----\n%# v\n-----", path, c.job, string(data))

	var stderr safeBuffer
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%s/bin/bash -c "cat >'%s'"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return fmt.Errorf("cannot write to %s on %s: %v", path, c.job, err)
	}

	if err := <-errch; err != nil {
		printf("Error writing to %s on %s: %v", path, c.job, err)
	}
	return nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	debugf("Reading from %s on %s...", path, c.job)

	var stdout, stderr safeBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%scat "%s"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		logf("Cannot read from %s on %s: %v", path, c.job, err)
		return nil, fmt.Errorf("cannot read from %s on %s: %v", path, c.job, err)
	}
	output := stdout.Bytes()
	debugf("Got data from %s on %s:\n-----\n%# v\n-----", path, c.job, string(output))
	return output, nil
}

type outputMode int

const (
	traceOutput outputMode = iota
	combinedOutput
	splitOutput
	shellOutput
	liveOutput
	perfOutput
)

func (c *Client) Run(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, combinedOutput)
	return err
}

func (c *Client) Output(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, splitOutput)
}

func (c *Client) CombinedOutput(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, combinedOutput)
}

func (c *Client) Trace(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, traceOutput)
}

func (c *Client) Shell(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, shellOutput)
	return err
}

func (c *Client) Live(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, liveOutput)
}

func (c *Client) Perf(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, perfOutput)
}

type rebootError struct {
	Key string
}

func (e *rebootError) Error() string { return "reboot requested" }

const (
	errorExitStatus      = 213
	breakpointExitStatus = 214
)

type breakpointError struct {
	msg    string
	output []byte
}

func (e *breakpointError) Error() string {
	msg := e.msg
	if msg == "" {
		msg = "breakpoint"
	}
	output := bytes.TrimSpace(e.output)
	if len(output) == 0 {
		return msg
	}
	if bytes.Contains(output, []byte{'\n'}) {
		return fmt.Sprintf("%s\n-----\n%s\n-----", msg, output)
	}
	return fmt.Sprintf("%s: %s", msg, output)
}

func isBreakpoint(err error) bool {
	_, ok := err.(*breakpointError)
	return ok
}

const maxReboots = 10

func (c *Client) run(script string, dir string, env *Environment, mode outputMode) (output []byte, err error) {
	return c.runScripts(stageScripts("script", strings.TrimSpace(script)), dir, env, mode)
}

func (c *Client) runScripts(scripts []StageScript, dir string, env *Environment, mode outputMode) (output []byte, err error) {
	if env == nil {
		env = NewEnvironment()
	}
	rebootKey := ""
	for reboot := 0; ; reboot++ {
		if rebootKey == "" {
			rebootKey = strconv.Itoa(reboot)
		}
		env.Set("SPREAD_REBOOT", rebootKey)
		output, err = c.runPart(scripts, dir, env, mode, output)
		rerr, ok := err.(*rebootError)
		if !ok {
			return output, err
		}
		if reboot > maxReboots {
			return nil, fmt.Errorf("rebooted on %s more than %d times", c.job, maxReboots)
		}

		printf("Rebooting on %s as requested...", c.job)

		rebootKey = rerr.Key
		output = append(output, '\n')

		bootID, err := c.getBootID()
		if err != nil {
			return nil, err
		}
		if err := c.requestReboot(); err != nil {
			return nil, err
		}

		if err := c.dialOnReboot(bootID); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

// requestReboot asks the remote system to reboot without waiting for command
// completion, then closes the current SSH client so reconnect can start
// immediately. Waiting here is unsafe because reboot often drops sshd
// uncleanly and may block for minutes.
func (c *Client) requestReboot() error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return fmt.Errorf("cannot open reboot session on %s: %v", c.job, err)
	}

	const rebootRequestTimeout = 20 * time.Second
	startDone := make(chan error, 1)
	go func() {
		startDone <- session.Start(c.sudo() + "reboot")
	}()

	select {
	case err := <-startDone:
		if err != nil {
			session.Close()
			return fmt.Errorf("cannot start reboot on %s: %v", c.job, err)
		}
	case <-time.After(rebootRequestTimeout):
	}

	// Do not wait for the reboot command completion. During reboot, remote sshd
	// often drops the transport uncleanly and waiting can block for minutes.
	session.Close()
	c.sshc.Close()
	return nil
}

func (c *Client) getBootID() (string, error) {
	rawBootID, err := c.Output("cat /proc/sys/kernel/random/boot_id", "", nil)
	if err != nil {
		return "", fmt.Errorf("cannot obtain the remote system boot_id: %v", err)
	}

	return string(bytes.TrimSpace(rawBootID)), nil
}

var toBashRC = map[string]bool{
	"PS1":            true,
	"SPREAD_PATH":    true,
	"SPREAD_BACKEND": true,
	"SPREAD_SYSTEM":  true,
}

func (c *Client) runPart(scripts []StageScript, dir string, env *Environment, mode outputMode, previous []byte) (output []byte, err error) {
	if len(scripts) == 0 && mode != shellOutput {
		return nil, nil
	}
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var buf bytes.Buffer
	buf.WriteString("set -eu\n")
	if mode != shellOutput {
		buf.WriteString("set -E\n")
	}
	var rc = func(use bool, s string) string { return s }
	if mode == shellOutput {
		buf.WriteString("true > /root/.bashrc\n")
		rc = func(use bool, s string) string {
			if !use {
				return ""
			}
			return "cat >> /root/.bashrc <<'END'\n" + s + "END\n"
		}
	}
	if dir != "" {
		buf.WriteString(fmt.Sprintf("cd \"%s\"\n", dir))
	}
	if c.sudo() != "" {
		buf.WriteString("unset SUDO_COMMAND\n")
		buf.WriteString("unset SUDO_USER\n")
		buf.WriteString("unset SUDO_UID\n")
		buf.WriteString("unset SUDO_GID\n")
	}
	buf.WriteString(rc(false, "REBOOT() { { set +xu; trap - ERR; } 2> /dev/null; [ -z \"$1\" ] && echo '<REBOOT>' || echo \"<REBOOT $1>\"; exit 213; }\n"))
	buf.WriteString(rc(false, "ERROR() { { set +xu; trap - ERR; } 2> /dev/null; [ -z \"$1\" ] && echo '<ERROR>' || echo \"<ERROR $@>\"; exit 213; }\n"))
	buf.WriteString(rc(false, "BREAKPOINT() { local pipestatus=\"${PIPESTATUS[*]}\"; { set +x; } 2>/dev/null; spread_stack_dump 214 \"BREAKPOINT${*:+ $*}\" \"${pipestatus:-214}\"; { trap - ERR; set +xu; } 2>/dev/null; [ -z \"$1\" ] && echo '<BREAKPOINT>' || echo \"<BREAKPOINT $@>\"; exit 214; }\n"))
	// We are not using pipes here, see:
	//  https://github.com/snapcore/spread/pull/64
	// We also run it in a subshell, see
	//  https://github.com/snapcore/spread/pull/67
	buf.WriteString(rc(true, "MATCH() ( { set +xu; } 2> /dev/null; [ ${#@} -gt 0 ] || { echo \"error: missing regexp argument\"; return 1; }; local stdin=\"$(cat)\"; grep -q -E \"$@\" <<< \"$stdin\" || { res=$?; echo \"grep error: pattern not found, got:\n$stdin\">&2; if [ $res != 1 ]; then echo \"unexpected grep exit status: $res\"; fi; return 1; }; )\n"))
	buf.WriteString(rc(true, "NOMATCH() ( { set +xu; } 2> /dev/null; [ ${#@} -gt 0 ] || { echo \"error: missing regexp argument\"; return 1; }; local stdin=\"$(cat)\"; if echo \"$stdin\" | grep -q -E \"$@\"; then echo \"NOMATCH pattern='$@' found in:\n$stdin\">&2; return 1; fi; )\n"))
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")
	buf.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin\n")

	for _, k := range env.Keys() {
		v := env.Get(k)
		var kv string
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			kv = fmt.Sprintf("export %s=%s\n", k, v)
		} else {
			kv = fmt.Sprintf("export %s=\"%s\"\n", k, v)
		}
		if toBashRC[k] {
			kv = rc(true, kv)
		}
		buf.WriteString(kv)
	}

	// Don't trace environment variables so secrets don't leak.
	if mode == shellOutput {
		buf.WriteString("\n/bin/bash\n")
	} else {
		writeScriptRuntime(&buf, scripts, mode == traceOutput || mode == perfOutput)
	}

	errch := make(chan error, 2)
	if mode == shellOutput {
		session.Stdin = os.Stdin
		errch <- nil
	} else {
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		defer stdin.Close()

		go func() {
			_, err := stdin.Write(buf.Bytes())
			if err != nil {
				errch <- err
			}
			errch <- stdin.Close()
		}()
	}

	debugf("Sending script for %s:\n-----\n%s\n------", c.job, buf.Bytes())

	var stdout, stderr safeBuffer
	var cmd string
	switch mode {
	case traceOutput, combinedOutput:
		cmd = c.sudo() + "/bin/bash - 2>&1"
		session.Stdout = &stdout
	case splitOutput:
		cmd = c.sudo() + "/bin/bash -"
		session.Stdout = &stdout
		session.Stderr = &stderr
	case liveOutput:
		cmd = c.sudo() + "/bin/bash - 2>&1"
		session.Stdout = os.Stdout
	case perfOutput:
		adddate := "awk '{cmd=\"(date +'%T.%3N')\"; cmd | getline d; print d,$0; close(cmd)}'"
		cmd = c.sudo() + "/bin/bash - 2>&1 | " + adddate
		session.Stdout = &stdout
	case shellOutput:
		cmd = fmt.Sprintf("{\nf=$(mktemp)\ntrap 'rm '$f EXIT\ncat > $f <<'SCRIPT_END'\n%s\nSCRIPT_END\n%s/bin/bash $f\n}", buf.String(), c.sudo())
		session.Stdout = os.Stdout
		session.Stderr = os.Stderr
		w, h, err := term.GetSize(0)
		if err != nil {
			return nil, fmt.Errorf("cannot get local terminal size: %v", err)
		}
		if err := session.RequestPty(getenv("TERM", "vt100"), h, w, nil); err != nil {
			return nil, fmt.Errorf("cannot get remote pseudo terminal: %v", err)
		}
	default:
		panic("internal error: invalid output mode")
	}

	if mode == shellOutput {
		termLock()
		tstate, terr := term.MakeRaw(0)
		if terr != nil {
			return nil, fmt.Errorf("cannot put local terminal in raw mode: %v", terr)
		}
		err = session.Run(cmd)
		term.Restore(0, tstate)
		termUnlock()
	} else {
		err = c.runCommand(session, cmd, &stdout, &stderr)
	}

	if stdout.Len() > 0 {
		if mode == perfOutput {
			logf("Output from running script on %s:\n-----\n%s\n-----", c.job, stdout.Bytes())
		} else {
			debugf("Output from running script on %s:\n-----\n%s\n-----", c.job, stdout.Bytes())
		}
	}
	if stderr.Len() > 0 {
		if mode == perfOutput {
			logf("Error output from running script on %s:\n-----\n%s\n-----", c.job, stderr.Bytes())
		} else {
			debugf("Error output from running script on %s:\n-----\n%s\n-----", c.job, stderr.Bytes())
		}
	}

	if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() == breakpointExitStatus {
		name, arg := lastControlCommand(stdout.Bytes())
		if name == "BREAKPOINT" || name == "" {
			return append(previous, stdout.Bytes()...), &breakpointError{msg: breakpointMessage(arg), output: stdout.Bytes()}
		}
	}
	if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() == errorExitStatus {
		lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
		m := commandExp.FindSubmatch(lines[len(lines)-1])
		if len(m) > 0 && string(m[1]) == "ERROR" {
			return nil, fmt.Errorf("%s", m[2])
		}
		if len(m) > 0 && string(m[1]) == "REBOOT" {
			return append(previous, stdout.Bytes()...), &rebootError{string(m[2])}
		}
		if mode == liveOutput {
			return append(previous, stdout.Bytes()...), &rebootError{"Reboot"}
		}
	}

	if err == nil || mode != splitOutput {
		output = stdout.Bytes()
	} else if mode == splitOutput {
		output = stderr.Bytes()
	}

	// When running scripts under Go's non-shell ssh session, this fails:
	// # echo echo ok | /bin/bash -c "/bin/bash --login" 2>&1 | cat
	const errmsg = "mesg: ttyname failed: Inappropriate ioctl for device"
	output = bytes.TrimSpace(bytes.TrimPrefix(output, []byte(errmsg)))

	output = append(previous, output...)
	if err != nil {
		return nil, outputErr(output, err)
	}
	if err := <-errch; err != nil {
		printf("Error writing script for %s: %v", c.job, err)
	}
	return output, nil
}

func (c *Client) sudo() string {
	if c.config.User == "root" {
		return ""
	}
	return "sudo -i "
}

func getenv(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}

func (c *Client) RemoveAll(path string) error {
	_, err := c.CombinedOutput(fmt.Sprintf(`rm -rf "%s"`, path), "", nil)
	return err
}

func (c *Client) SetupRootAccess(password string) error {
	var script string
	if c.config.User == "root" {
		script = fmt.Sprintf(`echo root:'%s' | chpasswd`, password)
	} else {
		script = strings.Join([]string{
			`sudo sed -i 's/^\s*#\?\s*\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config`,
			`echo root:'` + password + `' | sudo chpasswd`,
			`sudo pkill -o -HUP sshd || true`,
		}, "\n")
	}
	_, err := c.CombinedOutput(script, "", nil)
	if err != nil {
		return fmt.Errorf("cannot setup root access: %s", err)
	}
	if c.config.User == "root" {
		c.config.Auth = []ssh.AuthMethod{ssh.Password(password)}
	}
	return nil
}

func (c *Client) MissingOrEmpty(dir string) (bool, error) {
	output, err := c.Output(fmt.Sprintf(`! test -e "%s" || ls -a "%s"`, dir, dir), "", nil)
	if err != nil {
		return false, fmt.Errorf("cannot check if %s on %s is empty: %v", dir, c.job, err)
	}
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		for _, s := range strings.Split(string(output), "\n") {
			if s != "." && s != ".." {
				debugf("Found %q inside %q on %s, considering non-empty.", s, dir, c.job)
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Client) Send(from, to string, include, exclude []string) error {
	empty, err := c.MissingOrEmpty(to)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s on %s is not empty", to, c.job)
	}

	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	args := []string{
		"-cz",
		"--exclude=.spread-reuse.*",
	}
	for _, pattern := range exclude {
		args = append(args, "--exclude="+pattern)
	}
	args = append(args, include...)

	var stderr bytes.Buffer

	cmd := exec.Command("tar", args...)
	cmd.Dir = from
	cmd.Stdout = stdin
	cmd.Stderr = &stderr
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("cannot start local tar command: %v", err)
	}

	errch := make(chan error, 1)
	go func() {
		errch <- cmd.Wait()
		stdin.Close()
	}()

	var stdout safeBuffer
	session.Stdout = &stdout
	rcmd := fmt.Sprintf(`%s/bin/bash -c "mkdir -p '%s' && cd '%s' && /bin/tar --no-same-owner -xz 2>&1"`, c.sudo(), to, to)
	err = c.runCommand(session, rcmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", outputErr(stderr.Bytes(), err))
	}
	return nil
}

func (c *Client) SendTar(tar io.Reader, unpackDir string) error {
	empty, err := c.MissingOrEmpty(unpackDir)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s on %s is not empty", unpackDir, c.job)
	}

	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var stdout safeBuffer
	session.Stdin = tar
	session.Stdout = &stdout
	cmd := fmt.Sprintf(`%s/bin/bash -c "mkdir -p '%s' && cd '%s' && /bin/tar --no-same-owner -xz 2>&1"`, c.sudo(), unpackDir, unpackDir)
	err = c.runCommand(session, cmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}
	return nil
}

func (c *Client) RecvTar(packDir string, include []string, tar io.Writer) error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var args []string
	if len(include) == 0 {
		args = []string{"."}
	} else {
		args = make([]string, len(include))
		for i, arg := range include {
			arg = strings.Replace(arg, "'", `'"'"'`, -1)
			arg = strings.Replace(arg, "*", `'*'`, -1)
			args[i] = "'" + arg + "'"
		}
	}

	var stderr safeBuffer
	session.Stdout = tar
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%s/bin/tar -C %q -cz --sort=name --ignore-failed-read -- %s`, c.sudo(), packDir, strings.Join(args, " "))
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		return outputErr(stderr.Bytes(), err)
	}
	return nil
}

const (
	defaultWarnTimeout = 5 * time.Minute
	defaultKillTimeout = 15 * time.Minute
	maxTimeout         = 365 * 24 * time.Hour
)

func (c *Client) runCommand(session *ssh.Session, cmd string, stdout, stderr io.Writer) error {
	start := time.Now()

	err := session.Start(cmd)
	if err != nil {
		return fmt.Errorf("cannot start remote command on %s: %v", c.job, err)
	}

	done := make(chan error)
	go func() {
		done <- session.Wait()
	}()

	var lastOut, lastErr int

	kill := time.After(c.killTimeout)
	warn := time.NewTicker(c.warnTimeout)
	defer warn.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-kill:
			session.Signal(ssh.SIGKILL)
			out := stdout
			if out == nil {
				out = stderr
			}
			if out != nil {
				out.Write([]byte("\n<kill-timeout reached>"))
			}
			return fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			if buf, ok := stdout.(*safeBuffer); ok {
				output, lastOut = buf.Since(lastOut)
			}
			if buf, ok := stderr.(*safeBuffer); ok {
				errput, lastErr = buf.Since(lastErr)
				if len(output) == 0 || bytes.HasPrefix(errput, output) {
					// Also avoids double (... same ...) message.
					output = errput
				} else if len(errput) > 0 {
					output = append(output, '\n', '\n')
					output = append(output, errput...)
				}
			}
			// Use a different time so it has a different id on Travis, but keep
			// the original start time so the message shows the task time so far.
			start = start.Add(1)
			if bytes.Equal(output, unchangedMarker) {
				printft(start, startTime|endTime, "WARNING: %s running late. Output unchanged.", c.job)
			} else if len(output) == 0 {
				printft(start, startTime|endTime, "WARNING: %s running late. Output still empty.", c.job)
			} else {
				printft(start, startTime|endTime|startFold|endFold, "WARNING: %s running late. Current output:\n-----\n%s\n-----", c.job, tail(output))
			}
		}
	}
	panic("unreachable")
}

func tail(output []byte) []byte {
	display := 10
	min := display
	max := display + 3
	mark := 0
	for i := len(output) - 1; i >= 0; i-- {
		if output[i] != '\n' {
			continue
		}

		min--
		max--

		if min == 0 {
			mark = i + 1
			continue
		}
		if max == 0 {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "(... %d lines above ...)\n%s", bytes.Count(output, []byte{'\n'})-display, output[mark:])
			return buf.Bytes()
		}
	}
	return output
}

var commandExp = regexp.MustCompile("^<([A-Z_]+)(?: (.*))?>$")

func lastControlCommand(output []byte) (name, arg string) {
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		m := commandExp.FindSubmatch(bytes.TrimSpace(lines[i]))
		if len(m) > 0 {
			return string(m[1]), string(m[2])
		}
	}
	return "", ""
}

func breakpointMessage(arg string) string {
	if arg == "" {
		return "breakpoint"
	}
	return arg
}

// localScript holds and runs a local script in a polished manner.
//
// It's not used by the SSH client, but mimics the Client.runPart+runCommand closely.
type localScript struct {
	script      string
	scripts     []StageScript
	dir         string
	env         *Environment
	warnTimeout time.Duration
	killTimeout time.Duration
	mode        outputMode
	extraFiles  []*os.File
	stop        <-chan struct{}
}

func (s *localScript) resolvedScripts() []StageScript {
	if s.scripts != nil {
		return s.scripts
	}
	return stageScripts("script", strings.TrimSpace(s.script))
}

func (s *localScript) run() (stdout, stderr []byte, err error) {
	scripts := s.resolvedScripts()
	if len(scripts) == 0 {
		return nil, nil, nil
	}

	var buf bytes.Buffer
	buf.WriteString("set -eu\n")
	buf.WriteString("set -E\n")
	buf.WriteString("ADDRESS() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ADDRESS>' || echo \"<ADDRESS $1>\"; }\n")
	buf.WriteString("FATAL() { { set +xu; trap - ERR; } 2> /dev/null; [ -z \"$1\" ] && echo '<FATAL>' || echo \"<FATAL $@>\"; exit 213; }\n")
	buf.WriteString("ERROR() { { set +xu; trap - ERR; } 2> /dev/null; [ -z \"$1\" ] && echo '<ERROR>' || echo \"<ERROR $@>\"; exit 213; }\n")
	buf.WriteString("BREAKPOINT() { local pipestatus=\"${PIPESTATUS[*]}\"; { set +x; } 2>/dev/null; spread_stack_dump 214 \"BREAKPOINT${*:+ $*}\" \"${pipestatus:-214}\"; { trap - ERR; set +xu; } 2>/dev/null; [ -z \"$1\" ] && echo '<BREAKPOINT>' || echo \"<BREAKPOINT $@>\"; exit 214; }\n")
	buf.WriteString("MATCH() { { set +xu; } 2> /dev/null; local stdin=$(cat); echo $stdin | grep -q -E \"$@\" || { echo \"error: pattern not found on stdin:\\n$stdin\">&2; return 1; }; }\n")
	buf.WriteString("NOMATCH() { { set +xu; } 2> /dev/null; local stdin=$(cat); if echo $stdin | grep -q -E \"$@\"; then echo \"NOMATCH pattern='$@' found in:\n$stdin\">&2; return 1; fi }\n")
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")
	buf.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin\n")

	if s.env != nil {
		for _, k := range s.env.Keys() {
			v := s.env.Get(k)
			if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
				fmt.Fprintf(&buf, "export %s=%s\n", k, v)
			} else {
				fmt.Fprintf(&buf, "export %s=\"%s\"\n", k, v)
			}
		}
	}

	writeScriptRuntime(&buf, scripts, s.mode == traceOutput)

	debugf("Running local script:\n-----\n%s\n------", buf.Bytes())

	var outbuf, errbuf safeBuffer
	cmd := exec.Command("/bin/bash", "-eu", "-")
	cmd.Stdin = &buf
	cmd.Dir = s.dir
	cmd.ExtraFiles = s.extraFiles
	switch s.mode {
	case traceOutput, combinedOutput, liveOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &outbuf
	case splitOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &errbuf
	case shellOutput:
		panic("internal error: runScript does not support shell mode")
	default:
		panic("internal error: invalid output mode")
	}

	err = cmd.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot start local command: %v", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	warnTimeout := s.warnTimeout
	killTimeout := s.killTimeout
	if warnTimeout == 0 {
		warnTimeout = defaultWarnTimeout
	} else if warnTimeout == -1 {
		warnTimeout = maxTimeout
	}
	if killTimeout == 0 {
		killTimeout = defaultKillTimeout
	} else if killTimeout == -1 {
		killTimeout = maxTimeout
	}

	if killTimeout%warnTimeout == 0 {
		// So message from kill won't race with warning.
		killTimeout -= 1 * time.Second
	}

	var lastOut, lastErr int

	kill := time.After(killTimeout)
	warn := time.NewTicker(warnTimeout)
	defer warn.Stop()
Loop:
	for {
		select {
		case err = <-done:
			break Loop
		case <-s.stop:
			buf.Write([]byte("\n<interrupted>"))
			err = fmt.Errorf("interrupted")
			break Loop
		case <-kill:
			cmd.Process.Kill()
			buf := &outbuf
			if errbuf.Len() > 0 {
				buf = &errbuf
			}
			buf.Write([]byte("\n<kill-timeout reached>"))
			err = fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			output, lastOut = outbuf.Since(lastOut)
			errput, lastErr = errbuf.Since(lastErr)
			if len(output) == 0 || bytes.HasPrefix(errput, output) {
				// Also avoids double (... same ...) message.
				output = errput
			} else if len(errput) > 0 {
				output = append(output, '\n', '\n')
				output = append(output, errput...)
			}
			if bytes.Equal(output, unchangedMarker) {
				printf("WARNING: local script running late. Output unchanged.")
			} else if len(output) == 0 {
				printf("WARNING: local script running late. Output still empty.")
			} else {
				printf("WARNING: local script running late. Current output:\n-----\n%s\n-----", tail(output))
			}
		}
	}

	if outbuf.Len() > 0 {
		debugf("Output from running local script:\n-----\n%s\n-----", outbuf.Bytes())
	}
	if errbuf.Len() > 0 {
		debugf("Error output from running script:\n-----\n%s\n-----", errbuf.Bytes())
	}

	if exitStatus(err) == breakpointExitStatus {
		name, arg := lastControlCommand(outbuf.Bytes())
		if name == "BREAKPOINT" || name == "" {
			return outbuf.Bytes(), nil, &breakpointError{msg: breakpointMessage(arg), output: outbuf.Bytes()}
		}
	}
	if exitStatus(err) == errorExitStatus {
		lines := bytes.Split(bytes.TrimSpace(outbuf.Bytes()), []byte{'\n'})
		m := commandExp.FindSubmatch(lines[len(lines)-1])
		if len(m) > 0 && string(m[1]) == "ERROR" {
			return nil, nil, fmt.Errorf("%s", m[2])
		}
		if len(m) > 0 && string(m[1]) == "FATAL" {
			return nil, nil, &FatalError{fmt.Errorf("%s", m[2])}
		}
	}

	if err != nil {
		if errbuf.Len() > 0 {
			err = outputErr(errbuf.Bytes(), err)
		} else if outbuf.Len() > 0 {
			err = outputErr(outbuf.Bytes(), err)
		}
		return nil, nil, err
	}
	return outbuf.Bytes(), errbuf.Bytes(), nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return 1
	}
	wait, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	return wait.ExitStatus()
}

type safeBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (sbuf *safeBuffer) Write(data []byte) (int, error) {
	sbuf.mu.Lock()
	n, err := sbuf.buf.Write(data)
	sbuf.mu.Unlock()
	return n, err
}

func (sbuf *safeBuffer) Bytes() []byte {
	sbuf.mu.Lock()
	data := sbuf.buf.Bytes()
	sbuf.mu.Unlock()
	return data
}

var unchangedMarker = []byte("(...)")

func (sbuf *safeBuffer) Since(offset int) (data []byte, len int) {
	sbuf.mu.Lock()
	defer sbuf.mu.Unlock()

	data = sbuf.buf.Bytes()
	copy := true
	for i := offset - 1; i > 1; i-- {
		if data[i] == '\n' {
			data = append(unchangedMarker, data[i:]...)
			copy = false
			break
		}
	}
	if copy {
		data = append([]byte(nil), data...)
	}
	return bytes.TrimSpace(data), sbuf.buf.Len()
}

func (sbuf *safeBuffer) Len() int {
	sbuf.mu.Lock()
	l := sbuf.buf.Len()
	sbuf.mu.Unlock()
	return l
}

func scriptFileName(name string) string {
	if name == "" {
		name = "script"
	}
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + ".sh"
}

func scriptHeredocDelim(scripts []StageScript) string {
	delim := "SPREAD_SCRIPT_EOF"
	for {
		found := false
		for _, script := range scripts {
			if strings.Contains(script.Body, delim) {
				found = true
				break
			}
		}
		if !found {
			return delim
		}
		delim += "_X"
	}
}

func writeScriptRuntime(buf *bytes.Buffer, scripts []StageScript, enableTrace bool) {
	if len(scripts) == 0 {
		return
	}
	delim := scriptHeredocDelim(scripts)
	buf.WriteString(`SPREAD_SCRIPT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/spread-scripts.XXXXXX")
trap 'rm -rf "$SPREAD_SCRIPT_DIR"' EXIT
`)
	for _, script := range scripts {
		body := script.Body
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		fmt.Fprintf(buf, "cat > \"$SPREAD_SCRIPT_DIR/%s\" <<'%s'\n%s%s\n", scriptFileName(script.Name), delim, body, delim)
		if script.OriginFile != "" && script.OriginLine > 0 {
			fmt.Fprintf(buf, "printf '%%s\\n%%d\\n' %s %d > \"$SPREAD_SCRIPT_DIR/%s.origin\"\n", bashSingleQuote(script.OriginFile), script.OriginLine, scriptFileName(script.Name))
		}
	}

	buf.WriteString(spreadStackDumpFn)
	buf.WriteString("shopt -s extdebug\n")
	// Snapshot PIPESTATUS and $? in one assignment so the trap body does not clobber them.
	buf.WriteString("trap 'pipestatus=\"${PIPESTATUS[*]}\" status=$?; { set +x; } 2>/dev/null; spread_stack_dump \"$status\" \"$BASH_COMMAND\" \"$pipestatus\"' ERR\n")
	buf.WriteString(`: > "$SPREAD_SCRIPT_DIR/output"
mkfifo "$SPREAD_SCRIPT_DIR/out.pipe"
exec 3>&1
tee -a "$SPREAD_SCRIPT_DIR/output" < "$SPREAD_SCRIPT_DIR/out.pipe" >&3 &
SPREAD_TEE_PID=$!
trap 'trap - ERR; exec 1>&3 2>&3; wait "$SPREAD_TEE_PID" 2>/dev/null; rm -rf "$SPREAD_SCRIPT_DIR"' EXIT
exec > "$SPREAD_SCRIPT_DIR/out.pipe" 2>&1
`)
	if enableTrace {
		// Parameter expansion only: command substitution in PS4 would recurse under set -x.
		// BASH_SOURCE is unset in the wrapper (set -u), so expand it only when set.
		buf.WriteString("declare -A SPREAD_YAML_FILE SPREAD_YAML_BASE\n")
		for _, script := range scripts {
			if script.OriginFile == "" || script.OriginLine <= 0 {
				continue
			}
			fname := scriptFileName(script.Name)
			fmt.Fprintf(buf, "SPREAD_YAML_FILE[\"$SPREAD_SCRIPT_DIR/%s\"]=%s\n", fname, bashSingleQuote(script.OriginFile))
			fmt.Fprintf(buf, "SPREAD_YAML_BASE[\"$SPREAD_SCRIPT_DIR/%s\"]=%d\n", fname, script.OriginLine)
		}
		buf.WriteString("PS4='+ ${BASH_SOURCE[0]+${SPREAD_YAML_FILE[${BASH_SOURCE[0]}]:-${BASH_SOURCE[0]#$SPREAD_SCRIPT_DIR/}}:$((${SPREAD_YAML_BASE[${BASH_SOURCE[0]}]:-1}+LINENO-1))}: '\n")
	}
	buf.WriteString("{\n")
	for _, script := range scripts {
		if script.Path != "" {
			fmt.Fprintf(buf, "export SPREAD_PHASE_PATH=\"%s\"\n", escapeBashDoubleQuote(script.Path))
		}
		if enableTrace {
			// Delay set -x until the first command in the sourced file. The wrapper
			// is `bash -` (no BASH_SOURCE), so tracing `source $SPREAD_SCRIPT_DIR/...`
			// would print a bogus `+ :N: source /tmp/spread-scripts...` line.
			fmt.Fprintf(buf, "( set -T; trap 'case \"${BASH_SOURCE[0]-}\" in \"$SPREAD_SCRIPT_DIR\"/*) trap - DEBUG; set +T; set -x;; esac' DEBUG; source \"$SPREAD_SCRIPT_DIR/%s\" )\n", scriptFileName(script.Name))
		} else {
			fmt.Fprintf(buf, "( source \"$SPREAD_SCRIPT_DIR/%s\" )\n", scriptFileName(script.Name))
		}
	}
	buf.WriteString("} < /dev/null\n")
}

func escapeBashDoubleQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "$", "\\$")
	return s
}

func bashSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, `'`, `'\''`) + "'"
}

const spreadStackDumpFn = `spread_stack_dump() {
  local status=$1 cmd=$2 pipestatus=$3
  if [ "$status" = 213 ]; then
    return "$status"
  fi
  if [ -f "$SPREAD_SCRIPT_DIR/.stack-dumped" ]; then
    return "$status"
  fi
  : > "$SPREAD_SCRIPT_DIR/.stack-dumped"
  local -a _stack_argc _stack_argv
  set +u
  _stack_argc=("${BASH_ARGC[@]}")
  _stack_argv=("${BASH_ARGV[@]}")
  set -u
  (
    set +eu
    stack_frame_file() {
      local src=$1
      local origin="${src}.origin"
      if [ -r "$origin" ]; then
        sed -n '1p' "$origin"
        return
      fi
      case "$src" in
        "$SPREAD_SCRIPT_DIR"/*) printf '%s' "${src##*/}" ;;
        *) printf '%s' "$src" ;;
      esac
    }
    stack_frame_lineno() {
      local src=$1 lineno=$2
      local origin="${src}.origin" base
      if [ -r "$origin" ]; then
        base=$(sed -n '2p' "$origin")
        if [ -n "$base" ] && [ "$lineno" -gt 0 ] 2>/dev/null; then
          echo $((base + lineno - 1))
          return
        fi
      fi
      printf '%s' "$lineno"
    }
    stack_frame_line() {
      local src=$1 lineno=$2
      local origin="${src}.origin" yamlfile yamlline
      if [ -r "$origin" ]; then
        yamlfile=$(sed -n '1p' "$origin")
        yamlline=$(stack_frame_lineno "$src" "$lineno")
        if [ -n "$yamlfile" ] && [ -n "${SPREAD_PATH-}" ] && [ -r "$SPREAD_PATH/$yamlfile" ]; then
          sed -n "${yamlline}{p;q}" "$SPREAD_PATH/$yamlfile" 2>/dev/null
          return
        fi
      fi
      if [ -n "$lineno" ] && [ "$lineno" -gt 0 ] 2>/dev/null && [ -r "$src" ]; then
        sed -n "${lineno}{p;q}" "$src" 2>/dev/null
      fi
    }
    stack_frame_args() {
      local idx=$1
      local offset=0 k j argc arg n maxn=16 maxc=200 out
      for ((k=0; k<idx; k++)); do
        offset=$((offset + ${_stack_argc[k]:-0}))
      done
      argc=${_stack_argc[idx]:-0}
      [ "$argc" -gt 0 ] 2>/dev/null || return 0
      n=0
      out=""
      for ((j=argc-1; j>=0; j--)); do
        arg="${_stack_argv[offset+j]}"
        if [ "${#arg}" -gt "$maxc" ]; then
          arg="${arg:0:$maxc}..."
        fi
        arg=$(printf '%q' "$arg")
        if [ -n "$out" ]; then
          out="$out $arg"
        else
          out="$arg"
        fi
        n=$((n+1))
        if [ "$n" -ge "$maxn" ]; then
          if [ "$argc" -gt "$maxn" ]; then
            out="$out ..."
          fi
          break
        fi
      done
      printf '%s' "$out"
    }
    local src="${BASH_SOURCE[1]}"
    local fail_lineno="${BASH_LINENO[0]}"
    local file line frames i func disp lineno text n recent args fail_disp nlines
    file=$(stack_frame_file "$src")
    fail_disp=$(stack_frame_lineno "$src" "$fail_lineno")
    line=$(stack_frame_line "$src" "$fail_lineno")
    frames=""
    for ((i=${#BASH_SOURCE[@]}-1; i>=1; i--)); do
      src="${BASH_SOURCE[i]}"
      lineno="${BASH_LINENO[i-1]}"
      func="${FUNCNAME[i]:-MAIN}"
      case "$src" in
        ""|"-"|/dev/fd/*) continue ;;
      esac
      disp=$(stack_frame_file "$src")
      args=""
      if [ "$func" != source ] && [ "$func" != main ] && [ "$func" != MAIN ]; then
        args=$(stack_frame_args "$i")
      fi
      frames="${frames}  File: \"${disp}\", lineno: $(stack_frame_lineno "$src" "$lineno"), in ${func}"$'\n'
      text=$(stack_frame_line "$src" "$lineno")
      if [ -n "$text" ]; then
        frames="${frames}    ${text}"$'\n'
      fi
      if [ -n "$args" ]; then
        frames="${frames}    args: ${args}"$'\n'
      fi
    done
    n=${SPREAD_STACK_OUTPUT:-20}
    case "$n" in
      ''|*[!0-9]*) n=20 ;;
    esac
    if [ "$n" -gt 0 ] && [ -f "$SPREAD_SCRIPT_DIR/output" ]; then
      recent=$(tail -n "$n" "$SPREAD_SCRIPT_DIR/output" 2>/dev/null)
    fi
    echo "----- spread stack -----" >&2
    echo "job: ${SPREAD_JOB:-}" >&2
    echo "path: ${SPREAD_PHASE_PATH:-}" >&2
    echo "phase: ${SPREAD_OPERATION:-unknown}" >&2
    if [ "$status" = 214 ]; then
      echo "kind: breakpoint" >&2
    fi
    echo "file: ${file:-}" >&2
    echo "lineno: ${fail_disp:-}" >&2
    echo "line: ${line}" >&2
    echo "cmd: ${cmd}" >&2
    echo "exit code: ${status}" >&2
    echo "pipestatus: ${pipestatus}" >&2
    echo >&2
    if [ -n "$frames" ]; then
      echo "Traceback (most recent call last):" >&2
      printf '%s' "$frames" >&2
    fi
    if [ -n "$recent" ]; then
      nlines=$(printf '%s' "$recent" | awk 'END {print NR}')
      echo >&2
      echo "last output (${nlines} lines):" >&2
      printf '%s\n' "$recent" | sed 's/^/  /' >&2
    fi
  )
  return "$status"
}
`

func outputErr(output []byte, err error) error {
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		if bytes.Contains(output, []byte{'\n'}) {
			err = fmt.Errorf("\n-----\n%s\n-----", output)
		} else {
			err = fmt.Errorf("%s", output)
		}
	}
	return err
}

func waitPortUp(ctx context.Context, what fmt.Stringer, address string) error {
	if !strings.Contains(address, ":") {
		address += ":22"
	}

	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(15 * time.Second)
	defer relog.Stop()
	var retry = time.NewTicker(1 * time.Second)
	defer retry.Stop()

	for {
		debugf("Waiting until %s is listening at %s...", what, address)
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot connect to %s: %v", what, err)
		case <-timeout:
			return fmt.Errorf("cannot connect to %s: %v", what, err)
		case <-ctx.Done():
			return fmt.Errorf("cannot connect to %s: interrupted", what)
		}
	}
	return nil
}

func waitServerUp(ctx context.Context, server Server, username, password string, sshKeyFile string, sshKeyPass string) error {
	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(2 * time.Minute)
	defer relog.Stop()
	var retry = time.NewTicker(1 * time.Second)
	defer retry.Stop()

	for {
		debugf("Waiting until %s is listening...", server)
		client, err := Dial(server, username, password, sshKeyFile, sshKeyPass)
		if err == nil {
			client.Close()
			break
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot connect to %s: %v", server, err)
		case <-timeout:
			return fmt.Errorf("cannot connect to %s: %v", server, err)
		case <-ctx.Done():
			return fmt.Errorf("cannot connect to %s: interrupted", server)
		}
	}
	return nil
}
