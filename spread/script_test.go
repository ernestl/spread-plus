package spread_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	. "gopkg.in/check.v1"

	"github.com/canonical/spread-plus/spread"
)

type scriptSuite struct{}

var _ = Suite(&scriptSuite{})

func testJob() *spread.Job {
	return &spread.Job{
		Project: &spread.Project{
			PrepareEach: "echo project-each",
			RestoreEach: "echo project-restore-each",
			DebugEach:   "echo project-debug-each",
		},
		Backend: &spread.Backend{
			PrepareEach: "",
			RestoreEach: "echo backend-restore-each",
		},
		Suite: &spread.Suite{
			PrepareEach: "echo suite-each",
			RestoreEach: "",
			DebugEach:   "echo suite-debug-each",
		},
		Task: &spread.Task{
			Prepare: "echo task-prepare",
			Restore: "echo task-restore",
			Debug:   "",
		},
	}
}

func (s *scriptSuite) TestPrepareScriptsSkipsEmpty(c *C) {
	job := testJob()
	scripts := job.PrepareScripts()
	c.Assert(scripts, HasLen, 3)
	c.Check(scripts[0].Name, Equals, "project.prepare-each")
	c.Check(scripts[0].Body, Equals, "echo project-each")
	c.Check(scripts[1].Name, Equals, "suite.prepare-each")
	c.Check(scripts[2].Name, Equals, "task.prepare")
}

func (s *scriptSuite) TestRestoreScriptsOrder(c *C) {
	job := testJob()
	scripts := job.RestoreScripts()
	c.Assert(scripts, HasLen, 3)
	c.Check(scripts[0].Name, Equals, "task.restore")
	c.Check(scripts[1].Name, Equals, "backend.restore-each")
	c.Check(scripts[2].Name, Equals, "project.restore-each")
}

func (s *scriptSuite) TestDebugScriptsSkipsEmpty(c *C) {
	job := testJob()
	scripts := job.DebugScripts()
	c.Assert(scripts, HasLen, 2)
	c.Check(scripts[0].Name, Equals, "suite.debug-each")
	c.Check(scripts[1].Name, Equals, "project.debug-each")
}

func (s *scriptSuite) TestPrepareScriptsCopiesOrigin(c *C) {
	job := testJob()
	job.Task.PrepareOrigin = spread.YAMLOrigin{File: "fail/nested/task.yaml", Line: 3}
	scripts := job.PrepareScripts()
	c.Assert(scripts, HasLen, 3)
	c.Check(scripts[2].Name, Equals, "task.prepare")
	c.Check(scripts[2].OriginFile, Equals, "fail/nested/task.yaml")
	c.Check(scripts[2].OriginLine, Equals, 3)
	c.Check(scripts[0].OriginFile, Equals, "")
}

func (s *scriptSuite) TestPrepareStringStillJoinsSubshells(c *C) {
	job := testJob()
	got := job.Prepare()
	c.Check(got, Matches, `(?s)\(\necho project-each\n\)\n\n\(\necho suite-each\n\)\n\n\(\necho task-prepare\n\)`)
}

func (s *scriptSuite) TestSuccessPathUnchanged(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "echo ok"},
	}, "", successEnv())
	c.Assert(err, IsNil)
	c.Check(string(bytes.TrimSpace(output)), Equals, "ok")
	c.Check(string(output), Not(Matches), `(?s).*spread stack.*`)
}

func (s *scriptSuite) TestJoinScriptIsolation(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "project.prepare-each", Body: "export FOO=leaked\ncd /tmp"},
		{Name: "task.prepare", Body: "test -z \"${FOO:-}\"\necho isolated"},
	}, "", successEnv())
	c.Assert(err, IsNil)
	c.Check(string(bytes.TrimSpace(output)), Equals, "isolated")
}

func (s *scriptSuite) TestStdinRedirected(c *C) {
	started := time.Now()
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "read x || true\necho done"},
	}, "", successEnv())
	c.Assert(err, IsNil)
	c.Check(time.Since(started) < 5*time.Second, Equals, true)
	c.Check(string(bytes.TrimSpace(output)), Equals, "done")
}

