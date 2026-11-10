package dispatcher

import (
	"fmt"
	"slices"
	"strings"
)

// OrgJob is the closed set of steps an org plan consists of. A plan lists them in
// the order they must run: backup, then detection (which may trigger restic), then
// the mirror, so the mirror never publishes a state the backup has not seen.
type OrgJob int

const (
	orgJobUnset OrgJob = iota
	// OrgJobSicherung runs sichern.sh <repo> (append-only backup clone).
	OrgJobSicherung
	// OrgJobErkennung measures the diffstat, rates the event, snapshots with restic on
	// a critical finding and appends the findings to the befund file.
	OrgJobErkennung
	// OrgJobSpiegelFakten dispatches the mirror's facts workflow for the repo.
	OrgJobSpiegelFakten
	// OrgJobSpiegelOrg dispatches the mirror's org-wide workflow.
	OrgJobSpiegelOrg
	// orgJobCount is the number of defined values; tests loop up to it.
	orgJobCount
)

// String names the job for logs; out-of-range values say so instead of panicking.
func (j OrgJob) String() string {
	switch j {
	case OrgJobSicherung:
		return "sicherung"
	case OrgJobErkennung:
		return "erkennung"
	case OrgJobSpiegelFakten:
		return "spiegel-fakten"
	case OrgJobSpiegelOrg:
		return "spiegel-org"
	default: // orgJobUnset, orgJobCount and anything out of range
		return fmt.Sprintf("invalid org job %d", int(j))
	}
}

// OrgPlan is what RouteOrg decided for one delivery.
type OrgPlan struct {
	// Event is the X-GitHub-Event value, Body the verbatim verified body.
	Event string
	Body  []byte
	// Repo is the validated repository name, empty when the payload gave none or an
	// unusable one. Only a validated name ever reaches argv or a workflow input.
	Repo string
	// BadName says the payload carried a repository name that is empty or outside
	// the allowed alphabet: nothing runs and the detection job records an
	// unpruefbar finding instead.
	BadName bool
	// Jobs run in this order, one repo at a time.
	Jobs []OrgJob
}

// orgFields are the payload fields RouteOrg reads beyond Payload. Each is decoded on
// its own and leniently: a wrong type makes just that field absent, because the
// detection itself reports unreadable payloads.
type orgFields struct {
	action        string
	repoName      string
	repoNameSet   bool // repository.name present as a string
	defaultBranch string
}

func readOrgFields(body []byte) orgFields {
	var f orgFields
	root, ok := decodeObject(body)
	if !ok {
		return f
	}
	f.action, _ = asString(root["action"])
	if repo, ok := decodeObject(root["repository"]); ok {
		f.repoName, f.repoNameSet = asString(repo["name"])
		f.defaultBranch, _ = asString(repo["default_branch"])
	}
	return f
}

// Pull-request actions that change what the mirror shows.
func mirroredPullRequestAction(action string) bool {
	switch action {
	case "opened", "closed", "reopened", "synchronize", "ready_for_review", "converted_to_draft":
		return true
	default:
		return false
	}
}

// repositoryMirror says what a repository action does to the mirror: facts for the
// repo, the org-wide workflow (a deleted repo has no facts left) or nothing.
type repositoryMirror int

const (
	mirrorNothing repositoryMirror = iota
	mirrorFakten
	mirrorOrg
)

func repositoryMirrorFor(action string) repositoryMirror {
	switch action {
	case "created", "archived", "unarchived", "renamed", "edited", "privatized", "publicized", "transferred":
		return mirrorFakten
	case "deleted":
		return mirrorOrg
	default:
		return mirrorNothing
	}
}

// RouteOrg is Route for an instance in org mode (spec K1). Same order of checks: a
// signed ping is answered first, an unknown installation or foreign repository is
// rejected, then a new v* tag on a poke repo pokes as before (it takes precedence).
// Everything else becomes an org plan or an ignore with a reason. The events of the
// mirror repo itself never start a mirror job: the generator commits there, and the
// mirror's own state hash would otherwise trigger new runs without end.
func RouteOrg(event string, body []byte, p Payload, t Table) Decision {
	if event == "ping" {
		return Decision{Action: ActionPong}
	}
	_, repo, rejected := authorize(p, t)
	if rejected != nil {
		return *rejected
	}
	if event == "push" && !p.Deleted && strings.HasPrefix(p.Ref, tagRefPrefix) {
		if targets := t.Pokes[repo]; len(targets) > 0 {
			return Decision{Action: ActionPoke, Targets: slices.Clone(targets)}
		}
	}
	if t.Org == nil {
		return Decision{Action: ActionIgnore, Reason: "org mode is not configured"}
	}

	f := readOrgFields(body)
	jobs, reason := orgJobsFor(event, p, f, repo == t.Org.spiegelFullName())
	if len(jobs) == 0 {
		return Decision{Action: ActionIgnore, Reason: reason}
	}
	plan := OrgPlan{Event: event, Body: body, Jobs: jobs}
	switch {
	case validRepoName(f.repoName):
		plan.Repo = f.repoName
	case !f.repoNameSet && repoOptional(event):
		// The detection reports a payload without a repository by itself.
		plan.Jobs = []OrgJob{OrgJobErkennung}
	default:
		plan.BadName = true
		plan.Jobs = []OrgJob{OrgJobErkennung}
	}
	return Decision{Action: ActionOrg, Org: plan}
}

// repoOptional lists the events the detection rates itself, including broken payloads.
func repoOptional(event string) bool {
	return event == "push" || event == "delete" || event == "repository"
}

// orgJobsFor is the decision table. No jobs means ignore, with the reason.
func orgJobsFor(event string, p Payload, f orgFields, isSpiegel bool) ([]OrgJob, string) {
	// mirror gates a mirror job behind the loop protection.
	mirror := func(jobs ...OrgJob) ([]OrgJob, string) {
		if isSpiegel {
			return nil, event + " of the mirror repo never starts a mirror job (loop protection)"
		}
		return jobs, ""
	}
	switch event {
	case "push":
		jobs := []OrgJob{OrgJobSicherung, OrgJobErkennung}
		onDefault := f.defaultBranch != "" && p.Ref == "refs/heads/"+f.defaultBranch && !p.Deleted
		if onDefault && !isSpiegel {
			jobs = append(jobs, OrgJobSpiegelFakten)
		}
		return jobs, ""
	case "delete":
		return []OrgJob{OrgJobSicherung, OrgJobErkennung}, ""
	case "repository":
		jobs := []OrgJob{OrgJobErkennung}
		if isSpiegel {
			return jobs, ""
		}
		switch repositoryMirrorFor(f.action) {
		case mirrorFakten:
			jobs = append(jobs, OrgJobSpiegelFakten)
		case mirrorOrg:
			jobs = append(jobs, OrgJobSpiegelOrg)
		case mirrorNothing:
		}
		return jobs, ""
	case "pull_request":
		if !mirroredPullRequestAction(f.action) {
			return nil, "pull_request action " + clip(orDash(f.action), logFieldLimit) + " does not change the mirror"
		}
		return mirror(OrgJobSpiegelFakten)
	case "check_suite", "workflow_run":
		if f.action != "completed" {
			return nil, event + " action " + clip(orDash(f.action), logFieldLimit) + " is not completed"
		}
		return mirror(OrgJobSpiegelFakten)
	case "release":
		return mirror(OrgJobSpiegelFakten)
	case "workflow_job":
		return nil, "workflow_job on the org instance (no broker)"
	default:
		return nil, "event " + clip(orDash(event), logFieldLimit) + " is not handled by the org instance"
	}
}
