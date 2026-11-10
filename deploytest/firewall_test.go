package deploytest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	cDispatcherDest = "172.17.0.1"
	cEdgeSubnet     = "172.21.0.0/16"
	cRuleComment    = "Traefik edge network -> bcx-dispatcher"
	cDiscoverArgv   = "network inspect edge --format {{range .IPAM.Config}}{{.Subnet}}{{end}}"
	// cNever is a docker stub threshold no poll loop reaches.
	cNever = 1000
)

// cExpectedRule is ufw's own rendering of the one rule the script manages.
func cExpectedRule(src string) string {
	return "ufw allow from " + src + " to " + cDispatcherDest + " port 9100 proto tcp comment '" + cRuleComment + "'"
}

// cExpectedArgv is the argv of the one `ufw allow` call, each word bracketed so
// word splitting would show up as extra brackets.
func cExpectedArgv(src string) string {
	return "[allow][from][" + src + "][to][" + cDispatcherDest + "][port][9100][proto][tcp][comment][" + cRuleComment + "]"
}

const cDockerStub = `#!/bin/sh
printf '%s\n' "$*" >> '@LOG@'
if [ "$*" != "network inspect edge --format {{range .IPAM.Config}}{{.Subnet}}{{end}}" ]; then
  echo "stub docker: unexpected argv: $*" >&2
  exit 64
fi
calls=$(wc -l < '@LOG@' | tr -d ' ')
if [ "$calls" -lt @AFTER@ ]; then
  echo "Error: No such network: edge" >&2
  exit 1
fi
echo '@SUBNET@'
`

// The stub renders an accepted rule the way `ufw show added` does and refuses
// every call shape the script is not supposed to make.
const cUfwStub = `#!/bin/sh
{ for a in "$@"; do printf '[%s]' "$a"; done; echo; } >> '@LOG@'
if [ -e '@BROKEN@' ]; then
  echo "ERROR: You need to be root to run this script" >&2
  exit 1
fi
if [ "$1 $2" = "show added" ]; then
  echo "Added user rules (see 'ufw status' for running firewall):"
  cat '@ADDED@'
  exit 0
fi
if [ "$1" = "allow" ] && [ "$#" -eq 11 ]; then
  echo "ufw $1 $2 $3 $4 $5 $6 $7 $8 $9 ${10} '${11}'" >> '@ADDED@'
  exit 0
fi
echo "stub ufw: unexpected argv: $*" >&2
exit 64
`

// The poll loop would sleep for a minute; the stub records the request instead.
const cSleepStub = `#!/bin/sh
printf '%s\n' "$*" >> '@LOG@'
`

// cFwEnv is a temp tree whose PATH starts with recording stubs for docker, ufw
// and sleep, so broker-firewall.sh never reaches the real system.
type cFwEnv struct {
	t         *testing.T
	Dir       string
	Added     string // rules `ufw show added` reports, one rendered rule per line
	UfwBroken string // while this file exists every ufw call fails
	UfwLog    string // argv of every ufw call
	DockerLog string // argv of every docker call
	SleepLog  string // argv of every sleep call
	Env       []string
}

// newCFwEnv builds the stubs. The docker stub answers with cEdgeSubnet from its
// edgeAfter-th call on and fails before that (cNever: the network never exists).
func newCFwEnv(t *testing.T, edgeAfter int) cFwEnv {
	t.Helper()
	dir := shortTmp(t)
	bin := filepath.Join(dir, "bin")
	e := cFwEnv{
		t:         t,
		Dir:       dir,
		Added:     filepath.Join(dir, "ufw-added"),
		UfwBroken: filepath.Join(dir, "ufw-broken"),
		UfwLog:    filepath.Join(dir, "ufw.log"),
		DockerLog: filepath.Join(dir, "docker.log"),
		SleepLog:  filepath.Join(dir, "sleep.log"),
		Env:       []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")},
	}
	if err := os.WriteFile(e.Added, nil, 0o644); err != nil {
		t.Fatalf("seed %s: %v", e.Added, err)
	}
	fill := strings.NewReplacer(
		"@LOG@", e.DockerLog,
		"@AFTER@", strconv.Itoa(edgeAfter),
		"@SUBNET@", cEdgeSubnet,
	)
	writeExec(t, filepath.Join(bin, "docker"), fill.Replace(cDockerStub))
	writeExec(t, filepath.Join(bin, "ufw"), strings.NewReplacer("@LOG@", e.UfwLog, "@ADDED@", e.Added, "@BROKEN@", e.UfwBroken).Replace(cUfwStub))
	writeExec(t, filepath.Join(bin, "sleep"), strings.NewReplacer("@LOG@", e.SleepLog).Replace(cSleepStub))
	return e
}

