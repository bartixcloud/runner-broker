package deploytest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var bTenantNames = []string{"alpha-org", "kappa-org"}

// bStubGenerator stands in for bcx-gen-tenant-env. It enforces the argv contract
// (a wrong shape exits 64, which install.sh turns into exit 2) and is
// byte-deterministic: output depends on the tenants file only. It writes noise to
// stdout, which install.sh must discard, and leaves a "ran" marker next to itself
// so a test can prove the generator was never reached.
const bStubGenerator = `#!/bin/sh
set -eu
echo ran >> "$0.ran"
if [ "$#" -ne 4 ] || [ "$2" != --out ] || [ "$4" != --no-incus-check ]; then
  echo "stub generator: bad argv: $*" >&2
  exit 64
fi
tenants=$1
out=$3
echo "stub generator noise on stdout"
if grep -q INVALID "$tenants"; then
  echo "stub generator: invalid tenants file" >&2
  exit 2
fi
if grep -q EMPTY "$tenants"; then
  exit 0
fi
mkdir -p "$out"
list=
while IFS= read -r t; do
  [ -n "$t" ] || continue
  printf 'BROKER_TENANT=%s\n' "$t" > "$out/$t.env"
  list="$list\"$t\","
done < "$tenants"
printf '{"tenants":[%s]}\n' "${list%,}" > "$out/dispatcher.json"
`

func bStubBinary(name string) string {
	return "#!/bin/sh\n# stub " + name + "\nexit 0\n"
}

// bInstallEnv is one isolated install.sh run: a --root prefix, a bin dir of stub
// binaries, a tenants file and a systemctl stub that records its argv.
type bInstallEnv struct {
	t       *testing.T
	Base    string
	Root    string
	BinDir  string
	Tenants string
	Ctl     string
	CtlLog  string
}

func newBInstallEnv(t *testing.T) bInstallEnv {
	t.Helper()
	base := shortTmp(t)
	e := bInstallEnv{
		t:       t,
		Base:    base,
		Root:    filepath.Join(base, "root"),
		BinDir:  filepath.Join(base, "bin"),
		Tenants: filepath.Join(base, "tenants.yml"),
		Ctl:     filepath.Join(base, "ctl", "systemctl"),
		CtlLog:  filepath.Join(base, "systemctl.log"),
	}
	for _, b := range loadManifest(t) {
		content := bStubBinary(b.Name)
		if b.Role == "generator" {
			content = bStubGenerator
		}
		writeExec(t, filepath.Join(e.BinDir, b.Name), content)
	}
	e.writeTenants(strings.Join(bTenantNames, "\n") + "\n")
	writeExec(t, e.Ctl, "#!/bin/sh\necho \"$*\" >> '"+e.CtlLog+"'\n")
	return e
}

func (e bInstallEnv) writeTenants(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.Tenants, []byte(content), 0o644); err != nil {
		e.t.Fatalf("write tenants: %v", err)
	}
}

func (e bInstallEnv) script() string { return filepath.Join(repoRoot(e.t), "install.sh") }

func (e bInstallEnv) baseArgs() []string {
	return []string{"--tenants", e.Tenants, "--root", e.Root, "--bin-dir", e.BinDir}
}

// run executes the repo's install.sh with the standard flags plus extra, with the
// systemctl seam pointing at the recording stub.
func (e bInstallEnv) run(extra ...string) result {
	e.t.Helper()
	return runCmd(e.t, e.Base, []string{"SYSTEMCTL=" + e.Ctl}, e.script(), append(e.baseArgs(), extra...)...)
}

