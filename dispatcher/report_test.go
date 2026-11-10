package dispatcher

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	otherCommit = "fedcba9876543210fedcba9876543210fedcba98"
)

// convergeStatusJSON builds a minimal converge-status document naming one source,
// shaped like nuk's SourceBlock.to_wire.
func convergeStatusJSON(t *testing.T, source string, block map[string]any) string {
	t.Helper()
	sources := map[string]any{}
	if source != "" {
		sources[source] = block
	}
	b, err := json.Marshal(map[string]any{
		"role":         "edge",
		"appliedAt":    "2026-10-26T08:00:00Z",
		"sources":      sources,
		"reapWarnings": []string{},
	})
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return string(b)
}

func TestConvergeStateSuccess(t *testing.T) {
	state, description := ConvergeState("juno", testCommit, convergeStatusJSON(t, "juno", map[string]any{
		"result": "ok", "tag": "v0.3.3", "commit": testCommit, "error": nil,
	}))
	if state != "success" {
		t.Errorf("state = %q, want success", state)
	}
	if !strings.Contains(description, "applied v0.3.3") {
		t.Errorf("description = %q, want it to name the applied tag", description)
	}
}

func TestConvergeStateRefusedSource(t *testing.T) {
	const nukErr = "no v* tag verifies against /etc/nuk/allowed_signers.d/juno"
	state, description := ConvergeState("juno", testCommit, convergeStatusJSON(t, "juno", map[string]any{
		"result": "refused", "tag": nil, "commit": testCommit, "error": nukErr,
	}))
	if state != "failure" {
		t.Errorf("state = %q, want failure", state)
	}
	if !strings.Contains(description, nukErr) {
		t.Errorf("description = %q, want the host's error text", description)
	}
}

func TestConvergeStateDifferentCommit(t *testing.T) {
	state, description := ConvergeState("juno", testCommit, convergeStatusJSON(t, "juno", map[string]any{
		"result": "ok", "tag": "v0.3.2", "commit": otherCommit, "error": "tag v0.3.2 superseded",
	}))
	if state != "failure" {
		t.Errorf("state = %q, want failure", state)
	}
	for _, want := range []string{"v0.3.2", "superseded"} {
		if !strings.Contains(description, want) {
			t.Errorf("description = %q, want it to contain %q", description, want)
		}
	}
}

func TestConvergeStateMissingSource(t *testing.T) {
	state, description := ConvergeState("juno", testCommit, convergeStatusJSON(t, "alpha-org", map[string]any{
		"result": "ok", "tag": "v0.3.3", "commit": testCommit,
	}))
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
	if want := "source juno not in converge status"; !strings.Contains(description, want) {
		t.Errorf("description = %q, want %q", description, want)
	}
}

func TestConvergeStateBadJSON(t *testing.T) {
	for _, body := range []string{"", "{not json", `{"sources":"nope"}`} {
		state, description := ConvergeState("juno", testCommit, body)
		if state != "error" {
			t.Errorf("state(%q) = %q, want error", body, state)
		}
		if description == "" {
			t.Errorf("state(%q) has an empty description", body)
		}
	}
}

func TestConvergeStateClipsTo140(t *testing.T) {
	long := strings.Repeat("x", 500)
	_, description := ConvergeState("juno", testCommit, convergeStatusJSON(t, "juno", map[string]any{
		"result": "refused", "commit": testCommit, "error": long,
	}))
	if n := len([]rune(description)); n > 140 {
		t.Errorf("description is %d characters, want at most 140", n)
	}

	// A byte cut through a multi-byte rune must not leave invalid UTF-8 behind.
	multi := strings.Repeat("ü", 200)
	_, description = ConvergeState("juno", testCommit, convergeStatusJSON(t, "juno", map[string]any{
		"result": "refused", "commit": testCommit, "error": multi,
	}))
	if !utf8.ValidString(description) {
		t.Errorf("description is not valid UTF-8: %q", description)
	}
	if n := len([]rune(description)); n > 140 {
		t.Errorf("description is %d characters, want at most 140", n)
	}
}

// ------------------------------------------------------------ GitHubPoster ----

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyErr  error
)

// appKey writes a PKCS#1 app key once per test binary; generation dominates the
// suite's runtime under -race.
func appKey(t *testing.T) string {
	t.Helper()
	testKeyOnce.Do(func() {
		testKey, testKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if testKeyErr != nil {
		t.Fatalf("generate key: %v", testKeyErr)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(testKey),
	})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// appEnv writes the systemd EnvironmentFile the dispatcher reads the App id from.
func appEnv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte("BCX_APP_ID=12345\n"), 0o600); err != nil {
		t.Fatalf("write app.env: %v", err)
	}
	// The ambient environment wins over the file; keep the fixture in charge.
	t.Setenv("BCX_APP_ID", "")
	return path
}

type githubCall struct{ method, path, auth, body string }

// githubStub is an httptest GitHub serving the access_tokens and statuses routes,
// recording every request so assertions run outside the handler goroutine.
type githubStub struct {
	*httptest.Server
	mu    sync.Mutex
	calls []githubCall
}

