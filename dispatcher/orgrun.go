package dispatcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	scriptSichern = "sichern.sh"
	scriptRestic  = "restic-sichern.sh"
	envWurzel     = "SICHERUNG_WURZEL"

	// diffTimeout bounds the diffstat measurement; diffMaxStdout is the cap for its
	// output (one line per changed file, far above the log-output default).
	diffTimeout   = 2 * time.Minute
	diffMaxStdout = 64 << 20

	// logOutputLimit bounds how much of a script's output reaches one log line.
	logOutputLimit = 200
)

var (
	// shaPattern is a git object id as a hex string (SHA-1, or SHA-256 in new repos):
	// only hex, so no payload value can act as an option of `git diff`.
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	nullShaPattern = regexp.MustCompile(`^0+$`)
)

// OrgRunner executes an OrgPlan: the Go carrier of ereignis.py plus the mirror
// dispatch. Steps never abort each other: a failing backup or snapshot is itself a
// finding, and the mirror runs regardless. The zero value is not usable; fill Cfg,
// Cmd, Workflows and Log. One runner is shared by all jobs (the file write is locked).
type OrgRunner struct {
	Cfg       OrgConfig
	Cmd       OutputRunner
	Workflows WorkflowDispatcher
	Log       *slog.Logger
	// Notify, when set, is told about every finding right after it is recorded.
	Notify Notifier

	fileMu sync.Mutex
}

// runState carries what earlier steps of one plan tell later ones.
type runState struct {
	gesichert    bool
	ref          *string
	fehlschlaege []Befund // step failures, written together with the detection's findings
}

// Run executes the plan's jobs in order. It never returns an error: every failure
// ends up as a finding in the befund file or, for the mirror, in the log.
func (o *OrgRunner) Run(ctx context.Context, plan OrgPlan) {
	st := &runState{ref: eventRef(plan.Event, plan.Body)}
	for _, job := range plan.Jobs {
		o.guarded(job, plan, func() { o.runJob(ctx, job, plan, st) })
	}
	// Defensive: a plan without the detection job would otherwise lose its failures.
	o.write(st.fehlschlaege)
}

// Abandon records that a plan was dropped before it started (the dispatcher shut
// down while it waited): the event must not vanish without a trace.
func (o *OrgRunner) Abandon(plan OrgPlan) {
	repo := plan.Repo
	if repo == "" {
		repo = unbekanntesRepo
	}
	o.write([]Befund{unpruefbar(repo, eventRef(plan.Event, plan.Body), fmt.Sprintf(
		"Ereignis %s nicht verarbeitet: Dispatcher wurde beendet", clip(plan.Event, logFieldLimit)))})
}

// guarded keeps a panic in one step from killing the daemon or skipping the next
// steps; it is logged loudly.
func (o *OrgRunner) guarded(job OrgJob, plan OrgPlan, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			o.Log.Error("org job panicked", "job", job.String(), "repo", plan.Repo,
				"panic", clip(fmt.Sprint(rec), logOutputLimit))
		}
	}()
	fn()
}

func (o *OrgRunner) runJob(ctx context.Context, job OrgJob, plan OrgPlan, st *runState) {
	switch job {
	case OrgJobSicherung:
		o.sicherung(ctx, plan, st)
	case OrgJobErkennung:
		o.erkennung(ctx, plan, st)
	case OrgJobSpiegelFakten:
		o.dispatch(ctx, plan, WorkflowRequest{Workflow: o.Cfg.SpiegelWorkflow, Repo: plan.Repo})
	case OrgJobSpiegelOrg:
		o.dispatch(ctx, plan, WorkflowRequest{Workflow: o.Cfg.SpiegelOrgWorkflow})
	default: // orgJobUnset, orgJobCount and anything out of range fail loudly, never silently
		o.Log.Error("unhandled org job", "job", job.String(), "event", clip(plan.Event, logFieldLimit))
	}
}

