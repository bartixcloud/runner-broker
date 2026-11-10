package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bartixcloud/runner-broker/hcloud"
	"gopkg.in/yaml.v3"
)

type fakeHcloudOps struct {
	mu sync.Mutex

	servers       []hcloud.Server
	serverLists   []string
	listErr       error
	snapshots     []hcloud.Snapshot
	snapLists     []string
	snapshotErr   error
	firewallName  string
	firewallCalls []string
	firewallID    int64
	firewallErr   error

	creates     []hcloud.CreateRequest
	createErrs  []error
	blockCreate bool

	deletes   []int64
	deleteErr error
}

func (f *fakeHcloudOps) ListServersContext(_ context.Context, selector string) ([]hcloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverLists = append(f.serverLists, selector)
	return append([]hcloud.Server(nil), f.servers...), f.listErr
}

func (f *fakeHcloudOps) ListSnapshotsContext(_ context.Context, selector string) ([]hcloud.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapLists = append(f.snapLists, selector)
	return append([]hcloud.Snapshot(nil), f.snapshots...), f.snapshotErr
}

func (f *fakeHcloudOps) EnsureFirewallContext(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewallName = name
	f.firewallCalls = append(f.firewallCalls, name)
	return f.firewallID, f.firewallErr
}

func (f *fakeHcloudOps) CreateServerContext(ctx context.Context, req hcloud.CreateRequest) (hcloud.Server, error) {
	f.mu.Lock()
	i := len(f.creates)
	f.creates = append(f.creates, req)
	var err error
	if i < len(f.createErrs) {
		err = f.createErrs[i]
	}
	block := f.blockCreate
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return hcloud.Server{}, ctx.Err()
	}
	if err != nil {
		return hcloud.Server{}, err
	}
	return hcloud.Server{ID: int64(i + 1), Name: req.Name}, nil
}

func (f *fakeHcloudOps) DeleteServer(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return f.deleteErr
}

func (f *fakeHcloudOps) snapshot() (lists, snapLists []string, creates []hcloud.CreateRequest, deletes []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.serverLists...), append([]string(nil), f.snapLists...),
		append([]hcloud.CreateRequest(nil), f.creates...), append([]int64(nil), f.deletes...)
}

func hcloudFixture() tier {
	return tier{
		Backend:     backendHcloud,
		Image:       "bcx-e2e-base",
		ServerType:  "cpx42",
		Locations:   []string{"fsn1", "nbg1", "hel1"},
		RunnerGroup: "e2e-ephemeral",
		MaxRunners:  2,
		MaxAge:      2 * time.Hour,
		Allowlist:   []string{"alpha-org/e2e"},
		Secrets:     []string{},
	}
}

func hcloudBackendEnv(t *testing.T) *fakeHcloudOps {
	t.Helper()
	swap(t, &tenant, "alpha-org")
	swap(t, &hcloudLaunchTimeout, 25*time.Second)
	fake := &fakeHcloudOps{
		snapshots:  []hcloud.Snapshot{{ID: 4242, Created: time.Now(), Labels: map[string]string{"bcx-image": "bcx-e2e-base"}}},
		firewallID: 99,
	}
	swap(t, &hcloudAPI, hcloudOps(fake))
	return fake
}

func hcloudSpec() launchSpec {
	return launchSpec{
		Name:    "ci-e2e-7-abcdef",
		Tier:    "e2e",
		JobID:   7,
		Config:  hcloudFixture(),
		JIT:     "JIT-PAYLOAD",
		Secrets: map[string]string{},
	}
}

func TestHcloudRunningScopesListsToTenantAndTier(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.servers = []hcloud.Server{{ID: 1, Name: "ci-e2e-7-abcdef"}, {ID: 2, Name: "ci-e2e-8-123456"}}
	b := hcloudBackend{}

	got, err := b.Running("e2e")
	if err != nil || len(got) != 2 || got[0] != "ci-e2e-7-abcdef" {
		t.Fatalf("got (%v, %v)", got, err)
	}
	if _, err := b.Running(""); err != nil {
		t.Fatal(err)
	}
	lists, _, _, _ := fake.snapshot()
	if len(lists) != 2 || lists[0] != "bcx-managed=ci,bcx-tenant=alpha-org,bcx-tier=e2e" ||
		lists[1] != "bcx-managed=ci,bcx-tenant=alpha-org" {
		t.Errorf("selectors = %v", lists)
	}
}

func TestHcloudAdmitCountsServersAndReservationsWithoutMemory(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.servers = []hcloud.Server{{ID: 1, Name: "ci-e2e-7-abcdef"}}
	swap(t, &availableBytes, func() (uint64, error) { return 0, errors.New("must not be consulted") })
	b := hcloudBackend{}
	cfg := hcloudFixture()
	cfg.MaxRunners = 2

	if !b.Admit("e2e", cfg, 0) {
		t.Error("one server + no reservations must fit max=2")
	}
	if b.Admit("e2e", cfg, 1) {
		t.Error("one server + one reservation must not fit max=2")
	}
	if !b.Admit("e2e", cfg, 0) {
		t.Error("memory must not influence hcloud admission")
	}
}