func (s *scriptSuite) TestFailureStackNestedSource(c *C) {
	dir := c.MkDir()
	lib := filepath.Join(dir, "lib")
	c.Assert(os.Mkdir(lib, 0755), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "a.sh"), []byte("source lib/b.sh\na_call() {\n  b_call\n}\n"), 0644), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "b.sh"), []byte("source lib/c.sh\nb_call() {\n  c_fail\n}\n"), 0644), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "c.sh"), []byte("c_fail() {\n  false\n}\n"), 0644), IsNil)

	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "suite.prepare-each", Body: "echo suite-ok"},
		{Name: "task.execute", Body: "source lib/a.sh\na_call"},
	}, dir, successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*----- spread stack -----.*`)
	c.Check(text, Matches, `(?s).*job: google:ubuntu-22.04-64:tests/main/snap-info.*`)
	c.Check(text, Matches, `(?s).*path: spread-prepare → lxd-prepare → fail-prepare → fail/nested-prepare → fail/nested-execute.*`)
	c.Check(text, Matches, `(?s).*path: .*\nphase: executing.*`)
	c.Check(text, Matches, `(?s).*file: lib/c.sh.*`)
	c.Check(text, Matches, `(?s).*lineno: [0-9]+.*`)
	c.Check(text, Matches, `(?s).*line: *false.*`)
	c.Check(text, Matches, `(?s).*cmd: false.*`)
	c.Check(text, Matches, `(?s).*exit code: 1.*`)
	c.Check(text, Matches, `(?s).*Traceback \(most recent call last\):.*File: "task.execute.sh".*`)
	c.Check(text, Matches, `(?s).*File: "task.execute.sh", lineno: [0-9]+, in source.*`)
	c.Check(text, Matches, `(?s).*File: "lib/a.sh", lineno: [0-9]+, in a_call.*`)
	c.Check(text, Matches, `(?s).*File: "lib/b.sh", lineno: [0-9]+, in b_call.*`)
	c.Check(text, Matches, `(?s).*File: "lib/c.sh", lineno: [0-9]+, in c_fail.*`)
	c.Check(text, Matches, `(?s).*File: "task.execute.sh".*File: "lib/a.sh".*File: "lib/c.sh".*`)
	c.Check(text, Not(Matches), `(?s).*File: "suite.prepare-each.sh".*`)
	c.Check(text, Not(Matches), `(?s).*file: lib/c.sh.*backend: google.*`)
	c.Check(text, Not(Matches), `(?s).*\nscripts:.*`)
	c.Check(text, Not(Matches), `(?s).*args:.*`)
	c.Check(text, Matches, `(?s).*pipestatus: 1.*`)
}

func (s *scriptSuite) TestFailurePipelineStatus(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "true | false"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*exit code: 1.*`)
	c.Check(text, Matches, `(?s).*pipestatus: 0 1.*`)
}

func (s *scriptSuite) TestFailurePipefailStatus(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "set -o pipefail\nfalse | true"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*exit code: 1.*`)
	c.Check(text, Matches, `(?s).*pipestatus: 1 0.*`)
}

func (s *scriptSuite) TestFailurePipefailMiddleStatus(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "set -o pipefail\ntrue | false | true"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*line: true \| false \| true.*`)
	c.Check(text, Matches, `(?s).*exit code: 1.*`)
	c.Check(text, Matches, `(?s).*pipestatus: 0 1 0.*`)
}

func (s *scriptSuite) TestFailureStackFunctionArgs(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "a_call() {\n  false\n}\na_call snap core"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*File: "task.execute.sh", lineno: [0-9]+, in a_call.*`)
	c.Check(text, Matches, `(?s).*in a_call\n +false\n    args: snap core.*`)
	c.Check(text, Not(Matches), `(?s).*args: 1 false.*`)
}

func (s *scriptSuite) TestFailureInSecondScript(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "project.prepare-each", Body: "echo first-ok"},
		{Name: "task.prepare", Body: "echo second-start\nfalse"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*first-ok.*`)
	c.Check(text, Matches, `(?s).*File: "task.prepare.sh".*`)
	c.Check(text, Not(Matches), `(?s).*File: "project.prepare-each.sh".*`)
	c.Check(text, Matches, `(?s).*File: "task.prepare.sh".*last output \([0-9]+ lines\):.*first-ok.*`)
	c.Check(text, Not(Matches), `(?s).*last output.*File: "task.prepare.sh".*`)
}

