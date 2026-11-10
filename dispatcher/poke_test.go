package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var pokeTail = []string{"--", "systemctl", "start", "nuk-converge.service"}

func wantPokeArgv(project, vm string) []string {
	return slices.Concat([]string{"incus", "exec", "--project", project, vm}, pokeTail)
}

// sortedArgvs returns the recorded argvs sorted, because targets run in parallel and
// therefore record in any order.
func sortedArgvs(run *fakeRunner) [][]string {
	got := run.argvs()
	slices.SortFunc(got, slices.Compare)
	return got
}

func TestPokeRunsTheContractCommandAndSurvivesFailures(t *testing.T) {
	log, out := testLogger()
	run := &fakeRunner{failVM: map[string]string{"ingress": "Error: Instance is not running"}}

	Poke(context.Background(), run, log, time.Second,
		[]PokeTarget{{"platform", "ingress"}, {"alpha-org", "alpha-org-app"}}, nil, nil)

	want := [][]string{wantPokeArgv("platform", "ingress"), wantPokeArgv("alpha-org", "alpha-org-app")}
	slices.SortFunc(want, slices.Compare)
	if got := sortedArgvs(run); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	logged := out.String()
	for _, want := range []string{
		"poke platform/ingress failed", "Instance is not running", "poked alpha-org/alpha-org-app",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "poked platform/ingress") {
		t.Errorf("a failed poke was logged as success:\n%s", logged)
	}
}

func TestPokeWithNoTargetsDoesNothing(t *testing.T) {
	log, out := testLogger()
	run := &fakeRunner{}
	Poke(context.Background(), run, log, time.Second, nil, nil, nil)
	if got := run.argvs(); len(got) != 0 {
		t.Errorf("ran %q for no targets", got)
	}
	if out.String() != "" {
		t.Errorf("unexpected log output: %s", out.String())
	}
}

func TestPokeTruncatesLongStderrInTheLog(t *testing.T) {
	log, out := testLogger()
	run := &fakeRunner{failVM: map[string]string{"vm": "  " + strings.Repeat("x", 500) + "\n"}}
	Poke(context.Background(), run, log, time.Second, []PokeTarget{{"p", "vm"}}, nil, nil)
	logged := out.String()
	if !strings.Contains(logged, strings.Repeat("x", 200)) || strings.Contains(logged, strings.Repeat("x", 201)) {
		t.Errorf("stderr must be logged trimmed and cut to 200 bytes:\n%s", logged)
	}
}

