package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTokens is a tokenSource whose behaviour a test picks.
type fakeTokens struct {
	token    string
	err      error
	panicVal any
}

func (f fakeTokens) Get() (string, error) {
	if f.panicVal != nil {
		panic(f.panicVal)
	}
	return f.token, f.err
}

func newServer(tokens tokenSource) *server {
	return &server{tokens: tokens, tiers: tiersFixture, secrets: map[string]string{}}
}

// jobEvent is a workflow_job webhook body.
func jobEvent(action string, jobID, runID int, repo string, labels ...string) map[string]any {
	return map[string]any{
		"action":       action,
		"workflow_job": map[string]any{"id": jobID, "run_id": runID, "labels": labels},
		"repository":   map[string]any{"full_name": repo},
	}
}

// serve drives the handler directly; side effects run on tracked goroutines, so
// callers wait on srv.wg before asserting on them.
func serve(t *testing.T, srv *server, method, target, event string, body []byte) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	rec := httptest.NewRecorder()
	(&handler{srv: srv}).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func postJob(t *testing.T, srv *server, payload map[string]any) (int, string) {
	t.Helper()
	return serve(t, srv, http.MethodPost, "/github", "workflow_job", mustJSON(t, payload))
}

// userTenant switches the process to the user tenant juno with one repo.
func userTenant(t *testing.T) {
	t.Helper()
	swap(t, &tenant, "juno")
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	swap(t, &runnerGroupDefault, 1)
}

func ghPaths(calls []ghCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.method+" "+c.path)
	}
	return out
}

// ---- do_POST: deny / spawn wiring ----

// The not_registered deny must not cancel the run: the installation token
// cannot reach that repo.
func TestHandlerDeniesAnUnregisteredUserRepoWithoutCancelling(t *testing.T) {
	spawnEnv(t, nil)
	userTenant(t)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 7, 8, "Juno/other", "self-hosted", "juno", "build"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"denied (not registered)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(*calls) != 0 {
		t.Errorf("a not_registered deny must not touch GitHub: %v", ghPaths(*calls))
	}
}

func TestHandlerSpawnsForARegisteredUserRepoWithItsFullName(t *testing.T) {
	rec := spawnEnv(t, nil)
	userTenant(t)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{"encoded_jit_config":"JIT"}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 7, 8, "Juno/some-repo", "self-hosted", "juno", "build"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"spawning"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/repos/Juno/some-repo/actions/runners/generate-jitconfig" || (*calls)[0].token != "tok" {
		t.Errorf("gh calls = %+v", *calls)
	}
	launch := rec.withVerb("launch")
	if len(launch) != 1 || !strings.HasPrefix(launch[0].argv[5], "ci-build-7-") {
		t.Errorf("launch calls = %v", launch)
	}
}

// Positive control for the no-cancel assertions: this branch must cancel, so a
// harness that records nothing cannot make them pass vacuously.
func TestHandlerCancelsTheRunOnALabelVocabularyDeny(t *testing.T) {
	spawnEnv(t, nil)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 1, 99, "alpha-org/app", "self-hosted", "build", "infra"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"denied (label vocabulary)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if got := ghPaths(*calls); len(got) != 1 || got[0] != "POST /repos/alpha-org/app/actions/runs/99/cancel" {
		t.Errorf("gh calls = %v", got)
	}
}

func TestHandlerCancelsTheRunWhenTheRepoIsNotEntitled(t *testing.T) {
	spawnEnv(t, nil)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 1, 5, "alpha-org/some-random-repo", "self-hosted", "infra"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"denied (not entitled)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if got := ghPaths(*calls); len(got) != 1 || got[0] != "POST /repos/alpha-org/some-random-repo/actions/runs/5/cancel" {
		t.Errorf("gh calls = %v", got)
	}
}

