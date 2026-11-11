package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The two list endpoints the reconciler reads, matched off the recorded path so
// the stub can answer for whatever repo/run the code asks about.
var (
	queuedRunsRe = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs\?status=queued`)
	runJobsRe    = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/actions/runs/(\d+)/jobs`)
	runnersRe    = regexp.MustCompile(`^(/orgs/[^/]+|/repos/[^/]+/[^/]+)/actions/runners\?`)
)

// ghRunner is one entry of GitHub's runner list. busy is the only field the
// reconciler reads; status is there so a test can model an offline registration.
type ghRunner struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// reconcileStub answers the reconciler's list calls (repos, queued runs, jobs)
// plus the spawn path's runner-groups and jitconfig, for repos -> queued run
// ids, with jobsOf supplying each run's jobs.
func reconcileStub(t *testing.T, repos map[string][]int, jobsOf func(repo string, runID int) []any) *[]ghCall {
	t.Helper()
	return reconcileStubRunners(t, repos, jobsOf, func() []ghRunner { return nil })
}

// reconcileStubRunners is reconcileStub plus the runner list, which runners
// supplies on every call so a test can change it between passes.
func reconcileStubRunners(t *testing.T, repos map[string][]int, jobsOf func(repo string, runID int) []any, runners func() []ghRunner) *[]ghCall {
	t.Helper()
	return ghStub(t, func(c ghCall) ([]byte, error) {
		switch {
		case runnersRe.MatchString(c.path):
			list := runners()
			if list == nil {
				list = []ghRunner{}
			}
			return mustJSON(t, map[string]any{"total_count": len(list), "runners": list}), nil
		case strings.HasSuffix(c.path, "/runner-groups"):
			return groupsReply(t, "build-ephemeral", 42), nil
		case strings.HasPrefix(c.path, "/orgs/alpha-org/repos"):
			names := []any{}
			for name := range repos {
				names = append(names, map[string]any{"full_name": name})
			}
			return mustJSON(t, names), nil
		}
		if m := queuedRunsRe.FindStringSubmatch(c.path); m != nil {
			runs := []any{}
			for _, id := range repos[m[1]] {
				runs = append(runs, map[string]any{"id": id})
			}
			return mustJSON(t, map[string]any{"workflow_runs": runs}), nil
		}
		if m := runJobsRe.FindStringSubmatch(c.path); m != nil {
			runID, err := strconv.Atoi(m[2])
			if err != nil {
				t.Fatalf("unparseable run id in %q", c.path)
			}
			return mustJSON(t, map[string]any{"jobs": jobsOf(m[1], runID)}), nil
		}
		return []byte(`{"encoded_jit_config":"JIT-PAYLOAD"}`), nil
	})
}

// oneQueuedJob is the job list most tests want: a single queued job.
func oneQueuedJob(jobID int, labels []string) func(string, int) []any {
	return func(string, int) []any {
		return []any{map[string]any{"id": jobID, "status": "queued", "labels": labels}}
	}
}

// queuedJobEnv wires the reconciler for an org tenant with one repo holding one
// queued job, on top of the spawn seams.
func queuedJobEnv(t *testing.T, jobID int, labels []string) *incusRecorder {
	t.Helper()
	rec := spawnEnv(t, nil)
	reconcileStub(t, map[string][]int{"alpha-org/app": {100}}, oneQueuedJob(jobID, labels))
	return rec
}

func TestQueuedJobsKeepsOnlyQueuedJobs(t *testing.T) {
	spawnEnv(t, nil)
	reconcileStub(t, map[string][]int{"alpha-org/app": {100}}, func(string, int) []any {
		return []any{
			map[string]any{"id": 7, "status": "queued", "labels": []string{"self-hosted", "build"}},
			map[string]any{"id": 8, "status": "completed", "labels": []string{"self-hosted", "build"}},
			map[string]any{"id": 9, "status": "in_progress", "labels": []string{"self-hosted", "build"}},
		}
	})
	jobs, err := queuedJobs("tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want only the queued one", jobs)
	}
	got := jobs[0]
	if got.ID != 7 || got.RunID != 100 || got.Repo != "alpha-org/app" {
		t.Errorf("job = %+v, want {7 100 alpha-org/app}", got)
	}
	if len(got.Labels) != 2 || got.Labels[1] != "build" {
		t.Errorf("labels = %v, want the job's own labels", got.Labels)
	}
}

