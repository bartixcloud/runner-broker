package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// incusCall is one recorded subprocess invocation.
type incusCall struct {
	argv  []string
	stdin string
}

type incusRecorder struct {
	mu    sync.Mutex
	calls []incusCall
}

func (r *incusRecorder) all() []incusCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]incusCall(nil), r.calls...)
}

// withVerb returns the calls whose first post-project argument is verb.
func (r *incusRecorder) withVerb(verb string) []incusCall {
	var out []incusCall
	for _, c := range r.all() {
		if len(c.argv) > 3 && c.argv[3] == verb {
			out = append(out, c)
		}
	}
	return out
}

// incusStub replaces the subprocess seam; reply decides each call's outcome.
func incusStub(t *testing.T, reply func(c incusCall) (stdout, stderr string, code int)) *incusRecorder {
	t.Helper()
	r := &incusRecorder{}
	swap(t, &execCommand, func(argv []string, stdin string) (string, string, int) {
		c := incusCall{append([]string(nil), argv...), stdin}
		r.mu.Lock()
		r.calls = append(r.calls, c)
		r.mu.Unlock()
		return reply(c)
	})
	return r
}

func incusOK(incusCall) (string, string, int) { return "", "", 0 }

// The flag lives in ONE function so launch, list, exec and delete all inherit
// it (spec 1 §7.1): cross-tenant reaping is impossible because the other
// tenant's instances are not visible, not because a call site remembered.
func TestIncusCallsAreProjectScoped(t *testing.T) {
	swap(t, &incusProject, "juno")
	rec := incusStub(t, incusOK)
	incus("list", "^ci-", "--format", "csv", "-c", "n")
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("calls = %v", got)
	}
	if want := []string{"incus", "--project", "juno", "list", "^ci-", "--format", "csv", "-c", "n"}; !reflect.DeepEqual(got[0].argv, want) {
		t.Errorf("argv = %v, want %v", got[0].argv, want)
	}
}

func TestIncusWithInputIsProjectScopedAndCarriesStdin(t *testing.T) {
	swap(t, &incusProject, "juno")
	rec := incusStub(t, incusOK)
	incusInput("echo hi\n", "exec", "vm", "--", "sh")
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("calls = %v", got)
	}
	if want := []string{"incus", "--project", "juno", "exec", "vm", "--", "sh"}; !reflect.DeepEqual(got[0].argv, want) {
		t.Errorf("argv = %v, want %v", got[0].argv, want)
	}
	if got[0].stdin != "echo hi\n" {
		t.Errorf("stdin = %q", got[0].stdin)
	}
}

func TestRunningRunnersInheritsProjectScope(t *testing.T) {
	swap(t, &incusProject, "juno")
	rec := incusStub(t, func(incusCall) (string, string, int) { return "ci-build-1-aaa\n", "", 0 })
	got, err := runningRunners("build")
	if err != nil || !reflect.DeepEqual(got, []string{"ci-build-1-aaa"}) {
		t.Fatalf("got (%v, %v)", got, err)
	}
	if argv := rec.all()[0].argv; !reflect.DeepEqual(argv[:3], []string{"incus", "--project", "juno"}) {
		t.Errorf("argv = %v", argv)
	}
}

// A failed `incus list` is an unknown count, not a count of zero: reading "we
// couldn't ask" as "none running" would bypass the per-tier ceiling.
func TestRunningRunnersIsAnErrorWhenIncusFails(t *testing.T) {
	incusStub(t, func(incusCall) (string, string, int) { return "", "incus: daemon not reachable", 1 })
	got, err := runningRunners("build")
	if err == nil {
		t.Fatalf("got %v, want an error", got)
	}
	if !strings.Contains(err.Error(), "daemon not reachable") {
		t.Errorf("error should carry incus's stderr: %v", err)
	}
}

func TestRunningRunnersFiltersByTierPrefix(t *testing.T) {
	swap(t, &incusProject, "p")
	rec := incusStub(t, func(incusCall) (string, string, int) { return "ci-build-1-aaa\n\n  \nci-build-2-bbb\n", "", 0 })
	got, err := runningRunners("build")
	if err != nil || !reflect.DeepEqual(got, []string{"ci-build-1-aaa", "ci-build-2-bbb"}) {
		t.Fatalf("got (%v, %v): blank lines must be dropped", got, err)
	}
	if want := []string{"incus", "--project", "p", "list", "^ci-build-", "--format", "csv", "-c", "n"}; !reflect.DeepEqual(rec.all()[0].argv, want) {
		t.Errorf("argv = %v, want %v", rec.all()[0].argv, want)
	}
	if _, err := runningRunners(""); err != nil {
		t.Fatal(err)
	}
	if got := rec.all()[1].argv[4]; got != "^ci-" {
		t.Errorf("all-tier prefix = %q, want ^ci-", got)
	}
}

// The union over the configured tiers must stay exactly today's single Incus
// list when every tier uses the same backend: one call, same prefix, same
// order. A per-tier list would also change what the reconciler sees.
func TestRunningRunnersAllCollapsesOneBackendToOneListCall(t *testing.T) {
	swap(t, &incusProject, "p")
	rec := incusStub(t, func(incusCall) (string, string, int) { return "ci-build-1-aaa\nci-dependabot-2-bbb\n", "", 0 })
	tiers := map[string]tier{
		"build":      {Backend: backendIncus, MaxRunners: 1, Mem: "4GiB"},
		"dependabot": {Backend: backendIncus, MaxRunners: 1, Mem: "4GiB"},
	}
	got, err := runningRunnersAll(tiers)
	if err != nil || !reflect.DeepEqual(got, []string{"ci-build-1-aaa", "ci-dependabot-2-bbb"}) {
		t.Fatalf("got (%v, %v)", got, err)
	}
	if calls := rec.all(); len(calls) != 1 {
		t.Fatalf("list calls = %v, want one union call", calls)
	}
	if want := []string{"incus", "--project", "p", "list", "^ci-", "--format", "csv", "-c", "n"}; !reflect.DeepEqual(rec.all()[0].argv, want) {
		t.Errorf("argv = %v, want %v", rec.all()[0].argv, want)
	}
}

func TestExecCommandCapturesOutputExitCodeAndStdin(t *testing.T) {
	out, errOut, code := execCommand([]string{"sh", "-c", "cat; echo err >&2; exit 3"}, "hello")
	if out != "hello" || errOut != "err\n" || code != 3 {
		t.Errorf("got (%q, %q, %d)", out, errOut, code)
	}
}

// A missing binary used to surface as code -1 with empty stderr, so the log
// said "incus list failed: " with no reason.
func TestExecCommandReportsWhyAProcessCouldNotRun(t *testing.T) {
	_, errOut, code := execCommand([]string{"/nonexistent/incus-binary"}, "")
	if code == 0 || !strings.Contains(errOut, "/nonexistent/incus-binary") {
		t.Errorf("got (%q, %d), want a nonzero code and a reason", errOut, code)
	}
}

// incus list runs under launchLock; a wedged incusd would otherwise block every
// later reservation forever.
func TestExecCommandKillsAProcessThatOutlivesTheDeadline(t *testing.T) {
	swap(t, &execTimeout, 100*time.Millisecond)
	start := time.Now()
	_, errOut, code := execCommand([]string{"sleep", "30"}, "")
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("not killed: ran %s", elapsed)
	}
	if code == 0 || !strings.Contains(errOut, "timed out") {
		t.Errorf("got (%q, %d), want a timeout reason", errOut, code)
	}
}
