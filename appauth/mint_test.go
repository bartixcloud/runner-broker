package appauth

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNow = int64(1_700_000_000)

type seenRequest struct {
	method, path, auth, accept, apiVersion, body string
}

// githubStub is an httptest GitHub whose handler is swapped per test. It
// records every request so assertions run on the test goroutine, not inside the
// handler.
type githubStub struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func newGitHubStub(t *testing.T, handle http.HandlerFunc) *githubStub {
	t.Helper()
	g := &githubStub{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		g.mu.Lock()
		g.seen = append(g.seen, seenRequest{
			method:     r.Method,
			path:       r.URL.Path,
			auth:       r.Header.Get("Authorization"),
			accept:     r.Header.Get("Accept"),
			apiVersion: r.Header.Get("X-GitHub-Api-Version"),
			body:       string(body),
		})
		g.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		handle(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *githubStub) requests() []seenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]seenRequest(nil), g.seen...)
}

// source drives the real mint path (JWT signing, HTTP, response parsing)
// against the stub; only the base URL and clock are substituted.
func (g *githubStub) source(t *testing.T, keyPath string) *InstallationToken {
	t.Helper()
	tok := NewInstallationToken("12345", 777, keyPath)
	tok.apiBase = g.URL
	tok.clock = func() int64 { return testNow }
	return tok
}

func expiresAt(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func created(token string, expires int64) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"` + token + `","expires_at":"` + expiresAt(expires) + `"}`))
	}
}

func TestMintSendsSignedBearerJWTAndHonoursExpiry(t *testing.T) {
	keyPath, _, pub := genKey(t)
	stub := newGitHubStub(t, created("ghs_real", testNow+3600))
	tok := stub.source(t, keyPath)
	now := testNow
	tok.clock = func() int64 { return now }

	got, err := tok.Get()
	if err != nil || got != "ghs_real" {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.method)
	}
	if r.path != "/app/installations/777/access_tokens" {
		t.Errorf("path = %s", r.path)
	}
	if r.accept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", r.accept)
	}
	if r.apiVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", r.apiVersion)
	}
	if r.body != "" {
		t.Errorf("body = %q, want none: the unscoped mint must ask for no narrowing", r.body)
	}
	jwt, ok := strings.CutPrefix(r.auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a Bearer credential", r.auth)
	}
	_, claims := splitJWT(t, jwt, pub)
	if want := `{"iat":1699999940,"exp":1700000540,"iss":"12345"}`; claims != want {
		t.Errorf("claims = %s, want %s", claims, want)
	}

	// expires_at drives the cache: reuse until 300 s before it, refresh at it.
	now = testNow + 3600 - 300 - 1
	if _, err := tok.Get(); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.requests()); n != 1 {
		t.Errorf("%d requests one second before the margin, want the cached token (1)", n)
	}
	now++
	if _, err := tok.Get(); err != nil {
		t.Fatal(err)
	}
	if n := len(stub.requests()); n != 2 {
		t.Errorf("%d requests at the margin, want a refresh (2)", n)
	}
}

func TestMintAcceptsAny2xx(t *testing.T) {
	keyPath, _, _ := genKey(t)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"ghs_ok","expires_at":"` + expiresAt(testNow+3600) + `"}`))
	}
	tok := newGitHubStub(t, handler).source(t, keyPath)
	if got, err := tok.Get(); err != nil || got != "ghs_ok" {
		t.Errorf("Get() on 200 = %q, %v", got, err)
	}
}