// A user tenant has no org to enumerate: the registered repos ARE the scope,
// exactly as targetPath accepts them.
func TestQueuedJobsScansRegisteredReposForAUserTenant(t *testing.T) {
	spawnEnv(t, nil)
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	calls := reconcileStub(t, map[string][]int{"Juno/some-repo": {100}}, oneQueuedJob(7, []string{"self-hosted", "build"}))
	jobs, err := queuedJobs("tok")
	if err != nil || len(jobs) != 1 || jobs[0].Repo != "Juno/some-repo" {
		t.Fatalf("got (%+v, %v)", jobs, err)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c.path, "/orgs/") {
			t.Errorf("a user tenant must not enumerate an org: %v", c.path)
		}
	}
}

// The whole point: a job whose webhook never reached a working broker (refused
// reservation, lost launch, broker down) must still get a runner.
func TestReconcileSpawnsARunnerForAQueuedJobWithNoRunner(t *testing.T) {
	rec := queuedJobEnv(t, 7, []string{"self-hosted", "build"})
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()

	launch := rec.withVerb("launch")
	if len(launch) != 1 {
		t.Fatalf("launch calls = %v, want exactly one", launch)
	}
	if !strings.Contains(launch[0].argv[5], "ci-build-7-") {
		t.Errorf("launched %q, want a runner named for job 7", launch[0].argv[5])
	}
	if _, taken := claimed[7]; taken {
		t.Error("the claim outlived the spawn")
	}
}

func TestReconcileLeavesAJobThatAlreadyHasARunningInstance(t *testing.T) {
	rec := queuedJobEnv(t, 7, []string{"self-hosted", "build"})
	swap(t, &runningRunners, func(string) ([]string, error) { return []string{"ci-build-7-abc123"}, nil })
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()
	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launch calls = %d, want 0: job 7 already has a VM", n)
	}
}

// The webhook path is already launching this job; a second VM would leave one
// runner idle and hold a slot until the reaper.
func TestReconcileSkipsAJobThisProcessIsAlreadyLaunching(t *testing.T) {
	rec := queuedJobEnv(t, 7, []string{"self-hosted", "build"})
	if !claimJob(7, "build") {
		t.Fatal("claimJob(7) was already taken")
	}
	defer unclaimJob(7)
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()
	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launch calls = %d, want 0", n)
	}
}

// A denied job was cancelled by the webhook path; re-cancelling every pass
// would only fill the journal, and an ignored one was never ours.
func TestReconcileLeavesIgnoredAndDeniedJobsAlone(t *testing.T) {
	spawnEnv(t, nil)
	calls := reconcileStub(t, map[string][]int{"alpha-org/app": {100}}, func(string, int) []any {
		return []any{
			map[string]any{"id": 1, "status": "queued", "labels": []string{"ubuntu-latest"}},
			map[string]any{"id": 2, "status": "queued", "labels": []string{"self-hosted", "build", "infra"}},
			map[string]any{"id": 3, "status": "queued", "labels": []string{"self-hosted", "build"}},
		}
	})
	rec := incusStub(t, incusOK)
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()

	launch := rec.withVerb("launch")
	if len(launch) != 1 || !strings.Contains(launch[0].argv[5], "ci-build-3-") {
		t.Fatalf("launch calls = %v, want only job 3", launch)
	}
	if len(*calls) == 0 {
		t.Fatal("no API call recorded")
	}
	for _, c := range *calls {
		if strings.HasSuffix(c.path, "/cancel") {
			t.Errorf("the reconciler cancelled a run: %v", c.path)
		}
	}
}

func TestClaimJobIsExclusive(t *testing.T) {
	freshState(t)
	if !claimJob(1, "build") {
		t.Fatal("the first claim must win")
	}
	if claimJob(1, "build") {
		t.Error("a second claim on the same job must lose")
	}
	unclaimJob(1)
	if !claimJob(1, "build") {
		t.Error("a released job must be claimable again")
	}
	unclaimJob(1)
}

func TestJobIDFromInstanceName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
	}{
		{"ci-build-112888281804-8e81b5", 112888281804, true},
		{"ci-dependabot-12-abcdef", 12, true},
		{"ci-build-7", 0, false},
		{"ci-build-x-abcdef", 0, false},
		{"other-db", 0, false},
		{"", 0, false},
	} {
		got, ok := jobIDFromInstanceName(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("jobIDFromInstanceName(%q) = (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A scan that cannot reach GitHub must not take the broker down with it, and
// must not be silent either.
func TestReconcileSurvivesAGitHubFailure(t *testing.T) {
	spawnEnv(t, nil)
	logs := captureLog(t)
	ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("connection reset") })
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()
	if !strings.Contains(logs.String(), "ERROR reconcile:") || !strings.Contains(logs.String(), "connection reset") {
		t.Errorf("log = %q", logs.String())
	}
}

func TestReconcileSurvivesAnUnreadableInstanceList(t *testing.T) {
	rec := queuedJobEnv(t, 7, []string{"self-hosted", "build"})
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, errors.New("incus daemon wedged") })
	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()
	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launch calls = %d, want 0", n)
	}
}

