package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testToken = "ghs_secret_token"
	// testNow is a whole second so ages built from it are exact in float64.
	testNow = 1_800_000_000
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func response(status int, body, link string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
	if link != "" {
		resp.Header.Set("Link", link)
	}
	return resp
}

func jsonResponse(body, link string) *http.Response {
	return response(http.StatusOK, body, link)
}

type fakeInstance struct {
	name       string
	ageSeconds float64
}

// fixture wires run() to fakes for incus and GitHub and records what the pass
// did. Defaults describe a healthy org tenant; a test overrides only what the
// behaviour under test depends on.
type fixture struct {
	t          *testing.T
	env        map[string]string
	now        time.Time // the pass's clock; instance ages are relative to it
	d          deps
	logs       bytes.Buffer
	incusCalls [][]string
	requests   []*http.Request
	instances  []fakeInstance
	failDelete map[string]string // instance name -> stderr of the failing delete
	github     func(*http.Request) *http.Response
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:   t,
		now: time.Unix(testNow, 0),
		env: map[string]string{
			"BROKER_TENANT":          "alpha-org",
			"BROKER_TARGET_KIND":     "org",
			"BROKER_TARGET":          "alpha-org",
			"BROKER_INSTALLATION_ID": "42",
			"BROKER_INCUS_PROJECT":   "juno",
		},
		failDelete: map[string]string{},
		github: func(*http.Request) *http.Response {
			return jsonResponse(`{"total_count":1,"runners":[]}`, "")
		},
	}
	f.d = deps{
		now:   func() time.Time { return f.now },
		ghAPI: "https://api.github.com",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			f.requests = append(f.requests, req)
			return f.github(req), nil
		})},
		mintToken: func(int) (string, error) { return testToken, nil },
		execIncus: f.execIncus,
		log:       log.New(&f.logs, "", 0),
	}
	return f
}

func (f *fixture) execIncus(argv []string) incusResult {
	f.incusCalls = append(f.incusCalls, argv)
	switch {
	case len(argv) > 3 && argv[3] == "list":
		return incusResult{stdout: f.listJSON()}
	case len(argv) > 4 && argv[3] == "delete":
		if stderr, bad := f.failDelete[argv[4]]; bad {
			return incusResult{exitCode: 1, stderr: stderr}
		}
		return incusResult{}
	}
	f.t.Errorf("unexpected incus call %v", argv)
	return incusResult{exitCode: 1, stderr: "unexpected call"}
}

func (f *fixture) listJSON() string {
	elems := make([]string, 0, len(f.instances))
	for _, i := range f.instances {
		created := f.now.Add(-time.Duration(i.ageSeconds * float64(time.Second)))
		elems = append(elems, fmt.Sprintf(`{"name":%q,"created_at":%q}`, i.name, created.UTC().Format(time.RFC3339Nano)))
	}
	return "[" + strings.Join(elems, ",") + "]"
}

func (f *fixture) run() int {
	return run(nil, f.env, f.d)
}

func (f *fixture) deletes() [][]string {
	var out [][]string
	for _, argv := range f.incusCalls {
		if len(argv) > 3 && argv[3] == "delete" {
			out = append(out, argv)
		}
	}
	return out
}

func (f *fixture) userTenant(repos string) {
	f.env["BROKER_TARGET_KIND"] = "user"
	f.env["BROKER_TARGET"] = "Juno"
	f.env["BROKER_REPOS"] = repos
}

func (f *fixture) assertLogContains(want string) {
	f.t.Helper()
	if !strings.Contains(f.logs.String(), want) {
		f.t.Errorf("log does not contain %q:\n%s", want, f.logs.String())
	}
}

func idleInstance() fakeInstance { return fakeInstance{name: "ci-build-1-aaa", ageSeconds: 20 * 60} }

// ------------------------------------------------ decision function (pure)

func TestReapsOldIdleInstance(t *testing.T) {
	now := float64(testNow)
	instances := []instance{{Name: "ci-build-1-aaa", CreatedAt: now - 20*60}}
	got := instancesToReap(instances, map[string]struct{}{}, now, true)
	if len(got) != 1 || got[0] != "ci-build-1-aaa" {
		t.Errorf("instancesToReap = %v, want [ci-build-1-aaa]", got)
	}
}

