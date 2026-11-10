package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bartixcloud/runner-broker/hcloud"
)

const hcloudTestToken = "hcloud_secret_token"

// ------------------------------------------------ fakes and fixtures

// fakeHcloud is a stand-in for *hcloud.Client. It filters like the API does, by
// the label selector it is given, so a wrong selector shows up as a server that
// was not returned (or another tenant's server that was).
type fakeHcloud struct {
	servers      []hcloud.Server
	selectors    []string
	deleted      []int64
	listErr      error
	failSelector string // when set, only selectors containing it fail
	deleteErr    map[int64]error
}

func (f *fakeHcloud) ListServersContext(_ context.Context, selector string) ([]hcloud.Server, error) {
	f.selectors = append(f.selectors, selector)
	if f.listErr != nil && (f.failSelector == "" || strings.Contains(selector, f.failSelector)) {
		return nil, f.listErr
	}
	want := selectorValues(selector)
	var out []hcloud.Server
	for _, s := range f.servers {
		matches := true
		for k, v := range want {
			if s.Labels[k] != v {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeHcloud) DeleteServer(id int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr[id]
}

// unfilteredHcloud returns its servers for any selector, simulating an API, a
// proxy or a fake that ignores label_selector.
type unfilteredHcloud struct{ servers []hcloud.Server }

func (u unfilteredHcloud) ListServersContext(context.Context, string) ([]hcloud.Server, error) {
	return u.servers, nil
}

func (u unfilteredHcloud) DeleteServer(int64) error { return nil }

func selectorValues(selector string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(selector, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[k] = v
		}
	}
	return out
}

func hcloudServer(f *fixture, id int64, name, tenant, tierName, status string, ageSeconds float64) hcloud.Server {
	return hcloud.Server{
		ID:      id,
		Name:    name,
		Status:  status,
		Created: f.now.Add(-time.Duration(ageSeconds * float64(time.Second))),
		Labels: map[string]string{
			"bcx-managed": "ci",
			"bcx-tenant":  tenant,
			"bcx-tier":    tierName,
		},
	}
}

// useHcloud wires the fixture to a real tiers file and token file, a real tier
// loader and token reader, and a fake Hetzner API.
func (f *fixture) useHcloud(tiersYAML string, api *fakeHcloud) {
	f.t.Helper()
	dir := f.t.TempDir()
	tiersPath := filepath.Join(dir, "tiers.yml")
	if err := os.WriteFile(tiersPath, []byte(tiersYAML), 0o600); err != nil {
		f.t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "alpha-org.hcloud-token")
	if err := os.WriteFile(tokenPath, []byte(" "+hcloudTestToken+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.env["BROKER_TIERS_FILE"] = tiersPath
	f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenPath
	f.d.loadTiers = loadTiers
	f.d.readToken = readHcloudToken
	f.d.newHcloudClient = func(string) hcloudOps { return api }
}

const e2eTierYAML = `
tiers:
  e2e:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["alpha-org/e2e"]
    secrets: []
`

const maxAgeTierYAML = `
tiers:
  e2e:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    max_age: 90m
    allowlist: ["alpha-org/e2e"]
    secrets: []
`

// ------------------------------------------------ decision function (pure)

// Spec §6, rule by rule and at every boundary. Each case is checked through
// reapCandidates (the rule that fired) and instancesToReap (the documented
// name-only shape).
func TestInstancesToReapHcloudRules(t *testing.T) {
	now := float64(testNow)
	base := func(status string, age float64) instance {
		return instance{
			Backend:   backendHcloud,
			Tier:      "e2e",
			ID:        1,
			Name:      "ci-e2e-1-abc",
			CreatedAt: now - age,
			Status:    status,
			MaxAge:    2 * time.Hour,
		}
	}
	cases := []struct {
		name       string
		inst       instance
		busy       bool
		trust      bool
		want       bool
		wantReason reapReason
	}{
		{"off at exactly 2 min is kept", base(hcloudStatusOff, hcloudOffGraceSeconds), false, true, false, reasonNone},
		{"off one second past 2 min is reaped", base(hcloudStatusOff, hcloudOffGraceSeconds+1), false, true, true, reasonStatusOff},
		{"off is reaped while busy", base(hcloudStatusOff, hcloudOffGraceSeconds+1), true, true, true, reasonStatusOff},
		{"off is reaped when the busy check is untrusted", base(hcloudStatusOff, hcloudOffGraceSeconds+1), false, false, true, reasonStatusOff},
		{"running under the idle TTL is kept", base("running", 3*60), false, true, false, reasonNone},
		{"idle at exactly 10 min is kept", base("running", hcloudIdleSeconds), false, true, false, reasonNone},
		{"idle one second past 10 min is reaped", base("running", hcloudIdleSeconds+1), false, true, true, reasonIdle},
		{"busy past 10 min is kept", base("running", hcloudIdleSeconds+1), true, true, false, reasonNone},
		{"untrusted keeps an idle server past 10 min", base("running", hcloudIdleSeconds+1), false, false, false, reasonNone},
		{"max_age at exactly the ceiling is kept", base("running", (2 * time.Hour).Seconds()), true, true, false, reasonNone},
		{"max_age one second past the ceiling is reaped", base("running", (2*time.Hour).Seconds()+1), false, true, true, reasonMaxAge},
		{"max_age reaps a busy server", base("running", (2*time.Hour).Seconds()+1), true, true, true, reasonMaxAge},
		{"max_age reaps an untrusted pass", base("running", (2*time.Hour).Seconds()+1), false, false, true, reasonMaxAge},
		{"off wins over max_age when both fire", base(hcloudStatusOff, (2*time.Hour).Seconds()+1), false, true, true, reasonStatusOff},
		{
			"a running server under every rule is kept",
			base("running", 5*60), false, true, false, reasonNone,
		},
		{
			"a missing max_age never reaps by that rule",
			func() instance { i := base("running", 7*3600); i.MaxAge = 0; return i }(),
			false, false, false, reasonNone,
		},
		{
			"an incus instance with status off is not hit by the hcloud rule",
			instance{Backend: backendIncus, Name: "ci-build-1-aaa", CreatedAt: now - 3*60, Status: hcloudStatusOff},
			false, true, false, reasonNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busy := map[string]struct{}{}
			if tc.busy {
				busy[tc.inst.Name] = struct{}{}
			}
			got := reapCandidates([]instance{tc.inst}, busy, now, tc.trust)
			if reaped := len(got) == 1; reaped != tc.want {
				t.Fatalf("reapCandidates reaped = %v, want %v", reaped, tc.want)
			}
			if tc.want && got[0].Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", got[0].Reason, tc.wantReason)
			}
			names := instancesToReap([]instance{tc.inst}, busy, now, tc.trust)
			if reaped := len(names) == 1; reaped != tc.want {
				t.Errorf("instancesToReap reaped = %v, want %v", reaped, tc.want)
			}
		})
	}
}

// The Incus rules keep their constants and their strict comparisons next to the
// hcloud ones in one list: mixed input must not change either.
func TestInstancesToReapKeepsTheIncusRules(t *testing.T) {
	now := float64(testNow)
	instances := []instance{
		{Name: "ci-build-1-aaa", CreatedAt: now - idleTTLSeconds},
		{Name: "ci-build-2-bbb", CreatedAt: now - idleTTLSeconds - 1},
		{Name: "ci-build-3-ccc", CreatedAt: now - hardCeilingSeconds - 1},
	}
	got := instancesToReap(instances, map[string]struct{}{}, now, true)
	want := []string{"ci-build-2-bbb", "ci-build-3-ccc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("instancesToReap = %v, want %v", got, want)
	}
	if got := instancesToReap(instances, map[string]struct{}{}, now, false); !reflect.DeepEqual(got, []string{"ci-build-3-ccc"}) {
		t.Errorf("untrusted instancesToReap = %v, want only the past-ceiling instance", got)
	}
}

// ------------------------------------------------ sweep: hcloud

func TestSweepReapsOnlyThisTenantsHcloudServers(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 11, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", 20*60),
		hcloudServer(f, 12, "ci-e2e-2-bbb", "juno", "e2e", "running", 20*60),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{11}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want only this tenant's server %v", api.deleted, want)
	}
	wantSelector := "bcx-managed=ci,bcx-tenant=alpha-org,bcx-tier=e2e"
	if !reflect.DeepEqual(api.selectors, []string{wantSelector}) {
		t.Errorf("selectors = %v, want [%s]", api.selectors, wantSelector)
	}
	// A Hetzner VM is not in an Incus project; the line must not name one.
	f.assertLogContains("WARNING reaping orphaned instance ci-e2e-1-aaa (tenant=alpha-org project=-) backend=hcloud tier=e2e server=ci-e2e-1-aaa")
	f.assertLogContains("status=running rule=idle")
	if len(f.deletes()) != 0 {
		t.Errorf("incus deletes = %v, want none", f.deletes())
	}
	if strings.Contains(f.logs.String(), hcloudTestToken) {
		t.Errorf("log leaks the hcloud token:\n%s", f.logs.String())
	}
}

func TestSweepListsEveryUsableTierWithItsOwnSelector(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 21, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", 20*60),
		hcloudServer(f, 22, "ci-e2e2-1-bbb", "alpha-org", "e2e2", "running", 20*60),
	}}
	f.useHcloud(maxAgeTierYAML+`
  e2e2:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [nbg1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["alpha-org/other"]
    secrets: []
`, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	wantSelectors := []string{
		"bcx-managed=ci,bcx-tenant=alpha-org,bcx-tier=e2e",
		"bcx-managed=ci,bcx-tenant=alpha-org,bcx-tier=e2e2",
	}
	if !reflect.DeepEqual(api.selectors, wantSelectors) {
		t.Errorf("selectors = %v, want %v", api.selectors, wantSelectors)
	}
	if want := []int64{21, 22}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want %v", api.deleted, want)
	}
}

