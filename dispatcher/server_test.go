package dispatcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

// harness is a live Server on 127.0.0.1 with a stub broker for tenant "alpha-org"
// (installation 102). Tenant "juno" (installation 103) has no broker socket.
type harness struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	broker *brokerStub
	runner *fakeRunner
	logs   *syncBuffer
	dir    string
}

func newHarness(t *testing.T, tweak ...func(*Server)) *harness {
	t.Helper()
	dir := shortTmp(t)
	h := &harness{t: t, dir: dir, runner: &fakeRunner{}}
	h.broker = serveBroker(t, dir, "alpha-org", spawning)
	log, logs := testLogger()
	h.logs = logs
	srv, err := NewServer(Config{Secret: testSecret, SocketDir: dir}, testTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Runner = h.runner
	for _, f := range tweak {
		f(srv)
	}
	h.srv = srv
	h.ts = httptest.NewServer(srv)
	t.Cleanup(func() {
		h.ts.Close()
		srv.Close()
	})
	return h
}

// do sends one request and returns status, headers and body.
func (h *harness) do(method, path string, body io.Reader, header map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, body)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, resp.Header, out
}

// signedHeaders are the headers GitHub sends for body.
func signedHeaders(t *testing.T, event string, body []byte) map[string]string {
	t.Helper()
	return map[string]string{
		"X-GitHub-Event":      event,
		"X-GitHub-Delivery":   "d-1",
		"X-Hub-Signature-256": sign(t, testSecret, body),
		"Content-Type":        "application/json",
	}
}

// post sends a correctly signed delivery and returns the status code and the
// "status" message of the JSON reply.
func (h *harness) post(event, body string) (int, string) {
	h.t.Helper()
	code, _, out := h.do("POST", "/github", strings.NewReader(body), signedHeaders(h.t, event, []byte(body)))
	return code, statusMessage(h.t, out)
}

func statusMessage(t *testing.T, raw []byte) string {
	t.Helper()
	var reply struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("reply %q is not the JSON status object: %v", raw, err)
	}
	return reply.Status
}

func jobBody(inst int, repo string) string {
	return fmt.Sprintf(`{"action":"queued","installation":{"id":%d},"repository":{"full_name":%q}}`, inst, repo)
}

func tagPushBody(inst int, repo string) string {
	return fmt.Sprintf(`{"installation":{"id":%d},"repository":{"full_name":%q},"ref":"refs/tags/v1.2.0"}`, inst, repo)
}