// A wrong-owner delivery is a misrouted webhook; cancelling would act on
// another owner's run (broker.py decide_job_action docstring).
func TestHandlerNeverCancelsAnotherOwnersRun(t *testing.T) {
	spawnEnv(t, nil)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 1, 5, "juno/app", "self-hosted", "build"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"denied (wrong owner)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(*calls) != 0 {
		t.Errorf("wrong_owner must not cancel: %v", ghPaths(*calls))
	}
}

func TestHandlerIgnoresAJobWithoutATierLabelQuietly(t *testing.T) {
	spawnEnv(t, nil)
	logs := captureLog(t)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 1, 2, "alpha-org/app", "self-hosted", "linux"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"ignored (no tier label)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(*calls) != 0 {
		t.Errorf("gh calls = %v", ghPaths(*calls))
	}
	// Python logged this at DEBUG, i.e. never; one line per GitHub-hosted job in
	// the org would fill the journal.
	if logs.Len() != 0 {
		t.Errorf("an ignored job must not log: %q", logs.String())
	}
}

// The tenant's own runner label ([self-hosted, <tenant>, <tier>]) must be
// stripped by the real decision path, not only when passed explicitly.
func TestHandlerStripsTheProcessTenantLabel(t *testing.T) {
	spawnEnv(t, nil)
	ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	srv := newServer(fakeTokens{token: "tok"})
	code, body := postJob(t, srv, jobEvent("queued", 1, 2, "alpha-org/app", "self-hosted", "alpha-org", "build"))
	srv.wg.Wait()
	if code != 202 || body != `{"status":"spawning"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
}

func TestDecideJobActionStripsTheProcessTenantLabel(t *testing.T) {
	swap(t, &tenant, "alpha-org")
	d := decideJobAction([]string{"self-hosted", "alpha-org", "build"}, "alpha-org/app", tiersFixture, "")
	if d.action != actionSpawn || d.tier != "build" {
		t.Errorf("got %+v", d)
	}
}

func TestHandlerReplies(t *testing.T) {
	srv := newServer(fakeTokens{token: "tok"})
	valid := mustJSON(t, jobEvent("queued", 1, 2, "alpha-org/app", "self-hosted", "linux"))
	for _, c := range []struct {
		name         string
		method, path string
		event        string
		body         []byte
		wantCode     int
		wantBody     string
	}{
		{"healthz", "GET", "/healthz", "", nil, 200, `{"status":"ok"}`},
		{"healthz is GET only", "POST", "/healthz", "", nil, 404, `{"status":"not found"}`},
		{"unknown GET", "GET", "/github", "", nil, 404, `{"status":"not found"}`},
		{"wrong webhook path", "POST", "/webhook", "workflow_job", []byte("{}"), 404, `{"status":"not found"}`},
		{"query string is not the path", "POST", "/github?x=1", "workflow_job", valid, 404, `{"status":"not found"}`},
		{"escaped path is not the path", "POST", "/%67ithub", "workflow_job", valid, 404, `{"status":"not found"}`},
		{"other event", "POST", "/github", "ping", valid, 202, `{"status":"ignored (not workflow_job)"}`},
		{"no event header", "POST", "/github", "", valid, 202, `{"status":"ignored (not workflow_job)"}`},
		{"bad json", "POST", "/github", "workflow_job", []byte("{nope"), 400, `{"status":"bad json"}`},
		{"empty body", "POST", "/github", "workflow_job", nil, 400, `{"status":"bad json"}`},
		{"not queued", "POST", "/github", "workflow_job", mustJSON(t, jobEvent("completed", 1, 2, "alpha-org/app", "build")), 202, `{"status":"ignored (not queued)"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := serve(t, srv, c.method, c.path, c.event, c.body)
			if code != c.wantCode || body != c.wantBody {
				t.Errorf("got (%d, %s), want (%d, %s)", code, body, c.wantCode, c.wantBody)
			}
		})
	}
}

