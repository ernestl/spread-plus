package spread

import (
	"bytes"
	"time"

	"golang.org/x/crypto/ssh"
)

func MockClient() *Client {
	config := &ssh.ClientConfig{
		User:    "mock",
		Timeout: 10 * time.Second,
	}
	return &Client{
		config: config,
		job:    "mock-job",
	}
}

func DialOnReboot(cli *Client, prevBootID string) error {
	return cli.dialOnReboot(prevBootID)
}

func SetKillTimeout(cli *Client, killTimeout time.Duration) {
	cli.killTimeout = killTimeout
}

func SetWarnTimeout(cli *Client, warnTimeout time.Duration) {
	cli.warnTimeout = warnTimeout
}

func MockSshDial(f func(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)) (restore func()) {
	oldSshDial := sshDial
	sshDial = f
	return func() {
		sshDial = oldSshDial
	}
}

func MockTimeNow(f func() time.Time) (restore func()) {
	oldTimeNow := timeNow
	timeNow = f
	return func() {
		timeNow = oldTimeNow
	}
}

func RunLocalScripts(scripts []StageScript, dir string, env *Environment) (output []byte, err error) {
	if env == nil {
		env = NewEnvironment()
	}
	s := localScript{
		scripts:     scripts,
		dir:         dir,
		env:         env,
		mode:        combinedOutput,
		warnTimeout: 5 * time.Second,
		killTimeout: 15 * time.Second,
	}
	stdout, stderr, err := s.run()
	if len(stderr) > 0 {
		return append(stdout, stderr...), err
	}
	return stdout, err
}

func ScriptRuntime(scripts []StageScript, enableTrace bool) string {
	var buf bytes.Buffer
	writeScriptRuntime(&buf, scripts, enableTrace)
	return buf.String()
}

func JobPhasePath(verb string, job *Job, context interface{}) string {
	return jobPhasePath(verb, job, context)
}

func JobPhasePathAt(verb string, job *Job, context interface{}, scripts []StageScript, index int) string {
	return jobPhasePathAt(verb, job, context, scripts, index)
}

func YAMLBodyOrigin(data []byte, keys ...string) int {
	return yamlBodyOrigin(data, keys...)
}

func IsBreakpoint(err error) bool {
	return isBreakpoint(err)
}

func RunLocalScriptsTraced(scripts []StageScript, dir string, env *Environment) (output []byte, err error) {
	if env == nil {
		env = NewEnvironment()
	}
	s := localScript{
		scripts:     scripts,
		dir:         dir,
		env:         env,
		mode:        traceOutput,
		warnTimeout: 5 * time.Second,
		killTimeout: 15 * time.Second,
	}
	stdout, stderr, err := s.run()
	if len(stderr) > 0 {
		return append(stdout, stderr...), err
	}
	return stdout, err
}

var QemuCmd = qemuCmd