func tagPushBodyWithCommit(inst int, repo, commit string) string {
	return fmt.Sprintf(`{"installation":{"id":%d},"repository":{"full_name":%q},"ref":"refs/tags/v1.2.0","head_commit":{"id":%q}}`, inst, repo, commit)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ------------------------------------------------------------ end to end ----

func TestSignedWorkflowJobReachesTheTenantBroker(t *testing.T) {
	h := newHarness(t)
	body := jobBody(102, "alpha-org/app")
	code, msg := h.post("workflow_job", body)
	if code != 202 || msg != "spawning" {
		t.Fatalf("got %d %q, want 202 spawning", code, msg)
	}
	reqs := h.broker.requests()
	if len(reqs) != 1 || string(reqs[0].Body) != body {
		t.Fatalf("broker saw %d requests, want the verbatim body once: %+v", len(reqs), reqs)
	}
}

func TestBadSignatureIs401AndNothingIsForwarded(t *testing.T) {
	h := newHarness(t)
	body := []byte(jobBody(102, "alpha-org/app"))
	headers := signedHeaders(t, "workflow_job", body)
	headers["X-Hub-Signature-256"] = sign(t, "wrong-secret-wrong-secret", body)
	code, _, out := h.do("POST", "/github", bytes.NewReader(body), headers)
	if code != 401 || statusMessage(t, out) != "bad signature" {
		t.Errorf("got %d %s, want 401 bad signature", code, out)
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests after a bad signature", n)
	}
}

func TestUnknownInstallationIs403(t *testing.T) {
	h := newHarness(t)
	code, msg := h.post("workflow_job", `{"installation":{"id":999}}`)
	if code != 403 || msg != "forbidden" {
		t.Errorf("got %d %q, want 403 forbidden", code, msg)
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests for an unknown installation", n)
	}
}

func TestRepoOfAnotherOwnerIs403(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.post("workflow_job", jobBody(102, "kappa-org/infra")); code != 403 {
		t.Errorf("code = %d, want 403", code)
	}
}

func TestPingAndHealthz(t *testing.T) {
	h := newHarness(t)
	if code, msg := h.post("ping", `{"zen":"x","hook_id":1}`); code != 200 || msg != "pong" {
		t.Errorf("ping: got %d %q", code, msg)
	}
	code, _, out := h.do("GET", "/healthz", nil, nil)
	if code != 200 || string(out) != `{"status":"ok"}` {
		t.Errorf("healthz: got %d %q", code, out)
	}
}

func TestHealthzNeedsNoSignature(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("GET", "/healthz", nil, map[string]string{"X-Hub-Signature-256": "garbage"}); code != 200 {
		t.Errorf("code = %d, want 200", code)
	}
}

func TestRepliesAreCompactJSON(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"zen":"x"}`)
	code, header, out := h.do("POST", "/github", bytes.NewReader(body), signedHeaders(t, "ping", body))
	if code != 200 || string(out) != `{"status":"pong"}` {
		t.Errorf("got %d %q", code, out)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := header.Get("Content-Length"); got != fmt.Sprint(len(out)) {
		t.Errorf("Content-Length = %q, want %d", got, len(out))
	}
}

func TestVTagPushPokesInTheBackground(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(s *Server) { s.Runner = gatedRunner{release: release} })

	code, msg := h.post("push", tagPushBody(102, "alpha-org/infra"))
	if code != 202 || msg != "poking 3 vm(s)" {
		t.Fatalf("got %d %q, want 202 poking 3 vm(s) while the pokes are still blocked", code, msg)
	}
	close(release)
	h.srv.Wait()

	logged := h.logs.String()
	for _, vm := range []string{"platform/ingress", "platform/bartix-db", "alpha-org/alpha-org-app"} {
		if !strings.Contains(logged, "poked "+vm) {
			t.Errorf("log lacks %q:\n%s", "poked "+vm, logged)
		}
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("a poke must not be forwarded to the broker (saw %d)", n)
	}
}

func TestPokeRunsTheContractArgvOnEveryTarget(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.post("push", tagPushBody(102, "alpha-org/infra")); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	h.srv.Wait()
	want := [][]string{
		wantPokeArgv("platform", "ingress"),
		wantPokeArgv("platform", "bartix-db"),
		wantPokeArgv("alpha-org", "alpha-org-app"),
	}
	slices.SortFunc(want, slices.Compare)
	if got := sortedArgvs(h.runner); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestTagPushPokePostsTheConvergeStatus(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{
		"alpha-org-app": convergeStatusJSON(t, "alpha-org", map[string]any{
			"result": "ok", "tag": "v1.2.0", "commit": testCommit,
		}),
	}}
	poster := &fakePoster{}
	h := newHarness(t, func(s *Server) {
		s.Runner = runner
		s.Poster = poster
		s.Table = Table{
			Installations: map[string]Installation{"102": {Owner: "alpha-org", Tenant: strPtr("alpha-org")}},
			Pokes:         map[string][]PokeTarget{"alpha-org/infra": {{"alpha-org", "alpha-org-app"}}},
		}
	})

	if code, _ := h.post("push", tagPushBodyWithCommit(102, "Alpha-Org/infra", testCommit)); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	h.srv.Wait()

	// The blocking contract argv was what started the converge.
	wantArgv := [][]string{wantPokeArgv("alpha-org", "alpha-org-app")}
	if got := sortedArgvs(runner); !slices.EqualFunc(got, wantArgv, slices.Equal) {
		t.Errorf("argv = %q, want %q", got, wantArgv)
	}

	calls := poster.posted()
	if len(calls) != 1 {
		t.Fatalf("posted %d statuses, want 1: %+v", len(calls), calls)
	}
	wantJob := Report{Repo: "alpha-org/infra", Commit: testCommit, Source: "alpha-org", Installation: 102}
	if calls[0].job != wantJob {
		t.Errorf("report = %+v, want %+v", calls[0].job, wantJob)
	}
	if calls[0].statusContext != "nuk/converge/alpha-org-app" {
		t.Errorf("context = %q, want nuk/converge/alpha-org-app", calls[0].statusContext)
	}
	if calls[0].state != "success" || !strings.Contains(calls[0].description, "applied v1.2.0") {
		t.Errorf("status = %q %q, want success applied v1.2.0", calls[0].state, calls[0].description)
	}
}

func TestPlatformInstallationPokesDespiteHavingNoBroker(t *testing.T) {
	h := newHarness(t)
	code, msg := h.post("push", tagPushBody(100, "bartixcloud/infra"))
	if code != 202 || msg != "poking 2 vm(s)" {
		t.Errorf("got %d %q", code, msg)
	}
	h.srv.Wait()
	if n := len(h.runner.argvs()); n != 2 {
		t.Errorf("ran %d pokes, want 2", n)
	}
}

func TestPokeOutlivesTheRequestThatStartedIt(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(s *Server) { s.Runner = gatedRunner{release: release} })
	if code, _ := h.post("push", tagPushBody(103, "Juno/infra")); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	// The request is over; the poke goroutine must not have inherited its context.
	close(release)
	h.srv.Wait()
	logged := h.logs.String()
	if strings.Contains(logged, "failed") || !strings.Contains(logged, "poked platform/ingress") {
		t.Errorf("a poke must survive its request's end:\n%s", logged)
	}
}

func TestIgnoredDeliveriesAreAccepted(t *testing.T) {
	h := newHarness(t)
	cases := map[string]struct{ event, body, want string }{
		"branch push":    {"push", `{"installation":{"id":102},"repository":{"full_name":"alpha-org/infra"},"ref":"refs/heads/main"}`, "ignored (push is not a new v* tag)"},
		"platform job":   {"workflow_job", jobBody(100, "bartixcloud/nuk"), "ignored (workflow_job for the platform installation (no broker))"},
		"not a poke src": {"push", tagPushBody(100, "bartixcloud/nuk"), "ignored (bartixcloud/nuk is not a converge source)"},
	}
	for name, tc := range cases {
		code, msg := h.post(tc.event, tc.body)
		if code != 202 || msg != tc.want {
			t.Errorf("%s: got %d %q, want 202 %q", name, code, msg, tc.want)
		}
	}
	if n := len(h.broker.requests()) + len(h.runner.argvs()); n != 0 {
		t.Errorf("ignored deliveries caused %d side effects", n)
	}
}

// ----------------------------------------------------------- check order ----

func TestOtherPathsAndMethodsAre404JSONNot405(t *testing.T) {
	h := newHarness(t)
	cases := []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/github"}, {"POST", "/healthz"}, {"POST", "/"}, {"POST", "/github/"},
		{"POST", "/GitHub"}, {"POST", "/github/x"}, {"PUT", "/github"}, {"DELETE", "/github"},
		{"PATCH", "/github"}, {"OPTIONS", "/github"}, {"HEAD", "/healthz"}, {"PUT", "/healthz"},
		{"GET", "/healthz/"}, {"GET", "/metrics"},
	}
	for _, c := range cases {
		code, header, out := h.do(c.method, c.path, nil, nil)
		if code != 404 {
			t.Errorf("%s %s: code = %d, want 404", c.method, c.path, code)
			continue
		}
		if c.method == "HEAD" {
			continue // no body on HEAD
		}
		if statusMessage(t, out) != "not found" || header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: body %q, Content-Type %q", c.method, c.path, out, header.Get("Content-Type"))
		}
	}
}

func TestCheckOrderPathThenSizeThenSignatureThenJSON(t *testing.T) {
	h := newHarness(t, func(s *Server) { s.MaxBody = 64 })
	big := bytes.Repeat([]byte("x"), 65)
	notJSON := []byte("not json")

	// 404 beats 413: an unknown path never gets as far as the size check.
	if code, _, _ := h.do("POST", "/nope", bytes.NewReader(big), nil); code != 404 {
		t.Errorf("unknown path with a big body: %d, want 404", code)
	}
	// 413 beats 401: the body is not even considered for the HMAC.
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(big), nil); code != 413 {
		t.Errorf("big unsigned body: %d, want 413", code)
	}
	// 401 beats 400: an unsigned body is never parsed.
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(notJSON), nil); code != 401 {
		t.Errorf("unsigned bad json: %d, want 401", code)
	}
	// 400 beats routing: a signed ping that is not JSON is not a pong.
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(notJSON), signedHeaders(t, "ping", notJSON)); code != 400 {
		t.Errorf("signed bad json ping: %d, want 400", code)
	}
}

func TestOversizeContentLengthIs413(t *testing.T) {
	h := newHarness(t, func(s *Server) { s.MaxBody = 64 })
	body := bytes.Repeat([]byte("x"), 65)
	code, _, out := h.do("POST", "/github", bytes.NewReader(body), signedHeaders(t, "workflow_job", body))
	if code != 413 || statusMessage(t, out) != "payload too large" {
		t.Errorf("got %d %q, want 413 payload too large", code, out)
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests", n)
	}
}

func TestBodyOfExactlyMaxBodyIsAccepted(t *testing.T) {
	body := jobBody(102, "alpha-org/app")
	h := newHarness(t, func(s *Server) { s.MaxBody = int64(len(body)) })
	if code, _ := h.post("workflow_job", body); code != 202 {
		t.Errorf("code = %d, want 202 at exactly MaxBody", code)
	}
}

func TestOversizeContentLengthIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	h := newHarness(t)
	conn, err := net.Dial("tcp", h.ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	// Claim 100 MiB and send nothing: a handler that read first would hang until the deadline.
	req := "POST /github HTTP/1.1\r\nHost: x\r\nContent-Length: 104857600\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 413 {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestOversizeChunkedBodyIs413(t *testing.T) {
	h := newHarness(t, func(s *Server) { s.MaxBody = 64 })
	body := bytes.Repeat([]byte("x"), 200)
	// io.NopCloser hides the length, so the client sends Transfer-Encoding: chunked.
	code, _, out := h.do("POST", "/github", io.NopCloser(bytes.NewReader(body)), signedHeaders(t, "workflow_job", body))
	if code != 413 || statusMessage(t, out) != "payload too large" {
		t.Errorf("got %d %q, want 413", code, out)
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests", n)
	}
}

func TestChunkedBodyWithinTheCapIsServed(t *testing.T) {
	h := newHarness(t)
	body := []byte(jobBody(102, "alpha-org/app"))
	code, _, out := h.do("POST", "/github", io.NopCloser(bytes.NewReader(body)), signedHeaders(t, "workflow_job", body))
	if code != 202 || statusMessage(t, out) != "spawning" {
		t.Errorf("got %d %q, want 202 spawning", code, out)
	}
}

func TestMissingOrMalformedSignatureIs401(t *testing.T) {
	h := newHarness(t)
	body := []byte(jobBody(102, "alpha-org/app"))
	good := sign(t, testSecret, body)
	for name, sig := range map[string]string{
		"missing":      "",
		"no scheme":    strings.TrimPrefix(good, "sha256="),
		"sha1 scheme":  "sha1=" + strings.TrimPrefix(good, "sha256="),
		"empty digest": "sha256=",
		"truncated":    good[:len(good)-1],
	} {
		headers := signedHeaders(t, "workflow_job", body)
		if sig == "" {
			delete(headers, "X-Hub-Signature-256")
		} else {
			headers["X-Hub-Signature-256"] = sig
		}
		if code, _, _ := h.do("POST", "/github", bytes.NewReader(body), headers); code != 401 {
			t.Errorf("%s signature: code = %d, want 401", name, code)
		}
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests", n)
	}
}

func TestEmptyBodyWithoutSignatureIs401(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("POST", "/github", nil, nil); code != 401 {
		t.Errorf("code = %d, want 401", code)
	}
}

func TestSignedBodiesThatAreNotJSONObjectsAre400(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{``, `not json`, `null`, `[]`, `[1]`, `"str"`, `42`, `true`, `{`, `{} {}`,
		`{"installation":"x"}`, `{"installation":{"id":true}}`} {
		code, msg := h.post("workflow_job", body)
		if code != 400 || msg != "bad json" {
			t.Errorf("body %q: got %d %q, want 400 bad json", body, code, msg)
		}
	}
	if n := len(h.broker.requests()); n != 0 {
		t.Errorf("broker saw %d requests", n)
	}
}

// ---------------------------------------------------------------- forward ----

func TestOnlyWhitelistedHeadersReachTheBrokerAndTheSignatureNeverDoes(t *testing.T) {
	h := newHarness(t)
	body := []byte(jobBody(102, "alpha-org/app"))
	headers := signedHeaders(t, "workflow_job", body)
	headers["X-GitHub-Hook-Installation-Target-ID"] = "7"
	headers["X-Hub-Signature"] = "sha1=legacy"
	headers["Authorization"] = "Bearer secret-token"
	headers["Cookie"] = "a=b"
	headers["X-Forwarded-For"] = "203.0.113.9"
	headers["X-Custom"] = "1"
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(body), headers); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	got := h.broker.requests()[0].Header
	allowed := []string{
		"X-Github-Event", "X-Github-Delivery", "X-Github-Hook-Installation-Target-Id",
		"Content-Type", "Content-Length", "Connection",
	}
	for name := range got {
		if !slices.Contains(allowed, name) {
			t.Errorf("header %q reached the broker", name)
		}
	}
	for name, want := range map[string]string{
		"X-GitHub-Event": "workflow_job", "X-GitHub-Delivery": "d-1",
		"X-GitHub-Hook-Installation-Target-ID": "7", "Content-Type": "application/json",
	} {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want)
		}
	}
}

func TestBrokerStatusIsRelayedVerbatim(t *testing.T) {
	dir := shortTmp(t)
	serveBroker(t, dir, "alpha-org", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":"boom"}`)
	})
	h := newHarness(t, func(s *Server) { s.Forwarder.SocketDir = dir })

	// A broker failure is the broker's to report: no rewrite, no retry.
	code, msg := h.post("workflow_job", jobBody(102, "alpha-org/app"))
	if code != 500 || msg != "boom" {
		t.Errorf("got %d %q, want the broker's 500 boom", code, msg)
	}
}