func TestHandlerRefusesAnOversizedBody(t *testing.T) {
	swap(t, &maxBodyBytes, 64)
	srv := newServer(fakeTokens{token: "tok"})
	code, body := serve(t, srv, "POST", "/github", "workflow_job", bytes.Repeat([]byte("x"), 4096))
	if code != http.StatusRequestEntityTooLarge || body != `{"status":"payload too large"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
}

// Python logged the labels list's repr, which escapes newlines. A collaborator
// controls runs-on labels, so an unescaped label forged whole broker log lines.
func TestHandlerLogsAttackerControlledStringsEscaped(t *testing.T) {
	forged := "2026/10/07 12:00:00 spawned ci-infra-1-aaaaaa for job 1 (tier=infra)"
	for _, c := range []struct {
		name  string
		repo  string
		label []string
	}{
		{"label", "alpha-org/x", []string{"self-hosted", "build", "gpu\n" + forged}},
		{"repo on label deny", "alpha-org/x\n" + forged, []string{"self-hosted", "build", "infra"}},
		{"repo on not entitled", "alpha-org/x\n" + forged, []string{"self-hosted", "infra"}},
		{"repo on wrong owner", "evil\n" + forged + "/x", []string{"self-hosted", "build"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			spawnEnv(t, nil)
			ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
			logs := captureLog(t)
			srv := newServer(fakeTokens{token: "tok"})
			postJob(t, srv, jobEvent("queued", 1, 2, c.repo, c.label...))
			srv.wg.Wait()
			for _, l := range lines(logs.String()) {
				if strings.HasPrefix(l, "2026/") || strings.HasPrefix(l, "spawned") {
					t.Errorf("forged line reached the log: %q", l)
				}
			}
			if !strings.Contains(logs.String(), "DENY job 1") {
				t.Errorf("deny not logged: %q", logs.String())
			}
		})
	}
}

func TestHandlerLogsDenialsAtWarning(t *testing.T) {
	spawnEnv(t, nil)
	ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	logs := captureLog(t)
	srv := newServer(fakeTokens{token: "tok"})
	postJob(t, srv, jobEvent("queued", 1, 2, "alpha-org/app", "self-hosted", "build", "infra"))
	srv.wg.Wait()
	if !strings.HasPrefix(logs.String(), "WARNING DENY job 1") {
		t.Errorf("log = %q", logs.String())
	}
}

// ---- safeSpawn / safeCancel ----

func TestSafeCancelInvokesCancelJob(t *testing.T) {
	calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, nil })
	newServer(fakeTokens{token: "tok"}).safeCancel(999, "alpha-org/some-repo")
	if len(*calls) != 1 || (*calls)[0].token != "tok" || (*calls)[0].path != "/repos/alpha-org/some-repo/actions/runs/999/cancel" {
		t.Errorf("calls = %+v", *calls)
	}
}

// A broker goroutine must never die silently or take the process with it.
func TestSafeCancelSwallowsFailures(t *testing.T) {
	logs := captureLog(t)
	ghStub(t, func(ghCall) ([]byte, error) { panic("boom") })
	newServer(fakeTokens{token: "tok"}).safeCancel(1, "org/repo")
	newServer(fakeTokens{panicVal: "token panic"}).safeCancel(2, "org/repo")
	newServer(fakeTokens{err: errors.New("no token")}).safeCancel(3, "org/repo")
	for _, want := range []string{"cancel failed for run 1", "cancel failed for run 2", "cancel failed for run 3"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q: %q", want, logs.String())
		}
	}
}

func TestSafeSpawnInvokesSpawnWithATokenAndTheRepo(t *testing.T) {
	spawnEnv(t, nil)
	userTenant(t)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{"encoded_jit_config":"JIT"}`), nil })
	newServer(fakeTokens{token: "tok"}).safeSpawn(42, "build", "Juno/some-repo")
	if len(*calls) != 1 || (*calls)[0].token != "tok" || (*calls)[0].path != "/repos/Juno/some-repo/actions/runners/generate-jitconfig" {
		t.Errorf("calls = %+v", *calls)
	}
}