// ctlCalls returns one entry per recorded systemctl invocation.
func (e bInstallEnv) ctlCalls() []string {
	e.t.Helper()
	raw, err := os.ReadFile(e.CtlLog)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("read systemctl log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func (e bInstallEnv) mustInstall() {
	e.t.Helper()
	if r := e.run(); r.Code != 0 {
		e.t.Fatalf("install exit %d\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
	}
}

func bReadFile(t *testing.T, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return raw
}

func bWriteFile(t *testing.T, p string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, content, mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func bChmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
}

func bExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// bTarget is one file install.sh must place: root-relative path, mode and content.
type bTarget struct {
	Rel  string
	Mode os.FileMode
	Want []byte
}

func bTargets(t *testing.T) []bTarget {
	t.Helper()
	repo := repoRoot(t)
	var out []bTarget
	for _, b := range loadManifest(t) {
		want := []byte(bStubBinary(b.Name))
		if b.Role == "generator" {
			want = []byte(bStubGenerator)
		}
		out = append(out, bTarget{bInstallDir + "/" + b.Name, 0o755, want})
	}
	out = append(out,
		bTarget{bInstallDir + "/broker-firewall.sh", 0o755, bReadFile(t, filepath.Join(repo, "broker-firewall.sh"))},
		bTarget{bInstallDir + "/nat-ci-firewall.sh", 0o755, bReadFile(t, filepath.Join(repo, "nat-ci-firewall.sh"))},
		bTarget{"/etc/bcx-broker/tiers.yml", 0o644, bReadFile(t, filepath.Join(repo, "tiers.yml"))},
	)
	for _, u := range bShippedUnits {
		out = append(out, bTarget{"/etc/systemd/system/" + u, 0o644, bReadFile(t, filepath.Join(repo, u))})
	}
	var quoted []string
	for _, name := range bTenantNames {
		out = append(out, bTarget{"/etc/bcx-broker/" + name + ".env", 0o600, []byte("BROKER_TENANT=" + name + "\n")})
		quoted = append(quoted, `"`+name+`"`)
	}
	out = append(out, bTarget{"/etc/bcx-broker/dispatcher.json", 0o600, []byte(`{"tenants":[` + strings.Join(quoted, ",") + "]}\n")})
	return out
}

// bSnapshot records every path under root with its mode and content, so a test can
// prove a run touched nothing. A missing root is an empty snapshot.
func bSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if !bExists(root) {
		return out
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root)
		if d.IsDir() {
			out[rel] = "dir " + info.Mode().String()
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + " " + string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func bFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, strings.TrimPrefix(p, root))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestInstallPlacesEveryFileWithItsModeAndCheckIsThenClean(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()

	var wantPaths []string
	for _, want := range bTargets(t) {
		wantPaths = append(wantPaths, want.Rel)
		p := e.Root + want.Rel
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s: %v", want.Rel, err)
			continue
		}
		if got := info.Mode().Perm(); got != want.Mode {
			t.Errorf("%s: mode %04o, want %04o", want.Rel, got, want.Mode)
		}
		if got := bReadFile(t, p); !bytes.Equal(got, want.Want) {
			t.Errorf("%s: content differs from its source", want.Rel)
		}
	}
	sort.Strings(wantPaths)
	if got := bFilesUnder(t, e.Root); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("installed set:\n got %q\nwant %q", got, wantPaths)
	}

	// The stub generator writes to stdout; a clean --check printing nothing proves
	// install.sh discards it.
	r := e.run("--check")
	if r.Code != 0 || r.Stdout != "" {
		t.Fatalf("--check after install: exit %d, stdout %q, stderr %q", r.Code, r.Stdout, r.Stderr)
	}
}

func TestSecondInstallChangesNothing(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()
	before := bSnapshot(t, e.Root)
	calls := len(e.ctlCalls())

	r := e.run()
	if r.Code != 0 || r.Stdout != "" {
		t.Fatalf("second install: exit %d, stdout %q, stderr %q", r.Code, r.Stdout, r.Stderr)
	}
	if after := bSnapshot(t, e.Root); !reflect.DeepEqual(before, after) {
		t.Errorf("second install changed the tree")
	}
	if got := len(e.ctlCalls()); got != calls {
		t.Errorf("second install called systemctl %d more times", got-calls)
	}
}