func TestBrokerDownIs503(t *testing.T) {
	h := newHarness(t)
	// Installation 103 maps to tenant juno, which has no socket.
	code, msg := h.post("workflow_job", jobBody(103, "Juno/app"))
	if code != 503 || msg != "broker juno unavailable" {
		t.Errorf("got %d %q, want 503 broker juno unavailable", code, msg)
	}
	if !strings.Contains(h.logs.String(), "forward to juno failed") {
		t.Errorf("the failure must be logged:\n%s", h.logs.String())
	}
}

func TestSlowBrokerIs503(t *testing.T) {
	dir := shortTmp(t)
	serveBroker(t, dir, "alpha-org", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	h := newHarness(t, func(s *Server) {
		s.Forwarder = Forwarder{SocketDir: dir, Timeout: 100 * time.Millisecond}
	})
	if code, _ := h.post("workflow_job", jobBody(102, "alpha-org/app")); code != 503 {
		t.Errorf("code = %d, want 503", code)
	}
}

// ------------------------------------------------------------------- logs ----

func TestRejectLogLineKeepsItsGreppableToken(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.post("workflow_job", `{"installation":{"id":999}}`); code != 403 {
		t.Fatalf("code = %d, want 403", code)
	}
	logged := h.logs.String()
	// Operators grep: journalctl -u bcx-dispatcher | grep "REJECT .*unknown installation 1".
	if !regexp.MustCompile(`REJECT .*unknown installation 999`).MatchString(logged) {
		t.Errorf("log line lost the REJECT token:\n%s", logged)
	}
	for _, want := range []string{"delivery=d-1", "event=workflow_job", "level=WARN"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
}

func TestAttackerControlledDeliveryIDCannotForgeLogFields(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	headers := signedHeaders(t, "workflow_job", body)
	headers["X-Hub-Signature-256"] = "sha256=00"
	headers["X-GitHub-Delivery"] = `d"1 level=ERROR msg=pwned` + "\tx"
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(body), headers); code != 401 {
		t.Fatalf("code = %d, want 401", code)
	}
	logged := strings.TrimSpace(h.logs.String())
	if strings.Count(logged, "\n") != 0 {
		t.Errorf("one rejection must be one log line:\n%s", logged)
	}
	if !strings.Contains(logged, `delivery="d\"1 level=ERROR msg=pwned\tx"`) {
		t.Errorf("delivery id must be quoted and escaped:\n%s", logged)
	}
}

func TestOversizedDeliveryIDIsClippedInLogs(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	headers := signedHeaders(t, "workflow_job", body)
	headers["X-Hub-Signature-256"] = "sha256=00"
	headers["X-GitHub-Delivery"] = strings.Repeat("A", 8000)
	if code, _, _ := h.do("POST", "/github", bytes.NewReader(body), headers); code != 401 {
		t.Fatalf("code = %d, want 401", code)
	}
	if n := len(h.logs.String()); n > 400 {
		t.Errorf("an 8000-byte delivery id inflated the log line to %d bytes", n)
	}
}

func TestBadSignatureLogsAWarningWithoutTheSecret(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	headers := signedHeaders(t, "workflow_job", body)
	headers["X-Hub-Signature-256"] = "sha256=00"
	h.do("POST", "/github", bytes.NewReader(body), headers)
	logged := h.logs.String()
	if !strings.Contains(logged, "bad signature") || !strings.Contains(logged, "level=WARN") {
		t.Errorf("missing warning:\n%s", logged)
	}
	if strings.Contains(logged, testSecret) {
		t.Errorf("secret leaked into the log:\n%s", logged)
	}
}

// --------------------------------------------------------------- handlers ----

func TestEveryActionIsHandled(t *testing.T) {
	want := map[Action]int{
		ActionPong: 200, ActionReject: 403, ActionIgnore: 202, ActionPoke: 202,
		ActionForward: 503, // tenant "nobody" has no broker: the 503 path, not the 500 fallback
		ActionPin:     202,
		ActionOrg:     202,
	}
	rig := newOrgRig(t)
	h := newHarness(t, func(s *Server) { s.Org = rig.runner })
	for a := Action(1); a < actionCount; a++ {
		code, ok := want[a]
		if !ok {
			t.Fatalf("action %v has no expected status: extend this test", a)
		}
		rec := httptest.NewRecorder()
		h.srv.respond(t.Context(), rec, delivery{id: "d", event: "e", body: []byte("{}")},
			Decision{Action: a, Reason: "r", Tenant: "nobody", Targets: []PokeTarget{{"p", "vm"}},
				Org: OrgPlan{Event: "e", Repo: "demo", Jobs: []OrgJob{OrgJobSpiegelFakten}}})
		if rec.Code != code {
			t.Errorf("action %v: status = %d, want %d", a, rec.Code, code)
		}
	}
	h.srv.Wait()
}

func TestAnInvalidActionFailsClosedWith500(t *testing.T) {
	h := newHarness(t)
	for _, a := range []Action{actionUnset, actionCount, Action(99), Action(-1)} {
		rec := httptest.NewRecorder()
		h.srv.respond(t.Context(), rec, delivery{id: "d", event: "e"}, Decision{Action: a, Tenant: "alpha-org"})
		if rec.Code != 500 {
			t.Errorf("action %d: status = %d, want 500", int(a), rec.Code)
		}
	}
	if n := len(h.broker.requests()) + len(h.runner.argvs()); n != 0 {
		t.Errorf("an invalid action caused %d side effects", n)
	}
	if !strings.Contains(h.logs.String(), "level=ERROR") {
		t.Errorf("an invalid action must be logged as an error:\n%s", h.logs.String())
	}
}

func TestConcurrentDeliveries(t *testing.T) {
	h := newHarness(t)
	const n = 30
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := jobBody(102, "alpha-org/app")
			if i%3 == 0 {
				body = tagPushBody(102, "alpha-org/infra")
			}
			event := "workflow_job"
			if i%3 == 0 {
				event = "push"
			}
			codes[i], _ = h.post(event, body)
		}()
	}
	wg.Wait()
	h.srv.Wait()
	for i, code := range codes {
		if code != 202 {
			t.Errorf("delivery %d: code = %d, want 202", i, code)
		}
	}
	if got := len(h.broker.requests()); got != n-n/3 {
		t.Errorf("broker saw %d deliveries, want %d", got, n-n/3)
	}
	if got := len(h.runner.argvs()); got != (n/3)*3 {
		t.Errorf("ran %d pokes, want %d", got, (n/3)*3)
	}
}

