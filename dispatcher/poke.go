package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// pokeUnit is the converge service every poke target starts (contract G4).
	pokeUnit = "nuk-converge.service"
	// stderrLogLimit bounds how much of a failed command's stderr reaches the log.
	stderrLogLimit = 200
	// defaultGrace bounds how long Run waits for a killed child's pipes to close.
	defaultGrace = 5 * time.Second
)

// Runner runs one command. It is the seam that keeps tests off the real `incus`;
// argv is never passed through a shell.
type Runner interface {
	// Run executes a command and returns its stderr; stdout is discarded.
	Run(ctx context.Context, argv []string) (stderr string, err error)
	// Output executes a command and returns both streams, for commands whose
	// output carries the result.
	Output(ctx context.Context, argv []string) (stdout, stderr string, err error)
}

// Report identifies one converge job a poke should report: the commit, the source
// that dispatched it, and the installation whose token may post the status.
type Report struct {
	Repo         string
	Commit       string
	Source       string
	Installation int
}

// StatusPoster publishes one commit status. Implementations must tolerate concurrent
// calls: targets are poked in parallel.
type StatusPoster interface {
	Post(ctx context.Context, job Report, statusContext, state, description string) error
}

// ExecRunner is the production Runner: os/exec, argv only. The zero value is ready.
type ExecRunner struct {
	// Grace is how long Run waits, after killing a command, for its output pipes to
	// close; zero means defaultGrace.
	Grace time.Duration
}

// Run executes argv[0] with argv[1:] and discards stdout. A non-zero exit or a
// context deadline is an error.
func (r ExecRunner) Run(ctx context.Context, argv []string) (string, error) {
	_, stderr, err := r.exec(ctx, argv)
	return stderr, err
}

// Output executes argv[0] with argv[1:]. A non-zero exit or a context deadline is
// an error; both streams are returned regardless so the caller can log them.
func (r ExecRunner) Output(ctx context.Context, argv []string) (string, string, error) {
	return r.exec(ctx, argv)
}

// exec is the one place that builds an os/exec.Cmd: argv only, never a shell.
func (r ExecRunner) exec(ctx context.Context, argv []string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A grandchild that inherited our stderr pipe must not pin the goroutine after
	// the kill: give up on the pipes after the grace period.
	cmd.WaitDelay = r.Grace
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultGrace
	}
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// pokeArgv is the contract command: start nuk-converge inside one VM and wait for it
// to finish, so the poke ends only when that VM's converge has.
func pokeArgv(t PokeTarget) []string {
	return []string{"incus", "exec", "--project", t.Project, t.VM, "--",
		"systemctl", "start", pokeUnit}
}

// statusArgv reads the converge outcome the unit just wrote on that VM.
func statusArgv(t PokeTarget) []string {
	return []string{"incus", "exec", "--project", t.Project, t.VM, "--",
		"cat", convergeStatusPath}
}

// Poke starts nuk-converge on every target in parallel and waits for each, each under
// its own timeout. It never fails the caller: a stopped or absent VM is logged and
// the remaining targets are still poked. The poke carries no content and no
// authority, so a lost one only delays convergence.
//
// job and poster are the converge report and its poster; nil means no reporting.
// Reporting fails open: a post error is logged and changes nothing else.
func Poke(ctx context.Context, run Runner, log *slog.Logger, timeout time.Duration, targets []PokeTarget, job *Report, poster StatusPoster) {
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pokeOne(ctx, run, log, timeout, target, job, poster)
		}()
	}
	wg.Wait()
}

func pokeOne(ctx context.Context, run Runner, log *slog.Logger, timeout time.Duration, target PokeTarget, job *Report, poster StatusPoster) {
	name := target.Project + "/" + target.VM
	startStderr, startErr := runWithTimeout(ctx, run, timeout, pokeArgv(target))
	if startErr != nil {
		log.Error(fmt.Sprintf("poke %s failed", name),
			"err", startErr, "stderr", clip(strings.TrimSpace(startStderr), stderrLogLimit))
	} else {
		log.Info(fmt.Sprintf("poked %s", name))
	}
	if job == nil || poster == nil {
		return
	}
	statusJSON, readStderr, readErr := outputWithTimeout(ctx, run, timeout, statusArgv(target))
	var state, description string
	if readErr != nil {
		state, description = unreadableState(readStderr, startStderr, startErr)
	} else {
		state, description = ConvergeState(job.Source, job.Commit, statusJSON)
	}
	statusContext := statusContextPrefix + target.VM
	if err := poster.Post(ctx, *job, statusContext, state, description); err != nil {
		log.Error(fmt.Sprintf("status post %s failed", statusContext), "err", err)
		return
	}
	log.Info("reported "+statusContext, "state", state, "description", description)
}

// unreadableState reports a host whose status file could not be read, carrying the
// incus stderr (and the unit start's, when that failed too) so the log and the
// status say why.
func unreadableState(readStderr, startStderr string, startErr error) (string, string) {
	parts := []string{unreadableStatus}
	if s := strings.TrimSpace(readStderr); s != "" {
		parts = append(parts, clip(s, stderrLogLimit))
	}
	if startErr != nil {
		if s := strings.TrimSpace(startStderr); s != "" {
			parts = append(parts, clip(s, stderrLogLimit))
		}
	}
	return stateError, clip(strings.Join(parts, ": "), statusDescriptionLimit)
}

func runWithTimeout(ctx context.Context, run Runner, timeout time.Duration, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return run.Run(ctx, argv)
}

func outputWithTimeout(ctx context.Context, run Runner, timeout time.Duration, argv []string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return run.Output(ctx, argv)
}