func TestCheckReportsContentAndModeDriftWithoutFixingIt(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()
	envPath := e.Root + "/etc/bcx-broker/alpha-org.env"
	binPath := e.Root + "/opt/bcx-broker/bcx-broker"
	unitPath := e.Root + "/etc/systemd/system/bcx-dispatcher.service"
	bWriteFile(t, envPath, []byte("BROKER_TENANT=evil\n"), 0o600) // content only
	bChmod(t, binPath, 0o777)                                     // mode only
	bChmod(t, unitPath, 0o600)                                    // mode only, different kind
	drifted := bSnapshot(t, e.Root)
	calls := len(e.ctlCalls())

	r := e.run("--check")
	if r.Code != 1 {
		t.Fatalf("--check exit %d, want 1\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
	}
	got := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	sort.Strings(got)
	want := []string{
		"would change: /etc/bcx-broker/alpha-org.env",
		"would change: /etc/systemd/system/bcx-dispatcher.service",
		"would change: /opt/bcx-broker/bcx-broker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--check output:\n got %q\nwant %q", got, want)
	}
	if after := bSnapshot(t, e.Root); !reflect.DeepEqual(drifted, after) {
		t.Errorf("--check modified the tree")
	}
	if n := len(e.ctlCalls()); n != calls {
		t.Errorf("--check called systemctl %d times", n-calls)
	}

	// A real run repairs exactly that drift.
	e.mustInstall()
	if r := e.run("--check"); r.Code != 0 || r.Stdout != "" {
		t.Errorf("--check after repair: exit %d, stdout %q", r.Code, r.Stdout)
	}
	if got := bReadFile(t, envPath); string(got) != "BROKER_TENANT=alpha-org\n" {
		t.Errorf("env not repaired: %q", got)
	}
}

func TestCheckTreatsASymlinkedTargetAsDrift(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()
	tiers := e.Root + "/etc/bcx-broker/tiers.yml"
	real := filepath.Join(e.Base, "tiers.real")
	if err := os.Rename(tiers, real); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(real, tiers); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	r := e.run("--check")
	if r.Code != 1 || !strings.Contains(r.Stdout, "would change: /etc/bcx-broker/tiers.yml") {
		t.Fatalf("--check exit %d, stdout %q", r.Code, r.Stdout)
	}
	e.mustInstall()
	if info, err := os.Lstat(tiers); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		t.Errorf("install did not replace the symlink with a regular file: %v %v", info, err)
	}
}

func TestInvalidTenantsExitsTwoAndTouchesNothing(t *testing.T) {
	for _, extra := range [][]string{nil, {"--check"}} {
		e := newBInstallEnv(t)
		e.writeTenants("INVALID\n")
		r := e.run(extra...)
		if r.Code != 2 {
			t.Errorf("fresh root %v: exit %d, want 2", extra, r.Code)
		}
		if !strings.Contains(r.Stderr, "stub generator: invalid tenants file") {
			t.Errorf("generator stderr not surfaced: %q", r.Stderr)
		}
		if bExists(e.Root) {
			t.Errorf("fresh root %v: %s was created", extra, e.Root)
		}
		if calls := e.ctlCalls(); calls != nil {
			t.Errorf("systemctl called: %q", calls)
		}
	}

	// An existing, drifted tree stays exactly as it was: no partial repair.
	e := newBInstallEnv(t)
	e.mustInstall()
	bWriteFile(t, e.Root+"/etc/systemd/system/bcx-broker@.service", []byte("drift\n"), 0o644)
	before := bSnapshot(t, e.Root)
	calls := len(e.ctlCalls())
	e.writeTenants("INVALID\n")
	if r := e.run(); r.Code != 2 {
		t.Fatalf("installed root: exit %d, want 2", r.Code)
	}
	if after := bSnapshot(t, e.Root); !reflect.DeepEqual(before, after) {
		t.Errorf("a refused tenants.yml still changed the tree")
	}
	if n := len(e.ctlCalls()); n != calls {
		t.Errorf("a refused tenants.yml still called systemctl")
	}
}

func TestGeneratorWritingNothingExitsTwoAndTouchesNothing(t *testing.T) {
	e := newBInstallEnv(t)
	e.writeTenants("EMPTY\n")
	r := e.run()
	if r.Code != 2 || !strings.Contains(r.Stderr, "dispatcher.json") {
		t.Fatalf("exit %d, stderr %q; want 2 naming dispatcher.json", r.Code, r.Stderr)
	}
	if bExists(e.Root) {
		t.Errorf("%s was created", e.Root)
	}
}