func TestKeepsRecentIdleInstance(t *testing.T) {
	now := float64(testNow)
	instances := []instance{{Name: "ci-build-1-aaa", CreatedAt: now - 5*60}}
	got := instancesToReap(instances, map[string]struct{}{}, now, true)
	if len(got) != 0 {
		t.Errorf("instancesToReap = %v, want []", got)
	}
}

func TestKeepsBusyInstanceUnderHardCeiling(t *testing.T) {
	now := float64(testNow)
	instances := []instance{{Name: "ci-infra-1-aaa", CreatedAt: now - 20*60}}
	busy := map[string]struct{}{"ci-infra-1-aaa": {}}
	got := instancesToReap(instances, busy, now, true)
	if len(got) != 0 {
		t.Errorf("instancesToReap = %v, want []", got)
	}
}

func TestReapsBusyInstancePastHardCeiling(t *testing.T) {
	now := float64(testNow)
	instances := []instance{{Name: "ci-infra-1-aaa", CreatedAt: now - 7*3600}}
	busy := map[string]struct{}{"ci-infra-1-aaa": {}}
	got := instancesToReap(instances, busy, now, true)
	if len(got) != 1 || got[0] != "ci-infra-1-aaa" {
		t.Errorf("instancesToReap = %v, want [ci-infra-1-aaa]", got)
	}
}

func TestSkipsIdleReapWhenGitHubReportsZeroRunners(t *testing.T) {
	now := float64(testNow)
	instances := []instance{
		{Name: "ci-build-1-aaa", CreatedAt: now - 20*60},
		{Name: "ci-build-2-bbb", CreatedAt: now - 7*3600},
	}
	got := instancesToReap(instances, map[string]struct{}{}, now, false)
	if len(got) != 1 || got[0] != "ci-build-2-bbb" {
		t.Errorf("instancesToReap = %v, want [ci-build-2-bbb]", got)
	}
}

// The comparisons are strict: an instance exactly at a limit is still within it.
func TestTTLBoundariesAreStrict(t *testing.T) {
	cases := []struct {
		name string
		age  float64
		busy bool
		want bool
	}{
		{"idle at exactly the idle TTL is kept", idleTTLSeconds, false, false},
		{"idle one second past the idle TTL is reaped", idleTTLSeconds + 1, false, true},
		{"busy at exactly the hard ceiling is kept", hardCeilingSeconds, true, false},
		{"busy one second past the hard ceiling is reaped", hardCeilingSeconds + 1, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := float64(testNow)
			busy := map[string]struct{}{}
			if tc.busy {
				busy["ci-a"] = struct{}{}
			}
			got := instancesToReap([]instance{{Name: "ci-a", CreatedAt: now - tc.age}}, busy, now, true)
			if reaped := len(got) == 1; reaped != tc.want {
				t.Errorf("age %v busy %v: reaped = %v, want %v", tc.age, tc.busy, reaped, tc.want)
			}
		})
	}
}

// Whole-second truncation of both clocks reaped boundary instances up to a
// second off from the Python float arithmetic: created 0.1 s into a second and
// "now" 0.9 s into another make 900.8 s, which is past the TTL.
func TestSubSecondAgeIsNotTruncated(t *testing.T) {
	f := newFixture(t)
	f.now = time.Unix(testNow, 900_000_000)
	f.instances = []fakeInstance{{name: "ci-build-1-aaa", ageSeconds: 900.8}}
	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if len(f.deletes()) != 1 {
		t.Errorf("deletes = %v, want the 900.8 s old instance reaped", f.deletes())
	}
}

// ------------------------------------------------ Link header

func TestParseLinkHeaderWithNext(t *testing.T) {
	link := `<https://api.github.com/page2>; rel="next", <https://api.github.com/last>; rel="last"`
	if got := parseLinkHeader(link, "next"); got != "https://api.github.com/page2" {
		t.Errorf("parseLinkHeader = %q", got)
	}
}

func TestParseLinkHeaderWithoutRel(t *testing.T) {
	link := `<https://api.github.com/last>; rel="last"`
	if got := parseLinkHeader(link, "next"); got != "" {
		t.Errorf("parseLinkHeader = %q, want empty", got)
	}
}

func TestParseLinkHeaderEmpty(t *testing.T) {
	if got := parseLinkHeader("", "next"); got != "" {
		t.Errorf("parseLinkHeader(\"\") = %q", got)
	}
}

// ------------------------------------------------ GitHub busy / total