// Spec §6: an empty runner list is the normal post-job state for hcloud, so the
// off rule fires even when GitHub reports zero runners and the Incus guard has
// switched the idle rule off.
func TestSweepReapsOffHcloudServerWithAnEmptyRunnerList(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":0,"runners":[]}`, "")
	}
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 31, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{31}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want the off server %v", api.deleted, want)
	}
	f.assertLogContains("status=off rule=status-off")
}

// Spec §6 with both backends present: the zero-runner guard turns off only the
// idle rule. An overdue Incus orphan still goes at the hard ceiling, an hcloud
// VM still goes when off or past max_age, and a running hcloud VM under both
// stays.
func TestSweepMixedBackendsWithUntrustedBusyCheck(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{
		idleInstance(), // 20 min idle: kept because the guard is on
		{name: "ci-build-9-zzz", ageSeconds: 7 * 3600}, // past the Incus ceiling: reaped
	}
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":0,"runners":[]}`, "")
	}
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 41, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60),
		hcloudServer(f, 42, "ci-e2e-2-bbb", "alpha-org", "e2e", "running", 20*60),
		hcloudServer(f, 43, "ci-e2e-3-ccc", "alpha-org", "e2e", "running", (2*time.Hour).Seconds()+1),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{41, 43}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("hcloud deletes = %v, want only the off and past-max_age servers %v", api.deleted, want)
	}
	wantIncus := [][]string{{"incus", "--project", "juno", "delete", "ci-build-9-zzz", "--force"}}
	if got := f.deletes(); !reflect.DeepEqual(got, wantIncus) {
		t.Errorf("incus deletes = %v, want only the past-ceiling instance %v", got, wantIncus)
	}
	f.assertLogContains("ERROR GitHub reports 0 total runner(s)")
}