func TestMissingBinaryExitsTwoBeforeRenderingOrWriting(t *testing.T) {
	manifest := loadManifest(t)
	type breakage struct {
		name  string
		apply func(t *testing.T, binPath string)
	}
	remove := breakage{"absent", func(t *testing.T, p string) {
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}}
	notExec := breakage{"not executable", func(t *testing.T, p string) { bChmod(t, p, 0o644) }}
	isDir := breakage{"a directory", func(t *testing.T, p string) {
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}}

	type scenario struct {
		label   string
		broken  map[string]breakage // installed name -> breakage
		missing []string
	}
	var scenarios []scenario
	for _, b := range manifest {
		scenarios = append(scenarios, scenario{
			label:   b.Name + " absent",
			broken:  map[string]breakage{b.Name: remove},
			missing: []string{b.Name},
		})
	}
	scenarios = append(scenarios,
		scenario{label: "not executable", broken: map[string]breakage{"bcx-broker": notExec}, missing: []string{"bcx-broker"}},
		scenario{label: "a directory", broken: map[string]breakage{"bcx-reap": isDir}, missing: []string{"bcx-reap"}},
		scenario{label: "two at once", broken: map[string]breakage{"bcx-reap": remove, "bcx-appauth": notExec}, missing: []string{"bcx-reap", "bcx-appauth"}},
	)

	for _, sc := range scenarios {
		for _, mode := range [][]string{nil, {"--check"}} {
			e := newBInstallEnv(t)
			for name, br := range sc.broken {
				br.apply(t, filepath.Join(e.BinDir, name))
			}
			r := e.run(mode...)
			if r.Code != 2 {
				t.Errorf("%s %v: exit %d, want 2", sc.label, mode, r.Code)
			}
			for _, name := range sc.missing {
				line := "missing binary: " + filepath.Join(e.BinDir, name) + " (run build.sh)"
				if !strings.Contains(r.Stderr, line) {
					t.Errorf("%s %v: stderr lacks %q\n%s", sc.label, mode, line, r.Stderr)
				}
			}
			if bExists(e.Root) {
				t.Errorf("%s %v: %s was created", sc.label, mode, e.Root)
			}
			if bExists(filepath.Join(e.BinDir, "bcx-gen-tenant-env.ran")) {
				t.Errorf("%s %v: the generator ran before the binary preflight", sc.label, mode)
			}
			if calls := e.ctlCalls(); calls != nil {
				t.Errorf("%s %v: systemctl called: %q", sc.label, mode, calls)
			}
		}
	}
}

func TestInstallReloadsAndRestartsOnlyWhatChangedAndNeverEnablesOrStarts(t *testing.T) {
	e := newBInstallEnv(t)
	const restart = "try-restart bcx-dispatcher.service bcx-broker@*.service nat-ci-firewall.service"

	e.mustInstall()
	if got, want := e.ctlCalls(), []string{"daemon-reload", restart}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first install: systemctl calls %q, want %q", got, want)
	}

	e.mustInstall()
	if got := len(e.ctlCalls()); got != 2 {
		t.Fatalf("no-op install made %d systemctl calls, want none", got-2)
	}

	// Only a unit drifted: reload, no restart.
	bWriteFile(t, e.Root+"/etc/systemd/system/bcx-dispatcher.service", []byte("drift\n"), 0o644)
	e.mustInstall()
	if got, want := e.ctlCalls()[2:], []string{"daemon-reload"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unit drift: systemctl calls %q, want %q", got, want)
	}

	// Only a binary changed: restart, no reload.
	bWriteFile(t, filepath.Join(e.BinDir, "bcx-broker"), []byte(bStubBinary("bcx-broker v2")), 0o755)
	e.mustInstall()
	if got, want := e.ctlCalls()[3:], []string{restart}; !reflect.DeepEqual(got, want) {
		t.Fatalf("binary change: systemctl calls %q, want %q", got, want)
	}

	// Whatever happened, the only verbs ever used are reload and try-restart.
	for _, call := range e.ctlCalls() {
		verb, _, _ := strings.Cut(call, " ")
		if verb != "daemon-reload" && verb != "try-restart" {
			t.Errorf("forbidden systemctl call: %q", call)
		}
	}
}