const page2URL = "https://api.github.com/orgs/alpha-org/actions/runners?page=2&per_page=100"

func TestBusyRunnerNamesFollowsPaginationAndAuthenticates(t *testing.T) {
	f := newFixture(t)
	f.github = func(req *http.Request) *http.Response {
		if req.URL.Query().Get("page") == "2" {
			return jsonResponse(`{"total_count":2,"runners":[{"name":"ci-build-2-bbb","busy":true}]}`, "")
		}
		return jsonResponse(`{"total_count":2,"runners":[{"name":"ci-build-1-aaa","busy":true}]}`,
			`<`+page2URL+`>; rel="next"`)
	}

	busy, err := f.d.busyRunnerNames("/orgs/alpha-org", testToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci-build-1-aaa", "ci-build-2-bbb"} {
		if _, ok := busy[name]; !ok {
			t.Errorf("missing %s in %v", name, busy)
		}
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.requests))
	}
	if got := f.requests[0].URL.String(); got != "https://api.github.com/orgs/alpha-org/actions/runners?per_page=100" {
		t.Errorf("first URL = %q", got)
	}
	if got := f.requests[1].URL.String(); got != page2URL {
		t.Errorf("second URL = %q, want the Link rel=next URL", got)
	}
	for i, req := range f.requests {
		if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("request %d Authorization = %q", i, got)
		}
		if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("request %d Accept = %q", i, got)
		}
	}
}

func TestBusyRunnerNamesUsesTheGivenTargetPath(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":2,"runners":[{"name":"ci-build-1-aaa","busy":true},{"name":"ci-build-2-bbb","busy":false}]}`, "")
	}

	busy, err := f.d.busyRunnerNames("/repos/Juno/app", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := busy["ci-build-1-aaa"]; !ok || len(busy) != 1 {
		t.Errorf("busy = %v, want only ci-build-1-aaa", busy)
	}
	if len(f.requests) != 1 || !strings.HasPrefix(f.requests[0].URL.String(), "https://api.github.com/repos/Juno/app/actions/runners") {
		t.Errorf("requests = %v", f.requests)
	}
}

func TestRunnerTotalReadsTotalCountNotRunnerCount(t *testing.T) {
	f := newFixture(t)
	f.github = func(req *http.Request) *http.Response {
		if req.URL.Query().Get("per_page") != "1" {
			t.Errorf("total must be fetched with per_page=1, got %s", req.URL)
		}
		return jsonResponse(`{"total_count":7,"runners":[{"name":"ci-a","busy":false}]}`, "")
	}
	got, err := f.d.runnerTotal("/orgs/alpha-org", testToken)
	if err != nil || got != 7 {
		t.Errorf("runnerTotal = %d, %v; want 7, nil", got, err)
	}
}