// An hcloud-only tenant has no Incus instances, so the guard has to reach it
// too: with GitHub reporting zero runners the 10-minute idle rule is off, while
// off and max_age still fire.
func TestHcloudOnlyZeroRunnerGuardKeepsIdleAndReapsOffAndMaxAge(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":0,"runners":[]}`, "")
	}
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 44, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", 20*60),
		hcloudServer(f, 45, "ci-e2e-2-bbb", "alpha-org", "e2e", "off", 3*60),
		hcloudServer(f, 46, "ci-e2e-3-ccc", "alpha-org", "e2e", "running", (2*time.Hour).Seconds()+1),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{45, 46}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want the off and past-max_age servers %v, never the idle one", api.deleted, want)
	}
	f.assertLogContains(`INFO GitHub reports 0 total runner(s) under ["/orgs/alpha-org"] but 3 hcloud server(s) exist`)
	f.assertLogContains("skipping the hcloud idle rule this pass")
	if strings.Contains(f.logs.String(), "incus project") {
		t.Errorf("an hcloud-only guard line must not name an incus project:\n%s", f.logs.String())
	}
}

// With a trustworthy runner total the idle rule is live, and a busy runner is
// still kept.
func TestHcloudIdleRuleFiresOnlyWithATrustworthyRunnerTotal(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":2,"runners":[{"name":"ci-e2e-2-bbb","busy":true},{"name":"ci-e2e-1-aaa","busy":false}]}`, "")
	}
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 47, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", 20*60),
		hcloudServer(f, 48, "ci-e2e-2-bbb", "alpha-org", "e2e", "running", 20*60),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{47}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want only the idle non-busy server %v", api.deleted, want)
	}
	f.assertLogContains("rule=idle")
}

func TestSweepKeepsBusyHcloudServer(t *testing.T) {
	f := newFixture(t)
	f.github = func(*http.Request) *http.Response {
		return jsonResponse(`{"total_count":1,"runners":[{"name":"ci-e2e-1-aaa","busy":true}]}`, "")
	}
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 51, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", 20*60),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if len(api.deleted) != 0 {
		t.Errorf("deleted = %v, want none for a busy runner", api.deleted)
	}
	f.assertLogContains("INFO reap: hcloud: nothing to do (1 server(s) checked in tiers [e2e])")
}

