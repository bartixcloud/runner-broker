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

// claimJob marks jobID as being launched by this process, so the webhook path
// and the reconciler — which both see the same queued job — cannot launch two
// VMs for it. False means this process is already launching it.
func claimJob(jobID int) bool {
	launchLock.Lock()
	defer launchLock.Unlock()
	if _, taken := claimed[jobID]; taken {
		return false
	}
	claimed[jobID] = struct{}{}
	return true
}

func unclaimJob(jobID int) {
	launchLock.Lock()
	defer launchLock.Unlock()
	delete(claimed, jobID)
}

// coveredJobs returns the job ids that already have a runner: a ci-* instance
// on any backend a configured tier uses (named ci-<tier>-<jobID>-<hex>) and the
// jobs this process is launching right now.
func coveredJobs(tiers map[string]tier) (map[int]struct{}, error) {
	names, err := runningRunnersAll(tiers)
	if err != nil {
		return nil, err
	}
	out := make(map[int]struct{}, len(names)+len(claimed))
	for _, n := range names {
		if id, ok := jobIDFromInstanceName(n); ok {
			out[id] = struct{}{}
		}
	}
	launchLock.Lock()
	defer launchLock.Unlock()
	for id := range claimed {
		out[id] = struct{}{}
	}
	return out, nil
}

// jobIDFromInstanceName parses the name spawn gives an instance. The job id is
// the second-to-last dash-separated field, so a tier name containing a dash
// cannot shift it.
func jobIDFromInstanceName(name string) (int, bool) {
	fields := strings.Split(name, "-")
	if len(fields) < 4 || fields[0] != "ci" {
		return 0, false
	}
	id, err := strconv.Atoi(fields[len(fields)-2])
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// reconcileOnce spawns a runner for every queued job of this tenant that
// resolves to a tier and has no runner yet. Denied and ignored jobs are left
// alone: the webhook path already cancelled the denied ones, and cancelling
// again every pass would only fill the journal.
func (s *server) reconcileOnce(ctx context.Context, token string) {
	jobs, err := queuedJobs(token)
	if err != nil {
		logf("ERROR", "reconcile: %v", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	covered, err := coveredJobs(s.tiers)
	if err != nil {
		logf("ERROR", "reconcile: %v", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if _, ok := covered[j.ID]; ok {
			continue
		}
		dec := decideJobAction(j.Labels, j.Repo, s.tiers, "")
		if dec.action != actionSpawn {
			continue
		}
		logf("INFO", "reconcile: job %d (%s, tier=%s) is queued with no runner — spawning", j.ID, j.Repo, dec.tier)
		s.track(func() { s.safeSpawn(j.ID, dec.tier, j.Repo) })
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
