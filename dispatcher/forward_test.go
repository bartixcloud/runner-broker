package dispatcher

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// seenRequest is what a stub broker received.
type seenRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// brokerStub is a tenant broker on a unix socket that records what it receives.
type brokerStub struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (b *brokerStub) requests() []seenRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.seen)
}

// record wraps next so every request is stored before it is answered.
func (b *brokerStub) record(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.seen = append(b.seen, seenRequest{r.Method, r.URL.Path, r.Header.Clone(), body})
		b.mu.Unlock()
		next(w, r)
	}
}

// spawning is the Python stub's answer: 202 {"status": "spawning"}.
func spawning(w http.ResponseWriter, _ *http.Request) {
	out := []byte(`{"status": "spawning"}`)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(out)
}

// serveBroker listens on <dir>/<tenant>.sock and returns the stub plus the socket
// directory. The path stays short (macOS AF_UNIX cap) because dir comes from shortTmp.
func serveBroker(t *testing.T, dir, tenant string, h http.HandlerFunc) *brokerStub {
	t.Helper()
	stub := &brokerStub{}
	ln, err := net.Listen("unix", filepath.Join(dir, tenant+".sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(stub.record(h))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return stub
}

func testForwarder(dir string) Forwarder {
	return Forwarder{SocketDir: dir, Timeout: 5 * time.Second}
}

func TestForwardRelaysBodyHeadersAndStatus(t *testing.T) {
	dir := shortTmp(t)
	stub := serveBroker(t, dir, "alpha-org", spawning)

	got, err := testForwarder(dir).Forward(t.Context(), "alpha-org", []byte(`{"a":1}`), ForwardHeaders{
		Event: "workflow_job", Delivery: "d1", ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got.Status != 202 || string(got.Body) != `{"status": "spawning"}` {
		t.Errorf("response = %d %q", got.Status, got.Body)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("broker saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/github" || string(r.Body) != `{"a":1}` || r.Header.Get("X-GitHub-Event") != "workflow_job" {
		t.Errorf("broker saw %s %s %q event=%q", r.Method, r.Path, r.Body, r.Header.Get("X-GitHub-Event"))
	}
}

func TestForwardSendsExactlyTheWhitelistedHeaders(t *testing.T) {
	dir := shortTmp(t)
	stub := serveBroker(t, dir, "t1", spawning)

	_, err := testForwarder(dir).Forward(t.Context(), "t1", []byte(`{}`), ForwardHeaders{
		Event: "workflow_job", Delivery: "d1", TargetID: "42", ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	header := stub.requests()[0].Header
	// Content-Length and Connection are transport framing, not forwarded metadata.
	allowed := []string{
		"X-Github-Event", "X-Github-Delivery", "X-Github-Hook-Installation-Target-Id",
		"Content-Type", "Content-Length", "Connection",
	}
	for name := range header {
		if !slices.Contains(allowed, name) {
			t.Errorf("unexpected header %q = %q reached the broker", name, header[name])
		}
	}
	for name, want := range map[string]string{
		"X-GitHub-Event": "workflow_job", "X-GitHub-Delivery": "d1",
		"X-GitHub-Hook-Installation-Target-ID": "42", "Content-Type": "application/json",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if header.Get("X-Hub-Signature-256") != "" || header.Get("User-Agent") != "" || header.Get("Accept-Encoding") != "" {
		t.Errorf("signature or client noise leaked: %v", header)
	}
}

func TestForwardOmitsEmptyHeaders(t *testing.T) {
	dir := shortTmp(t)
	stub := serveBroker(t, dir, "t1", spawning)
	if _, err := testForwarder(dir).Forward(t.Context(), "t1", []byte(`{}`), ForwardHeaders{Event: "push"}); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	header := stub.requests()[0].Header
	for _, name := range []string{"X-GitHub-Delivery", "X-GitHub-Hook-Installation-Target-ID", "Content-Type"} {
		if _, present := header[http.CanonicalHeaderKey(name)]; present {
			t.Errorf("%s was sent although it was empty", name)
		}
	}
}

func TestForwardRelaysTheBrokersStatusAndBodyVerbatim(t *testing.T) {
	for _, status := range []int{200, 202, 400, 403, 404, 500, 503} {
		dir := shortTmp(t)
		want := `{"status":"from broker ` + http.StatusText(status) + `"}`
		serveBroker(t, dir, "t1", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, want)
		})
		got, err := testForwarder(dir).Forward(t.Context(), "t1", []byte(`{}`), ForwardHeaders{})
		if err != nil {
			t.Fatalf("status %d: Forward: %v", status, err)
		}
		if got.Status != status || string(got.Body) != want {
			t.Errorf("status %d: got %d %q, want the broker's answer verbatim", status, got.Status, got.Body)
		}
	}
}

func TestForwardDoesNotFollowRedirects(t *testing.T) {
	dir := shortTmp(t)
	stub := serveBroker(t, dir, "t1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
	})
	got, err := testForwarder(dir).Forward(t.Context(), "t1", []byte(`{}`), ForwardHeaders{})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got.Status != http.StatusFound {
		t.Errorf("status = %d, want the broker's 302 relayed", got.Status)
	}
	if n := len(stub.requests()); n != 1 {
		t.Errorf("broker saw %d requests, want 1", n)
	}
}

func TestForwardToAMissingBrokerIsAnError(t *testing.T) {
	_, err := testForwarder(shortTmp(t)).Forward(t.Context(), "nobody", []byte(`{}`), ForwardHeaders{})
	if err == nil {
		t.Fatal("expected an error for a missing broker socket")
	}
}

func TestForwardToAStaleNonSocketFileIsAnError(t *testing.T) {
	dir := shortTmp(t)
	if err := os.WriteFile(filepath.Join(dir, "gone.sock"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := testForwarder(dir).Forward(t.Context(), "gone", []byte(`{}`), ForwardHeaders{}); err == nil {
		t.Fatal("expected an error for a stale non-socket file")
	}
}

func TestForwardRefusesTenantNamesThatCouldEscapeTheSocketDir(t *testing.T) {
	root := shortTmp(t)
	dir := filepath.Join(root, "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A live broker one level above the socket dir: a traversal would reach it.
	outside := serveBroker(t, root, "outside", spawning)

	for _, tenant := range []string{"../outside", "a/b", "", ".", "..", "-x", "a b"} {
		if _, err := testForwarder(dir).Forward(t.Context(), tenant, []byte(`{}`), ForwardHeaders{}); err == nil {
			t.Errorf("tenant %q: expected an error", tenant)
		}
	}
	if n := len(outside.requests()); n != 0 {
		t.Errorf("a traversal reached the outside broker %d time(s)", n)
	}
}

func TestForwardTimesOutOnASlowBroker(t *testing.T) {
	dir := shortTmp(t)
	serveBroker(t, dir, "slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	fwd := Forwarder{SocketDir: dir, Timeout: 100 * time.Millisecond}
	start := time.Now()
	if _, err := fwd.Forward(t.Context(), "slow", []byte(`{}`), ForwardHeaders{}); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Forward took %v with a 100ms timeout", elapsed)
	}
}

func TestForwardRejectsAnOversizedBrokerResponse(t *testing.T) {
	dir := shortTmp(t)
	serveBroker(t, dir, "big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxBrokerResponse+1))
	})
	if _, err := testForwarder(dir).Forward(t.Context(), "big", []byte(`{}`), ForwardHeaders{}); err == nil {
		t.Fatal("expected an error for a response over the cap")
	}
}

func TestForwardAcceptsAResponseExactlyAtTheCap(t *testing.T) {
	dir := shortTmp(t)
	serveBroker(t, dir, "edge", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxBrokerResponse))
	})
	got, err := testForwarder(dir).Forward(t.Context(), "edge", []byte(`{}`), ForwardHeaders{})
	if err != nil || len(got.Body) != maxBrokerResponse {
		t.Fatalf("got %d bytes, err %v; want the full body", len(got.Body), err)
	}
}

func TestForwardHonoursAnAlreadyCancelledContext(t *testing.T) {
	dir := shortTmp(t)
	stub := serveBroker(t, dir, "t1", spawning)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := testForwarder(dir).Forward(ctx, "t1", []byte(`{}`), ForwardHeaders{}); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(stub.requests()); n != 0 {
		t.Errorf("broker saw %d requests after cancel", n)
	}
}