func TestSweepAppliesTheTierMaxAge(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 61, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", (90*time.Minute).Seconds()+1),
	}}
	f.useHcloud(maxAgeTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{61}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want %v", api.deleted, want)
	}
	f.assertLogContains("rule=max-age")
}

func TestSweepUsesTheDefaultMaxAge(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 62, "ci-e2e-1-aaa", "alpha-org", "e2e", "running", (2*time.Hour).Seconds()+1),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if want := []int64{62}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("deleted = %v, want the server past the 2h default", api.deleted)
	}
	f.assertLogContains("rule=max-age")
}

// A hostile or buggy API reply must not be able to start a forged journal line:
// the reaper's log is read during incident triage.
func TestReapLogFieldsCannotForgeALine(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{servers: []hcloud.Server{
		hcloudServer(f, 91, "ci-e2e-1-aaa\nINFO forged", "alpha-org", "e2e", "running", 20*60),
	}}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	if strings.Contains(f.logs.String(), "\nINFO forged") {
		t.Errorf("a control character in a server name forged a log line:\n%s", f.logs.String())
	}
}

// The tenant names the selector and the default token path. It must be checked
// before the token file is opened, not after.
func TestMissingTenantFailsBeforeTheTokenIsRead(t *testing.T) {
	f := newFixture(t)
	delete(f.env, "BROKER_TENANT")
	f.env["BROKER_TIERS_FILE"] = writeTiers(t, e2eTierYAML)
	f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFileAt(t, "token", hcloudTestToken+"\n", 0o600)
	f.d.loadTiers = loadTiers
	f.d.readToken = func(string) (string, error) {
		t.Error("must not read the token file without a tenant")
		return "", nil
	}
	f.d.newHcloudClient = func(string) hcloudOps {
		t.Error("must not build a client without a tenant")
		return &fakeHcloud{}
	}

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
	}
	f.assertLogContains("BROKER_TENANT")
}

// A pass that listed the first tier and failed on the second must discard the
// first tier's servers too: no hcloud delete may follow a partial Hetzner view.
// The Incus sweep is independent and still runs.
func TestSweepSkipsHcloudDeletesWhenALaterTierFails(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	api := &fakeHcloud{
		servers: []hcloud.Server{
			hcloudServer(f, 92, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60),
			hcloudServer(f, 93, "ci-e2e2-1-bbb", "alpha-org", "e2e2", "off", 3*60),
		},
		listErr:      &hcloud.APIError{Status: 500, Message: "boom"},
		failSelector: "bcx-tier=e2e2",
	}
	f.useHcloud(e2eTierYAML+`
  e2e2:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [nbg1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["alpha-org/other"]
    secrets: []
`, api)

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
	}
	if len(api.deleted) != 0 {
		t.Errorf("deleted = %v, want none after a partial list", api.deleted)
	}
	if len(api.selectors) != 2 {
		t.Errorf("selectors = %v, want both tiers attempted", api.selectors)
	}
	if got := f.deletes(); len(got) != 1 {
		t.Errorf("incus deletes = %v, want the overdue orphan to be reaped", got)
	}
}

func TestHcloudDeleteFailureIsLoggedAndFailsThePass(t *testing.T) {
	f := newFixture(t)
	api := &fakeHcloud{
		servers: []hcloud.Server{
			hcloudServer(f, 71, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60),
			hcloudServer(f, 72, "ci-e2e-2-bbb", "alpha-org", "e2e", "off", 4*60),
		},
		deleteErr: map[int64]error{71: errors.New("hcloud API: HTTP 500: boom")},
	}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1 so the unit shows failed", code)
	}
	if want := []int64{71, 72}; !reflect.DeepEqual(api.deleted, want) {
		t.Errorf("delete attempts = %v, want both %v", api.deleted, want)
	}
	f.assertLogContains("ERROR failed to delete hcloud server ci-e2e-1-aaa (id=71)")
	if strings.Contains(f.logs.String(), hcloudTestToken) {
		t.Errorf("log leaks the hcloud token:\n%s", f.logs.String())
	}
}

// ------------------------------------------------ Hetzner failure vs the Incus sweep