func TestMintRejectsBadResponses(t *testing.T) {
	valid := expiresAt(testNow + 3600)
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string // substring; "" accepts any error
	}{
		{"400", reply(400, `{"message":"Bad request"}`), "400"},
		{"401 carries GitHub's message", reply(401, `{"message":"Bad credentials"}`), "Bad credentials"},
		{"403", reply(403, `{"message":"Forbidden"}`), "403"},
		{"404", reply(404, `{"message":"Not Found"}`), "404"},
		{"500", reply(500, `upstream exploded`), "500"},
		{"503", reply(503, ``), "503"},
		{"3xx is not followed", reply(302, `{"token":"ghs_x","expires_at":"`+valid+`"}`), "302"},
		{"204 empty body", reply(204, ``), ""},
		{"201 empty body", reply(201, ``), ""},
		{"garbage body", reply(201, `<html>captive portal</html>`), ""},
		{"missing token", reply(201, `{"expires_at":"`+valid+`"}`), "token"},
		{"null token", reply(201, `{"token":null,"expires_at":"`+valid+`"}`), "token"},
		{"empty token", reply(201, `{"token":"","expires_at":"`+valid+`"}`), "token"},
		{"missing expires_at", reply(201, `{"token":"ghs_x"}`), "expires_at"},
		{"null expires_at", reply(201, `{"token":"ghs_x","expires_at":null}`), "expires_at"},
		{"unparseable expires_at", reply(201, `{"token":"ghs_x","expires_at":"soon"}`), "expires_at"},
		{"wrong token type", reply(201, `{"token":12,"expires_at":"`+valid+`"}`), ""},
	}
	keyPath, _, _ := genKey(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := newGitHubStub(t, tc.handler).source(t, keyPath)
			got, err := tok.Get()
			if err == nil {
				t.Fatalf("Get() = %q, nil; want an error", got)
			}
			if got != "" {
				t.Errorf("Get() returned token %q alongside an error", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			// A rejected mint must not poison the cache with a half-valid token.
			if tok.token != "" {
				t.Errorf("cached token %q after a failed mint", tok.token)
			}
		})
	}
}

func TestMintErrorsDoNotLeakCredentials(t *testing.T) {
	keyPath, _, _ := genKey(t)
	var mu sync.Mutex
	var jwt string
	// A hostile or buggy intermediary that echoes the Authorization header in
	// its error body must not turn that into a log line.
	echo := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad credential ` + jwt + `"}`))
	}
	tok := newGitHubStub(t, echo).source(t, keyPath)
	_, err := tok.Get()
	if err == nil {
		t.Fatal("expected error")
	}
	mu.Lock()
	defer mu.Unlock()
	if jwt == "" {
		t.Fatal("stub never saw a JWT")
	}
	// A prefix, not the whole JWT: the echoed message is length-capped, so a
	// partial leak would slip past a full-string check.
	if strings.Contains(err.Error(), jwt[:60]) {
		t.Errorf("error leaks the app JWT: %v", err)
	}

	// A 2xx whose expires_at is junk must not echo the token it did return.
	bad := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_secret_value","expires_at":"garbage"}`))
	}
	_, err = newGitHubStub(t, bad).source(t, keyPath).Get()
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "ghs_secret_value") {
		t.Errorf("error leaks the installation token: %v", err)
	}
}

func TestMintDoesNotFollowRedirects(t *testing.T) {
	keyPath, _, _ := genKey(t)
	elsewhere := newGitHubStub(t, created("ghs_elsewhere", testNow+3600))
	redirector := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		// 307 would replay the POST, Authorization header and all.
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	if _, err := redirector.source(t, keyPath).Get(); err == nil {
		t.Error("expected an error for a redirect")
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Errorf("redirect target received %d requests carrying the JWT", n)
	}
}

func TestMintCapsResponseBody(t *testing.T) {
	keyPath, _, _ := genKey(t)
	oversized := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"` + expiresAt(testNow+3600) + `","pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("A", maxMintBody)))
		_, _ = w.Write([]byte(`"}`))
	}
	got, err := newGitHubStub(t, oversized).source(t, keyPath).Get()
	if err == nil {
		t.Fatalf("Get() = %q, nil; want an error for a body over the cap", got)
	}
	// Named, not just any decode failure from a truncated read.
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to report the size cap", err)
	}
}