func newGitHub(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *githubStub {
	t.Helper()
	g := &githubStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_minted","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("/repos/juno-de/infra/statuses/"+testCommit, handle)
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		g.mu.Lock()
		g.calls = append(g.calls, githubCall{
			method: r.Method,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			body:   string(body),
		})
		g.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *githubStub) requests() []githubCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

func (g *githubStub) poster(t *testing.T) *GitHubPoster {
	t.Helper()
	return &GitHubPoster{
		AppEnvFile: appEnv(t),
		KeyPath:    appKey(t),
		APIBase:    g.URL,
		Client:     g.Client(),
	}
}

func testReport() Report {
	return Report{Repo: "juno-de/infra", Commit: testCommit, Source: "juno", Installation: 42}
}

func TestGitHubPosterPostsContextStateAndDescription(t *testing.T) {
	stub := newGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	poster := stub.poster(t)
	if err := poster.Post(context.Background(), testReport(), "nuk/converge/edge", "success", "applied v0.3.3"); err != nil {
		t.Fatalf("Post: %v", err)
	}

	var status *githubCall
	for _, call := range stub.requests() {
		if strings.Contains(call.path, "/statuses/") {
			c := call
			status = &c
		}
	}
	if status == nil {
		t.Fatalf("no status POST among %+v", stub.requests())
	}
	if status.method != http.MethodPost {
		t.Errorf("method = %s, want POST", status.method)
	}
	if want := "/repos/juno-de/infra/statuses/" + testCommit; status.path != want {
		t.Errorf("path = %s, want %s", status.path, want)
	}

	var body map[string]string
	if err := json.Unmarshal([]byte(status.body), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", status.body, err)
	}
	want := map[string]string{
		"state":       "success",
		"description": "applied v0.3.3",
		"context":     "nuk/converge/edge",
	}
	if len(body) != len(want) {
		t.Errorf("body has %d fields, want exactly %d: %s", len(body), len(want), status.body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %q, want %q", k, body[k], v)
		}
	}
}

func TestGitHubPosterMintsAScopedToken(t *testing.T) {
	stub := newGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	poster := stub.poster(t)
	if err := poster.Post(context.Background(), testReport(), "nuk/converge/edge", "failure", "refused"); err != nil {
		t.Fatalf("Post: %v", err)
	}

	calls := stub.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want a mint then a status POST: %+v", len(calls), calls)
	}
	mint, status := calls[0], calls[1]
	if want := "/app/installations/42/access_tokens"; mint.path != want {
		t.Errorf("mint path = %s, want %s", mint.path, want)
	}
	// Exact bytes: the token may do nothing but write statuses on this one repo.
	if want := `{"permissions":{"statuses":"write"},"repositories":["infra"]}`; mint.body != want {
		t.Errorf("mint body = %s, want %s", mint.body, want)
	}
	if mint.auth == "" || !strings.HasPrefix(mint.auth, "Bearer ") {
		t.Errorf("mint Authorization = %q, want a Bearer app JWT", mint.auth)
	}
	if want := "Bearer ghs_minted"; status.auth != want {
		t.Errorf("status Authorization = %q, want the minted token %q", status.auth, want)
	}
}

func TestGitHubPosterTreatsNon201AsError(t *testing.T) {
	stub := newGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	err := stub.poster(t).Post(context.Background(), testReport(), "nuk/converge/edge", "error", "host unreachable")
	if err == nil {
		t.Fatal("a 200 on a status create must be an error")
	}
	if !strings.Contains(err.Error(), "200") {
		t.Errorf("error = %v, want it to carry GitHub's status", err)
	}
}

func TestGitHubPosterRefusesANonCommitRevision(t *testing.T) {
	stub := newGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	for _, commit := range []string{"", "HEAD", "../../repos/x/statuses/y", strings.Repeat("a", 39), strings.Repeat("A", 40)} {
		job := testReport()
		job.Commit = commit
		if err := stub.poster(t).Post(context.Background(), job, "nuk/converge/edge", "success", "applied"); err == nil {
			t.Errorf("commit %q: expected an error", commit)
		}
	}
	if calls := stub.requests(); len(calls) != 0 {
		t.Errorf("sent %d requests for an unusable commit: %+v", len(calls), calls)
	}
}

// TestGitHubPosterRefusesAnUnusableRepository pins that the URL path and the mint
// scope only ever see a plain "owner/name".
func TestGitHubPosterRefusesAnUnusableRepository(t *testing.T) {
	stub := newGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	for _, repo := range []string{"", "juno-de", "/infra", "juno-de/", "juno-de/..", "juno-de/a b", "juno-de/infra/extra"} {
		job := testReport()
		job.Repo = repo
		if err := stub.poster(t).Post(context.Background(), job, "nuk/converge/edge", "success", "applied"); err == nil {
			t.Errorf("repo %q: expected an error", repo)
		}
	}
	if calls := stub.requests(); len(calls) != 0 {
		t.Errorf("sent %d requests for an unusable repo: %+v", len(calls), calls)
	}
}

func ExampleConvergeState() {
	status := `{"sources":{"juno":{"result":"ok","tag":"v0.3.3","commit":"0123456789abcdef0123456789abcdef01234567"}}}`
	fmt.Println(ConvergeState("juno", "0123456789abcdef0123456789abcdef01234567", status))
	// Output: success applied v0.3.3
}