func TestInstallLeavesSystemctlAloneWithARootUnlessTheSeamIsSet(t *testing.T) {
	e := newBInstallEnv(t)
	onPath := filepath.Join(e.Base, "pathctl")
	log := filepath.Join(e.Base, "pathctl.log")
	writeExec(t, filepath.Join(onPath, "systemctl"), "#!/bin/sh\necho \"$*\" >> '"+log+"'\n")
	env := []string{"SYSTEMCTL=", "PATH=" + onPath + string(os.PathListSeparator) + os.Getenv("PATH")}
	r := runCmd(t, e.Base, env, e.script(), e.baseArgs()...)
	if r.Code != 0 {
		t.Fatalf("install exit %d: %s", r.Code, r.Stderr)
	}
	if bExists(log) {
		t.Errorf("systemctl on PATH was called although --root was given: %s", bReadFile(t, log))
	}
}

func TestInstallReplacesFilesByRenameNotInPlace(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()
	target := e.Root + "/opt/bcx-broker/bcx-broker"
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	v2 := []byte(bStubBinary("bcx-broker v2"))
	bWriteFile(t, filepath.Join(e.BinDir, "bcx-broker"), v2, 0o755)
	e.mustInstall()

	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if os.SameFile(before, after) {
		t.Errorf("binary was rewritten in place; a running binary must be replaced by rename")
	}
	if got := bReadFile(t, target); !bytes.Equal(got, v2) {
		t.Errorf("binary not updated: %q", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(e.Root, "opt", "bcx-broker", "*.new.*"))
	if err != nil || len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v (%v)", leftovers, err)
	}
}

func TestEveryInstalledUnitExecsABinaryInTheManifest(t *testing.T) {
	e := newBInstallEnv(t)
	e.mustInstall()
	allowed := map[string]bool{"broker-firewall.sh": true, "nat-ci-firewall.sh": true}
	for _, b := range loadManifest(t) {
		allowed[b.Name] = true
	}
	units, err := filepath.Glob(filepath.Join(e.Root, "etc", "systemd", "system", "*"))
	if err != nil || len(units) == 0 {
		t.Fatalf("no installed units (%v)", err)
	}
	for _, p := range units {
		for _, exec := range parseUnit(t, p).values("Service", "ExecStart") {
			cmd, _, _ := strings.Cut(exec, " ")
			if dir := path.Dir(cmd); dir != bInstallDir {
				t.Errorf("%s: ExecStart %q is outside %s", filepath.Base(p), cmd, bInstallDir)
			}
			if !allowed[path.Base(cmd)] {
				t.Errorf("%s: ExecStart basename %q is not in binaries.txt", filepath.Base(p), path.Base(cmd))
			}
			info, err := os.Stat(e.Root + cmd)
			if err != nil || info.Mode().Perm()&0o111 == 0 {
				t.Errorf("%s: ExecStart target %s is not installed as an executable (%v)", filepath.Base(p), cmd, err)
			}
		}
	}
}

func TestHelpPrintsOnlyTheHeaderComment(t *testing.T) {
	e := newBInstallEnv(t)
	r := runCmd(t, e.Base, nil, e.script(), "-h")
	if r.Code != 0 {
		t.Fatalf("-h exit %d: %s", r.Code, r.Stderr)
	}
	for _, want := range []string{"install.sh", "--check", "--tenants", "--root", "--bin-dir", "would change:"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("-h output lacks %q", want)
		}
	}
	for _, code := range []string{"set -euo", "mktemp", "HERE=", "#!"} {
		if strings.Contains(r.Stdout, code) {
			t.Errorf("-h output leaks script body: %q", code)
		}
	}
	if bExists(e.Root) {
		t.Errorf("-h created %s", e.Root)
	}
}