// A required field that is absent or null must never decode to a zero value: a
// runner without "busy" would silently count as idle, a page without
// "total_count" as an empty tenant.
func TestRunnerPageDecodingRejectsMissingRequiredFields(t *testing.T) {
	bodies := map[string]string{
		"empty object":           `{}`,
		"null body":              `null`,
		"array body":             `[]`,
		"not json":               `<html>502 Bad Gateway</html>`,
		"missing total_count":    `{"runners":[]}`,
		"null total_count":       `{"total_count":null,"runners":[]}`,
		"missing runners":        `{"total_count":1}`,
		"null runners":           `{"total_count":1,"runners":null}`,
		"runner without name":    `{"total_count":1,"runners":[{"busy":true}]}`,
		"runner with empty name": `{"total_count":1,"runners":[{"name":"","busy":true}]}`,
		"runner without busy":    `{"total_count":1,"runners":[{"name":"ci-a"}]}`,
		"runner with null busy":  `{"total_count":1,"runners":[{"name":"ci-a","busy":null}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.github = func(*http.Request) *http.Response { return jsonResponse(body, "") }
			if _, err := f.d.busyRunnerNames("/orgs/alpha-org", testToken); err == nil {
				t.Error("busyRunnerNames accepted the body")
			}
			if _, err := f.d.runnerTotal("/orgs/alpha-org", testToken); err == nil {
				t.Error("runnerTotal accepted the body")
			}
		})
	}
}

func TestRunnerPageDecodingAcceptsEmptyTenant(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response { return jsonResponse(`{"total_count":0,"runners":[]}`, "") }
	if busy, err := f.d.busyRunnerNames("/orgs/alpha-org", testToken); err != nil || len(busy) != 0 {
		t.Errorf("busyRunnerNames = %v, %v", busy, err)
	}
	if total, err := f.d.runnerTotal("/orgs/alpha-org", testToken); err != nil || total != 0 {
		t.Errorf("runnerTotal = %d, %v", total, err)
	}
}

// A Link header is attacker-influenced input from the reaper's point of view
// (a compromised upstream or proxy): the installation token must never be sent
// to a host other than the configured API.
func TestLinkNextToAnotherHostIsRefused(t *testing.T) {
	for _, next := range []string{
		"http://127.0.0.1:9/collect",
		"https://evil.example/orgs/alpha-org/actions/runners?page=2",
		"http://api.github.com/orgs/alpha-org/actions/runners?page=2",
		"/orgs/alpha-org/actions/runners?page=2",
	} {
		t.Run(next, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			f.github = func(*http.Request) *http.Response {
				return jsonResponse(`{"total_count":1,"runners":[]}`, `<`+next+`>; rel="next"`)
			}
			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1", code)
			}
			if len(f.requests) != 1 {
				t.Errorf("requests = %d, want only the first page (token must not follow %s)", len(f.requests), next)
			}
			if len(f.deletes()) != 0 {
				t.Errorf("deletes = %v, want none", f.deletes())
			}
		})
	}
}

// ------------------------------------------------ fail closed on GitHub errors

// Python's urlopen raises on any non-2xx, which aborts the pass before a
// delete. Reading a 4xx/5xx JSON body as "no runner is busy" instead kills live
// CI VMs, so every failure shape at every call site must stop the pass.
func TestGitHubFailureAbortsThePassBeforeAnyDelete(t *testing.T) {
	failures := map[string]func() *http.Response{
		"401":          func() *http.Response { return response(401, `{"message":"Bad credentials"}`, "") },
		"403":          func() *http.Response { return response(403, `{"message":"secondary rate limit"}`, "") },
		"404":          func() *http.Response { return response(404, `{"message":"Not Found"}`, "") },
		"500":          func() *http.Response { return response(500, `{"message":"boom"}`, "") },
		"200 garbage":  func() *http.Response { return jsonResponse(`<html>not json</html>`, "") },
		"200 no total": func() *http.Response { return jsonResponse(`{"runners":[]}`, "") },
	}
	calls := []struct {
		name   string
		setup  func(*fixture)
		failOn func(*http.Request) bool
	}{
		{"busy first page", func(*fixture) {}, func(r *http.Request) bool {
			return r.URL.Query().Get("per_page") == "100" && r.URL.Query().Get("page") == ""
		}},
		{"busy later page", func(*fixture) {}, func(r *http.Request) bool {
			return r.URL.Query().Get("page") == "2"
		}},
		{"second repo of a user tenant", func(f *fixture) { f.userTenant("a,b") }, func(r *http.Request) bool {
			return strings.Contains(r.URL.Path, "/b/")
		}},
		{"total call", func(*fixture) {}, func(r *http.Request) bool {
			return r.URL.Query().Get("per_page") == "1"
		}},
	}
	for _, call := range calls {
		for failName, fail := range failures {
			t.Run(call.name+"/"+failName, func(t *testing.T) {
				f := newFixture(t)
				call.setup(f)
				// Both an idle-reapable and a past-ceiling instance: neither may be
				// deleted once GitHub's answer cannot be trusted.
				f.instances = []fakeInstance{idleInstance(), {name: "ci-infra-2-bbb", ageSeconds: 7 * 3600}}
				f.github = func(req *http.Request) *http.Response {
					if call.failOn(req) {
						return fail()
					}
					link := ""
					if req.URL.Query().Get("per_page") == "100" && req.URL.Query().Get("page") == "" && strings.HasPrefix(req.URL.Path, "/orgs/") {
						link = `<` + page2URL + `>; rel="next"`
					}
					return jsonResponse(`{"total_count":1,"runners":[]}`, link)
				}

				if code := f.run(); code != 1 {
					t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
				}
				if d := f.deletes(); len(d) != 0 {
					t.Errorf("deletes = %v, want none", d)
				}
				f.assertLogContains("reap:")
				if strings.Contains(f.logs.String(), testToken) {
					t.Errorf("log leaks the installation token:\n%s", f.logs.String())
				}
			})
		}
	}
}

func TestHTTPErrorIncludesStatusAndTruncatedBody(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return response(403, `{"message":"rate limited `+strings.Repeat("x", 500)+`"}`, "")
	}
	_, err := f.d.runnerTotal("/orgs/alpha-org", testToken)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want status and body", err)
	}
	if len(err.Error()) > 400 {
		t.Errorf("error is %d bytes, body must be truncated", len(err.Error()))
	}
}

func TestTransportErrorAbortsThePass(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.d.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	if len(f.deletes()) != 0 {
		t.Errorf("deletes = %v, want none", f.deletes())
	}
}

// ------------------------------------------------ run(): the whole pass

func TestRunReapsIdleInstanceAndLogsLevels(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	want := [][]string{{"incus", "--project", "juno", "delete", "ci-build-1-aaa", "--force"}}
	if got := f.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("deletes = %v, want %v", got, want)
	}
	f.assertLogContains("WARNING reaping orphaned instance ci-build-1-aaa (tenant=alpha-org project=juno)")
}

func TestRunWithNothingToDoLogsInfoAndExitsZero(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{{name: "ci-build-1-aaa", ageSeconds: 60}}
	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if len(f.deletes()) != 0 {
		t.Errorf("deletes = %v", f.deletes())
	}
	f.assertLogContains("INFO reap: nothing to do (1 instance(s) checked in project juno)")
}

// Through run(), not only the pure function: dropping the trustBusyCheck wiring
// would delete every live runner VM older than the idle TTL whenever GitHub
// momentarily reports zero runners.
func TestRunSkipsIdleReapButKeepsCeilingWhenGitHubReportsZeroRunners(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance(), {name: "ci-build-2-bbb", ageSeconds: 7 * 3600}}
	f.github = func(*http.Request) *http.Response { return jsonResponse(`{"total_count":0,"runners":[]}`, "") }

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	want := [][]string{{"incus", "--project", "juno", "delete", "ci-build-2-bbb", "--force"}}
	if got := f.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("deletes = %v, want only the past-ceiling instance", got)
	}
	f.assertLogContains(`ERROR GitHub reports 0 total runner(s) under ["/orgs/alpha-org"] but 2 ci-* instance(s) exist in incus project juno`)
}

func TestRunChecksEveryRegisteredRepoOfAUserTenant(t *testing.T) {
	for _, busyRepo := range []string{"a", "b"} {
		t.Run("busy on "+busyRepo, func(t *testing.T) {
			f := newFixture(t)
			f.userTenant("a,b")
			f.instances = []fakeInstance{idleInstance()}
			f.d.mintToken = func(id int) (string, error) {
				if id != 42 {
					t.Errorf("installation id = %d, want 42", id)
				}
				return "tok", nil
			}
			f.github = func(req *http.Request) *http.Response {
				if strings.HasPrefix(req.URL.Path, "/repos/Juno/"+busyRepo+"/") && req.URL.Query().Get("per_page") == "100" {
					return jsonResponse(`{"total_count":1,"runners":[{"name":"ci-build-1-aaa","busy":true}]}`, "")
				}
				return jsonResponse(`{"total_count":1,"runners":[]}`, "")
			}

			if code := f.run(); code != 0 {
				t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
			}
			var busyPaths []string
			for _, req := range f.requests {
				if req.URL.Query().Get("per_page") == "100" {
					busyPaths = append(busyPaths, req.URL.Path)
				}
			}
			wantPaths := []string{"/repos/Juno/a/actions/runners", "/repos/Juno/b/actions/runners"}
			if !reflect.DeepEqual(busyPaths, wantPaths) {
				t.Errorf("busy queries = %v, want %v", busyPaths, wantPaths)
			}
			if d := f.deletes(); len(d) != 0 {
				t.Errorf("deletes = %v, want [] (busy on repo %s)", d, busyRepo)
			}
		})
	}
}

func TestListInstancesIsProjectScoped(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d", code)
	}
	want := []string{"incus", "--project", "juno", "list", "^ci-", "--format", "json"}
	if len(f.incusCalls) == 0 || !reflect.DeepEqual(f.incusCalls[0], want) {
		t.Errorf("first incus call = %v, want %v", f.incusCalls, want)
	}
}

// Spec 1 section 7.1: an unscoped delete reaches the other tenant's runners.
// Every incus call must carry --project, the delete included, and --force.
func TestDeleteFailureIsLoggedProjectScopedAndDoesNotAbort(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance(), {name: "ci-build-2-bbb", ageSeconds: 30 * 60}}
	f.failDelete["ci-build-1-aaa"] = "permission denied"

	code := f.run()

	want := [][]string{
		{"incus", "--project", "juno", "delete", "ci-build-1-aaa", "--force"},
		{"incus", "--project", "juno", "delete", "ci-build-2-bbb", "--force"},
	}
	if got := f.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("deletes = %v, want both attempted: %v", got, want)
	}
	f.assertLogContains("ERROR failed to delete instance ci-build-1-aaa: permission denied")
	if code != 1 {
		t.Errorf("run() = %d, want 1 so the unit shows failed", code)
	}
}

func TestEveryIncusCallIsProjectScoped(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.run()
	if len(f.incusCalls) < 2 {
		t.Fatalf("incus calls = %v", f.incusCalls)
	}
	for _, argv := range f.incusCalls {
		if len(argv) < 3 || argv[0] != "incus" || argv[1] != "--project" || argv[2] != "juno" {
			t.Errorf("incus call %v is not project-scoped", argv)
		}
	}
}

func TestIncusListFailureAbortsThePass(t *testing.T) {
	f := newFixture(t)
	f.d.execIncus = func(argv []string) incusResult {
		f.incusCalls = append(f.incusCalls, argv)
		return incusResult{exitCode: 1, stderr: "socket unreachable"}
	}
	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	f.assertLogContains("incus list failed: socket unreachable")
	if len(f.requests) != 0 || len(f.incusCalls) != 1 {
		t.Errorf("requests=%d incus calls=%v, want the pass to stop at list", len(f.requests), f.incusCalls)
	}
}

// ------------------------------------------------ configuration fails closed

func TestIncusProjectIsRequired(t *testing.T) {
	for name, set := range map[string]func(map[string]string){
		"unset": func(e map[string]string) { delete(e, "BROKER_INCUS_PROJECT") },
		"empty": func(e map[string]string) { e["BROKER_INCUS_PROJECT"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			set(f.env)
			f.d.mintToken = func(int) (string, error) {
				t.Error("must not mint a token without a project")
				return "", nil
			}
			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1", code)
			}
			f.assertLogContains("BROKER_INCUS_PROJECT")
			if len(f.incusCalls) != 0 || len(f.requests) != 0 {
				t.Errorf("incus calls = %v, requests = %d; want none", f.incusCalls, len(f.requests))
			}
		})
	}
}

func TestUnmintableTokenFailsLoudly(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.d.mintToken = func(int) (string, error) { return "", errors.New("HTTP Error 401: Unauthorized") }

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	f.assertLogContains("reap: ")
	f.assertLogContains("401")
	if len(f.incusCalls) != 0 {
		t.Errorf("incus calls = %v, must not list instances", f.incusCalls)
	}
}

func TestMissingInstallationIDFailsLoudly(t *testing.T) {
	for _, id := range []string{"", "abc", "0", "-1"} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			f := newFixture(t)
			f.env["BROKER_INSTALLATION_ID"] = id
			f.d.mintToken = func(int) (string, error) {
				t.Error("must not mint a token with a bad installation id")
				return "", nil
			}
			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1", code)
			}
			f.assertLogContains("reap: ")
			f.assertLogContains("BROKER_INSTALLATION_ID")
			if len(f.incusCalls) != 0 {
				t.Errorf("incus calls = %v, want none", f.incusCalls)
			}
		})
	}
}

func TestBadTargetConfigFailsLoudlyBeforeAnyCall(t *testing.T) {
	f := newFixture(t)
	f.env["BROKER_TARGET"] = ""
	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	f.assertLogContains("reap: ")
	if len(f.incusCalls) != 0 || len(f.requests) != 0 {
		t.Errorf("incus calls = %v, requests = %d", f.incusCalls, len(f.requests))
	}
}

// bcx-reap takes no arguments (deploy contract); an unknown flag such as a
// hoped-for --dry-run must not be silently ignored by a binary that deletes.
func TestUnexpectedArgumentsAreRefused(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	if code := run([]string{"--dry-run"}, f.env, f.d); code != 2 {
		t.Errorf("run() = %d, want 2", code)
	}
	if len(f.incusCalls) != 0 || len(f.requests) != 0 {
		t.Errorf("incus calls = %v, requests = %d; want none", f.incusCalls, len(f.requests))
	}
	f.assertLogContains("--dry-run")
}

// The missing-credentials failures come from the real mint path; none of them
// reaches the network (the JWT cannot be built).
func TestMintTokenMissingAppIDFailsLoudly(t *testing.T) {
	t.Setenv("BCX_APP_ID", "")
	mint := newMintToken(filepath.Join(t.TempDir(), "app.env"), "/nonexistent/app.pem")
	if _, err := mint(42); err == nil || !strings.Contains(err.Error(), "BCX_APP_ID") {
		t.Errorf("err = %v, want BCX_APP_ID error", err)
	}
}

func TestMintTokenMissingKeyFailsLoudly(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	mint := newMintToken(filepath.Join(t.TempDir(), "app.env"), "/nonexistent/app.pem")
	if _, err := mint(42); err == nil || !strings.Contains(err.Error(), "/nonexistent/app.pem") {
		t.Errorf("err = %v, want key path error", err)
	}
}

func TestParseInstallationID(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr string
	}{
		{"42", 42, ""},
		{" 42", 42, ""},
		{"+42\n", 42, ""},
		{"", 0, "BROKER_INSTALLATION_ID"},
		{"-1", 0, "positive integer"},
		{"0", 0, "positive integer"},
		{"4_2", 0, "positive integer"},
		{"abc", 0, "positive integer"},
		{"99999999999999999999", 0, "out of range"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, err := parseInstallationID(tc.in)
			if tc.wantErr == "" {
				if err != nil || got != tc.want {
					t.Errorf("parseInstallationID = %d, %v; want %d", got, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseInstallationID = %d, %v; want error containing %q", got, err, tc.wantErr)
			}
		})
	}
}

// ------------------------------------------------ tenant scoping (spec 1 section 7.1)

func TestTargetPathsAreKindTyped(t *testing.T) {
	got, err := targetPaths(map[string]string{"BROKER_TARGET_KIND": "org", "BROKER_TARGET": "alpha-org"})
	if err != nil || !reflect.DeepEqual(got, []string{"/orgs/alpha-org"}) {
		t.Errorf("targetPaths(org) = %v, %v", got, err)
	}

	got, err = targetPaths(map[string]string{
		"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "some-repo,infra,,.github",
	})
	want := []string{"/repos/Juno/some-repo", "/repos/Juno/infra", "/repos/Juno/.github"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("targetPaths(user) = %v, %v; want %v", got, err, want)
	}
}

func TestTargetKindDefaultsToOrgOnlyWhenUnset(t *testing.T) {
	got, err := targetPaths(map[string]string{"BROKER_TARGET": "alpha-org"})
	if err != nil || !reflect.DeepEqual(got, []string{"/orgs/alpha-org"}) {
		t.Errorf("targetPaths(unset kind) = %v, %v", got, err)
	}
	if _, err := targetPaths(map[string]string{"BROKER_TARGET": "alpha-org", "BROKER_TARGET_KIND": ""}); err == nil {
		t.Error("empty BROKER_TARGET_KIND accepted")
	}
}

func TestTargetPathsFailClosed(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown kind":            {"BROKER_TARGET_KIND": "repo", "BROKER_TARGET": "Juno"},
		"empty target":            {"BROKER_TARGET_KIND": "org", "BROKER_TARGET": ""},
		"missing target":          {"BROKER_TARGET_KIND": "org"},
		"slash in org target":     {"BROKER_TARGET_KIND": "org", "BROKER_TARGET": "a/b"},
		"slash in user target":    {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno/some-repo", "BROKER_REPOS": "x"},
		"space in target":         {"BROKER_TARGET_KIND": "org", "BROKER_TARGET": "a b"},
		"dotdot target":           {"BROKER_TARGET_KIND": "org", "BROKER_TARGET": ".."},
		"query in target":         {"BROKER_TARGET_KIND": "org", "BROKER_TARGET": "a?x=1"},
		"empty repos":             {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": ""},
		"only separators":         {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": ",,"},
		"space in repo":           {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "a, b"},
		"dotdot repo":             {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "a,.."},
		"dot repo":                {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "."},
		"query in repo":           {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "a?b"},
		"fragment in repo":        {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "a#b"},
		"percent-encoded in repo": {"BROKER_TARGET_KIND": "user", "BROKER_TARGET": "Juno", "BROKER_REPOS": "a%2Fb"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := targetPaths(env); err == nil {
				t.Errorf("targetPaths = %v, want error", got)
			}
		})
	}
}

// ------------------------------------------------ incus plumbing

func TestParseListedInstances(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{{name: "ci-a", ageSeconds: 10.5}}
	got, err := f.d.listInstances("juno")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "ci-a" || got[0].CreatedAt != testNow-10.5 {
		t.Errorf("listInstances = %+v, want ci-a created at %v", got, float64(testNow)-10.5)
	}
}

func TestListInstancesRejectsMalformedRecords(t *testing.T) {
	cases := map[string]string{
		"whitespace-only stdout": "  \n",
		"not json":               "Error: something",
		"null":                   "null",
		"object, not an array":   `{"name":"ci-a"}`,
		"missing name":           `[{"created_at":"2020-01-01T00:00:00Z"}]`,
		"empty name":             `[{"name":"","created_at":"2020-01-01T00:00:00Z"}]`,
		"not a ci- name":         `[{"name":"prod-db","created_at":"2020-01-01T00:00:00Z"}]`,
		"missing created_at":     `[{"name":"ci-a"}]`,
		"bad created_at":         `[{"name":"ci-a","created_at":"yesterday"}]`,
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.d.execIncus = func([]string) incusResult { return incusResult{stdout: stdout} }
			if got, err := f.d.listInstances("juno"); err == nil {
				t.Errorf("listInstances = %+v, want error", got)
			}
		})
	}
}

// Python: json.loads(out.stdout or "[]") — truly empty output means no instances.
func TestListInstancesTreatsEmptyStdoutAsNone(t *testing.T) {
	f := newFixture(t)
	f.d.execIncus = func([]string) incusResult { return incusResult{} }
	got, err := f.d.listInstances("juno")
	if err != nil || len(got) != 0 {
		t.Errorf("listInstances = %v, %v", got, err)
	}
}

func TestDefaultExecIncusReportsWhyTheBinaryCouldNotRun(t *testing.T) {
	res := defaultExecIncus([]string{"/nonexistent/incus-binary", "list"})
	if res.exitCode == 0 || !strings.Contains(res.stderr, "/nonexistent/incus-binary") {
		t.Errorf("result = %+v, want a non-zero code and the cause in stderr", res)
	}
}

func TestDefaultExecIncusCapturesExitCodeAndStreams(t *testing.T) {
	res := defaultExecIncus([]string{"sh", "-c", "echo out; echo oops >&2; exit 3"})
	if res.exitCode != 3 || strings.TrimSpace(res.stdout) != "out" || strings.TrimSpace(res.stderr) != "oops" {
		t.Errorf("result = %+v", res)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	got := truncate("  "+strings.Repeat("a", 199)+"é tail  ", 200)
	if !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 199) + "é"; got != want {
		t.Errorf("truncate = %q, want %q", got, want)
	}
	if got := truncate(" short \n", 200); got != "short" {
		t.Errorf("truncate = %q", got)
	}
}

func TestEnvMapSplitsOnFirstEquals(t *testing.T) {
	got := envMap([]string{"A=1", "B=", "C=x=y", "no-equals"})
	want := map[string]string{"A": "1", "B": "", "C": "x=y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envMap = %v, want %v", got, want)
	}
}

// ------------------------------------------------ main() wiring

// main must hand run()'s status to os.Exit; the child re-executes this test
// binary as bcx-reap with an empty environment (no network or incus is reached:
// the configuration check fails first).
func TestMainExitsWithRunStatus(t *testing.T) {
	if os.Getenv("REAP_TEST_RUN_MAIN") == "1" {
		os.Args = append(os.Args[:1:1], strings.Fields(os.Getenv("REAP_TEST_ARGS"))...)
		main()
		return
	}
	cases := []struct {
		name string
		args string
		want int
	}{
		{"failed pass", "", 1},
		{"usage error", "--dry-run", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainExitsWithRunStatus$")
			cmd.Env = []string{"REAP_TEST_RUN_MAIN=1", "REAP_TEST_ARGS=" + tc.args}
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != tc.want {
				t.Errorf("exit = %v, want code %d", err, tc.want)
			}
		})
	}
}
