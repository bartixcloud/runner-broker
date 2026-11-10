package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

const (
	// defaultMaxOutput caps what one command's stdout and stderr keep each.
	defaultMaxOutput = 1 << 20
	// dispatchErrBodyLimit bounds how much of GitHub's error reply reaches an error.
	dispatchErrBodyLimit = 300
	defaultDispatchTime  = 30 * time.Second
)

// Command is one process invocation: argv (never a shell) plus variables added to
// the dispatcher's own environment.
type Command struct {
	Argv []string
	Env  []string
	// MaxStdout overrides the runner's cap for this command (the diffstat needs more
	// than a script's log output); zero keeps the runner default.
	MaxStdout int
}

// CommandOutput is what a finished command left. ExitCode is -1 when the process
// never exited on its own (failed to start, killed by the context, signalled).
type CommandOutput struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool // stdout exceeded its cap; the rest was discarded
}

// OutputRunner is the seam for commands whose output matters. Runner discards
// stdout and stays as it is; this is the small second interface. A non-nil error
// covers every failure: non-zero exit, start failure, context end.
type OutputRunner interface {
	RunOutput(ctx context.Context, cmd Command) (CommandOutput, error)
}

// ExecOutputRunner is the production OutputRunner: os/exec, argv only. The zero
// value is ready.
type ExecOutputRunner struct {
	// Grace is how long RunOutput waits, after killing a command, for its pipes to
	// close; zero means defaultGrace.
	Grace time.Duration
	// MaxOutput caps stdout and stderr each; zero means defaultMaxOutput.
	MaxOutput int
}

// RunOutput executes cmd.Argv. Output beyond the cap is discarded but still drained,
// so a chatty child can neither block on a full pipe nor exhaust memory.
func (r ExecOutputRunner) RunOutput(ctx context.Context, cmd Command) (CommandOutput, error) {
	if len(cmd.Argv) == 0 {
		return CommandOutput{ExitCode: -1}, errors.New("empty argv")
	}
	limit := orInt(cmd.MaxStdout, orInt(r.MaxOutput, defaultMaxOutput))
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	stdout, stderr := &cappedBuffer{limit: limit}, &cappedBuffer{limit: orInt(r.MaxOutput, defaultMaxOutput)}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = r.Grace
	if c.WaitDelay == 0 {
		c.WaitDelay = defaultGrace
	}
	err := c.Run()
	out := CommandOutput{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), Truncated: stdout.truncated}
	switch {
	case err == nil:
		out.ExitCode = 0
	case ctx.Err() != nil:
		out.ExitCode = -1
		err = fmt.Errorf("%w: %w", ctx.Err(), err)
	default:
		out.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
		}
	}
	return out, err
}

func orInt(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// cappedBuffer keeps the first limit bytes and swallows the rest, always claiming
// to have taken everything so the writer never sees a short write.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	kept := p
	if room < len(p) {
		c.truncated = true
		kept = p[:max(room, 0)]
	}
	c.buf.Write(kept)
	return len(p), nil
}

// ------------------------------------------------------ workflow dispatch ----

// WorkflowRequest is one workflow_dispatch of the mirror repo. Repo, when set, is
// the workflow's single input repo=<name>; org.yml takes none.
type WorkflowRequest struct {
	Workflow string
	Repo     string
}

// WorkflowDispatcher is the seam for the GitHub call that starts a mirror workflow.
type WorkflowDispatcher interface {
	Dispatch(ctx context.Context, req WorkflowRequest) error
}

// TokenSource yields an installation token; *appauth.InstallationToken is one.
type TokenSource interface {
	Get() (string, error)
}

var _ TokenSource = (*appauth.InstallationToken)(nil)

type dispatchInputs struct {
	Repo string `json:"repo"`
}

type dispatchBody struct {
	Ref    string          `json:"ref"`
	Inputs *dispatchInputs `json:"inputs,omitempty"`
}

// GitHubWorkflowDispatcher POSTs
// <APIBase>/repos/<SpiegelRepo>/actions/workflows/<workflow>/dispatches with an
// installation token of the akd-spiegel App. Neither the token nor the response
// body beyond a clipped excerpt ever reaches an error.
type GitHubWorkflowDispatcher struct {
	APIBase     string // https://api.github.com in production
	SpiegelRepo string // "<owner>/<name>"
	Ref         string
	Tokens      TokenSource
	Client      *http.Client
}

// Dispatch validates req (it reaches a URL path and a workflow input), then calls
// the API. Anything but 204 is an error.
func (d *GitHubWorkflowDispatcher) Dispatch(ctx context.Context, req WorkflowRequest) error {
	if !workflowFilePattern.MatchString(req.Workflow) {
		return fmt.Errorf("refusing to dispatch workflow %q", clip(req.Workflow, logFieldLimit))
	}
	body := dispatchBody{Ref: d.Ref}
	if req.Repo != "" {
		if !validRepoName(req.Repo) {
			return errors.New("refusing to dispatch with an invalid repo input")
		}
		body.Inputs = &dispatchInputs{Repo: req.Repo}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode dispatch body: %w", err)
	}
	token, err := d.Tokens.Get()
	if err != nil {
		return fmt.Errorf("installation token for %s: %w", d.SpiegelRepo, err)
	}
	if token == "" {
		return errors.New("installation token is empty")
	}
	endpoint := d.APIBase + "/repos/" + d.SpiegelRepo + "/actions/workflows/" + url.PathEscape(req.Workflow) + "/dispatches"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build dispatch request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "bcx-dispatcher")

	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: defaultDispatchTime}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// *url.Error carries the URL, never a header.
		return fmt.Errorf("dispatch %s: %w", req.Workflow, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, dispatchErrBodyLimit))
	return fmt.Errorf("dispatch %s: status %d: %s", req.Workflow, resp.StatusCode, clip(string(bytes.TrimSpace(excerpt)), dispatchErrBodyLimit))
}
