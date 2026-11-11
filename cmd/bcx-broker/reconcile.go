package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// listPage is the page size of the three list endpoints the reconciler reads.
const listPage = 100

// maxRepoPages bounds the org repo walk. An org with more repos than this needs
// a different scan (per-repo webhooks), and silently reading only the first
// page would be a forgotten job with extra steps.
const maxRepoPages = 10

// reconcileInterval is how often the broker re-asks GitHub which of this
// tenant's jobs are still queued. A workflow_job webhook is delivered once and
// never redelivered: a broker that was down, a reservation it refused (tier at
// max_runners, memory floor) or a launch that failed used to leave that job
// queued until GitHub cancelled it 24h later, with a newer job taking the slot
// that freed. The pass is the safety net that turns "forgotten" into "picked up
// within one interval".
var reconcileInterval = 30 * time.Second

// queuedJob is one queued workflow job of this tenant. It carries exactly what
// decideJobAction reads, so both the webhook path and the reconciler judge a
// job by the same rules.
type queuedJob struct {
	ID     int
	RunID  int
	Repo   string
	Labels []string
}

// orgReposReply, runsReply and jobsReply are the parts of the list endpoints
// the reconciler reads. A missing field decodes to its zero value, so callers
// validate what they use.
type orgReposReply []struct {
	FullName string `json:"full_name"`
}

type runsReply struct {
	WorkflowRuns []struct {
		ID int `json:"id"`
	} `json:"workflow_runs"`
}

type jobsReply struct {
	Jobs []struct {
		ID     int      `json:"id"`
		Status string   `json:"status"`
		Labels []string `json:"labels"`
	} `json:"jobs"`
}

// scanningRepos returns the repos whose queued jobs the reconciler looks at:
// every repo of an org tenant (the installation token sees only what the App is
// installed on), and the registered repos of a user tenant — the same scope
// targetPath accepts, so a scan cannot reach a repo a launch could not.
func scanningRepos(token string) ([]string, error) {
	switch parseTargetKind(targetKind) {
	case kindOrg:
		var out []string
		for page := 1; page <= maxRepoPages; page++ {
			raw, err := gh("GET", fmt.Sprintf("/orgs/%s/repos?per_page=%d&type=all&page=%d", target, listPage, page), token, nil)
			if err != nil {
				return nil, fmt.Errorf("cannot list the repos of %s: %w", target, err)
			}
			var reply orgReposReply
			if err := json.Unmarshal(raw, &reply); err != nil {
				return nil, fmt.Errorf("repo list of %s is not the expected JSON: %w", target, err)
			}
			for _, r := range reply {
				// The name becomes part of an API path below and repoNameRe is
				// what cancelJob insists on for the same reason.
				if !repoNameRe.MatchString(r.FullName) {
					logf("WARNING", "reconcile: skipping repo with an unexpected name %q", r.FullName)
					continue
				}
				out = append(out, r.FullName)
			}
			if len(reply) < listPage {
				return out, nil
			}
		}
		return nil, fmt.Errorf("%s has more than %d repos: refusing a partial scan", target, listPage*maxRepoPages)
	case kindUser:
		out := make([]string, 0, len(repos))
		for _, r := range repos {
			out = append(out, target+"/"+r)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown BROKER_TARGET_KIND %q", targetKind)
	}
}

// queuedJobs lists every queued job of this tenant's repos. A repo whose runs
// or jobs cannot be read is logged and skipped: one unreadable repo must not
// blind the reconciler to the others.
func queuedJobs(token string) ([]queuedJob, error) {
	names, err := scanningRepos(token)
	if err != nil {
		return nil, err
	}
	var out []queuedJob
	for _, repo := range names {
		runs, err := queuedRuns(token, repo)
		if err != nil {
			logf("WARNING", "reconcile: %v", err)
			continue
		}
		for _, runID := range runs {
			jobs, err := runJobs(token, repo, runID)
			if err != nil {
				logf("WARNING", "reconcile: %v", err)
				continue
			}
			out = append(out, jobs...)
		}
	}
	return out, nil
}

// queuedRuns returns the ids of repo's queued runs.
func queuedRuns(token, repo string) ([]int, error) {
	raw, err := gh("GET", fmt.Sprintf("/repos/%s/actions/runs?status=queued&per_page=%d", repo, listPage), token, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot list the queued runs of %s: %w", repo, err)
	}
	var reply runsReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("run list of %s is not the expected JSON: %w", repo, err)
	}
	// A full page may hide further runs behind it. Not paginating keeps the
	// interval cheap; saying so keeps it from being silent.
	if len(reply.WorkflowRuns) == listPage {
		logf("WARNING", "reconcile: %s has at least %d queued runs; only the first page is scanned", repo, listPage)
	}
	out := make([]int, 0, len(reply.WorkflowRuns))
	for _, r := range reply.WorkflowRuns {
		out = append(out, r.ID)
	}
	return out, nil
}