// An hcloud failure must never stall the Incus sweep: that is the outage class
// that once filled the shared Incus pool. The pass still exits 1 (so the unit
// shows failed and the Hetzner cause is visible), and no hcloud delete happens.
func TestHcloudListFailureStillReapsIncusAndSkipsHcloudDeletes(t *testing.T) {
	cases := map[string]*fakeHcloud{
		"500":       {listErr: &hcloud.APIError{Status: 500, Message: "boom"}},
		"malformed": {listErr: errors.New("hcloud API: undecodable response: unexpected end of JSON input")},
	}
	for name, api := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			f.useHcloud(e2eTierYAML, api)

			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
			}
			if got := f.deletes(); len(got) != 1 {
				t.Errorf("incus deletes = %v, want the overdue orphan reaped", got)
			}
			if len(api.deleted) != 0 {
				t.Errorf("hcloud deletes = %v, want none", api.deleted)
			}
			f.assertLogContains("ERROR hcloud sweep failed, skipping every hcloud delete this pass:")
			f.assertLogContains("reap: hcloud sweep:")
		})
	}
}

func TestHcloudMalformedServerStillReapsIncusAndSkipsHcloudDeletes(t *testing.T) {
	cases := map[string]func(*fixture) hcloud.Server{
		"no id": func(f *fixture) hcloud.Server {
			s := hcloudServer(f, 0, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60)
			return s
		},
		"no created": func(f *fixture) hcloud.Server {
			return hcloud.Server{ID: 2, Name: "ci-e2e-1-aaa", Status: "off",
				Labels: map[string]string{"bcx-managed": "ci", "bcx-tenant": "alpha-org", "bcx-tier": "e2e"}}
		},
		"not a ci- name": func(f *fixture) hcloud.Server {
			return hcloudServer(f, 3, "prod-db", "alpha-org", "e2e", "off", 3*60)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			api := &fakeHcloud{servers: []hcloud.Server{mk(f)}}
			f.useHcloud(e2eTierYAML, api)

			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
			}
			if got := f.deletes(); len(got) != 1 {
				t.Errorf("incus deletes = %v, want the overdue orphan reaped", got)
			}
			if len(api.deleted) != 0 {
				t.Errorf("hcloud deletes = %v, want none", api.deleted)
			}
		})
	}
}

func TestHcloudReplyOutsideTheSelectorScopeStillReapsIncus(t *testing.T) {
	cases := map[string]func(*fixture) hcloud.Server{
		"another tenant": func(f *fixture) hcloud.Server {
			return hcloudServer(f, 81, "ci-e2e-1-aaa", "juno", "e2e", "off", 3*60)
		},
		"another tier": func(f *fixture) hcloud.Server {
			return hcloudServer(f, 82, "ci-e2e-1-aaa", "alpha-org", "build", "off", 3*60)
		},
		"not managed by bcx": func(f *fixture) hcloud.Server {
			s := hcloudServer(f, 83, "ci-e2e-1-aaa", "alpha-org", "e2e", "off", 3*60)
			s.Labels["bcx-managed"] = "someone-else"
			return s
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			f.env["BROKER_TIERS_FILE"] = writeTiers(t, e2eTierYAML)
			f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFileAt(t, "token", hcloudTestToken+"\n", 0o600)
			f.d.loadTiers = loadTiers
			f.d.readToken = readHcloudToken
			api := unfilteredHcloud{servers: []hcloud.Server{mk(f)}}
			f.d.newHcloudClient = func(string) hcloudOps { return api }

			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
			}
			if got := f.deletes(); len(got) != 1 {
				t.Errorf("incus deletes = %v, want the overdue orphan reaped", got)
			}
			f.assertLogContains("outside the tenant/tier scope")
		})
	}
}

// Missing or malformed tiers.yml and a bad token file are all hcloud-side
// configuration failures: no hcloud call, no hcloud delete, and the Incus sweep
// still runs.
func TestHcloudConfigFailureStillReapsIncus(t *testing.T) {
	cases := map[string]func(*fixture){
		"tiers file missing": func(f *fixture) {
			f.env["BROKER_TIERS_FILE"] = filepath.Join(f.t.TempDir(), "absent.yml")
			f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFileAt(f.t, "token", hcloudTestToken+"\n", 0o600)
			f.d.loadTiers = loadTiers
			f.d.readToken = readHcloudToken
			f.d.newHcloudClient = noHcloudClient(f.t)
		},
		"tiers file malformed for the reader": func(f *fixture) {
			f.env["BROKER_TIERS_FILE"] = writeTiers(f.t, "tiers: [\n")
			f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFileAt(f.t, "token", hcloudTestToken+"\n", 0o600)
			f.d.loadTiers = loadTiers
			f.d.readToken = readHcloudToken
			f.d.newHcloudClient = noHcloudClient(f.t)
		},
		"bad token mode": func(f *fixture) {
			f.env["BROKER_TIERS_FILE"] = writeTiers(f.t, e2eTierYAML)
			f.env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFileAt(f.t, "token", hcloudTestToken+"\n", 0o644)
			f.d.loadTiers = loadTiers
			f.d.readToken = readHcloudToken
			f.d.newHcloudClient = noHcloudClient(f.t)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			setup(f)

			if code := f.run(); code != 1 {
				t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
			}
			if got := f.deletes(); len(got) != 1 {
				t.Errorf("incus deletes = %v, want the overdue orphan reaped", got)
			}
			f.assertLogContains("ERROR hcloud sweep failed, skipping every hcloud delete this pass:")
		})
	}
}