func TestSafeSpawnSwallowsFailuresAndLogsWhy(t *testing.T) {
	spawnEnv(t, nil)
	logs := captureLog(t)
	srv := newServer(fakeTokens{token: "tok"})

	newServer(fakeTokens{panicVal: "token panic"}).safeSpawn(1, "build", "alpha-org/app")
	newServer(fakeTokens{err: errors.New("no token")}).safeSpawn(2, "build", "alpha-org/app")
	ghStub(t, func(ghCall) ([]byte, error) { return nil, &httpError{code: 403, body: "rate limited"} })
	srv.safeSpawn(3, "build", "alpha-org/app")
	ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("connection reset") })
	srv.safeSpawn(4, "build", "alpha-org/app")

	for _, want := range []string{
		"spawn failed for job 1", "token panic",
		"github api error spawning for job 2", "no token",
		"github api error spawning for job 3", "HTTP 403",
		"spawn failed for job 4", "connection reset",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q: %q", want, logs.String())
		}
	}
}

// ---- unix socket transport (C5) ----

// shortTmp returns a directory short enough for an AF_UNIX path (macOS caps
// them near 104 bytes, which t.TempDir() exceeds).
func shortTmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "bcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestBrokerServesOnAPrivateUnixSocketWithoutHMAC(t *testing.T) {
	spawnEnv(t, nil)
	path := filepath.Join(shortTmp(t), "alpha-org.sock")
	httpd, ln, err := bindSocket(path, &handler{srv: newServer(fakeTokens{token: "tok"})})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = httpd.Serve(ln) }()
	defer func() { _ = httpd.Close() }()

	if m := mode(t, path); m&os.ModeSocket == 0 || m.Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a 0600 socket", m)
	}
	c := unixClient(path)

	resp, err := c.Get("http://broker/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"status":"ok"}` {
		t.Errorf("healthz = (%d, %s)", resp.StatusCode, b)
	}

	// No X-Hub-Signature-256: the dispatcher is the only HMAC check (C5).
	req, _ := http.NewRequest("POST", "http://broker/github", bytes.NewReader(mustJSON(t, jobEvent("queued", 1, 2, "alpha-org/app", "self-hosted", "linux"))))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 202 || string(b) != `{"status":"ignored (no tier label)"}` {
		t.Errorf("unsigned POST = (%d, %s)", resp.StatusCode, b)
	}

	resp, err = c.Post("http://broker/webhook", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("/webhook = %d, want 404", resp.StatusCode)
	}
}

