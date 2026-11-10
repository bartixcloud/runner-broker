package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// orgEvents is one ordered log shared by every fake, so a test can assert the
// order across processes, the detection and the workflow dispatch.
type orgEvents struct {
	mu  sync.Mutex
	log []string
}

func (e *orgEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *orgEvents) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// fakeOutput is an OutputRunner keyed by the program's base name.
type fakeOutput struct {
	events   *orgEvents
	mu       sync.Mutex
	calls    []Command
	deadline map[string]time.Duration // base name -> time left on the context at call time
	exit     map[string]int           // base name -> exit code (0 when absent)
	stdout   map[string]string
	trunc    map[string]bool
	onCall   func(Command)
	// gate, when set, holds every call until it is closed or the context ends.
	gate chan struct{}
}

func (f *fakeOutput) RunOutput(ctx context.Context, cmd Command) (CommandOutput, error) {
	name := filepath.Base(cmd.Argv[0])
	label := name
	switch name {
	case "sichern.sh", "restic-sichern.sh":
		label += " " + cmd.Argv[len(cmd.Argv)-1]
	case "git":
		label = "git-diff"
	}
	f.events.add(label)
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	if dl, ok := ctx.Deadline(); ok {
		if f.deadline == nil {
			f.deadline = map[string]time.Duration{}
		}
		f.deadline[name] = time.Until(dl)
	}
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(cmd)
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return CommandOutput{ExitCode: -1}, ctx.Err()
		}
	}
	out := CommandOutput{Stdout: f.stdout[name], Truncated: f.trunc[name]}
	if code := f.exit[name]; code != 0 {
		out.ExitCode = code
		return out, errors.New("exit status " + strconv.Itoa(code))
	}
	return out, nil
}

func (f *fakeOutput) commands(name string) []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Command
	for _, c := range f.calls {
		if filepath.Base(c.Argv[0]) == name {
			out = append(out, c)
		}
	}
	return out
}

type fakeWorkflows struct {
	events *orgEvents
	err    error
	boom   bool
	mu     sync.Mutex
	reqs   []WorkflowRequest
}

func (f *fakeWorkflows) Dispatch(_ context.Context, req WorkflowRequest) error {
	f.events.add("dispatch " + req.Workflow + " " + req.Repo)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.boom {
		panic("workflow dispatcher exploded")
	}
	return f.err
}

type orgRig struct {
	t      *testing.T
	table  Table
	runner *OrgRunner
	out    *fakeOutput
	wf     *fakeWorkflows
	events *orgEvents
	logs   *syncBuffer
}

func newOrgRig(t *testing.T) *orgRig {
	t.Helper()
	dir := t.TempDir()
	table := orgTestTable()
	table.Org.SicherungDir = "/opt/sicherung"
	table.Org.SicherungWurzel = filepath.Join(dir, "wurzel")
	table.Org.BefundDatei = filepath.Join(dir, "var", "lib", "befunde.jsonl") // parent does not exist yet
	events := &orgEvents{}
	log, logs := testLogger()
	r := &orgRig{t: t, table: table, events: events, logs: logs,
		out: &fakeOutput{events: events}, wf: &fakeWorkflows{events: events}}
	r.runner = &OrgRunner{Cfg: *table.Org, Cmd: r.out, Workflows: r.wf, Log: log}
	return r
}

func (r *orgRig) befundPath() string { return r.table.Org.BefundDatei }

// clone makes <wurzel>/<repo>.git exist, as sichern.sh would.
func (r *orgRig) clone(repo string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.table.Org.SicherungWurzel, repo+".git"), 0o750); err != nil {
		r.t.Fatal(err)
	}
}

// handle routes body like the server does and runs the resulting plan.
func (r *orgRig) handle(event string, body []byte) Decision {
	r.t.Helper()
	p, err := ParsePayload(body)
	if err != nil {
		r.t.Fatalf("ParsePayload: %v", err)
	}
	dec := RouteOrg(event, body, p, r.table)
	if dec.Action == ActionOrg {
		r.runner.Run(r.t.Context(), dec.Org)
	}
	return dec
}