// waitFor polls cond until it holds or five seconds pass.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// A running broker must start the loop: it is what rescues a job whose webhook
// never reached a working broker.
func TestRunStartsTheReconciler(t *testing.T) {
	lookup, sock := runEnv(t, buildOnlyTiers())
	incusStub(t, incusOK)
	ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("no API call expected") })
	started := make(chan struct{}, 1)
	swap(t, &startReconciler, func(context.Context, *server, tokenSource) { started <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- run(ctx, lookup) }()
	waitForSocket(t, sock)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start the reconciler")
	}
	cancel()
	if err := waitResult(t, res, 5*time.Second); err != nil {
		t.Fatalf("clean shutdown must return nil, got %v", err)
	}
}

// reconcileLoop reconciles once at startup — that pass is what rescues a job
// that arrived while the broker was down — and then stops with ctx.
func TestReconcileLoopReconcilesOnceAtStartupAndStopsWithContext(t *testing.T) {
	rec := queuedJobEnv(t, 7, []string{"self-hosted", "build"})
	swap(t, &reconcileInterval, time.Hour) // one pass, no tick
	srv := newServer(fakeTokens{token: "tok"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); srv.reconcileLoop(ctx, srv.tokens) }()
	if !waitFor(t, func() bool { return len(rec.withVerb("launch")) == 1 }) {
		t.Fatal("the startup pass launched no runner")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("reconcileLoop did not return when ctx was cancelled")
	}
	// The startup pass spawned on a tracked goroutine; the loop returns without
	// waiting for it, and a spawn still in flight would outlive the test's stubs.
	srv.wg.Wait()
}