// script runs one of the sicherung scripts for repo under its own timeout and logs
// its clipped output.
func (o *OrgRunner) script(ctx context.Context, name, repo string, timeout time.Duration) (CommandOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := o.Cmd.RunOutput(ctx, Command{
		Argv: []string{filepath.Join(o.Cfg.SicherungDir, name), repo},
		Env:  []string{envWurzel + "=" + o.Cfg.SicherungWurzel},
	})
	if err != nil {
		o.Log.Error(name+" failed", "repo", repo, "exit", out.ExitCode, "err", clip(err.Error(), logOutputLimit),
			"stdout", clip(strings.TrimSpace(out.Stdout), logOutputLimit),
			"stderr", clip(strings.TrimSpace(out.Stderr), logOutputLimit))
		return out, err
	}
	o.Log.Info(name+" finished", "repo", repo, "stdout", clip(strings.TrimSpace(out.Stdout), logOutputLimit))
	return out, nil
}

func exitText(out CommandOutput) string {
	if out.ExitCode >= 0 {
		return "Exit " + strconv.Itoa(out.ExitCode)
	}
	return "ohne Exit-Code"
}

func (o *OrgRunner) sicherung(ctx context.Context, plan OrgPlan, st *runState) {
	if plan.Repo == "" {
		return
	}
	out, err := o.script(ctx, scriptSichern, plan.Repo, o.Cfg.SicherungTimeout)
	if err != nil {
		st.fehlschlaege = append(st.fehlschlaege, unpruefbar(plan.Repo, st.ref,
			fmt.Sprintf("Sicherung fehlgeschlagen (%s %s)", scriptSichern, exitText(out))))
		return
	}
	st.gesichert = true
}

// erkennung is ereignis.py steps 2 to 5: diffstat, rating, restic on a critical
// finding, and the append to the befund file.
func (o *OrgRunner) erkennung(ctx context.Context, plan OrgPlan, st *runState) {
	if plan.BadName {
		st.fehlschlaege = append(st.fehlschlaege, unpruefbar(unbekanntesRepo, nil, fmt.Sprintf(
			"Repository-Name im Ereignis %s fehlt oder ist ungültig, kein Lauf", clip(plan.Event, logFieldLimit))))
		o.write(st.fehlschlaege)
		st.fehlschlaege = nil
		return
	}
	var diff *DiffStat
	if st.gesichert && plan.Repo != "" {
		diff = o.measureDiffstat(ctx, plan, st)
	}
	befunde := Bewerte(plan.Event, plan.Body, o.Cfg.Schwellen, diff)

	// The findings are recorded and announced BEFORE restic runs: a snapshot to a slow or
	// hanging target can take hours, and a critical finding must not wait for it.
	o.write(append(befunde, st.fehlschlaege...))
	st.fehlschlaege = nil

	if plan.Repo != "" {
		for _, b := range befunde {
			if b.Schwere != SchwereKritisch {
				continue
			}
			if out, err := o.script(ctx, scriptRestic, plan.Repo, o.Cfg.ResticTimeout); err != nil {
				o.write([]Befund{unpruefbar(b.Repo, b.Ref,
					fmt.Sprintf("restic-Snapshot fehlgeschlagen (%s %s)", scriptRestic, exitText(out)))})
			}
			break // one snapshot per event: it covers the whole clone
		}
	}
}

// measureDiffstat counts deleted files and lines between before and after in the
// backup clone. nil means "not measurable": new or deleted refs (null id), ids that
// are not plain hex, a missing clone, or git failing. An output too large to count
// is reported as a finding rather than guessed.
func (o *OrgRunner) measureDiffstat(ctx context.Context, plan OrgPlan, st *runState) *DiffStat {
	head := readHead(plan.Body)
	for _, id := range []string{head.before, head.after} {
		if !shaPattern.MatchString(id) || nullShaPattern.MatchString(id) {
			return nil
		}
	}
	clone := filepath.Join(o.Cfg.SicherungWurzel, plan.Repo+".git")
	if info, err := os.Stat(clone); err != nil || !info.IsDir() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()
	out, err := o.Cmd.RunOutput(ctx, Command{
		Argv:      []string{"git", "-C", clone, "diff", "--numstat", "--summary", "--no-renames", head.before, head.after, "--"},
		MaxStdout: diffMaxStdout,
	})
	if err != nil {
		o.Log.Warn("diffstat not measurable", "repo", plan.Repo, "exit", out.ExitCode)
		return nil
	}
	if out.Truncated {
		st.fehlschlaege = append(st.fehlschlaege, unpruefbar(plan.Repo, st.ref,
			"Diffstat nicht messbar: Ausgabe von git diff zu groß"))
		return nil
	}
	stat := parseDiffstat(out.Stdout)
	return &stat
}