func noHcloudClient(t *testing.T) func(string) hcloudOps {
	return func(string) hcloudOps {
		t.Error("must not build a client when the hcloud configuration is unusable")
		return &fakeHcloud{}
	}
}

func TestHcloudTokenFileFailureStillReapsIncus(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.env["BROKER_TIERS_FILE"] = writeTiers(t, e2eTierYAML)
	f.env["BROKER_HCLOUD_TOKEN_FILE"] = filepath.Join(t.TempDir(), "missing-token")
	f.d.loadTiers = loadTiers
	f.d.readToken = readHcloudToken
	f.d.newHcloudClient = noHcloudClient(t)

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
	}
	if got := f.deletes(); len(got) != 1 {
		t.Errorf("incus deletes = %v, want the overdue orphan reaped", got)
	}
	f.assertLogContains("hcloud token file")
}

func TestHcloudFailureAndIncusDeleteFailureAreBothReported(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.failDelete["ci-build-1-aaa"] = "permission denied"
	api := &fakeHcloud{listErr: errors.New("hetzner down")}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
	}
	f.assertLogContains("reap: hcloud sweep:")
	f.assertLogContains("hetzner down")
	f.assertLogContains("could not be deleted")
}

func TestHcloudFailureAndGitHubTotalFailureAreBothReported(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	f.github = func(req *http.Request) *http.Response {
		if req.URL.Query().Get("per_page") == "1" {
			return response(500, `{"message":"boom"}`, "")
		}
		return jsonResponse(`{"total_count":1,"runners":[]}`, "")
	}
	api := &fakeHcloud{listErr: errors.New("hetzner down")}
	f.useHcloud(e2eTierYAML, api)

	if code := f.run(); code != 1 {
		t.Errorf("run() = %d, want 1\n%s", code, f.logs.String())
	}
	if len(f.deletes()) != 0 || len(api.deleted) != 0 {
		t.Errorf("deletes = %v / %v, want none", f.deletes(), api.deleted)
	}
	f.assertLogContains("hetzner down")
	f.assertLogContains("HTTP 500")
}

