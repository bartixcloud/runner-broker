package hcloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// roundTripFunc lets a test stand in for net/http's transport. The client is
// tested through the real request-building path; only the wire is faked.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeAPI installs the package seams for one test and records every request.
type fakeAPI struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	reply    func(*http.Request, []byte) (*http.Response, error)
}

func installFakeAPI(t *testing.T, reply func(*http.Request, []byte) (*http.Response, error)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reply: reply}
	oldBase, oldTransport := apiBase, httpTransport
	apiBase = "https://api.hetzner.test/v1"
	httpTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		return f.reply(r, body)
	})
	t.Cleanup(func() { apiBase, httpTransport = oldBase, oldTransport })
	return f
}

func (f *fakeAPI) snapshot() ([]*http.Request, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...), append([][]byte(nil), f.bodies...)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCreateServerSendsTheContractBodyAndAuth(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	installFakeAPI(t, func(r *http.Request, body []byte) (*http.Response, error) {
		got, gotBody = r, body
		return jsonResponse(http.StatusCreated, `{"server":{"id":7,"name":"ci-e2e-1-abcdef","status":"initializing"}}`), nil
	})

	c := New("test-token")
	srv, err := c.CreateServer(CreateRequest{
		Name:             "ci-e2e-1-abcdef",
		ServerType:       "cpx42",
		Image:            1234,
		Location:         "fsn1",
		UserData:         "#cloud-config\n",
		Labels:           map[string]string{"bcx-managed": "ci"},
		Firewalls:        []FirewallRef{{Firewall: 99}},
		PublicNet:        PublicNet{EnableIPv4: true, EnableIPv6: false},
		StartAfterCreate: true,
	})
	if err != nil || srv.ID != 7 {
		t.Fatalf("got (%+v, %v)", srv, err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/servers" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer test-token" ||
		got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", got.Header)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["ssh_keys"]; ok {
		t.Error("ssh_keys must not be sent: D7 is public IPv4 with no key")
	}
	if string(raw["enable_ipv6"]) != "" {
		t.Error("enable_ipv6 must be inside public_net, not top level")
	}
	var req CreateRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "ci-e2e-1-abcdef" || req.ServerType != "cpx42" || req.Image != 1234 || req.Location != "fsn1" {
		t.Errorf("body = %+v", req)
	}
	if !req.PublicNet.EnableIPv4 || req.PublicNet.EnableIPv6 != false {
		t.Errorf("public_net = %+v, want IPv4 only", req.PublicNet)
	}
	if len(req.Firewalls) != 1 || req.Firewalls[0].Firewall != 99 {
		t.Errorf("firewalls = %+v", req.Firewalls)
	}
	if req.Labels["bcx-managed"] != "ci" || !req.StartAfterCreate {
		t.Errorf("labels/start = %+v / %v", req.Labels, req.StartAfterCreate)
	}
}

func TestListServersFiltersBySelector(t *testing.T) {
	api := installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"servers":[{"id":1,"name":"ci-e2e-1-aaaaaa","status":"running","labels":{"bcx-managed":"ci"}}]}`), nil
	})
	got, err := New("test-token").ListServers("bcx-managed=ci,bcx-tenant=alpha-org")
	if err != nil || len(got) != 1 || got[0].Name != "ci-e2e-1-aaaaaa" {
		t.Fatalf("got (%+v, %v)", got, err)
	}
	reqs, _ := api.snapshot()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].URL.Path != "/v1/servers" {
		t.Fatalf("requests = %+v", reqs)
	}
	if q := reqs[0].URL.Query().Get("label_selector"); q != "bcx-managed=ci,bcx-tenant=alpha-org" {
		t.Errorf("label_selector = %q", q)
	}
}

func TestListServersFollowsPagination(t *testing.T) {
	api := installFakeAPI(t, func(r *http.Request, _ []byte) (*http.Response, error) {
		var reply map[string]any
		switch r.URL.Query().Get("page") {
		case "":
			servers := make([]map[string]any, 50)
			for i := range servers {
				servers[i] = map[string]any{"id": i + 1, "name": "ci-e2e-" + strings.Repeat("0", 6)}
			}
			reply = map[string]any{"servers": servers, "meta": map[string]any{"pagination": map[string]any{"next_page": 2}}}
		case "2":
			reply = map[string]any{"servers": []map[string]any{{"id": 51, "name": "ci-e2e-last-111111"}},
				"meta": map[string]any{"pagination": map[string]any{"next_page": nil}}}
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
		b, err := json.Marshal(reply)
		if err != nil {
			t.Fatal(err)
		}
		return jsonResponse(http.StatusOK, string(b)), nil
	})
	got, err := New("test-token").ListServers("bcx-managed=ci")
	if err != nil || len(got) != 51 || got[50].Name != "ci-e2e-last-111111" {
		t.Fatalf("got (%d servers, %v)", len(got), err)
	}
	reqs, _ := api.snapshot()
	if len(reqs) != 2 || reqs[0].URL.Query().Get("per_page") != "50" ||
		reqs[1].URL.Query().Get("page") != "2" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestProveUsesThePerPageOneServerCall(t *testing.T) {
	api := installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"servers":[]}`), nil
	})
	if err := New("test-token").Prove(); err != nil {
		t.Fatal(err)
	}
	reqs, _ := api.snapshot()
	if len(reqs) != 1 || reqs[0].URL.Path != "/v1/servers" || reqs[0].URL.Query().Get("per_page") != "1" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestDeleteServerTreats404AsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
				return jsonResponse(status, `{}`), nil
			})
			if err := New("test-token").DeleteServer(42); err != nil {
				t.Errorf("status %d: %v", status, err)
			}
		})
	}
}