// runJobs returns the queued jobs of one run, with the labels the decision core
// reads. GitHub reports completed and in-progress jobs of the same run here,
// and only a queued job needs a runner.
func runJobs(token, repo string, runID int) ([]queuedJob, error) {
	raw, err := gh("GET", fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?per_page=%d", repo, runID, listPage), token, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot list the jobs of %s run %d: %w", repo, runID, err)
	}
	var reply jobsReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("job list of %s run %d is not the expected JSON: %w", repo, runID, err)
	}
	var out []queuedJob
	for _, j := range reply.Jobs {
		if j.Status != "queued" {
			continue
		}
		out = append(out, queuedJob{ID: j.ID, RunID: runID, Repo: repo, Labels: j.Labels})
	}
	return out, nil
}

// claimJob marks jobID as being launched by this process for tier t, so the
// webhook path and the reconciler — which both see the same queued job — cannot
// launch two VMs for it, and so the reconciler can count the runner that launch
// is about to produce. False means this process is already launching it.
func claimJob(jobID int, t string) bool {
	launchLock.Lock()
	defer launchLock.Unlock()
	if _, taken := claimed[jobID]; taken {
		return false
	}
	claimed[jobID] = t
	return true
}

func unclaimJob(jobID int) {
	launchLock.Lock()
	defer launchLock.Unlock()
	delete(claimed, jobID)
}

// maxRunnerPages bounds the runner list walk, for the same reason as
// maxRepoPages: a partial list would overstate the idle runners.
const maxRunnerPages = 10

type runnersReply struct {
	Runners []struct {
		Name string `json:"name"`
		Busy bool   `json:"busy"`
	} `json:"runners"`
}

// busyRunners returns the names of this tenant's registered runners that are
// running a job right now. It is the one thing a VM list cannot say: a VM that
// exists is idle, booting or busy, and only an idle or booting one can take a
// queued job. The list is read where the mint registers runners (the org, or
// each registered repo of a user tenant). A registration whose VM is gone shows
// up here too, so callers must never count this list as capacity.
func busyRunners(token string) (map[string]struct{}, error) {
	var bases []string
	switch parseTargetKind(targetKind) {
	case kindOrg:
		bases = []string{"/orgs/" + target}
	case kindUser:
		for _, r := range repos {
			bases = append(bases, "/repos/"+target+"/"+r)
		}
	default:
		return nil, fmt.Errorf("unknown BROKER_TARGET_KIND %q", targetKind)
	}
	busy := make(map[string]struct{})
	for _, base := range bases {
		done := false
		for page := 1; page <= maxRunnerPages && !done; page++ {
			raw, err := gh("GET", fmt.Sprintf("%s/actions/runners?per_page=%d&page=%d", base, listPage, page), token, nil)
			if err != nil {
				return nil, fmt.Errorf("cannot list the runners of %s: %w", base, err)
			}
			var reply runnersReply
			if err := json.Unmarshal(raw, &reply); err != nil {
				return nil, fmt.Errorf("runner list of %s is not the expected JSON: %w", base, err)
			}
			for _, r := range reply.Runners {
				if r.Busy {
					busy[r.Name] = struct{}{}
				}
			}
			done = len(reply.Runners) < listPage
		}
		if !done {
			return nil, fmt.Errorf("%s has more than %d runners: refusing a partial list", base, listPage*maxRunnerPages)
		}
	}
	return busy, nil
}