func (e cFwEnv) script() string { return filepath.Join(repoRoot(e.t), "broker-firewall.sh") }

// run executes the script directly, which also proves its exec bit and shebang.
func (e cFwEnv) run(args ...string) result {
	e.t.Helper()
	return runCmd(e.t, e.Dir, e.Env, e.script(), args...)
}

// lines returns the non-empty lines of a stub log; a log never written is nil.
func (e cFwEnv) lines(path string) []string {
	e.t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (e cFwEnv) expectLines(what, path string, want ...string) {
	e.t.Helper()
	if got := e.lines(path); !reflect.DeepEqual(got, want) {
		e.t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func (e cFwEnv) expectUntouched(what, path string) {
	e.t.Helper()
	if got := e.lines(path); len(got) != 0 {
		e.t.Errorf("%s = %q, want no calls", what, got)
	}
}

func TestFirewallEnsuresExactlyTheDispatcherRule(t *testing.T) {
	e := newCFwEnv(t, 1)
	r := e.run()
	if r.Code != 0 {
		t.Fatalf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	e.expectLines("ufw added rules", e.Added, cExpectedRule(cEdgeSubnet))
	e.expectLines("ufw argv", e.UfwLog, cExpectedArgv(cEdgeSubnet))
	e.expectLines("docker argv", e.DockerLog, cDiscoverArgv)
	e.expectUntouched("sleep", e.SleepLog)
	if !strings.HasPrefix(r.Stdout, "ensured: ") {
		t.Errorf("stdout = %q, want an ensured: line", r.Stdout)
	}
}

func TestFirewallCheckFailsWhenMissingAndPassesOncePresent(t *testing.T) {
	e := newCFwEnv(t, 1)
	missing := e.run("--check")
	if missing.Code != 1 {
		t.Fatalf("--check on an empty ruleset: exit %d, want 1", missing.Code)
	}
	if !strings.HasPrefix(missing.Stderr, "FAIL - ") || !strings.Contains(missing.Stderr, cExpectedRule(cEdgeSubnet)) {
		t.Errorf("stderr = %q, want a FAIL line naming the expected rule", missing.Stderr)
	}
	e.expectLines("ufw argv after a failed check", e.UfwLog, "[show][added]")

	if r := e.run(); r.Code != 0 {
		t.Fatalf("ensure: exit %d, stderr %q", r.Code, r.Stderr)
	}
	present := e.run("--check")
	if present.Code != 0 {
		t.Fatalf("--check with the rule present: exit %d, stderr %q", present.Code, present.Stderr)
	}
	if !strings.HasPrefix(present.Stdout, "ok   - ") {
		t.Errorf("stdout = %q, want an ok line", present.Stdout)
	}
	// ensure made one allow call; each check only listed.
	e.expectLines("ufw argv", e.UfwLog, "[show][added]", cExpectedArgv(cEdgeSubnet), "[show][added]")
}

// grep -x matches whole lines: a rule that merely contains the expected one, or
// differs in source, port or comment, must not count as present.
func TestFirewallCheckRequiresTheExactRuleLine(t *testing.T) {
	exact := cExpectedRule(cEdgeSubnet)
	stale := map[string]string{
		"other source subnet": cExpectedRule("172.22.0.0/16"),
		"old per-tenant rule": "ufw allow from " + cEdgeSubnet + " to any port 9010 proto tcp comment 'Traefik edge network -> runner-broker alpha-org'",
		"other comment":       strings.Replace(exact, cRuleComment, "something else", 1),
		"other destination":   strings.Replace(exact, "to "+cDispatcherDest, "to any", 1),
		"prefixed line":       "# " + exact,
		"suffixed line":       exact + " # stale",
	}
	for name, line := range stale {
		t.Run(name, func(t *testing.T) {
			e := newCFwEnv(t, 1)
			if err := os.WriteFile(e.Added, []byte(line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if r := e.run("--check"); r.Code != 1 {
				t.Fatalf("--check accepted %q: exit %d, stdout %q", line, r.Code, r.Stdout)
			}
		})
	}
	t.Run("exact line among others", func(t *testing.T) {
		e := newCFwEnv(t, 1)
		body := cExpectedRule("172.22.0.0/16") + "\n" + exact + "\n"
		if err := os.WriteFile(e.Added, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := e.run("--check"); r.Code != 0 {
			t.Fatalf("--check rejected the exact rule: exit %d, stderr %q", r.Code, r.Stderr)
		}
	})
}

// A ufw that cannot list its rules (not root, not installed) is not the same
// finding as a missing rule, and must not be reported as a silent exit.
func TestFirewallCheckFailsLoudlyWhenUfwCannotListRules(t *testing.T) {
	e := newCFwEnv(t, 1)
	if err := os.WriteFile(e.UfwBroken, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.run("--check")
	if r.Code != 1 {
		t.Fatalf("exit %d, want 1 (stdout %q)", r.Code, r.Stdout)
	}
	if !strings.Contains(r.Stderr, "FAIL - ") || !strings.Contains(r.Stderr, "need to be root") {
		t.Errorf("stderr = %q, want a FAIL line and ufw's own error", r.Stderr)
	}
	if r.Stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.Stdout)
	}
}

func TestFirewallSourceOverrideSkipsDiscovery(t *testing.T) {
	const override = "10.0.10.0/24"
	e := newCFwEnv(t, 1)
	if r := e.run("--source", override); r.Code != 0 {
		t.Fatalf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	e.expectLines("ufw added rules", e.Added, cExpectedRule(override))
	e.expectLines("ufw argv", e.UfwLog, cExpectedArgv(override))
	e.expectUntouched("docker", e.DockerLog)

	if r := e.run("--check", "--source", override); r.Code != 0 {
		t.Fatalf("--check --source: exit %d, stderr %q", r.Code, r.Stderr)
	}
	// the discovered subnet was never ensured, so checking it still fails.
	if r := e.run("--check"); r.Code != 1 {
		t.Fatalf("--check against the discovered subnet: exit %d, want 1", r.Code)
	}
}

func TestFirewallUnknownArgumentExitsTwoAndChangesNothing(t *testing.T) {
	cases := map[string][]string{
		"old env-dir flag":      {"--env-dir", "/etc/nuroot-runner-broker"},
		"unknown flag":          {"--bogus"},
		"positional":            {"extra"},
		"unknown after a valid": {"--check", "--bogus"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := newCFwEnv(t, 1)
			r := e.run(args...)
			if r.Code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", r.Code, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "unknown argument: ") {
				t.Errorf("stderr = %q, want an unknown argument message", r.Stderr)
			}
			e.expectUntouched("ufw", e.UfwLog)
			e.expectUntouched("docker", e.DockerLog)
		})
	}
}

// An empty --source must not fall back to discovery: it would ensure a rule for
// a subnet the operator did not name.
func TestFirewallSourceWithoutAValueExitsTwo(t *testing.T) {
	cases := map[string][]string{
		"missing":       {"--source"},
		"empty":         {"--source", ""},
		"missing check": {"--check", "--source"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := newCFwEnv(t, 1)
			r := e.run(args...)
			if r.Code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", r.Code, r.Stderr)
			}
			if !strings.Contains(r.Stderr, "--source needs a CIDR") {
				t.Errorf("stderr = %q, want the --source message", r.Stderr)
			}
			e.expectUntouched("ufw", e.UfwLog)
			e.expectUntouched("docker", e.DockerLog)
		})
	}
}

// --check is a health report: with no edge network it fails at once instead of
// polling, and never tries to change the ruleset.
func TestFirewallCheckWithoutEdgeNetworkExitsOneWithoutPolling(t *testing.T) {
	e := newCFwEnv(t, cNever)
	r := e.run("--check")
	if r.Code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", r.Code, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "cannot discover the edge network subnet") {
		t.Errorf("stderr = %q, want the discovery failure", r.Stderr)
	}
	e.expectLines("docker argv", e.DockerLog, cDiscoverArgv)
	e.expectUntouched("sleep", e.SleepLog)
	e.expectUntouched("ufw", e.UfwLog)
}

// At boot the script can run before `edge` exists, so ensure mode polls.
func TestFirewallPollsForTheEdgeNetwork(t *testing.T) {
	e := newCFwEnv(t, 3)
	r := e.run()
	if r.Code != 0 {
		t.Fatalf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	e.expectLines("docker argv", e.DockerLog, cDiscoverArgv, cDiscoverArgv, cDiscoverArgv)
	e.expectLines("sleep argv", e.SleepLog, "2", "2")
	e.expectLines("ufw added rules", e.Added, cExpectedRule(cEdgeSubnet))
}

func TestFirewallGivesUpAfterThirtyPollsAndChangesNothing(t *testing.T) {
	e := newCFwEnv(t, cNever)
	r := e.run()
	if r.Code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", r.Code, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "cannot discover the edge network subnet") {
		t.Errorf("stderr = %q, want the discovery failure", r.Stderr)
	}
	if got := len(e.lines(e.DockerLog)); got != 30 {
		t.Errorf("docker polled %d times, want 30", got)
	}
	for _, s := range e.lines(e.SleepLog) {
		if s != "2" {
			t.Errorf("sleep %q, want 2", s)
		}
	}
	e.expectUntouched("ufw", e.UfwLog)
}

func TestFirewallHelpPrintsOnlyTheHeaderComment(t *testing.T) {
	e := newCFwEnv(t, 1)
	r := e.run("-h")
	if r.Code != 0 {
		t.Fatalf("exit %d, stderr %q", r.Code, r.Stderr)
	}
	for _, want := range []string{"broker-firewall.sh", "--check", "--source", "172.17.0.1"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("-h output lacks %q", want)
		}
	}
	for _, code := range []string{"set -euo", "#!", "discover()"} {
		if strings.Contains(r.Stdout, code) {
			t.Errorf("-h output leaks script body: %q", code)
		}
	}
	e.expectUntouched("ufw", e.UfwLog)
	e.expectUntouched("docker", e.DockerLog)
}

func TestFirewallScriptIsExecutable(t *testing.T) {
	info, err := os.Stat(filepath.Join(repoRoot(t), "broker-firewall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("broker-firewall.sh mode %v is not executable", info.Mode().Perm())
	}
}

// macOS ships bash 3.2 as /bin/bash; the script must run there, not just under
// the newer bash that PATH resolves to.
func TestFirewallWorksUnderSystemBash(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	e := newCFwEnv(t, 1)
	version := runCmd(t, e.Dir, nil, "/bin/bash", "--version")
	t.Logf("/bin/bash: %s", strings.SplitN(version.Stdout, "\n", 2)[0])

	run := func(args ...string) result {
		return runCmd(t, e.Dir, e.Env, "/bin/bash", append([]string{e.script()}, args...)...)
	}
	if r := run("--check"); r.Code != 1 {
		t.Fatalf("--check before ensure: exit %d, want 1", r.Code)
	}
	if r := run(); r.Code != 0 {
		t.Fatalf("ensure: exit %d, stderr %q", r.Code, r.Stderr)
	}
	if r := run("--check"); r.Code != 0 {
		t.Fatalf("--check after ensure: exit %d, stderr %q", r.Code, r.Stderr)
	}
	if r := run("--env-dir", "/x"); r.Code != 2 {
		t.Fatalf("unknown argument: exit %d, want 2", r.Code)
	}
	e.expectLines("ufw added rules", e.Added, cExpectedRule(cEdgeSubnet))
}