func TestBadArgumentsExitTwoAndTouchNothing(t *testing.T) {
	for _, bad := range [][]string{{"--bogus"}, {"--tenants"}, {"--root"}, {"--bin-dir"}} {
		e := newBInstallEnv(t)
		r := e.run(bad...)
		if r.Code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, r.Code)
		}
		if bExists(e.Root) || e.ctlCalls() != nil {
			t.Errorf("%v: touched the system", bad)
		}
	}
	// An empty value must not silently mean "the real /".
	e := newBInstallEnv(t)
	r := runCmd(t, e.Base, []string{"SYSTEMCTL=" + e.Ctl}, e.script(), "--tenants", e.Tenants, "--bin-dir", e.BinDir, "--root", "")
	if r.Code != 2 || e.ctlCalls() != nil {
		t.Errorf("--root \"\": exit %d, want 2 with no systemctl call", r.Code)
	}
}

// bTree copies the files install.sh reads into a script directory of its own, so
// the HERE-relative defaults and the source preflight can be exercised. skip names
// one file to leave out.
func (e bInstallEnv) bTree(skip string) string {
	e.t.Helper()
	repo := repoRoot(e.t)
	tree := filepath.Join(e.Base, "tree")
	names := append([]string{"install.sh", "binaries.txt", "tiers.yml", "broker-firewall.sh", "nat-ci-firewall.sh"}, bShippedUnits...)
	for _, name := range names {
		if name == skip {
			continue
		}
		info, err := os.Stat(filepath.Join(repo, name))
		if err != nil {
			e.t.Fatalf("stat %s: %v", name, err)
		}
		if err := os.MkdirAll(tree, 0o755); err != nil {
			e.t.Fatalf("mkdir: %v", err)
		}
		bWriteFile(e.t, filepath.Join(tree, name), bReadFile(e.t, filepath.Join(repo, name)), info.Mode().Perm())
	}
	return tree
}

func TestInstallDefaultsBinDirToBinNextToTheScript(t *testing.T) {
	e := newBInstallEnv(t)
	tree := e.bTree("")
	if err := os.Rename(e.BinDir, filepath.Join(tree, "bin")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	r := runCmd(t, e.Base, []string{"SYSTEMCTL=" + e.Ctl}, filepath.Join(tree, "install.sh"), "--tenants", e.Tenants, "--root", e.Root)
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stderr)
	}
	if !bExists(e.Root + "/opt/bcx-broker/bcx-dispatcher") {
		t.Errorf("binary from <script dir>/bin was not installed")
	}
}

func TestMissingSourceFileExitsTwoBeforeWriting(t *testing.T) {
	e := newBInstallEnv(t)
	tree := e.bTree("bcx-broker@.service")
	r := runCmd(t, e.Base, []string{"SYSTEMCTL=" + e.Ctl}, filepath.Join(tree, "install.sh"), e.baseArgs()...)
	if r.Code != 2 || !strings.Contains(r.Stderr, "missing source: "+filepath.Join(tree, "bcx-broker@.service")) {
		t.Fatalf("exit %d, stderr %q; want 2 naming the missing unit", r.Code, r.Stderr)
	}
	if bExists(e.Root) {
		t.Errorf("%s was created", e.Root)
	}
}

// macOS ships bash 3.2 as /bin/bash; the scripts must run there, not just under
// the newer bash that PATH resolves to.
func TestInstallWorksUnderSystemBash(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	e := newBInstallEnv(t)
	version := runCmd(t, e.Base, nil, "/bin/bash", "--version")
	t.Logf("/bin/bash: %s", strings.SplitN(version.Stdout, "\n", 2)[0])

	run := func(extra ...string) result {
		return runCmd(t, e.Base, []string{"SYSTEMCTL=" + e.Ctl}, "/bin/bash", append([]string{e.script()}, append(e.baseArgs(), extra...)...)...)
	}
	if r := run(); r.Code != 0 {
		t.Fatalf("install: exit %d\n%s", r.Code, r.Stderr)
	}
	if r := run("--check"); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("--check: exit %d, stdout %q, stderr %q", r.Code, r.Stdout, r.Stderr)
	}
	if r := run(); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("second install: exit %d, stdout %q", r.Code, r.Stdout)
	}
	bChmod(t, e.Root+"/opt/bcx-broker/bcx-reap", 0o700)
	if r := run("--check"); r.Code != 1 || !strings.Contains(r.Stdout, "would change: /opt/bcx-broker/bcx-reap") {
		t.Fatalf("--check drift: exit %d, stdout %q", r.Code, r.Stdout)
	}
}