func TestPokeGivesEachTargetItsOwnTimeout(t *testing.T) {
	log, out := testLogger()
	run := &fakeRunner{blockOn: true}
	start := time.Now()
	Poke(context.Background(), run, log, 30*time.Millisecond, []PokeTarget{{"p", "a"}, {"p", "b"}}, nil, nil)

	if n := len(run.argvs()); n != 2 {
		t.Errorf("ran %d targets, want 2: a hung first target must not starve the second", n)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Poke took %v with a 30ms per-target timeout", elapsed)
	}
	logged := out.String()
	for _, want := range []string{"poke p/a failed", "poke p/b failed", "deadline exceeded"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
}

func TestPokeStopsWhenTheParentContextIsCancelled(t *testing.T) {
	log, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	run := &fakeRunner{blockOn: true}
	done := make(chan struct{})
	go func() {
		Poke(ctx, run, log, time.Minute, []PokeTarget{{"p", "a"}, {"p", "b"}}, nil, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Poke ignored the cancelled parent context")
	}
}

// TestPokeRunsTargetsInParallel makes every call return only once both calls have
// started: a sequential Poke can never satisfy that and times out on its first call.
func TestPokeRunsTargetsInParallel(t *testing.T) {
	log, _ := testLogger()
	run := &fakeRunner{}
	var mu sync.Mutex
	started := 0
	bothStarted := make(chan struct{})
	run.onCall = func([]string) {
		mu.Lock()
		started++
		if started == 2 {
			close(bothStarted)
		}
		mu.Unlock()
		select {
		case <-bothStarted:
		case <-time.After(2 * time.Second):
			t.Error("a target finished before the other target had started")
		}
	}

	Poke(context.Background(), run, log, time.Minute, []PokeTarget{{"p", "a"}, {"p", "b"}}, nil, nil)

	if n := len(run.argvs()); n != 2 {
		t.Errorf("ran %d targets, want 2", n)
	}
}

// ------------------------------------------------------------------ report ----

type postCall struct {
	job                               Report
	statusContext, state, description string
}

// fakePoster records every status post and fails only for chosen contexts.
type fakePoster struct {
	mu    sync.Mutex
	calls []postCall
	fail  map[string]error // status context -> error to return
}

func (p *fakePoster) Post(_ context.Context, job Report, statusContext, state, description string) error {
	p.mu.Lock()
	p.calls = append(p.calls, postCall{job: job, statusContext: statusContext, state: state, description: description})
	p.mu.Unlock()
	return p.fail[statusContext]
}

func (p *fakePoster) posted() []postCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func sortedContexts(calls []postCall) []string {
	contexts := make([]string, len(calls))
	for i, c := range calls {
		contexts[i] = c.statusContext
	}
	slices.Sort(contexts)
	return contexts
}

func TestPokePostsOneStatusPerTarget(t *testing.T) {
	log, _ := testLogger()
	ok := convergeStatusJSON(t, "juno", map[string]any{
		"result": "ok", "tag": "v1.0.0", "commit": testCommit,
	})
	run := &fakeRunner{outputs: map[string]string{"a": ok, "b": ok}}
	poster := &fakePoster{}
	job := &Report{Repo: "juno-de/infra", Commit: testCommit, Source: "juno", Installation: 42}

	Poke(context.Background(), run, log, time.Second,
		[]PokeTarget{{"alpha-org", "a"}, {"alpha-org", "b"}}, job, poster)

	calls := poster.posted()
	if want := []string{"nuk/converge/a", "nuk/converge/b"}; !slices.Equal(sortedContexts(calls), want) {
		t.Fatalf("contexts = %q, want %q", sortedContexts(calls), want)
	}
	for _, call := range calls {
		if call.state != "success" {
			t.Errorf("%s: state = %q, want success", call.statusContext, call.state)
		}
		if !strings.Contains(call.description, "applied v1.0.0") {
			t.Errorf("%s: description = %q", call.statusContext, call.description)
		}
		if call.job != *job {
			t.Errorf("%s: job = %+v, want %+v", call.statusContext, call.job, *job)
		}
	}

	// The result is read fresh from the host, after its own converge waited.
	want := [][]string{
		{"incus", "exec", "--project", "alpha-org", "a", "--", "cat", "/var/lib/nuk/converge-status.json"},
		{"incus", "exec", "--project", "alpha-org", "b", "--", "cat", "/var/lib/nuk/converge-status.json"},
	}
	got := run.outputArgvs()
	slices.SortFunc(got, slices.Compare)
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestPokeReportsErrorForADeadVMAndStillReportsTheRest(t *testing.T) {
	log, _ := testLogger()
	alive := convergeStatusJSON(t, "juno", map[string]any{
		"result": "ok", "tag": "v1.0.0", "commit": testCommit,
	})
	run := &fakeRunner{
		failVM:    map[string]string{"dead": "Error: Instance is not running"},
		outputErr: map[string]string{"dead": "incus exec: vm is down"},
		outputs:   map[string]string{"alive": alive},
	}
	poster := &fakePoster{}
	job := &Report{Repo: "juno-de/infra", Commit: testCommit, Source: "juno", Installation: 42}

	Poke(context.Background(), run, log, time.Second,
		[]PokeTarget{{"alpha-org", "dead"}, {"alpha-org", "alive"}}, job, poster)

	byContext := map[string]postCall{}
	for _, call := range poster.posted() {
		byContext[call.statusContext] = call
	}
	dead, ok := byContext["nuk/converge/dead"]
	if !ok {
		t.Fatalf("no status for the dead VM: %+v", poster.posted())
	}
	if dead.state != "error" {
		t.Errorf("dead VM state = %q, want error", dead.state)
	}
	for _, want := range []string{"host unreachable or status unreadable", "incus exec: vm is down", "Instance is not running"} {
		if !strings.Contains(dead.description, want) {
			t.Errorf("dead VM description = %q, want it to contain %q", dead.description, want)
		}
	}
	if alive, ok := byContext["nuk/converge/alive"]; !ok || alive.state != "success" {
		t.Errorf("alive VM status = %+v, want a success", alive)
	}
}

func TestPokeSurvivesAFailingPoster(t *testing.T) {
	log, out := testLogger()
	ok := convergeStatusJSON(t, "juno", map[string]any{
		"result": "ok", "tag": "v1.0.0", "commit": testCommit,
	})
	run := &fakeRunner{outputs: map[string]string{"a": ok, "b": ok}}
	poster := &fakePoster{fail: map[string]error{"nuk/converge/a": errors.New("github is down")}}
	job := &Report{Repo: "juno-de/infra", Commit: testCommit, Source: "juno", Installation: 42}

	Poke(context.Background(), run, log, time.Second,
		[]PokeTarget{{"alpha-org", "a"}, {"alpha-org", "b"}}, job, poster)

	if calls := poster.posted(); len(calls) != 2 {
		t.Fatalf("%d posts, want both targets attempted: %+v", len(calls), calls)
	}
	logged := out.String()
	for _, want := range []string{"nuk/converge/a", "github is down", "nuk/converge/b"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
}

// ------------------------------------------------------------- ExecRunner ----

// stubIncus installs an `incus` script in a fresh PATH directory and returns that
// directory; /usr/bin:/bin stay on PATH for sleep and friends.
func stubIncus(t *testing.T, script string) string {
	t.Helper()
	dir := shortTmp(t)
	if err := os.WriteFile(filepath.Join(dir, "incus"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return dir
}

func TestExecRunnerPassesArgvVerbatimWithoutAShell(t *testing.T) {
	dir := shortTmp(t)
	record := filepath.Join(dir, "argv")
	stubIncus(t, fmt.Sprintf("printf '%%s\\0' \"$@\" > %q\n", record))

	argv := []string{"incus", "exec", "--project", "a b;$(reboot)", "vm\nx", "--", "systemctl"}
	stderr, err := ExecRunner{}.Run(context.Background(), argv)
	if err != nil || stderr != "" {
		t.Fatalf("Run = %q, %v; want success", stderr, err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if !slices.Equal(got, argv[1:]) {
		t.Errorf("stub saw %q, want %q", got, argv[1:])
	}
}

func TestExecRunnerReturnsStderrAndTheExitError(t *testing.T) {
	stubIncus(t, "echo 'Error: Instance is not running' >&2\nexit 3\n")
	stderr, err := ExecRunner{}.Run(context.Background(), []string{"incus", "exec"})
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(stderr, "Instance is not running") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestExecRunnerOutputReturnsStdoutAndStderr(t *testing.T) {
	stubIncus(t, "echo converged\necho warning >&2\n")
	stdout, stderr, err := ExecRunner{}.Output(context.Background(), []string{"incus", "exec"})
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if stdout != "converged\n" || stderr != "warning\n" {
		t.Errorf("Output = %q, %q; want %q, %q", stdout, stderr, "converged\n", "warning\n")
	}
}

func TestExecRunnerKillsAProcessThatOutlivesItsContext(t *testing.T) {
	stubIncus(t, "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, []string{"incus", "exec"})
	if err == nil {
		t.Fatal("a killed process must be an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v, want it killed near the 150ms deadline", elapsed)
	}
}

func TestExecRunnerDoesNotWaitForAGrandchildHoldingItsPipes(t *testing.T) {
	// The stub's `sleep` is a child of the killed shell and still holds stderr open.
	stubIncus(t, "sleep 3\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ExecRunner{Grace: 100 * time.Millisecond}.Run(ctx, []string{"incus", "exec"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run took %v: it waited for the orphaned grandchild instead of giving up after Grace", elapsed)
	}
}

func TestExecRunnerMissingBinaryIsAnError(t *testing.T) {
	t.Setenv("PATH", shortTmp(t))
	if _, err := (ExecRunner{}).Run(context.Background(), []string{"incus", "exec"}); err == nil {
		t.Fatal("expected an error when incus is not on PATH")
	}
}

func TestExecRunnerRefusesAnEmptyArgv(t *testing.T) {
	if _, err := (ExecRunner{}).Run(context.Background(), nil); err == nil {
		t.Fatal("expected an error for an empty argv")
	}
}