// parseInstanceName splits the name spawn gives an instance,
// ci-<tier>-<jobID>-<hex6>. The job id and the hex are the last two
// dash-separated fields, so a tier name containing a dash cannot shift them.
func parseInstanceName(name string) (tier string, jobID int, ok bool) {
	fields := strings.Split(name, "-")
	if len(fields) < 4 || fields[0] != "ci" {
		return "", 0, false
	}
	id, err := strconv.Atoi(fields[len(fields)-2])
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return strings.Join(fields[1:len(fields)-2], "-"), id, true
}

// jobIDFromInstanceName is the job id of an instance name.
func jobIDFromInstanceName(name string) (int, bool) {
	_, id, ok := parseInstanceName(name)
	return id, ok
}

// idleRunners counts, per tier, the runners that can still take a queued job:
// VMs that are not busy (idle or still booting), plus launches in flight that
// have no VM yet.
//
// GitHub gives an idle JIT runner ANY queued job that matches its labels, not
// the job it was spawned for, so whether a VM is named for a job says nothing
// about whether that job will get a runner. A VM with no registration yet counts
// as booting; a registration with no VM is not counted at all.
func idleRunners(vmNames []string, busy map[string]struct{}) map[string]int {
	idle := make(map[string]int)
	idleVMJobs := make(map[int]struct{}, len(vmNames))
	for _, n := range vmNames {
		t, id, ok := parseInstanceName(n)
		if !ok {
			continue
		}
		if _, isBusy := busy[n]; isBusy {
			continue
		}
		idle[t]++
		idleVMJobs[id] = struct{}{}
	}
	launchLock.Lock()
	defer launchLock.Unlock()
	for id, t := range claimed {
		// Launch holds the claim while its VM is already listed: one runner.
		if _, listed := idleVMJobs[id]; listed {
			continue
		}
		idle[t]++
	}
	return idle
}

// reconcileOnce spawns runners for the queued jobs of this tenant that no
// runner will pick up. It decides per tier, by count: queued jobs that resolve
// to a tier, minus that tier's idle and booting runners, is how many to spawn.
// Denied and ignored jobs are left alone: the webhook path already cancelled the
// denied ones, and cancelling again every pass would only fill the journal.
func (s *server) reconcileOnce(ctx context.Context, token string) {
	jobs, err := queuedJobs(token)
	if err != nil {
		logf("ERROR", "reconcile: %v", err)
		return
	}
	wanted := make(map[string][]queuedJob)
	for _, j := range jobs {
		if dec := decideJobAction(j.Labels, j.Repo, s.tiers, ""); dec.action == actionSpawn {
			wanted[dec.tier] = append(wanted[dec.tier], j)
		}
	}
	if len(wanted) == 0 {
		return
	}
	names, err := runningRunnersAll(s.tiers)
	if err != nil {
		logf("ERROR", "reconcile: %v", err)
		return
	}
	// Spawning on a guess about which runners are busy is the bug this count
	// exists to avoid; an unreadable list waits for the next pass.
	busy, err := busyRunners(token)
	if err != nil {
		logf("ERROR", "reconcile: %v", err)
		return
	}
	idle := idleRunners(names, busy)
	for t, queued := range wanted {
		short := len(queued) - idle[t]
		for _, j := range queued {
			if short <= 0 || ctx.Err() != nil {
				break
			}
			launchLock.Lock()
			_, inFlight := claimed[j.ID]
			launchLock.Unlock()
			if inFlight {
				continue // counted in idle already
			}
			logf("INFO", "reconcile: tier %s has %d queued job(s) and %d idle runner(s) — spawning for job %d (%s)", t, len(queued), idle[t], j.ID, j.Repo)
			short--
			s.track(func() { s.safeSpawn(j.ID, t, j.Repo) })
		}
	}
}

// reconcileLoop reconciles once at startup and then every reconcileInterval
// until ctx is cancelled. The startup pass is what rescues jobs that arrived
// while the broker was down.
//
// The loop is deliberately not tracked by the server's WaitGroup: a pass checks
// ctx before each spawn, so shutdown never abandons a half-built VM, and a
// wedged API call delays nothing but the next pass.
func (s *server) reconcileLoop(ctx context.Context, tokens tokenSource) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() == nil {
			if token, err := tokens.Get(); err != nil {
				logf("ERROR", "reconcile: %v", err)
			} else {
				s.reconcileOnce(ctx, token)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