func TestScriptsAreExecutableAndBinIsIgnored(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"install.sh", "build.sh"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
	}
	if got, want := string(bReadFile(t, filepath.Join(root, "bin", ".gitignore"))), "*\n!.gitignore\n"; got != want {
		t.Errorf("bin/.gitignore = %q, want %q", got, want)
	}
}

// bBuildEnv is a copy of build.sh and binaries.txt in a temp tree whose `go` is a
// recording stub, so the command line build.sh composes is observable without
// compiling anything.
type bBuildEnv struct {
	t      *testing.T
	Base   string
	Src    string
	GoLog  string
	StubGo string
}

// bGoCall is one recorded `go` invocation.
type bGoCall struct {
	Dir, CGO, GOOS, GOARCH string
	Args                   []string
}

func newBBuildEnv(t *testing.T, withPackages ...string) bBuildEnv {
	t.Helper()
	base := shortTmp(t)
	e := bBuildEnv{
		t: t, Base: base,
		Src:    filepath.Join(base, "src"),
		GoLog:  filepath.Join(base, "go.log"),
		StubGo: filepath.Join(base, "stub"),
	}
	repo := repoRoot(t)
	script, err := os.Stat(filepath.Join(repo, "build.sh"))
	if err != nil {
		t.Fatalf("stat build.sh: %v", err)
	}
	if err := os.MkdirAll(e.Src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bWriteFile(t, filepath.Join(e.Src, "build.sh"), bReadFile(t, filepath.Join(repo, "build.sh")), script.Mode().Perm())
	bWriteFile(t, filepath.Join(e.Src, "binaries.txt"), bReadFile(t, filepath.Join(repo, "binaries.txt")), 0o644)
	for _, b := range loadManifest(t) {
		for _, want := range withPackages {
			if b.Name == want {
				if err := os.MkdirAll(filepath.Join(e.Src, b.Package), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
		}
	}
	writeExec(t, filepath.Join(e.StubGo, "go"), `#!/bin/sh
{
  printf '%s\t%s\t%s\t%s' "$PWD" "${CGO_ENABLED-}" "${GOOS-}" "${GOARCH-}"
  for a in "$@"; do printf '\t%s' "$a"; done
  printf '\n'
} >> '`+e.GoLog+`'
exit "${STUB_GO_EXIT:-0}"
`)
	return e
}

func (e bBuildEnv) run(env []string, args ...string) result {
	e.t.Helper()
	path := "PATH=" + e.StubGo + string(os.PathListSeparator) + os.Getenv("PATH")
	return runCmd(e.t, e.Base, append([]string{path}, env...), filepath.Join(e.Src, "build.sh"), args...)
}

func (e bBuildEnv) calls() []bGoCall {
	e.t.Helper()
	raw, err := os.ReadFile(e.GoLog)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatalf("read go log: %v", err)
	}
	var out []bGoCall
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			e.t.Fatalf("malformed go log line %q", line)
		}
		out = append(out, bGoCall{Dir: f[0], CGO: f[1], GOOS: f[2], GOARCH: f[3], Args: f[4:]})
	}
	return out
}

func bAllNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, b := range loadManifest(t) {
		out = append(out, b.Name)
	}
	return out
}