func (s *scriptSuite) TestJoinPhasePathUsesCurrentScript(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "project.prepare-each", Body: "echo first-ok", Path: "spread-prepare-each"},
		{Name: "task.prepare", Body: "false", Path: "spread-prepare-each → fail/nested-prepare"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*path: spread-prepare-each → fail/nested-prepare.*`)
	c.Check(text, Not(Matches), `(?s).*path: spread-prepare-each\n.*`)
}

func (s *scriptSuite) TestStackOutputDisabled(c *C) {
	env := successEnv()
	env.Set("SPREAD_STACK_OUTPUT", "0")
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "echo visible\nfalse"},
	}, "", env)
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*----- spread stack -----.*`)
	c.Check(text, Not(Matches), `(?s).*last output:.*`)
}

func (s *scriptSuite) TestDumpQuietUnderXtrace(c *C) {
	output, err := spread.RunLocalScriptsTraced([]spread.StageScript{
		{Name: "task.execute", Body: "false"},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*----- spread stack -----.*`)
	c.Check(text, Not(Matches), `(?s).*----- spread stack -----.*\+\+\+ local status.*`)
	c.Check(text, Not(Matches), `(?s).*last output.*----- spread stack -----.*`)
}

func (s *scriptSuite) TestJobPhasePath(c *C) {
	job := &spread.Job{
		Project: &spread.Project{Name: "spread"},
		Backend: &spread.Backend{Name: "lxd"},
		System:  &spread.System{Name: "ubuntu-22.04"},
		Suite:   &spread.Suite{Name: "fail/"},
		Task:    &spread.Task{Name: "fail/nested"},
	}
	c.Check(spread.JobPhasePath("preparing", job, job.Project), Equals, "spread-prepare")
	c.Check(spread.JobPhasePath("preparing", job, job.Backend), Equals, "spread-prepare → lxd-prepare")
	c.Check(spread.JobPhasePath("preparing", job, job.Suite), Equals, "spread-prepare → lxd-prepare → fail-prepare")
	c.Check(spread.JobPhasePath("preparing", job, job), Equals, "spread-prepare → lxd-prepare → fail-prepare → fail/nested-prepare")
	c.Check(spread.JobPhasePath("executing", job, job), Equals, "spread-prepare → lxd-prepare → fail-prepare → fail/nested-prepare → fail/nested-execute")
	c.Check(spread.JobPhasePath("restoring", job, job), Equals, "spread-prepare → lxd-prepare → fail-prepare → fail/nested-prepare → fail/nested-execute → fail/nested-restore")
	c.Check(spread.JobPhasePath("checking", job, job.Suite), Equals, "spread-prepare → lxd-prepare → fail-skip")

	join := []spread.StageScript{
		{Name: "project.prepare-each"},
		{Name: "backend.prepare-each"},
		{Name: "suite.prepare-each"},
		{Name: "task.prepare"},
	}
	c.Check(spread.JobPhasePathAt("preparing", job, job, join, 1), Equals,
		"spread-prepare → lxd-prepare → fail-prepare → spread-prepare-each → lxd-prepare-each")
	c.Check(spread.JobPhasePathAt("preparing", job, job, join, 3), Equals,
		"spread-prepare → lxd-prepare → fail-prepare → spread-prepare-each → lxd-prepare-each → fail-prepare-each → fail/nested-prepare")
}

func (s *scriptSuite) TestErrorHelperQuiet(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: `ERROR "boom"`},
	}, "", successEnv())
	c.Assert(err, ErrorMatches, "boom")
	c.Check(string(output), Not(Matches), `(?s).*spread stack.*`)
	c.Check(err.Error(), Not(Matches), `(?s).*spread stack.*`)
}

func (s *scriptSuite) TestBreakpointDumpsStack(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: `BREAKPOINT "stop here"`},
	}, "", successEnv())
	c.Assert(err, NotNil)
	c.Check(spread.IsBreakpoint(err), Equals, true)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*----- spread stack -----.*`)
	c.Check(text, Matches, `(?s).*kind: breakpoint.*`)
	c.Check(text, Matches, `(?s).*exit code: 214.*`)
	c.Check(text, Matches, `(?s).*cmd: BREAKPOINT stop here.*`)
	c.Check(text, Matches, `(?s).*File: "task.execute.sh".*`)
	c.Check(err.Error(), Matches, `(?s).*stop here.*`)
}

func (s *scriptSuite) TestFatalHelperQuiet(c *C) {
	_, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "script", Body: `FATAL "nope"`},
	}, "", successEnv())
	c.Assert(err, NotNil)
	c.Check(err.Error(), Not(Matches), `(?s).*spread stack.*`)
}