func TestBindSocketSetsServerTimeouts(t *testing.T) {
	httpd, ln, err := bindSocket(filepath.Join(shortTmp(t), "t.sock"), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if httpd.ReadHeaderTimeout <= 0 || httpd.ReadTimeout <= 0 || httpd.WriteTimeout <= 0 || httpd.IdleTimeout <= 0 {
		t.Errorf("a stalled peer must not hold a goroutine forever: %+v", httpd)
	}
}

func TestBindSocketReplacesAStaleSocketFile(t *testing.T) {
	path := filepath.Join(shortTmp(t), "t.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, ln, err := bindSocket(path, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if mode(t, path)&os.ModeSocket == 0 {
		t.Error("the stale file was not replaced by a socket")
	}
}

// A crash leaves a real socket inode behind; nothing listens on it.
func TestBindSocketReplacesADeadSocket(t *testing.T) {
	path := filepath.Join(shortTmp(t), "t.sock")
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ul, ok := old.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T", old)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	if mode(t, path)&os.ModeSocket == 0 {
		t.Fatal("setup: no stale socket left behind")
	}
	_, ln, err := bindSocket(path, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}

// Contract: unlink only our own stale socket. A second instance (or a mistyped
// BROKER_SOCKET) must not steal a live neighbour's socket.
func TestBindSocketRefusesALiveSocket(t *testing.T) {
	path := filepath.Join(shortTmp(t), "t.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := live.Accept(); err == nil {
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()
	if _, _, err := bindSocket(path, http.NotFoundHandler()); err == nil {
		t.Fatal("bound over a live socket")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the live socket must survive: %v", err)
	}
	_ = c.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Error("the original listener no longer receives connections")
	}
}

// os.Remove deletes an empty directory, so BROKER_SOCKET naming the shared
// /run/bcx-broker while it is empty would have removed it.
func TestBindSocketRefusesToReplaceADirectory(t *testing.T) {
	dir := filepath.Join(shortTmp(t), "t.sock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bindSocket(dir, http.NotFoundHandler()); err == nil {
		t.Fatal("expected an error")
	}
	if !mode(t, dir).IsDir() {
		t.Error("the directory was removed")
	}
}

func TestBindSocketCreatesAMissingDir0700(t *testing.T) {
	sockDir := filepath.Join(shortTmp(t), "run")
	old := umask(0o022)
	defer umask(old)
	_, ln, err := bindSocket(filepath.Join(sockDir, "t.sock"), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if m := mode(t, sockDir); !m.IsDir() || m.Perm() != 0o700 {
		t.Errorf("dir mode = %v, want drwx------", m)
	}
}

// The shared dir belongs to every tenant's broker; one broker must never
// re-mode it.
func TestBindSocketLeavesAnExistingDirAlone(t *testing.T) {
	sockDir := shortTmp(t)
	if err := os.Chmod(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, ln, err := bindSocket(filepath.Join(sockDir, "t.sock"), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if m := mode(t, sockDir); !m.IsDir() || m.Perm() != 0o755 {
		t.Errorf("dir mode = %v, want it untouched (drwxr-xr-x)", m)
	}
}

func TestBindSocketLeavesTheProcessUmaskAlone(t *testing.T) {
	old := umask(0o022)
	defer umask(old)
	_, ln, err := bindSocket(filepath.Join(shortTmp(t), "t.sock"), http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if got := umask(0o022); got != 0o022 {
		t.Errorf("umask = %04o, want 0022", got)
	}
}

// ---- makeTokens fails closed ----

type tokenProbe struct {
	err   error
	gets  int
	value string
}

func (p *tokenProbe) Get() (string, error) { p.gets++; return p.value, p.err }

func tokenSeams(t *testing.T, appIDErr error, probe *tokenProbe) {
	t.Helper()
	swap(t, &installationID, 42)
	swap(t, &appIDFunc, func() (string, error) { return "1", appIDErr })
	swap(t, &newInstallationTokens, func(appID string, instID int) tokenSource {
		if appID != "1" || instID != 42 {
			t.Errorf("token source built for app %q installation %d", appID, instID)
		}
		return probe
	})
}

func TestMakeTokensFailsClosedWhenNoTokenCanBeMinted(t *testing.T) {
	tokenSeams(t, nil, &tokenProbe{err: errors.New("openssl cannot sign with /etc/bartixcloud/app.pem")})
	if _, err := makeTokens(); err == nil || !strings.Contains(err.Error(), "installation 42") ||
		!strings.Contains(err.Error(), "openssl cannot sign") {
		t.Errorf("err = %v", err)
	}
}

func TestMakeTokensFailsClosedWithoutAnAppID(t *testing.T) {
	probe := &tokenProbe{}
	tokenSeams(t, errors.New("BCX_APP_ID missing or not numeric"), probe)
	if _, err := makeTokens(); err == nil || !strings.Contains(err.Error(), "installation 42") {
		t.Errorf("err = %v", err)
	}
	if probe.gets != 0 {
		t.Error("no token may be requested without an app id")
	}
}

func TestMakeTokensFailsClosedOnAGarbledGitHubReply(t *testing.T) {
	tokenSeams(t, nil, &tokenProbe{err: errors.New("Expecting value: line 1 column 1 (char 0)")})
	if _, err := makeTokens(); err == nil || !strings.Contains(err.Error(), "installation 42") {
		t.Errorf("err = %v", err)
	}
}

func TestMakeTokensProvesTheTokenOnceAtStartup(t *testing.T) {
	probe := &tokenProbe{value: "tok"}
	tokenSeams(t, nil, probe)
	src, err := makeTokens()
	if err != nil || src == nil {
		t.Fatalf("got (%v, %v)", src, err)
	}
	if probe.gets != 1 {
		t.Errorf("startup proof made %d Get calls, want 1", probe.gets)
	}
}

// ---- run: startup, serving, graceful shutdown ----

// runEnv prepares a runnable broker: a tiers file, an absent age file, a short
// socket path, stub token and incus seams. It returns the environment lookup
// and the socket path.
func runEnv(t *testing.T, tiersYAMLContent string) (func(string) (string, bool), string) {
	t.Helper()
	dir := shortTmp(t)
	tiersPath := filepath.Join(dir, "tiers.yml")
	if err := os.WriteFile(tiersPath, []byte(tiersYAMLContent), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "alpha-org.sock")
	e := orgEnv()
	e["BROKER_SOCKET"] = sock
	e["BROKER_TIERS_FILE"] = tiersPath
	e["BROKER_SECRETS_AGE"] = filepath.Join(dir, "absent.age")
	// run installs the config globals; restore them for the next test.
	swap(t, &tenant, tenant)
	swap(t, &targetKind, targetKind)
	swap(t, &target, target)
	swap(t, &tenantOwner, tenantOwner)
	swap(t, &repos, repos)
	swap(t, &installationID, installationID)
	swap(t, &incusProject, incusProject)
	swap(t, &runnerGroupDefault, runnerGroupDefault)
	swap(t, &memFloorBytes, memFloorBytes)
	swap(t, &tiersFile, tiersFile)
	swap(t, &socketPath, socketPath)
	swap(t, &ageKey, ageKey)
	swap(t, &secretsAge, secretsAge)
	swap(t, &appIDFunc, func() (string, error) { return "1", nil })
	swap(t, &newInstallationTokens, func(string, int) tokenSource { return fakeTokens{token: "tok"} })
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	swap(t, &pause, func(time.Duration) {})
	return lookupOf(e), sock
}

// brokerEnvNames is every variable loadConfig reads.
var brokerEnvNames = []string{"BROKER_TENANT", "BROKER_TARGET_KIND", "BROKER_TARGET", "BROKER_REPOS",
	"BROKER_INSTALLATION_ID", "BROKER_INCUS_PROJECT", "BROKER_SOCKET", "BROKER_TIERS_FILE", "BROKER_AGE_KEY",
	"BROKER_SECRETS_AGE", "BROKER_CAPACITY_MEM", "BROKER_MEM_FLOOR", "BROKER_RUNNER_GROUP_ID",
	"BROKER_HCLOUD_TOKEN_FILE"}

// waitNoReservations blocks until every spawn that reserved a slot has released
// it, i.e. has fully returned.
func waitNoReservations(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		launchLock.Lock()
		n := reserved["build"]
		launchLock.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a spawn never finished")
}

func buildOnlyTiers() string { return tierYAML("build", nil) }

// waitForSocket polls until the broker answers /healthz.
func waitForSocket(t *testing.T, sock string) {
	t.Helper()
	c := unixClient(sock)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := c.Get("http://broker/healthz"); err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("broker never came up")
}

// startRun runs the broker on a goroutine. The reconcile loop is neutralised:
// its background passes would call the API stubs and read the globals these
// tests swap concurrently, which is a race, not a test.
func startRun(t *testing.T, lookup func(string) (string, bool)) (cancel func(), result <-chan error) {
	t.Helper()
	swap(t, &startReconciler, func(context.Context, *server, tokenSource) {})
	ctx, cancelCtx := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- run(ctx, lookup) }()
	t.Cleanup(cancelCtx)
	return cancelCtx, res
}

func waitResult(t *testing.T, res <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(within):
		t.Fatal("run did not return")
		return nil
	}
}

// Ports test_main_serves_on_the_unix_socket_with_no_tier_secrets: a tenant with
// no age file has no secrets, and the broker serves on its unix socket.
func TestRunServesOnTheUnixSocketWithNoTierSecrets(t *testing.T) {
	logs := captureLog(t)
	lookup, sock := runEnv(t, buildOnlyTiers())
	incusStub(t, incusOK)
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	cancel, res := startRun(t, lookup)
	waitForSocket(t, sock)
	if mode(t, sock).Perm() != 0o600 {
		t.Errorf("socket mode = %v", mode(t, sock))
	}
	cancel()
	if err := waitResult(t, res, 5*time.Second); err != nil {
		t.Fatalf("clean shutdown must return nil, got %v", err)
	}
	for _, want := range []string{"reconcile-on-start: 0 existing ci-* instance(s): none", "broker up on " + sock, "tiers=[build]"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q: %q", want, logs.String())
		}
	}
}

// SIGTERM: the socket is unlinked, the shared directory is untouched, and the
// process drains instead of dying between `incus launch` and injection.
func TestRunShutdownUnlinksOnlyItsOwnSocketAndLeavesTheDirAlone(t *testing.T) {
	lookup, sock := runEnv(t, buildOnlyTiers())
	dir := filepath.Dir(sock)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(dir, "juno.sock")
	nl, err := net.Listen("unix", neighbour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nl.Close() }()
	cancel, res := startRun(t, lookup)
	waitForSocket(t, sock)
	cancel()
	if err := waitResult(t, res, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("own socket still present: %v", err)
	}
	if mode(t, neighbour)&os.ModeSocket == 0 {
		t.Error("another tenant's socket was removed")
	}
	if m := mode(t, dir); !m.IsDir() || m.Perm() != 0o700 {
		t.Errorf("shared dir mode = %v", m)
	}
}

func postUnix(t *testing.T, sock string, payload map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://broker/github", bytes.NewReader(mustJSON(t, payload)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	resp, err := unixClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// A spawn in flight at SIGTERM must finish its injection: abandoning it leaves
// an idle ephemeral VM with no jitconfig until the reaper's TTL.
func TestRunDrainsInFlightSpawnsBeforeReturning(t *testing.T) {
	lookup, sock := runEnv(t, buildOnlyTiers())
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	launching, release := make(chan struct{}), make(chan struct{})
	rec := incusStub(t, func(c incusCall) (string, string, int) {
		if c.argv[3] == "launch" {
			close(launching)
			<-release
		}
		return "", "", 0
	})
	cancel, res := startRun(t, lookup)
	waitForSocket(t, sock)
	postUnix(t, sock, jobEvent("queued", 5, 6, "alpha-org/app", "self-hosted", "build"))
	<-launching

	cancel()
	select {
	case err := <-res:
		t.Fatalf("run returned (%v) while a spawn was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := waitResult(t, res, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var injected bool
	for _, c := range rec.withVerb("exec") {
		injected = injected || c.stdin != ""
	}
	if !injected {
		t.Error("the in-flight spawn was abandoned before injection")
	}
}

func TestRunGivesUpDrainingAfterTheBound(t *testing.T) {
	swap(t, &drainTimeout, 200*time.Millisecond)
	lookup, sock := runEnv(t, buildOnlyTiers())
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"encoded_jit_config":"JIT"}`), nil
	})
	launching, release, injected := make(chan struct{}), make(chan struct{}), make(chan struct{})
	incusStub(t, func(c incusCall) (string, string, int) {
		switch {
		case c.argv[3] == "launch":
			close(launching)
			<-release
		case c.stdin != "":
			close(injected)
		}
		return "", "", 0
	})
	cancel, res := startRun(t, lookup)
	waitForSocket(t, sock)
	postUnix(t, sock, jobEvent("queued", 5, 6, "alpha-org/app", "self-hosted", "build"))
	<-launching
	cancel()
	if err := waitResult(t, res, 5*time.Second); err == nil {
		t.Error("an undrained shutdown must not report success")
	}
	// Let the abandoned spawn finish before the seams are restored.
	close(release)
	<-injected
	waitNoReservations(t)
}

func TestRunFailsOnStartupErrorsBeforeServing(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, env map[string]string)
		want  string
	}{
		{"config", func(t *testing.T, env map[string]string) { delete(env, "BROKER_SOCKET") }, "BROKER_SOCKET"},
		{"floor", func(t *testing.T, env map[string]string) { env["BROKER_MEM_FLOOR"] = "banana" }, "BROKER_MEM_FLOOR"},
		{"tiers file", func(t *testing.T, env map[string]string) { env["BROKER_TIERS_FILE"] = "/nonexistent/tiers.yml" }, "tiers.yml"},
		{"tier secrets", func(t *testing.T, env map[string]string) {
			p := filepath.Join(filepath.Dir(env["BROKER_SOCKET"]), "infra.yml")
			content := tierYAML("infra", map[string]string{"allowlist": `["*"]`, "secrets": `[INFRA_HCLOUD_TOKEN]`})
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			env["BROKER_TIERS_FILE"] = p
		}, "INFRA_HCLOUD_TOKEN"},
		{"tokens", func(t *testing.T, env map[string]string) {
			swap(t, &appIDFunc, func() (string, error) { return "", errors.New("BCX_APP_ID missing") })
		}, "installation 42"},
		{"incus unreachable", func(t *testing.T, env map[string]string) {
			swap(t, &runningRunners, func(string) ([]string, error) { return nil, errors.New("incus list failed: down") })
		}, "incus list failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			captureLog(t)
			lookup, sock := runEnv(t, buildOnlyTiers())
			env := map[string]string{}
			for _, k := range brokerEnvNames {
				if v, ok := lookup(k); ok {
					env[k] = v
				}
			}
			c.setup(t, env)
			err := run(context.Background(), lookupOf(env))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			if _, statErr := os.Lstat(sock); !os.IsNotExist(statErr) {
				t.Error("a broker that failed startup must not leave a socket")
			}
		})
	}
}

// realMain is main's whole body: exit 0 on SIGTERM, 1 on a config error.
func TestMainExitsZeroOnSIGTERM(t *testing.T) {
	// The process-wide path starts a real reconcile loop; TestRunStartsTheReconciler
	// covers that it does, so here it is neutralised: an unconstrained loop would
	// call the live API and outlive the test.
	swap(t, &startReconciler, func(context.Context, *server, tokenSource) {})
	lookup, sock := runEnv(t, buildOnlyTiers())
	for _, k := range brokerEnvNames {
		v, present := lookup(k)
		t.Setenv(k, v) // registers the restore
		if !present {
			_ = os.Unsetenv(k)
		}
	}
	code := make(chan int, 1)
	go func() { code <- realMain() }()
	waitForSocket(t, sock)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("exit code = %d, want 0", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("main did not exit on SIGTERM")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not unlinked: %v", err)
	}
}

func TestMainExitsNonZeroOnAConfigError(t *testing.T) {
	logs := captureLog(t)
	for _, k := range brokerEnvNames {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	if code := realMain(); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "BROKER_TENANT") {
		t.Errorf("the message must name the variable: %q", logs.String())
	}
}

func TestHandlerJSONReplyIsCompact(t *testing.T) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(`{"status": "ok"}`)); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	writeReply(rec, 200, "ok")
	if rec.Body.String() != buf.String() || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("reply = %q (%s)", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}