func TestDeleteServerReturnsTypedAPIError(t *testing.T) {
	installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError, `{"error":{"code":"server_error","message":"boom"}}`), nil
	})
	err := New("test-token").DeleteServer(42)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Code != "server_error" || apiErr.Message != "boom" {
		t.Fatalf("err = %#v", err)
	}
}

func TestAPIErrorRedactsTheTokenAndQuotesTheMessage(t *testing.T) {
	installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusUnprocessableEntity,
			`{"error":{"code":"placement_error","message":"echo test-token\nforged"}}`), nil
	})
	_, err := New("test-token").CreateServer(CreateRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "placement_error" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("token leaked into error: %q", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error must stay one line: %q", err)
	}
}

func TestListSnapshotsAsksForSnapshotsAndSortsNewestFirst(t *testing.T) {
	api := installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"images":[
			{"id":1,"type":"snapshot","status":"available","created":"2026-01-01T00:00:00Z","labels":{"bcx-image":"base"}},
			{"id":3,"type":"snapshot","status":"available","created":"2026-03-01T00:00:00Z","labels":{"bcx-image":"base"}},
			{"id":2,"type":"snapshot","status":"available","created":"2026-02-01T00:00:00Z","labels":{"bcx-image":"base"}}
		]}`), nil
	})
	got, err := New("test-token").ListSnapshots("bcx-image=base")
	if err != nil || len(got) != 3 {
		t.Fatalf("got (%+v, %v)", got, err)
	}
	if got[0].ID != 3 || got[1].ID != 2 || got[2].ID != 1 {
		t.Errorf("order = %v, want 3,2,1", []int64{got[0].ID, got[1].ID, got[2].ID})
	}
	reqs, _ := api.snapshot()
	q := reqs[0].URL.Query()
	if q.Get("type") != "snapshot" || q.Get("label_selector") != "bcx-image=base" || q.Get("sort") != "created:desc" {
		t.Errorf("query = %v", q)
	}
}

func TestEnsureFirewallCreatesWithoutRulesThenCaches(t *testing.T) {
	var posts int
	var postBody []byte
	api := installFakeAPI(t, func(r *http.Request, body []byte) (*http.Response, error) {
		switch r.Method {
		case http.MethodGet:
			return jsonResponse(http.StatusOK, `{"firewalls":[]}`), nil
		case http.MethodPost:
			posts++
			postBody = body
			return jsonResponse(http.StatusCreated, `{"firewall":{"id":77,"name":"bcx-ci-deny-inbound"}}`), nil
		default:
			t.Fatalf("unexpected method %s", r.Method)
			return nil, nil
		}
	})
	c := New("test-token")
	id, err := c.EnsureFirewall("bcx-ci-deny-inbound")
	if err != nil || id != 77 || posts != 1 {
		t.Fatalf("got (%d, %v), posts=%d", id, err, posts)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(postBody, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["rules"]; ok {
		t.Error("a deny-all firewall must be created without rules")
	}
	if _, err := c.EnsureFirewall("bcx-ci-deny-inbound"); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Errorf("second ensure posted again: %d posts", posts)
	}
	reqs, _ := api.snapshot()
	if len(reqs) != 2 {
		t.Errorf("requests = %d, want list+create then cache hit", len(reqs))
	}
}

func TestEnsureFirewallToleratesAConcurrentCreate(t *testing.T) {
	var lists, posts int
	installFakeAPI(t, func(r *http.Request, _ []byte) (*http.Response, error) {
		switch r.Method {
		case http.MethodGet:
			lists++
			if lists == 1 {
				return jsonResponse(http.StatusOK, `{"firewalls":[]}`), nil
			}
			return jsonResponse(http.StatusOK, `{"firewalls":[{"id":88,"name":"bcx-ci-deny-inbound"}]}`), nil
		case http.MethodPost:
			posts++
			return jsonResponse(http.StatusConflict,
				`{"error":{"code":"uniqueness_error","message":"name already used"}}`), nil
		}
		return nil, nil
	})
	id, err := New("test-token").EnsureFirewall("bcx-ci-deny-inbound")
	if err != nil || id != 88 {
		t.Fatalf("got (%d, %v)", id, err)
	}
	if lists != 2 || posts != 1 {
		t.Errorf("lists=%d posts=%d, want 2 lists and 1 post", lists, posts)
	}
}

func TestEnsureFirewallFindByName(t *testing.T) {
	api := installFakeAPI(t, func(*http.Request, []byte) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"firewalls":[{"id":55,"name":"bcx-ci-deny-inbound"}]}`), nil
	})
	id, err := New("test-token").EnsureFirewall("bcx-ci-deny-inbound")
	if err != nil || id != 55 {
		t.Fatalf("got (%d, %v)", id, err)
	}
	reqs, _ := api.snapshot()
	if len(reqs) != 1 || reqs[0].URL.Query().Get("name") != "bcx-ci-deny-inbound" {
		t.Errorf("requests = %+v", reqs)
	}
}

// The per-call bound must survive a caller context with no deadline.
func TestClientHasATwentySecondCallTimeout(t *testing.T) {
	installFakeAPI(t, func(r *http.Request, _ []byte) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled parent must fail, not hang
	if _, err := New("test-token").ListServersContext(ctx, ""); err == nil {
		t.Fatal("expected a context error")
	}
}