func (s *scriptSuite) TestScriptRuntimeSourcesFiles(c *C) {
	script := spread.ScriptRuntime([]spread.StageScript{
		{Name: "task.execute", Body: "true"},
	}, false)
	c.Check(script, Matches, `(?s).*trap 'pipestatus="\$\{PIPESTATUS\[\*\]\}" status=\$\?; \{ set \+x; \} 2>/dev/null; spread_stack_dump "\$status" "\$BASH_COMMAND" "\$pipestatus"' ERR.*`)
	c.Check(script, Matches, `(?s).*shopt -s extdebug.*`)
	c.Check(script, Matches, `(?s).*source "\$SPREAD_SCRIPT_DIR/task.execute.sh".*`)
	c.Check(script, Matches, `(?s).*\} < /dev/null.*`)
	c.Check(script, Not(Matches), `(?s).*set -x.*`)
}

func (s *scriptSuite) TestTraceKeepsSetX(c *C) {
	script := spread.ScriptRuntime([]spread.StageScript{
		{Name: "task.execute", Body: "false"},
	}, true)
	c.Check(script, Matches, `(?s).*declare -A SPREAD_YAML_FILE SPREAD_YAML_BASE.*`)
	c.Check(script, Matches, `(?s).*PS4='\+ \$\{BASH_SOURCE\[0\]\+\$\{SPREAD_YAML_FILE\[\$\{BASH_SOURCE\[0\]\}\]:-\$\{BASH_SOURCE\[0\]#\$SPREAD_SCRIPT_DIR/\}\}:\$\(\(\$\{SPREAD_YAML_BASE\[\$\{BASH_SOURCE\[0\]\}\]:-1\}\+LINENO-1\)\)\}: '.*`)
	c.Check(script, Matches, `(?s).*set -T; trap 'case "\$\{BASH_SOURCE\[0\]-\}" in "\$SPREAD_SCRIPT_DIR"/\*\) trap - DEBUG; set \+T; set -x;; esac' DEBUG; source "\$SPREAD_SCRIPT_DIR/task.execute.sh".*`)
}

func (s *scriptSuite) TestScriptRuntimeExportsPhasePath(c *C) {
	script := spread.ScriptRuntime([]spread.StageScript{
		{Name: "task.execute", Body: "true", Path: "spread-prepare → fail/nested-execute"},
	}, false)
	c.Check(script, Matches, `(?s).*export SPREAD_PHASE_PATH="spread-prepare → fail/nested-execute".*source "\$SPREAD_SCRIPT_DIR/task.execute.sh".*`)
}

func (s *scriptSuite) TestXtraceHasFileLinenoPrefix(c *C) {
	dir := c.MkDir()
	lib := filepath.Join(dir, "lib")
	c.Assert(os.Mkdir(lib, 0755), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "c.sh"), []byte("c_fail() {\n  false\n}\n"), 0644), IsNil)

	output, err := spread.RunLocalScriptsTraced([]spread.StageScript{
		{Name: "task.execute", Body: "echo ok\nsource lib/c.sh\nc_fail"},
	}, dir, successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*\+.*task\.execute\.sh:1: echo ok.*`)
	c.Check(text, Matches, `(?s).*\+.*task\.execute\.sh:2: source lib/c.sh.*`)
	c.Check(text, Matches, `(?s).*\+.*task\.execute\.sh:3: c_fail.*`)
	c.Check(text, Matches, `(?s).*\+.*lib/c\.sh:2: false.*`)
	c.Check(text, Not(Matches), `(?s).*\+ :[0-9]+: source .*`)
	c.Check(text, Not(Matches), `(?s).*spread-scripts\.[^/\s]+/task\.execute\.sh.*`)
}

func (s *scriptSuite) TestFailureStackYAMLOrigin(c *C) {
	dir := c.MkDir()
	c.Assert(ioutil.WriteFile(filepath.Join(dir, "task.yaml"), []byte("summary: origin dump\n\nexecute: |\n    source lib/a.sh\n    a_call snap core\n"), 0644), IsNil)
	lib := filepath.Join(dir, "lib")
	c.Assert(os.Mkdir(lib, 0755), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "a.sh"), []byte("a_call() {\n  false\n}\n"), 0644), IsNil)

	env := successEnv()
	env.Set("SPREAD_PATH", dir)
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "source lib/a.sh\na_call snap core", OriginFile: "task.yaml", OriginLine: 4},
	}, dir, env)
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*File: "task.yaml", lineno: 5, in source.*`)
	c.Check(text, Matches, `(?s).*File: "lib/a.sh", lineno: [0-9]+, in a_call.*`)
	c.Check(text, Not(Matches), `(?s).*File: "task.execute.sh".*`)
	c.Check(text, Matches, `(?s).*file: lib/a.sh.*`)
}