// fixture loads a shared Python fixture and adds the installation the router needs.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(readTestdata(t, name), &obj); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	obj["installation"] = json.RawMessage(`{"id":5}`)
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func withFields(t *testing.T, body []byte, fields map[string]string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		obj[k] = json.RawMessage(v)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *orgRig) befunde() []Befund {
	r.t.Helper()
	data, err := os.ReadFile(r.befundPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	var out []Befund
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var b Befund
		if err := json.Unmarshal([]byte(line), &b); err != nil {
			r.t.Fatalf("befund line %q: %v", line, err)
		}
		out = append(out, b)
	}
	return out
}

func arten(bs []Befund) []Art {
	var out []Art
	for _, b := range bs {
		out = append(out, b.Art)
	}
	return out
}

// ------------------------------------------------------------------ order ----

func TestOrgRunOrderIsBackupThenDetectionThenResticThenMirror(t *testing.T) {
	r := newOrgRig(t)
	r.clone("demo")
	// A forced push to the default branch: critical finding, and a mirror event.
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.out.onCall = func(c Command) {
		if filepath.Base(c.Argv[0]) == "sichern.sh" {
			if _, err := os.Stat(r.befundPath()); err == nil {
				t.Error("the befund file existed before the backup ran: detection ran first")
			}
		}
	}
	r.handle("push", body)
	want := []string{"sichern.sh demo", "git-diff", "restic-sichern.sh demo", "dispatch fakten.yml demo"}
	if got := r.events.all(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if got := arten(r.befunde()); !slices.Equal(got, []Art{ArtForcePush}) {
		t.Errorf("befunde = %v", got)
	}
}

func TestOrgRunScriptsAreExecutedAsArgvWithTheCloneRootInTheEnvironment(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", fixture(t, "push_force.json"))
	sichern := r.out.commands("sichern.sh")
	restic := r.out.commands("restic-sichern.sh")
	if len(sichern) != 1 || len(restic) != 1 {
		t.Fatalf("sichern %d, restic %d calls", len(sichern), len(restic))
	}
	if !slices.Equal(sichern[0].Argv, []string{"/opt/sicherung/sichern.sh", "demo"}) ||
		!slices.Equal(restic[0].Argv, []string{"/opt/sicherung/restic-sichern.sh", "demo"}) {
		t.Errorf("argv = %v / %v", sichern[0].Argv, restic[0].Argv)
	}
	wantEnv := []string{"SICHERUNG_WURZEL=" + r.table.Org.SicherungWurzel}
	if !slices.Equal(sichern[0].Env, wantEnv) || !slices.Equal(restic[0].Env, wantEnv) {
		t.Errorf("env = %v / %v, want %v", sichern[0].Env, restic[0].Env, wantEnv)
	}
}

func TestOrgRunTimeoutsPerCallAreTheConfiguredOnes(t *testing.T) {
	r := newOrgRig(t)
	r.runner.Cfg.SicherungTimeout, r.runner.Cfg.ResticTimeout = 7*time.Minute, 5*time.Hour
	r.handle("push", fixture(t, "push_force.json"))
	for name, want := range map[string]time.Duration{"sichern.sh": 7 * time.Minute, "restic-sichern.sh": 5 * time.Hour} {
		left := r.out.deadline[name]
		if left <= want-time.Minute || left > want {
			t.Errorf("%s ran with %v left, want about %v", name, left, want)
		}
	}
}

func TestOrgRunNormalPushOnlyBacksUp(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", fixture(t, "push_normal.json"))
	if got := r.events.all(); !slices.Equal(got, []string{"sichern.sh demo"}) {
		t.Errorf("events = %v", got)
	}
	if _, err := os.Stat(r.befundPath()); err == nil {
		t.Error("a harmless push must not create the befund file")
	}
}

func TestOrgRunWarningAloneDoesNotTriggerRestic(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", fixture(t, "push_abgeschnitten.json"))
	if got := r.events.all(); !slices.Equal(got, []string{"sichern.sh demo"}) {
		t.Errorf("events = %v", got)
	}
	if got := arten(r.befunde()); !slices.Equal(got, []Art{ArtUnpruefbar}) {
		t.Errorf("befunde = %v", got)
	}
}

func TestOrgRunDeleteEventBacksUpAndSnapshots(t *testing.T) {
	r := newOrgRig(t)
	r.handle("delete", fixture(t, "delete_tag.json"))
	if got := r.events.all(); !slices.Equal(got, []string{"sichern.sh demo", "restic-sichern.sh demo"}) {
		t.Errorf("events = %v", got)
	}
	bs := r.befunde()
	if len(bs) != 1 || bs[0].Art != ArtRefGeloescht || bs[0].Ref == nil || *bs[0].Ref != "refs/tags/v1.0.0" {
		t.Errorf("befunde = %+v", bs)
	}
}

// -------------------------------------------------------------- repository ----

func TestOrgRunRepositoryEventsNeverBackUp(t *testing.T) {
	cases := []struct {
		fixture string
		want    []string
		arten   []Art
	}{
		{"repository_deleted.json", []string{"restic-sichern.sh demo", "dispatch org.yml "}, []Art{ArtRepoGeloescht}},
		{"repository_renamed.json", []string{"restic-sichern.sh demo", "dispatch fakten.yml demo"}, []Art{ArtRepoUmbenannt}},
		{"repository_transferred.json", []string{"restic-sichern.sh demo", "dispatch fakten.yml demo"}, []Art{ArtRepoUebertragen}},
		{"repository_publicized.json", []string{"restic-sichern.sh demo", "dispatch fakten.yml demo"}, []Art{ArtRepoOeffentlich}},
		{"repository_archived.json", []string{"dispatch fakten.yml demo"}, nil},
		{"repository_privatized.json", []string{"dispatch fakten.yml demo"}, nil},
		{"repository_created.json", []string{"dispatch fakten.yml demo"}, nil},
		{"repository_edited.json", []string{"dispatch fakten.yml demo"}, nil},
		{"repository_unarchived.json", []string{"dispatch fakten.yml demo"}, nil},
		{"repository_frobnicated.json", nil, []Art{ArtUnpruefbar}},
		{"repository_ohne_action.json", nil, []Art{ArtUnpruefbar}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			r := newOrgRig(t)
			r.handle("repository", fixture(t, tc.fixture))
			if got := r.events.all(); !slices.Equal(got, tc.want) {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
			if got := arten(r.befunde()); !slices.Equal(got, tc.arten) {
				t.Errorf("befunde = %v, want %v", got, tc.arten)
			}
			if n := len(r.out.commands("sichern.sh")); n != 0 {
				t.Errorf("sichern.sh ran %d times for a repository event", n)
			}
		})
	}
}

// -------------------------------------------------------- failure handling ----

func TestOrgRunFailedBackupIsAFindingAndTheRestRunsAnyway(t *testing.T) {
	r := newOrgRig(t)
	r.out.exit = map[string]int{"sichern.sh": 1}
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.handle("push", body)
	want := []string{"sichern.sh demo", "restic-sichern.sh demo", "dispatch fakten.yml demo"}
	if got := r.events.all(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v (no diffstat after a failed backup)", got, want)
	}
	bs := r.befunde()
	if got := arten(bs); !slices.Equal(got, []Art{ArtForcePush, ArtUnpruefbar}) {
		t.Fatalf("befunde = %v", got)
	}
	if bs[1].Grund != "Sicherung fehlgeschlagen (sichern.sh Exit 1)" || bs[1].Schwere != SchwereWarnung ||
		bs[1].Ref == nil || *bs[1].Ref != "refs/heads/main" {
		t.Errorf("failure befund = %+v", bs[1])
	}
}

func TestOrgRunFailedResticIsAFindingAndTheMirrorStillRuns(t *testing.T) {
	r := newOrgRig(t)
	r.out.exit = map[string]int{"restic-sichern.sh": 5}
	body := withFields(t, fixture(t, "push_force.json"), map[string]string{
		"repository": `{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"}`})
	r.handle("push", body)
	if got := r.events.all(); got[len(got)-1] != "dispatch fakten.yml demo" {
		t.Errorf("the mirror did not run last: %v", got)
	}
	bs := r.befunde()
	if got := arten(bs); !slices.Equal(got, []Art{ArtForcePush, ArtUnpruefbar}) {
		t.Fatalf("befunde = %v", got)
	}
	if bs[1].Grund != "restic-Snapshot fehlgeschlagen (restic-sichern.sh Exit 5)" {
		t.Errorf("grund = %q", bs[1].Grund)
	}
}

func TestOrgRunStartFailureOfAScriptIsReportedWithoutAnExitCode(t *testing.T) {
	r := newOrgRig(t)
	r.runner.Cmd = errorRunner{err: errors.New("fork/exec: no such file")}
	r.handle("push", fixture(t, "push_normal.json"))
	bs := r.befunde()
	if len(bs) != 1 || bs[0].Art != ArtUnpruefbar || !strings.Contains(bs[0].Grund, "Sicherung fehlgeschlagen") {
		t.Errorf("befunde = %+v", bs)
	}
}

type errorRunner struct{ err error }

func (e errorRunner) RunOutput(context.Context, Command) (CommandOutput, error) {
	return CommandOutput{ExitCode: -1}, e.err
}

func TestOrgRunMirrorFailureIsLoggedWithoutStoppingAnything(t *testing.T) {
	r := newOrgRig(t)
	r.wf.err = errors.New("dispatch fakten.yml: status 422: boom")
	body := orgBody("demo", `"action":"opened"`)
	r.handle("pull_request", body)
	if !strings.Contains(r.logs.String(), "level=ERROR") || !strings.Contains(r.logs.String(), "status 422") {
		t.Errorf("a failed dispatch must be logged:\n%s", r.logs.String())
	}
}

func TestOrgRunSurvivesAPanickingStep(t *testing.T) {
	r := newOrgRig(t)
	r.wf.boom = true
	r.handle("pull_request", orgBody("demo", `"action":"opened"`))
	if !strings.Contains(r.logs.String(), "panic") {
		t.Errorf("a panic must be logged:\n%s", r.logs.String())
	}
}

func TestOrgRunStopsWorkAfterContextCancelButStillRecordsTheBackupFailure(t *testing.T) {
	r := newOrgRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	r.out.onCall = func(Command) { cancel() }
	r.out.exit = map[string]int{"sichern.sh": 1}
	body := fixture(t, "push_normal.json")
	p, _ := ParsePayload(body)
	r.runner.Run(ctx, RouteOrg("push", body, p, r.table).Org)
	if bs := r.befunde(); len(bs) != 1 || bs[0].Art != ArtUnpruefbar {
		t.Errorf("befunde = %+v: the failure must be recorded even though the context ended", bs)
	}
}

// ----------------------------------------------------------------- diffstat ----

func TestOrgRunMeasuresTheDiffstatInTheBackupClone(t *testing.T) {
	r := newOrgRig(t)
	r.clone("demo")
	var numstat strings.Builder
	for i := range 60 {
		numstat.WriteString("0\t3\tdatei" + string(rune('a'+i%26)) + ".txt\n")
	}
	for range 60 {
		numstat.WriteString(" delete mode 100644 x.txt\n")
	}
	r.out.stdout = map[string]string{"git": numstat.String()}
	r.handle("push", fixture(t, "push_normal.json"))
	git := r.out.commands("git")
	if len(git) != 1 {
		t.Fatalf("git ran %d times", len(git))
	}
	want := []string{"git", "-C", filepath.Join(r.table.Org.SicherungWurzel, "demo.git"), "diff", "--numstat", "--summary",
		"--no-renames", strings.Repeat("1", 40), strings.Repeat("2", 40), "--"}
	if !slices.Equal(git[0].Argv, want) {
		t.Errorf("argv = %v, want %v", git[0].Argv, want)
	}
	if git[0].MaxStdout <= defaultMaxOutput {
		t.Errorf("MaxStdout = %d: the diffstat needs a larger cap than log output", git[0].MaxStdout)
	}
	bs := r.befunde()
	if len(bs) != 1 || bs[0].Art != ArtMassenaenderung || !strings.Contains(bs[0].Grund, "60 Dateien und 180 Zeilen") {
		t.Errorf("befunde = %+v", bs)
	}
	if n := len(r.out.commands("restic-sichern.sh")); n != 0 {
		t.Error("a warning must not trigger restic")
	}
}

func TestParseDiffstat(t *testing.T) {
	cases := []struct {
		name, in string
		want     DiffStat
	}{
		{"empty", "", DiffStat{}},
		{"numstat", "3\t2\ta.txt\n0\t10\tb.txt\n", DiffStat{0, 12}},
		{"binary counts as zero lines", "-\t-\timg.png\n", DiffStat{}},
		{"deleted files", " delete mode 100644 a\n delete mode 100755 b\n create mode 100644 c\n", DiffStat{2, 0}},
		{"mixed", "1\t5\ta\n0\t0\tb\n delete mode 100644 b\n", DiffStat{1, 5}},
		{"path with spaces", "2\t1\tdir/a file.txt\n", DiffStat{0, 1}},
		{"garbage lines are ignored", "warning: x\n\n1\tx\ty\n", DiffStat{}},
		{"crlf", "2\t2\ta\r\n", DiffStat{0, 2}},
	}
	for _, tc := range cases {
		if got := parseDiffstat(tc.in); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestOrgRunDiffstatIsSkippedWhenNotMeasurable(t *testing.T) {
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name   string
		fields map[string]string
		clone  bool
		fail   bool
	}{
		{"no clone", nil, false, false},
		{"new branch (null before)", map[string]string{"before": `"` + zero + `"`}, true, false},
		{"deleted ref (null after)", map[string]string{"after": `"` + zero + `"`}, true, false},
		{"not hex", map[string]string{"before": `"--output=/etc/x"`}, true, false},
		{"upper-case hex", map[string]string{"after": `"` + strings.Repeat("A", 40) + `"`}, true, false},
		{"short hex", map[string]string{"after": `"abc123"`}, true, false},
		{"wrong type", map[string]string{"before": `5`}, true, false},
		{"missing", map[string]string{"before": `null`}, true, false},
		{"git fails", nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newOrgRig(t)
			if tc.clone {
				r.clone("demo")
			}
			if tc.fail {
				r.out.exit = map[string]int{"git": 128}
			}
			r.handle("push", withFields(t, fixture(t, "push_normal.json"), tc.fields))
			if tc.clone && !tc.fail && len(r.out.commands("git")) != 0 {
				t.Errorf("git diff ran with argv %v", r.out.commands("git")[0].Argv)
			}
			if bs := r.befunde(); len(bs) != 0 {
				t.Errorf("befunde = %+v, want none (commit list is complete)", bs)
			}
		})
	}
}

func TestOrgRunDiffstatSha256ObjectIdsAreAccepted(t *testing.T) {
	r := newOrgRig(t)
	r.clone("demo")
	r.handle("push", withFields(t, fixture(t, "push_normal.json"), map[string]string{
		"before": `"` + strings.Repeat("a", 64) + `"`, "after": `"` + strings.Repeat("b", 64) + `"`}))
	if len(r.out.commands("git")) != 1 {
		t.Error("a 64-hex object id (sha256 repo) must be measured")
	}
}

func TestOrgRunOversizedDiffstatIsAFindingNotSilence(t *testing.T) {
	r := newOrgRig(t)
	r.clone("demo")
	r.out.trunc = map[string]bool{"git": true}
	r.handle("push", fixture(t, "push_normal.json"))
	bs := r.befunde()
	if len(bs) != 1 || bs[0].Art != ArtUnpruefbar || !strings.Contains(bs[0].Grund, "Diffstat") {
		t.Errorf("befunde = %+v", bs)
	}
}

// ------------------------------------------------------------ names, file ----

func TestOrgRunInvalidNameRunsNothingAndRecordsAFinding(t *testing.T) {
	r := newOrgRig(t)
	body := []byte(`{"installation":{"id":5},"repository":{"name":"-rf","full_name":"ekd-akd/x","default_branch":"main"},"ref":"refs/heads/main","forced":true}`)
	r.handle("push", body)
	if got := r.events.all(); len(got) != 0 {
		t.Errorf("events = %v, want nothing to run", got)
	}
	bs := r.befunde()
	if len(bs) != 1 || bs[0].Art != ArtUnpruefbar || bs[0].Repo != "unbekannt" || bs[0].Ref != nil || bs[0].Schwere != SchwereWarnung {
		t.Fatalf("befunde = %+v", bs)
	}
	if strings.Contains(bs[0].Grund, "-rf") {
		t.Errorf("the rejected name was echoed into the finding: %q", bs[0].Grund)
	}
}

func TestOrgRunBrokenPayloadIsReportedByTheDetection(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", []byte(`{"installation":{"id":5},"ref":"refs/heads/main"}`))
	if got := r.events.all(); len(got) != 0 {
		t.Errorf("events = %v", got)
	}
	if bs := r.befunde(); len(bs) != 1 || bs[0].Art != ArtUnpruefbar || bs[0].Repo != "unbekannt" {
		t.Errorf("befunde = %+v", bs)
	}
}

func TestOrgRunAppendsToTheBefundFileAndCreatesItsDirectory(t *testing.T) {
	r := newOrgRig(t)
	if err := os.MkdirAll(filepath.Dir(r.befundPath()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.befundPath(), []byte("{\"vorhanden\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.handle("push", fixture(t, "push_force.json"))
	r.handle("push", fixture(t, "push_force_und_loeschung.json"))
	data, err := os.ReadFile(r.befundPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 4 || lines[0] != `{"vorhanden":true}` {
		t.Fatalf("file = %q", data)
	}
	if !strings.HasPrefix(lines[1], `{"art":"force_push","repo":"demo","ref":"refs/heads/main","schwere":"kritisch"`) {
		t.Errorf("line = %s", lines[1])
	}
}

func TestOrgRunCreatesTheBefundDirectoryWhenMissing(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", fixture(t, "push_force.json"))
	if got := arten(r.befunde()); len(got) != 1 {
		t.Errorf("befunde = %v", got)
	}
}

func TestOrgRunScriptStdoutIsClippedInTheLog(t *testing.T) {
	r := newOrgRig(t)
	r.out.stdout = map[string]string{"sichern.sh": strings.Repeat("Z", 5000)}
	r.handle("push", fixture(t, "push_normal.json"))
	logs := r.logs.String()
	if !strings.Contains(logs, "ZZZZ") || strings.Contains(logs, strings.Repeat("Z", 500)) {
		t.Errorf("stdout must be logged but clipped (%d log bytes)", len(logs))
	}
}

func TestOrgRunMirrorEventsRunNoProcesses(t *testing.T) {
	r := newOrgRig(t)
	r.handle("pull_request", orgBody("demo", `"action":"synchronize"`))
	r.handle("release", orgBody("demo", `"action":"published"`))
	if got := r.events.all(); !slices.Equal(got, []string{"dispatch fakten.yml demo", "dispatch fakten.yml demo"}) {
		t.Errorf("events = %v", got)
	}
	if len(r.out.calls) != 0 {
		t.Errorf("processes ran: %v", r.out.calls)
	}
}

func TestOrgRunMirrorOwnEventsDispatchNothing(t *testing.T) {
	r := newOrgRig(t)
	r.handle("push", withFields(t, orgBody("spiegel", `"ref":"refs/heads/main","deleted":false`), nil))
	r.handle("workflow_run", orgBody("spiegel", `"action":"completed"`))
	r.handle("pull_request", orgBody("spiegel", `"action":"opened"`))
	for _, e := range r.events.all() {
		if strings.HasPrefix(e, "dispatch") {
			t.Errorf("the mirror's own event dispatched: %s", e)
		}
	}
	// Backup still runs for the mirror repo.
	if got := r.events.all(); !slices.Equal(got, []string{"sichern.sh spiegel"}) {
		t.Errorf("events = %v", got)
	}
}