func TestHcloudLaunchLoopsLocationsAndSendsTheContract(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.createErrs = []error{
		&hcloud.APIError{Status: http.StatusPreconditionFailed, Code: "resource_unavailable", Message: "no capacity"},
		nil,
	}
	b := hcloudBackend{}
	spec := hcloudSpec()

	if err := b.Launch(spec); err != nil {
		t.Fatal(err)
	}
	_, snapLists, creates, _ := fake.snapshot()
	if len(snapLists) != 1 || snapLists[0] != "bcx-image=bcx-e2e-base" {
		t.Errorf("snapshot selectors = %v", snapLists)
	}
	if fake.firewallName != "bcx-ci-deny-inbound" {
		t.Errorf("firewall = %q", fake.firewallName)
	}
	if len(creates) != 2 || creates[0].Location != "fsn1" || creates[1].Location != "nbg1" {
		t.Fatalf("creates = %+v", creates)
	}
	got := creates[1]
	if got.Name != spec.Name || got.ServerType != "cpx42" || got.Image != 4242 ||
		!got.PublicNet.EnableIPv4 || got.PublicNet.EnableIPv6 || !got.StartAfterCreate {
		t.Errorf("create body = %+v", got)
	}
	if len(got.Firewalls) != 1 || got.Firewalls[0].Firewall != 99 {
		t.Errorf("firewalls = %+v", got.Firewalls)
	}
	wantLabels := map[string]string{
		"bcx-managed": "ci", "bcx-tenant": "alpha-org", "bcx-tier": "e2e", "bcx-job": "7",
	}
	if len(got.Labels) != len(wantLabels) {
		t.Errorf("labels = %v", got.Labels)
	}
	for k, v := range wantLabels {
		if got.Labels[k] != v {
			t.Errorf("labels[%q] = %q, want %q", k, got.Labels[k], v)
		}
	}

	var cloud struct {
		WriteFiles []struct {
			Path        string `yaml:"path"`
			Permissions string `yaml:"permissions"`
			Owner       string `yaml:"owner"`
			Encoding    string `yaml:"encoding"`
			Content     string `yaml:"content"`
		} `yaml:"write_files"`
		RunCmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(got.UserData), &cloud); err != nil {
		t.Fatalf("user_data is not cloud-config YAML: %v\n%s", err, got.UserData)
	}
	if len(cloud.WriteFiles) != 1 {
		t.Fatalf("write_files = %+v", cloud.WriteFiles)
	}
	wf := cloud.WriteFiles[0]
	if wf.Path != "/etc/actions-runner/jitconfig" || wf.Permissions != "0600" ||
		wf.Owner != "runner:runner" || wf.Encoding != "b64" {
		t.Errorf("write_file = %+v", wf)
	}
	jit, err := base64.StdEncoding.DecodeString(wf.Content)
	if err != nil || string(jit) != spec.JIT {
		t.Errorf("decoded jit = %q (%v), want %q", jit, err, spec.JIT)
	}
	if len(cloud.RunCmd) != 1 || strings.Join(cloud.RunCmd[0], " ") != "systemctl start --no-block bcx-ci-runner.service" {
		t.Errorf("runcmd = %v", cloud.RunCmd)
	}
	if strings.Contains(got.UserData, "test-token") {
		t.Error("the hcloud token leaked into user_data")
	}
}

func TestHcloudLaunchWaitsWhenEveryLocationIsUnavailable(t *testing.T) {
	logs := captureLog(t)
	fake := hcloudBackendEnv(t)
	for i := 0; i < 3; i++ {
		fake.createErrs = append(fake.createErrs, &hcloud.APIError{Status: 422, Code: "placement_error"})
	}
	err := (&hcloudBackend{}).Launch(hcloudSpec())
	if !errors.Is(err, errLaunchAbandoned) {
		t.Fatalf("err = %v, want errLaunchAbandoned", err)
	}
	_, _, creates, _ := fake.snapshot()
	if len(creates) != 3 || creates[2].Location != "hel1" {
		t.Errorf("creates = %+v", creates)
	}
	if !strings.Contains(logs.String(), "WARNING") {
		t.Errorf("log = %q", logs.String())
	}
}

func TestHcloudLaunchTreatsLimitsAsWait(t *testing.T) {
	for _, c := range []struct {
		name   string
		apiErr *hcloud.APIError
	}{
		{"project limit", &hcloud.APIError{Status: 403, Code: "resource_limit_exceeded"}},
		{"rate limit", &hcloud.APIError{Status: 429, Code: "rate_limit_exceeded"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLog(t)
			fake := hcloudBackendEnv(t)
			fake.createErrs = []error{c.apiErr}
			err := (&hcloudBackend{}).Launch(hcloudSpec())
			if !errors.Is(err, errLaunchAbandoned) {
				t.Fatalf("err = %v, want a wait, not a spawn failure", err)
			}
			_, _, creates, _ := fake.snapshot()
			if len(creates) != 1 {
				t.Errorf("creates = %d, want no retry on a non-placement limit", len(creates))
			}
			if !strings.Contains(logs.String(), "WARNING") {
				t.Errorf("log = %q", logs.String())
			}
		})
	}
}