// parseDiffstat reads `git diff --numstat --summary`. --shortstat would not do: it
// counts changed files, not deleted ones. Binary files report "-" and count as 0 lines.
func parseDiffstat(out string) DiffStat {
	var stat DiffStat
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, " delete mode ") {
			stat.DateienGeloescht++
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 && isASCIIDigits(parts[1]) {
			stat.ZeilenGeloescht += n
		}
	}
	return stat
}

func isASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// dispatch starts a mirror workflow. A failure is logged, never fatal: the mirror
// is a view, and the next event regenerates it.
func (o *OrgRunner) dispatch(ctx context.Context, plan OrgPlan, req WorkflowRequest) {
	ctx, cancel := context.WithTimeout(ctx, defaultDispatchTime)
	defer cancel()
	if err := o.Workflows.Dispatch(ctx, req); err != nil {
		o.Log.Error("mirror dispatch failed", "workflow", req.Workflow, "repo", plan.Repo,
			"err", clip(err.Error(), dispatchErrBodyLimit+100))
		return
	}
	o.Log.Info("mirror dispatched", "workflow", req.Workflow, "repo", plan.Repo)
}

// write appends the findings as JSON lines in a single write call, so lines of
// concurrent runs never interleave. Findings are logged as well: if the file cannot
// be written they must not vanish.
func (o *OrgRunner) write(befunde []Befund) {
	if len(befunde) == 0 {
		return
	}
	for _, b := range befunde {
		o.Log.Warn("befund", "art", string(b.Art), "repo", clip(b.Repo, logFieldLimit),
			"schwere", string(b.Schwere), "grund", clip(b.Grund, logOutputLimit))
	}
	if data, err := EncodeBefunde(befunde); err != nil {
		o.Log.Error("encoding befunde failed", "err", err)
	} else {
		o.appendFile(data)
	}
	o.notify(befunde)
}

// appendFile adds the encoded findings to the befund file under the file lock.
func (o *OrgRunner) appendFile(data []byte) {
	o.fileMu.Lock()
	defer o.fileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(o.Cfg.BefundDatei), 0o750); err != nil {
		o.Log.Error("creating the befund directory failed", "err", err)
		return
	}
	f, err := os.OpenFile(o.Cfg.BefundDatei, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		o.Log.Error("opening the befund file failed", "err", err)
		return
	}
	if _, err := f.Write(data); err != nil {
		o.Log.Error("writing the befund file failed", "err", err)
	}
	if err := f.Close(); err != nil {
		o.Log.Error("closing the befund file failed", "err", err)
	}
}

// notify tells the optional Notifier about the findings. It runs after the file append
// and under its own timeout; a failure is logged and never stops the plan.
func (o *OrgRunner) notify(befunde []Befund) {
	if o.Notify == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	if err := o.Notify.Notify(ctx, befunde); err != nil {
		o.Log.Error("notifying about befunde failed", "err", clip(err.Error(), logOutputLimit))
	}
}

// pushHead is the part of a payload the runner needs beyond the routing fields.
type pushHead struct {
	before, after string
}

func readHead(body []byte) pushHead {
	var h pushHead
	if root, ok := decodeObject(body); ok {
		h.before, _ = asString(root["before"])
		h.after, _ = asString(root["after"])
	}
	return h
}

// eventRef is the full ref findings of this event carry, or nil.
func eventRef(event string, body []byte) *string {
	root, ok := decodeObject(body)
	if !ok {
		return nil
	}
	ref, ok := asString(root["ref"])
	if !ok {
		return nil
	}
	if event != "delete" {
		return &ref
	}
	var refType string
	refType, _ = asString(root["ref_type"])
	switch refType {
	case "branch":
		full := "refs/heads/" + ref
		return &full
	case "tag":
		full := "refs/tags/" + ref
		return &full
	default:
		return nil
	}
}
