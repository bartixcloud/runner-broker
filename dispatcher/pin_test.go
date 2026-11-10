package dispatcher

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGitHubPinDispatchesWithScopedToken(t *testing.T) {
	var calls []struct {
		method, path, auth, body string
	}
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, struct {
			method, path, auth, body string
		}{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(body)})
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_minted","expires_at":"2099-01-01T00:00:00Z"}`)
		case "/repos/alpha-org/infra/actions/workflows/pin.yml/dispatches":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.Close)

	poster := &GitHubPin{AppEnvFile: appEnv(t), KeyPath: appKey(t), APIBase: stub.URL, Client: stub.Client()}
	job := PinJob{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2.0", Installation: 42}
	if err := poster.Pin(context.Background(), job); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("%d requests, want mint then dispatch: %+v", len(calls), calls)
	}
	mint, dispatch := calls[0], calls[1]
	if mint.path != "/app/installations/42/access_tokens" {
		t.Errorf("mint path = %s", mint.path)
	}
	if want := `{"permissions":{"actions":"write"},"repositories":["infra"]}`; mint.body != want {
		t.Errorf("mint body = %s, want %s", mint.body, want)
	}
	if dispatch.method != http.MethodPost {
		t.Errorf("method = %s, want POST", dispatch.method)
	}
	if dispatch.path != "/repos/alpha-org/infra/actions/workflows/pin.yml/dispatches" {
		t.Errorf("dispatch path = %s", dispatch.path)
	}
	if want := "Bearer ghs_minted"; dispatch.auth != want {
		t.Errorf("dispatch auth = %q, want %q", dispatch.auth, want)
	}
	if want := `{"ref":"main","inputs":{"repo":"app","tag":"v1.2.0"}}`; dispatch.body != want {
		t.Errorf("dispatch body = %s, want %s", dispatch.body, want)
	}
}

func TestGitHubPinTreatsNon204AsError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_minted","expires_at":"2099-01-01T00:00:00Z"}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(stub.Close)

	pin := &GitHubPin{AppEnvFile: appEnv(t), KeyPath: appKey(t), APIBase: stub.URL, Client: stub.Client()}
	job := PinJob{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2.0", Installation: 42}
	err := pin.Pin(context.Background(), job)
	if err == nil {
		t.Fatal("expected error on non-204")
	}
	if !strings.Contains(err.Error(), "200") {
		t.Errorf("error = %v, want status 200", err)
	}
}

func TestGitHubPinRefusesInvalidTagOrRepoWithoutNetwork(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network reached for invalid input: %s", r.URL.Path)
	}))
	t.Cleanup(stub.Close)

	pin := &GitHubPin{AppEnvFile: appEnv(t), KeyPath: appKey(t), APIBase: stub.URL, Client: stub.Client()}
	cases := []PinJob{
		{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2", Installation: 42},
		{Owner: "", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2.0", Installation: 42},
		{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "a/b", Tag: "v1.2.0", Installation: 42},
	}
	for _, job := range cases {
		if err := pin.Pin(context.Background(), job); err == nil {
			t.Errorf("job %+v: expected an error", job)
		}
	}
}