func TestMintTimesOutOnAStalledServer(t *testing.T) {
	keyPath, _, _ := genKey(t)
	release := make(chan struct{})
	stalled := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	stub := newGitHubStub(t, stalled)
	t.Cleanup(func() { close(release) })

	tok := stub.source(t, keyPath)
	tok.client = &http.Client{Timeout: 150 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := tok.Get()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Get() on a stalled server returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get() still blocked 5 s after a 150 ms client timeout")
	}

	// The mutex must be free again: a stuck mint may not wedge later callers.
	go func() {
		_, err := tok.Get()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("second Get() on a stalled server returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Get() blocked: the token mutex was not released after the timeout")
	}
}

func TestNewInstallationTokenUsesTwentySecondClientTimeout(t *testing.T) {
	// Python's urlopen(timeout=20); the broker's and reaper's GitHub clients
	// use the same bound.
	tok := NewInstallationToken("1", 1, "k")
	if tok.client == nil {
		t.Fatal("no HTTP client configured")
	}
	if tok.client.Timeout != 20*time.Second {
		t.Errorf("client timeout = %v, want 20s", tok.client.Timeout)
	}
	if tok.apiBase != GHAPI {
		t.Errorf("apiBase = %q, want %q", tok.apiBase, GHAPI)
	}
}

func TestFailedMintIsRetriedNotCached(t *testing.T) {
	keyPath, _, _ := genKey(t)
	var mu sync.Mutex
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		created("ghs_"+strconv.Itoa(n), testNow+3600)(w, r)
	}
	tok := newGitHubStub(t, handler).source(t, keyPath)
	if _, err := tok.Get(); err == nil {
		t.Fatal("first Get() should fail on a 502")
	}
	if got, err := tok.Get(); err != nil || got != "ghs_2" {
		t.Errorf("second Get() = %q, %v, want a fresh mint", got, err)
	}
}

func TestMintScopedSendsPermissionsAndRepositories(t *testing.T) {
	keyPath, _, pub := genKey(t)
	stub := newGitHubStub(t, created("ghs_scoped", testNow+3600))

	// nil client means the default mint client; apiBase still points at the stub.
	got, err := MintScoped(nil, stub.URL, "12345", 777, keyPath,
		Scope{Permissions: map[string]string{"statuses": "write"}, Repositories: []string{"infra"}})
	if err != nil || got != "ghs_scoped" {
		t.Fatalf("MintScoped() = %q, %v", got, err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.method)
	}
	if r.path != "/app/installations/777/access_tokens" {
		t.Errorf("path = %s", r.path)
	}
	// Exact bytes: GitHub grants exactly what this body names, and nothing more.
	if want := `{"permissions":{"statuses":"write"},"repositories":["infra"]}`; r.body != want {
		t.Errorf("body = %s, want %s", r.body, want)
	}
	jwt, ok := strings.CutPrefix(r.auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a Bearer credential", r.auth)
	}
	splitJWT(t, jwt, pub)
}

func TestMintScopedPropagatesGitHubErrors(t *testing.T) {
	keyPath, _, _ := genKey(t)
	var mu sync.Mutex
	var jwt string
	echo := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden ` + jwt + `"}`))
	}
	stub := newGitHubStub(t, echo)

	got, err := MintScoped(stub.Client(), stub.URL, "12345", 777, keyPath,
		Scope{Permissions: map[string]string{"statuses": "write"}, Repositories: []string{"infra"}})
	if err == nil {
		t.Fatalf("MintScoped() = %q, nil; want an error", got)
	}
	if got != "" {
		t.Errorf("MintScoped() returned token %q alongside an error", got)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want it to carry GitHub's status", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if jwt == "" {
		t.Fatal("stub never saw a JWT")
	}
	if strings.Contains(err.Error(), jwt[:60]) {
		t.Errorf("error leaks the app JWT: %v", err)
	}
}