func TestHcloudLaunchRetriesAUniqueNameOnce(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.createErrs = []error{nil, nil}
	fake.createErrs[0] = &hcloud.APIError{Status: 409, Code: "uniqueness_error"}
	err := (&hcloudBackend{}).Launch(hcloudSpec())
	if err != nil {
		t.Fatal(err)
	}
	_, _, creates, _ := fake.snapshot()
	if len(creates) != 2 {
		t.Fatalf("creates = %d, want one retry", len(creates))
	}
	if creates[0].Name == creates[1].Name {
		t.Errorf("retry reused the taken name %q", creates[0].Name)
	}
	if !regexp.MustCompile(`^ci-e2e-7-[0-9a-f]{6}$`).MatchString(creates[1].Name) {
		t.Errorf("retry name = %q", creates[1].Name)
	}
}

func TestHcloudLaunchRejectsOversizedUserDataBeforeAnyCall(t *testing.T) {
	fake := hcloudBackendEnv(t)
	spec := hcloudSpec()
	spec.JIT = strings.Repeat("x", 40<<10)
	err := (&hcloudBackend{}).Launch(spec)
	if err == nil || !strings.Contains(err.Error(), "32 KiB") {
		t.Fatalf("err = %v, want a size error", err)
	}
	_, snapLists, creates, _ := fake.snapshot()
	if len(snapLists) != 0 || len(creates) != 0 {
		t.Errorf("oversized user_data reached the API: snapshots=%v creates=%v", snapLists, creates)
	}
}

func TestHcloudDeleteUsesTheTenantSelectorAndIsIdempotent(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.servers = []hcloud.Server{{ID: 41, Name: "ci-e2e-7-abcdef"}, {ID: 42, Name: "ci-e2e-8-123456"}}
	b := hcloudBackend{}

	if err := b.Delete("ci-e2e-7-abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("ci-e2e-999-ffffff"); err != nil {
		t.Fatalf("unknown name must be nil: %v", err)
	}
	lists, _, _, deletes := fake.snapshot()
	if len(lists) != 2 || lists[0] != "bcx-managed=ci,bcx-tenant=alpha-org" || lists[1] != lists[0] {
		t.Errorf("delete selectors = %v", lists)
	}
	if len(deletes) != 1 || deletes[0] != 41 {
		t.Errorf("deletes = %v, want only the matching server", deletes)
	}
}

func TestHcloudLaunchIsBounded(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.blockCreate = true
	swap(t, &hcloudLaunchTimeout, 50*time.Millisecond)
	start := time.Now()
	err := (&hcloudBackend{}).Launch(hcloudSpec())
	if !errors.Is(err, errLaunchAbandoned) {
		t.Fatalf("err = %v, want a bounded wait", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Launch took %s, want <= the loop bound", elapsed)
	}
}

func TestHcloudLaunchFailsLoudlyWhenNoSnapshotMatches(t *testing.T) {
	fake := hcloudBackendEnv(t)
	fake.snapshots = nil
	err := (&hcloudBackend{}).Launch(hcloudSpec())
	if err == nil || !strings.Contains(err.Error(), "bcx-image=bcx-e2e-base") {
		t.Fatalf("err = %v, want one naming the missing snapshot label", err)
	}
	_, _, creates, _ := fake.snapshot()
	if len(creates) != 0 {
		t.Errorf("created without a snapshot: %+v", creates)
	}
}

func TestHcloudBackendWithoutAClientFailsClosed(t *testing.T) {
	swap(t, &hcloudAPI, nil)
	if _, err := (hcloudBackend{}).Running("e2e"); err == nil || !strings.Contains(err.Error(), "token is not configured") {
		t.Fatalf("err = %v, want a loud missing-client error", err)
	}
}

// A tenant that cannot use an hcloud tier has no hcloud token, so the all-tier
// lookup must not ask Hetzner or fail startup on that tier.
func TestRunningRunnersAllSkipsAnUnusableHcloudTier(t *testing.T) {
	swap(t, &tenant, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &hcloudAPI, nil)
	swap(t, &incusProject, "alpha-org")
	rec := incusStub(t, func(incusCall) (string, string, int) { return "ci-build-1-aaaaaa\n", "", 0 })
	cfg := hcloudFixture()
	cfg.Allowlist = []string{"juno/e2e"}
	tiers := map[string]tier{"build": tiersFixture["build"], "e2e": cfg}

	got, err := runningRunnersAll(tiers)
	if err != nil || !reflect.DeepEqual(got, []string{"ci-build-1-aaaaaa"}) {
		t.Fatalf("got (%v, %v)", got, err)
	}
	if calls := rec.all(); len(calls) != 1 {
		t.Errorf("incus calls = %v, want one", calls)
	}
}