func TestBuildRunsOneStaticCrossBuildPerManifestLine(t *testing.T) {
	e := newBBuildEnv(t, bAllNames(t)...)
	if r := e.run(nil); r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stderr)
	}
	calls := e.calls()
	manifest := loadManifest(t)
	if len(calls) != len(manifest) {
		t.Fatalf("%d go calls, want %d: %+v", len(calls), len(manifest), calls)
	}
	wantDir, err := filepath.EvalSymlinks(e.Src)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	for i, b := range manifest {
		got := calls[i]
		if dir, err := filepath.EvalSymlinks(got.Dir); err != nil || dir != wantDir {
			t.Errorf("%s: go ran in %q, want the script directory %q", b.Name, got.Dir, wantDir)
		}
		if got.CGO != "0" || got.GOOS != "linux" || got.GOARCH != "amd64" {
			t.Errorf("%s: env CGO_ENABLED=%q GOOS=%q GOARCH=%q", b.Name, got.CGO, got.GOOS, got.GOARCH)
		}
		want := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", "bin/" + b.Name, b.Package}
		if !reflect.DeepEqual(got.Args, want) {
			t.Errorf("%s: go args\n got %q\nwant %q", b.Name, got.Args, want)
		}
	}
}

func TestBuildOnlyBuildsOneRoleAndHonoursTargetOverrides(t *testing.T) {
	e := newBBuildEnv(t, bAllNames(t)...)
	r := e.run([]string{"BCX_GOOS=darwin", "BCX_GOARCH=arm64"}, "--only", "dispatcher")
	if r.Code != 0 {
		t.Fatalf("exit %d: %s", r.Code, r.Stderr)
	}
	calls := e.calls()
	if len(calls) != 1 {
		t.Fatalf("%d go calls, want 1: %+v", len(calls), calls)
	}
	if calls[0].GOOS != "darwin" || calls[0].GOARCH != "arm64" || calls[0].CGO != "0" {
		t.Errorf("env CGO_ENABLED=%q GOOS=%q GOARCH=%q", calls[0].CGO, calls[0].GOOS, calls[0].GOARCH)
	}
	if got := calls[0].Args[len(calls[0].Args)-2:]; !reflect.DeepEqual(got, []string{"bin/bcx-dispatcher", "./cmd/bcx-dispatcher"}) {
		t.Errorf("tail of go args = %q", got)
	}
}

func TestBuildNamesEveryMissingPackageDirAndBuildsNothing(t *testing.T) {
	e := newBBuildEnv(t, "bcx-dispatcher")
	r := e.run(nil)
	if r.Code != 2 {
		t.Errorf("exit %d, want 2", r.Code)
	}
	for _, b := range loadManifest(t) {
		if b.Name == "bcx-dispatcher" {
			continue
		}
		if !strings.Contains(r.Stderr, "package directory not found: "+b.Package) {
			t.Errorf("stderr does not name %s:\n%s", b.Package, r.Stderr)
		}
	}
	if calls := e.calls(); calls != nil {
		t.Errorf("go ran although packages are missing: %+v", calls)
	}

	// The one role whose package exists still builds on its own.
	if r := e.run(nil, "--only", "dispatcher"); r.Code != 0 || len(e.calls()) != 1 {
		t.Errorf("--only dispatcher: exit %d, %d calls: %s", r.Code, len(e.calls()), r.Stderr)
	}
	// And a role whose package is missing fails the same way when asked for alone.
	if r := e.run(nil, "--only", "broker"); r.Code != 2 || !strings.Contains(r.Stderr, "package directory not found: ./cmd/bcx-broker") {
		t.Errorf("--only broker: exit %d: %s", r.Code, r.Stderr)
	}
}

func TestBuildRejectsBadArguments(t *testing.T) {
	for _, bad := range [][]string{{"--only", "nope"}, {"--only"}, {"--bogus"}} {
		e := newBBuildEnv(t, bAllNames(t)...)
		r := e.run(nil, bad...)
		if r.Code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, r.Code)
		}
		if calls := e.calls(); calls != nil {
			t.Errorf("%v: go ran: %+v", bad, calls)
		}
	}
}

func TestBuildStopsAtTheFirstFailedGoBuild(t *testing.T) {
	e := newBBuildEnv(t, bAllNames(t)...)
	r := e.run([]string{"STUB_GO_EXIT=7"})
	if r.Code == 0 {
		t.Fatalf("build.sh reported success although go failed")
	}
	if n := len(e.calls()); n != 1 {
		t.Errorf("%d go calls after the first failure, want 1", n)
	}
}
