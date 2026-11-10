package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------- ExecOutputRunner ----

func TestExecOutputRunnerCapturesStdoutAndStderr(t *testing.T) {
	out, err := ExecOutputRunner{}.RunOutput(t.Context(), Command{Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2"}})
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("err = %v, exit = %d", err, out.ExitCode)
	}
	if out.Stdout != "out\n" || out.Stderr != "err\n" || out.Truncated {
		t.Errorf("output = %+v", out)
	}
}

func TestExecOutputRunnerReportsTheExitCode(t *testing.T) {
	out, err := ExecOutputRunner{}.RunOutput(t.Context(), Command{Argv: []string{"/bin/sh", "-c", "echo partial; exit 75"}})
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if out.ExitCode != 75 || out.Stdout != "partial\n" {
		t.Errorf("output = %+v", out)
	}
}

func TestExecOutputRunnerStartFailureHasExitCodeMinusOne(t *testing.T) {
	out, err := ExecOutputRunner{}.RunOutput(t.Context(), Command{Argv: []string{"/nonexistent/bin"}})
	if err == nil || out.ExitCode != -1 {
		t.Errorf("err = %v, exit = %d, want an error and -1", err, out.ExitCode)
	}
	if _, err := (ExecOutputRunner{}).RunOutput(t.Context(), Command{}); err == nil {
		t.Error("an empty argv must be an error")
	}
}

func TestExecOutputRunnerKillsOnContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := ExecOutputRunner{Grace: 200 * time.Millisecond}.RunOutput(ctx, Command{Argv: []string{"sleep", "30"}})
	if err == nil || out.ExitCode != -1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, exit = %d, want the deadline error and -1", err, out.ExitCode)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v: the child was not killed", time.Since(start))
	}
}

func TestExecOutputRunnerAddsToTheEnvironment(t *testing.T) {
	out, err := ExecOutputRunner{}.RunOutput(t.Context(), Command{Argv: []string{"env"}, Env: []string{"SICHERUNG_WURZEL=/x/y"}})
	if err != nil || !strings.Contains(out.Stdout, "SICHERUNG_WURZEL=/x/y\n") {
		t.Errorf("err = %v, stdout lacks the variable: %q", err, out.Stdout)
	}
}

func TestExecOutputRunnerCapsOutputWithoutBlockingTheChild(t *testing.T) {
	out, err := ExecOutputRunner{MaxOutput: 16}.RunOutput(t.Context(),
		Command{Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 2000 ]; do echo 0123456789; i=$((i+1)); done"}})
	if err != nil {
		t.Fatalf("err = %v: a chatty child must not fail on the cap", err)
	}
	if len(out.Stdout) != 16 || !out.Truncated {
		t.Errorf("stdout = %d bytes, truncated = %v, want 16 and true", len(out.Stdout), out.Truncated)
	}
	big, err := ExecOutputRunner{MaxOutput: 16}.RunOutput(t.Context(),
		Command{Argv: []string{"echo", strings.Repeat("a", 100)}, MaxStdout: 1000})
	if err != nil || big.Truncated || len(big.Stdout) != 101 {
		t.Errorf("per-command cap: err = %v, %d bytes, truncated = %v", err, len(big.Stdout), big.Truncated)
	}
}

// -------------------------------------------------- GitHubWorkflowDispatcher ----

type fakeTokens struct {
	token string
	err   error
}

func (f fakeTokens) Get() (string, error) { return f.token, f.err }

type dispatchSeen struct {
	method, path, auth, accept, version, ctype string
	body                                       string
}

func dispatchServer(t *testing.T, status int, reply string) (*GitHubWorkflowDispatcher, *[]dispatchSeen) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []dispatchSeen
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, dispatchSeen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept"),
			r.Header.Get("X-GitHub-Api-Version"), r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(ts.Close)
	return &GitHubWorkflowDispatcher{
		APIBase: ts.URL, SpiegelRepo: "ekd-akd/spiegel", Ref: "main",
		Tokens: fakeTokens{token: "tok-secret-123"}, Client: ts.Client(),
	}, &seen
}