// A tenant that can use no hcloud tier must not read the token file at all: the
// pass behaves exactly as the Incus-only reaper did.
func TestNoUsableHcloudTierDoesNotReadTheToken(t *testing.T) {
	cases := map[string]string{
		"incus only": `
tiers:
  build:
    image: bcx-base
    network: nat-ci
    runner_group: default
    cpu: "2"
    mem: 4GiB
    max_runners: 3
    allowlist: ["*"]
    secrets: []
`,
		"hcloud tier owned by another tenant": `
tiers:
  e2e:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["juno/e2e"]
    secrets: []
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.instances = []fakeInstance{idleInstance()}
			f.env["BROKER_TIERS_FILE"] = writeTiers(t, yaml)
			f.env["BROKER_HCLOUD_TOKEN_FILE"] = "/nonexistent/token"
			f.d.loadTiers = loadTiers
			f.d.readToken = func(string) (string, error) {
				t.Error("the token file must not be read without a usable hcloud tier")
				return "", nil
			}
			f.d.newHcloudClient = func(string) hcloudOps {
				t.Error("no hcloud client may be built without a usable hcloud tier")
				return &fakeHcloud{}
			}

			if code := f.run(); code != 0 {
				t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
			}
			if len(f.deletes()) != 1 {
				t.Errorf("incus deletes = %v, want the idle instance", f.deletes())
			}
		})
	}
}

// ------------------------------------------------ mirrored tier reader

func writeTiers(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiers.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTiersReadsOnlyWhatTheReaperNeeds(t *testing.T) {
	tiers, err := loadTiers(writeTiers(t, `
tiers:
  build:
    image: bcx-base
    network: nat-ci
    runner_group: default
    cpu: "2"
    mem: 4GiB
    max_runners: 3
    allowlist: ["*"]
    secrets: []
  e2e:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["alpha-org/e2e"]
    secrets: []
    future_key: ignored
  other:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    max_age: 90m
    allowlist: ["Juno/app"]
    secrets: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := tiers["build"]; got.Backend != backendIncus {
		t.Errorf("build backend = %s, want incus (absent backend key)", got.Backend)
	}
	if got := tiers["e2e"]; got.Backend != backendHcloud || got.MaxAge != hcloudMaxAgeDefault ||
		!reflect.DeepEqual(got.Allowlist, []string{"alpha-org/e2e"}) {
		t.Errorf("e2e tier = %+v, want hcloud with the 2h default and its allowlist", got)
	}
	if got := tiers["other"]; got.MaxAge != 90*time.Minute {
		t.Errorf("other max_age = %s, want 90m", got.MaxAge)
	}
}

// The same rejections the broker's own tier tests pin for max_age, allowlist,
// backend and shape (cmd/bcx-broker/tiers_hcloud_test.go).
func TestLoadTiersRejectsMalformedHcloudTiers(t *testing.T) {
	base := `
tiers:
  e2e:
    backend: hcloud
    image: bcx-e2e-base
    server_type: cpx42
    locations: [fsn1]
    runner_group: e2e-ephemeral
    max_runners: 2
    allowlist: ["alpha-org/e2e"]
    secrets: []
`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad max_age", base + "    max_age: soon\n", "max_age"},
		{"zero max_age", base + "    max_age: 0\n", "max_age"},
		{"negative max_age", base + "    max_age: -1h\n", "max_age"},
		{"bool max_age", base + "    max_age: true\n", "max_age"},
		{"missing allowlist", strings.Replace(base, `    allowlist: ["alpha-org/e2e"]`+"\n", "", 1), "allowlist"},
		{"empty allowlist", strings.Replace(base, `["alpha-org/e2e"]`, "[]", 1), "allowlist"},
		{"scalar allowlist", strings.Replace(base, `["alpha-org/e2e"]`, "alpha-org/e2e", 1), "allowlist"},
		{"unknown backend", strings.Replace(base, "backend: hcloud", "backend: HCloud", 1), "backend"},
		{"tier is not a mapping", "tiers:\n  e2e: hcloud\n", "e2e"},
		{"no tiers", "tiers: {}\n", "no tiers"},
		{"not yaml", "tiers: [\n", "tiers.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTiers(t, tc.body)
			_, err := loadTiers(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

// An Incus tier never reaches the hcloud-only keys: a value the broker would
// reject for an hcloud tier must not make the Incus reaper skip its own work.
func TestLoadTiersIgnoresKeysAnIncusTierDoesNotUse(t *testing.T) {
	tiers, err := loadTiers(writeTiers(t, `
tiers:
  build:
    image: bcx-base
    network: nat-ci
    runner_group: default
    cpu: "2"
    mem: 4GiB
    max_runners: 3
    allowlist: not-a-list
    max_age: soon
    secrets: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := tiers["build"].Backend; got != backendIncus {
		t.Errorf("backend = %s, want incus", got)
	}
}

// ------------------------------------------------ mirrored usability and folding

func TestTierIsUsableMirrorsTheBrokerRule(t *testing.T) {
	cases := []struct {
		name   string
		allow  []string
		target string
		want   bool
	}{
		{"owner match", []string{"alpha-org/e2e"}, "alpha-org", true},
		{"foreign owner", []string{"alpha-org/e2e"}, "juno", false},
		{"case-insensitive owner", []string{"Juno/app"}, "juno", true},
		{"case-insensitive target", []string{"juno/e2e"}, "Juno", true},
		{"wildcard", []string{"*"}, "anyone", true},
		{"bare owner entry", []string{"alpha-org"}, "alpha-org", true},
		{"empty allowlist", nil, "alpha-org", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tierIsUsable(tier{Allowlist: tc.allow}, tc.target); got != tc.want {
				t.Errorf("tierIsUsable(%v, %q) = %v, want %v", tc.allow, tc.target, got, tc.want)
			}
		})
	}
}

func TestOwnerFoldingMirrorsTheBroker(t *testing.T) {
	cases := map[string]string{
		"Juno/app":        "juno",
		"alpha-org/infra": "alpha-org",
		"alpha-org":       "alpha-org",
	}
	for in, want := range cases {
		if got := ownerOf(in); got != want {
			t.Errorf("ownerOf(%q) = %q, want %q", in, got, want)
		}
	}
	// Owners are ASCII: Unicode folding would match names GitHub does not.
	if got := ownerOf("İstanbul/x"); got == "istanbul" {
		t.Errorf("ownerOf folded U+0130 to %q", got)
	}
	if got := asciiLower("ſafe"); got != "ſafe" {
		t.Errorf("asciiLower changed a non-ASCII rune: %q", got)
	}
	if got := asciiLower("Juno"); got != "juno" {
		t.Errorf("asciiLower(%q) = %q", "Juno", got)
	}
}

// ------------------------------------------------ mirrored token rules

func TestHcloudTokenPathMirrorsTheBrokerDefault(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"default", map[string]string{"BROKER_TENANT": "alpha-org"}, "/etc/bcx-broker/alpha-org.hcloud-token", ""},
		{"override", map[string]string{"BROKER_TENANT": "alpha-org", "BROKER_HCLOUD_TOKEN_FILE": "/tmp/other-token"}, "/tmp/other-token", ""},
		{"empty override means default", map[string]string{"BROKER_TENANT": "alpha-org", "BROKER_HCLOUD_TOKEN_FILE": ""}, "/etc/bcx-broker/alpha-org.hcloud-token", ""},
		{"missing tenant", map[string]string{}, "", "BROKER_TENANT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hcloudTokenPath(tc.env)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("hcloudTokenPath = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func tokenFileAt(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// The same file checks as cmd/bcx-broker/config_hcloud_test.go
// TestHcloudTokenStartupValidation, without the startup proof.
func TestReadHcloudTokenValidation(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "token-dir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := tokenFileAt(t, "valid", "test-token\n", 0o600)
	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing", filepath.Join(dir, "absent"), "absent"},
		{"not a regular file", subdir, "regular"},
		{"group or other bits", tokenFileAt(t, "wide", "test-token\n", 0o644), "mode"},
		{"empty after trimming", tokenFileAt(t, "empty", " \n\t", 0o600), "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := readHcloudToken(tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if token != "" {
				t.Errorf("token = %q, want empty on failure", token)
			}
			if strings.Contains(err.Error(), "test-token") {
				t.Errorf("token leaked into error: %q", err)
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		token, err := readHcloudToken(valid)
		if err != nil || token != "test-token" {
			t.Errorf("readHcloudToken = %q, %v; want the trimmed token", token, err)
		}
	})
}

func TestIncusReapLogNamesTheRule(t *testing.T) {
	f := newFixture(t)
	f.instances = []fakeInstance{idleInstance()}
	if code := f.run(); code != 0 {
		t.Fatalf("run() = %d, want 0\n%s", code, f.logs.String())
	}
	f.assertLogContains("backend=incus tier=- server=ci-build-1-aaa")
	f.assertLogContains("status=- rule=idle")
}

func TestReapReasonStrings(t *testing.T) {
	cases := map[reapReason]string{
		reasonNone:      "none",
		reasonStatusOff: "status-off",
		reasonMaxAge:    "max-age",
		reasonCeiling:   "ceiling",
		reasonIdle:      "idle",
	}
	for reason, want := range cases {
		if got := reason.String(); got != want {
			t.Errorf("reapReason(%d) = %q, want %q", reason, got, want)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an unknown reap reason must panic, not print a zero value")
			}
		}()
		_ = reapReason(99).String()
	}()
}

func TestBackendString(t *testing.T) {
	if got := backendIncus.String(); got != "incus" {
		t.Errorf("backendIncus = %q", got)
	}
	if got := backendHcloud.String(); got != "hcloud" {
		t.Errorf("backendHcloud = %q", got)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("an unknown backend must panic, not print a zero value")
			}
		}()
		_ = backendKind(99).String()
	}()
}

func TestDashIfEmpty(t *testing.T) {
	if got := dashIfEmpty(""); got != "-" {
		t.Errorf("dashIfEmpty(\"\") = %q", got)
	}
	if got := dashIfEmpty("off"); got != "off" {
		t.Errorf("dashIfEmpty(off) = %q", got)
	}
}

func TestFakeHcloudFiltersBySelector(t *testing.T) {
	// Guards the fixture itself: a fake that ignored the selector would let the
	// two-tenant test pass for the wrong reason.
	api := &fakeHcloud{servers: []hcloud.Server{
		{ID: 1, Labels: map[string]string{"bcx-managed": "ci", "bcx-tenant": "alpha-org", "bcx-tier": "e2e"}},
		{ID: 2, Labels: map[string]string{"bcx-managed": "ci", "bcx-tenant": "juno", "bcx-tier": "e2e"}},
	}}
	got, err := api.ListServersContext(context.Background(), "bcx-managed=ci,bcx-tenant=alpha-org,bcx-tier=e2e")
	if err != nil || len(got) != 1 || got[0].ID != 1 {
		t.Errorf("fake filter = %v, %v; want only id 1", got, err)
	}
	if fmt.Sprint(api.selectors) == "" {
		t.Error("selector was not recorded")
	}
}