func (s *scriptSuite) TestFailureHeaderYAMLOrigin(c *C) {
	output, err := spread.RunLocalScripts([]spread.StageScript{
		{Name: "task.execute", Body: "true\nfalse", OriginFile: "task.yaml", OriginLine: 4},
	}, "", successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*file: task.yaml.*`)
	c.Check(text, Matches, `(?s).*lineno: 5\n.*`)
	c.Check(text, Matches, `(?s).*File: "task.yaml", lineno: 5, in source.*`)
	c.Check(text, Not(Matches), `(?s).*File: "task.execute.sh".*`)
}

func (s *scriptSuite) TestXtraceYAMLOrigin(c *C) {
	dir := c.MkDir()
	lib := filepath.Join(dir, "lib")
	c.Assert(os.Mkdir(lib, 0755), IsNil)
	c.Assert(ioutil.WriteFile(filepath.Join(lib, "c.sh"), []byte("c_fail() {\n  false\n}\n"), 0644), IsNil)

	output, err := spread.RunLocalScriptsTraced([]spread.StageScript{
		{Name: "task.execute", Body: "source lib/c.sh\nc_fail", OriginFile: "fail/nested/task.yaml", OriginLine: 4},
	}, dir, successEnv())
	c.Assert(err, NotNil)
	text := string(output) + err.Error()
	c.Check(text, Matches, `(?s).*\+.*fail/nested/task\.yaml:4: source lib/c.sh.*`)
	c.Check(text, Matches, `(?s).*\+.*fail/nested/task\.yaml:5: c_fail.*`)
	c.Check(text, Matches, `(?s).*\+.*lib/c\.sh:2: false.*`)
	c.Check(text, Not(Matches), `(?s).*task\.execute\.sh:.*`)
}

func (s *scriptSuite) TestScriptRuntimeWritesOrigin(c *C) {
	script := spread.ScriptRuntime([]spread.StageScript{
		{Name: "task.execute", Body: "false", OriginFile: "fail/nested/task.yaml", OriginLine: 4},
	}, true)
	c.Check(script, Matches, `(?s).*printf '%s\\n%d\\n' 'fail/nested/task.yaml' 4 > "\$SPREAD_SCRIPT_DIR/task.execute.sh.origin".*`)
	c.Check(script, Matches, `(?s).*SPREAD_YAML_FILE\["\$SPREAD_SCRIPT_DIR/task.execute.sh"\]='fail/nested/task.yaml'.*`)
	c.Check(script, Matches, `(?s).*SPREAD_YAML_BASE\["\$SPREAD_SCRIPT_DIR/task.execute.sh"\]=4.*`)
}

func successEnv() *spread.Environment {
	return spread.NewEnvironment(
		"SPREAD_OPERATION", "executing",
		"SPREAD_PHASE_PATH", "spread-prepare → lxd-prepare → fail-prepare → fail/nested-prepare → fail/nested-execute",
		"SPREAD_BACKEND", "google",
		"SPREAD_SYSTEM", "ubuntu-22.04-64",
		"SPREAD_SUITE", "tests/main/",
		"SPREAD_TASK", "tests/main/snap-info",
		"SPREAD_VARIANT", "",
		"SPREAD_JOB", "google:ubuntu-22.04-64:tests/main/snap-info",
	)
}