// safeSpawn owns the claim: the webhook path and the reconciler must never
// launch two VMs for one job.
func TestSafeSpawnHoldsTheClaimForTheWholeLaunch(t *testing.T) {
	spawnEnv(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		close(entered)
		<-release
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	srv := newServer(fakeTokens{token: "tok"})
	done := make(chan struct{})
	go func() { defer close(done); srv.safeSpawn(7, "build", "alpha-org/app") }()
	<-entered
	if claimJob(7, "build") {
		t.Error("a job being launched here was claimable by the reconciler")
	}
	close(release)
	<-done
	if !claimJob(7, "build") {
		t.Error("the claim leaked past the spawn")
	}
	unclaimJob(7)
}

// ---- count-based spawning ----
//
// GitHub hands an idle JIT runner ANY matching queued job, not the one it was
// spawned for, so "a VM is named for this job" says nothing about whether the
// job will get a runner. The reconciler counts per tier instead: it spawns only
// while the tier's queued jobs exceed its runners that can still take one.

// countEnv wires an org tenant whose queued jobs are queued() and whose GitHub
// runner list is runners(), and returns the launch recorder. vms is the VM list
// the Incus seam reports.
func countEnv(t *testing.T, queued func() []int, vms func() []string, runners func() []ghRunner) *incusRecorder {
	t.Helper()
	rec := spawnEnv(t, nil)
	swap(t, &runningRunners, func(string) ([]string, error) { return vms(), nil })
	reconcileStubRunners(t, map[string][]int{"alpha-org/app": {100}}, func(string, int) []any {
		var jobs []any
		for _, id := range queued() {
			jobs = append(jobs, map[string]any{"id": id, "status": "queued", "labels": []string{"self-hosted", "build"}})
		}
		return jobs
	}, runners)
	return rec
}

func reconcilePass(t *testing.T, srv *server) {
	t.Helper()
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()
}

func fixed[T any](v ...T) func() []T { return func() []T { return v } }

// The observed bug, replayed. Three jobs A=1, B=2, C=3 each got a VM named for
// them. A's VM ran A and went away; B's VM took C. When B's VM finished and was
// deleted, job B was still queued and C's VM — idle, named for C — was about to
// take it. Keyed on job id, the reconciler saw "no VM named for B" and spawned
// a fourth runner that never got a job.
func TestReconcileReplaysTheCrossAssignmentWithoutAnExtraRunner(t *testing.T) {
	queued := []int{1, 2, 3}
	var vms []string
	var runners []ghRunner
	rec := countEnv(t, func() []int { return queued }, func() []string { return vms }, func() []ghRunner { return runners })
	srv := newServer(fakeTokens{token: "tok"})

	// Pass 1: three queued jobs, no runners — three spawns.
	reconcilePass(t, srv)
	launch := rec.withVerb("launch")
	if len(launch) != 3 {
		t.Fatalf("pass 1 launches = %d, want 3", len(launch))
	}
	var names []string
	for _, l := range launch {
		names = append(names, l.argv[5])
	}
	nameFor := func(job string) string {
		for _, n := range names {
			if strings.Contains(n, "ci-build-"+job+"-") {
				return n
			}
		}
		t.Fatalf("no launched VM for job %s in %v", job, names)
		return ""
	}
	vmB, vmC := nameFor("2"), nameFor("3")

	// A's VM took A and exited; B's VM took C and is busy; C's VM is idle and B
	// is still queued.
	queued = []int{2}
	vms = []string{vmB, vmC}
	runners = []ghRunner{{vmB, "online", true}, {vmC, "online", false}}
	reconcilePass(t, srv)
	if n := len(rec.withVerb("launch")); n != 3 {
		t.Fatalf("pass 2: launches = %d, want still 3: C's idle VM will take job B", n)
	}

	// B's VM finished and was deleted. Job B is still queued, and no VM carries
	// its id any more — the state that used to trigger the extra spawn.
	vms = []string{vmC}
	runners = []ghRunner{{vmC, "online", false}}
	reconcilePass(t, srv)
	if n := len(rec.withVerb("launch")); n != 3 {
		t.Fatalf("pass 3: launches = %d, want still 3: an idle runner of the tier covers the queued job", n)
	}
}

func TestReconcileSpawnsWhenEveryRunnerOfTheTierIsBusy(t *testing.T) {
	rec := countEnv(t, fixed(7), fixed("ci-build-9-aaaaaa"), fixed(ghRunner{"ci-build-9-aaaaaa", "online", true}))
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	if n := len(rec.withVerb("launch")); n != 1 {
		t.Errorf("launches = %d, want 1: the only runner is busy, so job 7 has none", n)
	}
}

// A registration whose VM is gone (the two offline runners left behind on
// 2026-11-11) is not capacity: counting it would hold the tier shut.
func TestReconcileIgnoresOfflineRegistrationsWithoutAVM(t *testing.T) {
	rec := countEnv(t, fixed(7), fixed[string](), fixed(
		ghRunner{"ci-build-5-aaaaaa", "offline", false},
		ghRunner{"ci-build-6-bbbbbb", "offline", false},
	))
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	if n := len(rec.withVerb("launch")); n != 1 {
		t.Errorf("launches = %d, want 1", n)
	}
}

// A VM that has not registered or connected yet is booting, and a booting
// runner will take the job once it is up.
func TestReconcileCountsABootingVMAsCapacity(t *testing.T) {
	rec := countEnv(t, fixed(7), fixed("ci-build-9-aaaaaa"), fixed[ghRunner]())
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launches = %d, want 0", n)
	}
}

func TestReconcileSpawnsOnlyTheShortfall(t *testing.T) {
	rec := countEnv(t, fixed(1, 2, 3), fixed("ci-build-9-aaaaaa"), fixed(ghRunner{"ci-build-9-aaaaaa", "online", false}))
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	if n := len(rec.withVerb("launch")); n != 2 {
		t.Errorf("launches = %d, want 2: three queued jobs, one idle runner", n)
	}
}

// An idle runner of one tier cannot take another tier's job.
func TestReconcileCountsPerTier(t *testing.T) {
	rec := countEnv(t, fixed(7), fixed("ci-dependabot-9-aaaaaa"), fixed(ghRunner{"ci-dependabot-9-aaaaaa", "online", false}))
	srv := newServer(fakeTokens{token: "tok"})
	srv.tiers = map[string]tier{"build": tiersFixture["build"], "dependabot": tiersFixture["build"]}
	reconcilePass(t, srv)
	if n := len(rec.withVerb("launch")); n != 1 {
		t.Errorf("launches = %d, want 1: the idle runner belongs to the dependabot tier", n)
	}
}