func TestWorkflowDispatchPostsTheDocumentedRequest(t *testing.T) {
	d, seen := dispatchServer(t, http.StatusNoContent, "")
	if err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "fakten.yml", Repo: "demo"}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "org.yml"}); err != nil {
		t.Fatalf("Dispatch org: %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("saw %d requests", len(*seen))
	}
	first, second := (*seen)[0], (*seen)[1]
	if first.method != "POST" || first.path != "/repos/ekd-akd/spiegel/actions/workflows/fakten.yml/dispatches" {
		t.Errorf("first request = %s %s", first.method, first.path)
	}
	if first.auth != "Bearer tok-secret-123" || first.accept != "application/vnd.github+json" ||
		first.version != "2022-11-28" || first.ctype != "application/json" {
		t.Errorf("headers = %+v", first)
	}
	if first.body != `{"ref":"main","inputs":{"repo":"demo"}}` {
		t.Errorf("fakten body = %s", first.body)
	}
	if second.path != "/repos/ekd-akd/spiegel/actions/workflows/org.yml/dispatches" || second.body != `{"ref":"main"}` {
		t.Errorf("org request = %+v", second)
	}
}

func TestWorkflowDispatchRefusesAnUnsafeRepoInput(t *testing.T) {
	d, seen := dispatchServer(t, http.StatusNoContent, "")
	for _, repo := range []string{"-x", "a b", "a/b", "", strings.Repeat("a", 101)} {
		if repo == "" {
			continue // empty means "no input" (org.yml), tested above
		}
		if err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "fakten.yml", Repo: repo}); err == nil {
			t.Errorf("repo %q was dispatched", repo)
		}
	}
	if err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "a/b.yml"}); err == nil {
		t.Error("a workflow with a path was dispatched")
	}
	if len(*seen) != 0 {
		t.Errorf("%d requests left the process", len(*seen))
	}
}

func TestWorkflowDispatchNon204IsAnErrorWithoutTheToken(t *testing.T) {
	d, _ := dispatchServer(t, http.StatusUnprocessableEntity, `{"message":"Workflow does not have 'workflow_dispatch' trigger"}`)
	err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "fakten.yml", Repo: "demo"})
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "workflow_dispatch") {
		t.Fatalf("err = %v, want one with the status and GitHub's message", err)
	}
	if strings.Contains(err.Error(), "tok-secret-123") {
		t.Errorf("the token leaked into the error: %v", err)
	}
}

func TestWorkflowDispatchClipsAHugeErrorBody(t *testing.T) {
	d, _ := dispatchServer(t, http.StatusInternalServerError, strings.Repeat("x", 10_000))
	err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "fakten.yml", Repo: "demo"})
	if err == nil || len(err.Error()) > 600 {
		t.Errorf("error is %d bytes, want a clipped one", len(err.Error()))
	}
}

func TestWorkflowDispatchTokenFailureSendsNothing(t *testing.T) {
	d, seen := dispatchServer(t, http.StatusNoContent, "")
	d.Tokens = fakeTokens{err: errors.New("mint failed")}
	err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "fakten.yml", Repo: "demo"})
	if err == nil || !strings.Contains(err.Error(), "mint failed") || len(*seen) != 0 {
		t.Errorf("err = %v, %d requests", err, len(*seen))
	}
	d.Tokens = fakeTokens{token: ""}
	if err := d.Dispatch(t.Context(), WorkflowRequest{Workflow: "org.yml"}); err == nil || len(*seen) != 0 {
		t.Errorf("an empty token must be refused: err = %v, %d requests", err, len(*seen))
	}
}

func TestWorkflowDispatchHonoursTheContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server only watches the connection once the body is read
		<-r.Context().Done()
	}))
	defer ts.Close()
	d := &GitHubWorkflowDispatcher{APIBase: ts.URL, SpiegelRepo: "ekd-akd/spiegel", Ref: "main",
		Tokens: fakeTokens{token: "t"}, Client: ts.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := d.Dispatch(ctx, WorkflowRequest{Workflow: "org.yml"}); err == nil {
		t.Error("a hanging API must end with the context")
	}
}

func TestWorkflowRequestBodyIsStructShaped(t *testing.T) {
	b, err := json.Marshal(dispatchBody{Ref: "main", Inputs: &dispatchInputs{Repo: "demo"}})
	if err != nil || string(b) != `{"ref":"main","inputs":{"repo":"demo"}}` {
		t.Errorf("body = %s, %v", b, err)
	}
}