// ---------------------------------------------------------- Wait and Close ----

func TestWaitBlocksUntilInFlightPokesFinish(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(s *Server) { s.Runner = gatedRunner{release: release} })
	if code, _ := h.post("push", tagPushBody(103, "Juno/infra")); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	done := make(chan struct{})
	go func() {
		h.srv.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Wait returned while a poke was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after the pokes finished")
	}
}

func TestCloseCancelsInFlightPokesAndWaits(t *testing.T) {
	runner := &fakeRunner{blockOn: true}
	h := newHarness(t, func(s *Server) { s.Runner = runner })
	if code, _ := h.post("push", tagPushBody(103, "Juno/infra")); code != 202 {
		t.Fatalf("code = %d, want 202", code)
	}
	waitFor(t, "the first poke to start", func() bool { return len(runner.argvs()) >= 1 })

	done := make(chan struct{})
	go func() {
		h.srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the blocked poke")
	}
	if !strings.Contains(h.logs.String(), "poke platform/ingress failed") {
		t.Errorf("the cancelled poke must be logged:\n%s", h.logs.String())
	}
}

func TestAPokeAfterCloseIs503AndRunsNothing(t *testing.T) {
	h := newHarness(t)
	h.srv.Close()
	code, msg := h.post("push", tagPushBody(102, "alpha-org/infra"))
	if code != 503 || msg != "shutting down" {
		t.Errorf("got %d %q, want 503 shutting down so GitHub can redeliver", code, msg)
	}
	if n := len(h.runner.argvs()); n != 0 {
		t.Errorf("ran %d pokes after Close", n)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.srv.Close()
	h.srv.Close()
	h.srv.Wait()
}

// -------------------------------------------------------- NewServer / HTTP ----

func TestNewServerRefusesAShortSecret(t *testing.T) {
	log, _ := testLogger()
	_, err := NewServer(Config{Secret: "zq-7731", SocketDir: "/x"}, testTable(), log)
	if err == nil || !strings.Contains(err.Error(), "BCX_WEBHOOK_SECRET") {
		t.Errorf("err = %v, want one naming BCX_WEBHOOK_SECRET", err)
	}
}

func TestNewServerDefaults(t *testing.T) {
	log, _ := testLogger()
	srv, err := NewServer(Config{Secret: testSecret, SocketDir: "/run/x"}, testTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	if srv.MaxBody != 25<<20 {
		t.Errorf("MaxBody = %d, want 25 MiB", srv.MaxBody)
	}
	if srv.PokeTimeout != 5*time.Minute {
		t.Errorf("PokeTimeout = %v, want 5m", srv.PokeTimeout)
	}
	if srv.Forwarder.Timeout != 10*time.Second || srv.Forwarder.SocketDir != "/run/x" {
		t.Errorf("Forwarder = %+v", srv.Forwarder)
	}
	if _, ok := srv.Runner.(ExecRunner); !ok {
		t.Errorf("Runner = %T, want ExecRunner", srv.Runner)
	}
	poster, ok := srv.Poster.(GitHubPoster)
	if !ok {
		t.Fatalf("Poster = %T, want GitHubPoster", srv.Poster)
	}
	if poster.AppEnvFile != appauth.APPEnv || poster.KeyPath != appauth.APPKey || poster.APIBase != appauth.GHAPI {
		t.Errorf("Poster = %+v, want the installed app credentials", poster)
	}
	pinner, ok := srv.Pinner.(GitHubPin)
	if !ok {
		t.Fatalf("Pinner = %T, want GitHubPin", srv.Pinner)
	}
	if pinner.AppEnvFile != appauth.APPEnv || pinner.KeyPath != appauth.APPKey || pinner.APIBase != appauth.GHAPI {
		t.Errorf("Pinner = %+v, want the installed app credentials", pinner)
	}
}

func TestNewServerUsesTheConfiguredPokeTimeout(t *testing.T) {
	log, _ := testLogger()
	srv, err := NewServer(Config{Secret: testSecret, SocketDir: "/run/x", PokeTimeout: 90 * time.Second}, testTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	if srv.PokeTimeout != 90*time.Second {
		t.Errorf("PokeTimeout = %v, want the configured 90s", srv.PokeTimeout)
	}
}

func TestHTTPServerTimeoutsAreSet(t *testing.T) {
	log, _ := testLogger()
	hs := NewHTTPServer(http.NotFoundHandler(), log)
	if hs.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", hs.ReadHeaderTimeout)
	}
	if hs.ReadTimeout != 60*time.Second || hs.WriteTimeout != 60*time.Second {
		t.Errorf("Read/WriteTimeout = %v/%v, want 60s", hs.ReadTimeout, hs.WriteTimeout)
	}
	if hs.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", hs.IdleTimeout)
	}
	if hs.MaxHeaderBytes != 64<<10 {
		t.Errorf("MaxHeaderBytes = %d, want 64 KiB", hs.MaxHeaderBytes)
	}
	if hs.Handler == nil || hs.ErrorLog == nil {
		t.Errorf("Handler and ErrorLog must be set: %+v", hs)
	}
}

func TestHTTPServerHeaderErrorsGoToTheStructuredLog(t *testing.T) {
	log, out := testLogger()
	hs := NewHTTPServer(http.NotFoundHandler(), log)
	hs.ErrorLog.Print("http: test error line")
	if !strings.Contains(out.String(), "test error line") || !strings.Contains(out.String(), "level=ERROR") {
		t.Errorf("server errors must flow through slog:\n%s", out.String())
	}
}

// gatedRunner blocks every command until release is closed, then reports the
// context's state: a poke that inherited a finished request context fails here.
type gatedRunner struct{ release chan struct{} }

func (g gatedRunner) Run(ctx context.Context, _ []string) (string, error) {
	<-g.release
	return "", ctx.Err()
}

// Output blocks like Run: a poke that reads a target's status must not skip the gate.
func (g gatedRunner) Output(ctx context.Context, argv []string) (string, string, error) {
	_, err := g.Run(ctx, argv)
	return "", "", err
}

// fakePinDispatcher records every pin dispatch and may fail for testing.
type fakePinDispatcher struct {
	mu    sync.Mutex
	calls []PinJob
	err   error
}

func (f *fakePinDispatcher) Pin(_ context.Context, job PinJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, job)
	return f.err
}

func (f *fakePinDispatcher) pinned() []PinJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func TestTagPushAppRepoCallsPinDispatcher(t *testing.T) {
	pinner := &fakePinDispatcher{}
	h := newHarness(t, func(s *Server) { s.Pinner = pinner })
	code, msg := h.post("push", tagPushBody(102, "alpha-org/app"))
	if code != 202 {
		t.Fatalf("got %d %q, want 202", code, msg)
	}
	waitFor(t, "pin dispatch", func() bool { return len(pinner.pinned()) == 1 })
	got := pinner.pinned()[0]
	want := PinJob{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2.0", Installation: 102}
	if got != want {
		t.Errorf("pin job = %+v, want %+v", got, want)
	}

	// A failing dispatcher must not change the HTTP reply.
	pinner.err = errors.New("boom")
	code, _ = h.post("push", tagPushBody(102, "alpha-org/app"))
	if code != 202 {
		t.Fatalf("failing pin: got %d, want 202", code)
	}
	waitFor(t, "failing pin dispatch", func() bool { return len(pinner.pinned()) == 2 })
	h.srv.Wait()
}