// If GitHub cannot say which runners are busy, spawning on a guess is exactly
// the bug; wait for the next pass instead, and say so.
func TestReconcileSpawnsNothingWhenTheRunnerListIsUnreadable(t *testing.T) {
	rec := spawnEnv(t, nil)
	logs := captureLog(t)
	ghStub(t, func(c ghCall) ([]byte, error) {
		switch {
		case runnersRe.MatchString(c.path):
			return nil, errors.New("403 resource not accessible")
		case strings.HasPrefix(c.path, "/orgs/alpha-org/repos"):
			return []byte(`[{"full_name":"alpha-org/app"}]`), nil
		case queuedRunsRe.MatchString(c.path):
			return []byte(`{"workflow_runs":[{"id":100}]}`), nil
		case runJobsRe.MatchString(c.path):
			return mustJSON(t, map[string]any{"jobs": oneQueuedJob(7, []string{"self-hosted", "build"})("", 0)}), nil
		}
		return []byte(`{"encoded_jit_config":"J"}`), nil
	})
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launches = %d, want 0", n)
	}
	if !strings.Contains(logs.String(), "ERROR reconcile:") || !strings.Contains(logs.String(), "403") {
		t.Errorf("log = %q", logs.String())
	}
}

// A busy runner on a later page must still be seen: offline registrations pile
// up, and a partial list would overstate the idle capacity.
func TestBusyRunnersWalksEveryPage(t *testing.T) {
	spawnEnv(t, nil)
	var pages []string
	ghStub(t, func(c ghCall) ([]byte, error) {
		pages = append(pages, c.path)
		if strings.HasSuffix(c.path, "page=1") {
			full := make([]ghRunner, listPage)
			for i := range full {
				full[i] = ghRunner{Name: "ci-build-" + strconv.Itoa(i+1) + "-aaaaaa", Status: "offline"}
			}
			return mustJSON(t, map[string]any{"runners": full}), nil
		}
		return mustJSON(t, map[string]any{"runners": []ghRunner{{"ci-build-500-bbbbbb", "online", true}}}), nil
	})
	busy, err := busyRunners("tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := busy["ci-build-500-bbbbbb"]; !ok || len(busy) != 1 {
		t.Errorf("busy = %v, want only the runner on page 2", busy)
	}
	if len(pages) != 2 {
		t.Errorf("pages = %v, want two", pages)
	}
}

func TestBusyRunnersListsTheRepoRunnersOfAUserTenant(t *testing.T) {
	spawnEnv(t, nil)
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &repos, []string{"some-repo"})
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		return mustJSON(t, map[string]any{"runners": []ghRunner{{"ci-build-1-aaaaaa", "online", true}}}), nil
	})
	busy, err := busyRunners("tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := busy["ci-build-1-aaaaaa"]; !ok {
		t.Errorf("busy = %v", busy)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0].path, "/repos/Juno/some-repo/actions/runners?") {
		t.Errorf("calls = %v, want one repo-level runner list", *calls)
	}
}

func TestInstanceNameParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier string
		job  int
		ok   bool
	}{
		{"ci-build-112888281804-8e81b5", "build", 112888281804, true},
		{"ci-build-arm-9-aaaaaa", "build-arm", 9, true},
		{"ci-build-7", "", 0, false},
		{"ci-build-x-abcdef", "", 0, false},
		{"other-db", "", 0, false},
	} {
		tier, job, ok := parseInstanceName(tc.name)
		if ok != tc.ok || tier != tc.tier || job != tc.job {
			t.Errorf("parseInstanceName(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, tier, job, ok, tc.tier, tc.job, tc.ok)
		}
	}
}

// A launch in flight has no VM yet, so it is the only record that a runner is
// about to exist for the tier.
func TestReconcileCountsALaunchInFlightAsCapacity(t *testing.T) {
	rec := countEnv(t, fixed(7, 8), fixed[string](), fixed[ghRunner]())
	if !claimJob(7, "build") {
		t.Fatal("claimJob(7) was already taken")
	}
	defer unclaimJob(7)
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	launch := rec.withVerb("launch")
	if len(launch) != 1 || !strings.Contains(launch[0].argv[5], "ci-build-8-") {
		t.Errorf("launch calls = %v, want only job 8: job 7's runner is being launched", launch)
	}
}

// Once the VM of a launch in flight is listed, the claim and the VM are one
// runner, not two.
func TestReconcileDoesNotCountALaunchAndItsVMTwice(t *testing.T) {
	rec := countEnv(t, fixed(7, 8), fixed("ci-build-7-aaaaaa"), fixed(ghRunner{"ci-build-7-aaaaaa", "offline", false}))
	if !claimJob(7, "build") {
		t.Fatal("claimJob(7) was already taken")
	}
	defer unclaimJob(7)
	reconcilePass(t, newServer(fakeTokens{token: "tok"}))
	launch := rec.withVerb("launch")
	if len(launch) != 1 || !strings.Contains(launch[0].argv[5], "ci-build-8-") {
		t.Errorf("launch calls = %v, want only job 8", launch)
	}
}
