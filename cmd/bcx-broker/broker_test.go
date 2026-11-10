package main

import (
	"encoding/base64"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain mirrors tests/conftest.py: pin a tenant identity before any test
// runs, since broker refuses to default its identity. Tests that change a
// global do it through swap, so nothing leaks from one test to the next.
func TestMain(m *testing.M) {
	tenant = "alpha-org"
	targetKind = "org"
	target = "alpha-org"
	tenantOwner = "alpha-org"
	incusProject = "alpha-org"
	installationID = 42
	os.Exit(m.Run())
}

var tiersFixture = map[string]tier{
	"build": {
		Allowlist: []string{"*"}, MaxRunners: 4, Image: "runner-base",
		Network: "nat-ci", RunnerGroup: "build-ephemeral",
		CPU: "2", Mem: "4GiB", Secrets: []string{},
	},
	"infra": {
		Allowlist: []string{"alpha-org/platform"}, MaxRunners: 2,
		Image: "runner-infra", Network: "nat-ci",
		RunnerGroup: "infra-ephemeral", CPU: "1", Mem: "2GiB",
		Secrets: []string{"INFRA_HCLOUD_TOKEN"},
	},
}

func tiersKnown() map[string]struct{} {
	return map[string]struct{}{"build": {}, "infra": {}}
}

// ---- resolveTier ----

func TestResolveTierIgnoresUnrelatedLabels(t *testing.T) {
	v, tier := resolveTier([]string{"self-hosted", "linux", "x64"}, tiersKnown(), "")
	if v != verdictIgnore || tier != "" {
		t.Errorf("got (%v, %q), want (ignore, \"\")", v, tier)
	}
}

func TestResolveTierResolvesSingleKnownTier(t *testing.T) {
	v, tier := resolveTier([]string{"self-hosted", "build"}, tiersKnown(), "")
	if v != verdictResolved || tier != "build" {
		t.Errorf("got (%v, %q), want (resolved, build)", v, tier)
	}
	v, tier = resolveTier([]string{"self-hosted", "INFRA"}, tiersKnown(), "")
	if v != verdictResolved || tier != "infra" {
		t.Errorf("got (%v, %q), want (resolved, infra)", v, tier)
	}
}

func TestResolveTierDeniesMultiTierRequest(t *testing.T) {
	v, _ := resolveTier([]string{"self-hosted", "build", "infra"}, tiersKnown(), "")
	if v != verdictDeny {
		t.Errorf("got %v, want deny", v)
	}
}

func TestResolveTierDeniesStrayNonTierLabel(t *testing.T) {
	v, _ := resolveTier([]string{"self-hosted", "build", "gpu"}, tiersKnown(), "")
	if v != verdictDeny {
		t.Errorf("got %v, want deny", v)
	}
}

func TestResolveTierIgnoresReservedOnly(t *testing.T) {
	v, _ := resolveTier([]string{"self-hosted"}, tiersKnown(), "")
	if v != verdictIgnore {
		t.Errorf("got %v, want ignore", v)
	}
}

func TestResolveTierStripsOwnTenantLabel(t *testing.T) {
	swap(t, &tenant, "alpha-org")
	v, tier := resolveTier([]string{"self-hosted", "alpha-org", "build"}, tiersKnown(), "alpha-org")
	if v != verdictResolved || tier != "build" {
		t.Errorf("got (%v, %q), want (resolved, build)", v, tier)
	}
	v, tier = resolveTier([]string{"self-hosted", "Alpha-Org", "build"}, tiersKnown(), "alpha-org")
	if v != verdictResolved || tier != "build" {
		t.Errorf("got (%v, %q), want (resolved, build)", v, tier)
	}
}

func TestResolveTierDeniesAnotherTenantsLabel(t *testing.T) {
	v, _ := resolveTier([]string{"self-hosted", "kappa-org", "build"}, tiersKnown(), "alpha-org")
	if v != verdictDeny {
		t.Errorf("got %v, want deny", v)
	}
}

// Python's str.lower() turns U+0130 into "i" + U+0307, which matches nothing;
// Go's strings.ToLower turned it into a plain "i", so "BUİLD" resolved to build
// for a label GitHub itself would never pair with the runner.
func TestResolveTierFoldsASCIIOnly(t *testing.T) {
	for _, l := range []string{"BUİLD", "İNFRA", "buıld"} {
		if v, tier := resolveTier([]string{"self-hosted", l}, tiersKnown(), ""); v != verdictIgnore || tier != "" {
			t.Errorf("%q = (%v, %q), want ignore", l, v, tier)
		}
	}
	if v, tier := resolveTier([]string{"self-hosted", "BUILD"}, tiersKnown(), ""); v != verdictResolved || tier != "build" {
		t.Errorf("BUILD = (%v, %q)", v, tier)
	}
}

// ---- entitlement ----

func TestIsEntitledWildcardAllowsAnyRepo(t *testing.T) {
	if !isEntitled("anyone/anything", "build", tiersFixture, "anyone") {
		t.Error("expected entitled")
	}
}

func TestIsEntitledExplicitAllowlist(t *testing.T) {
	if !isEntitled("alpha-org/platform", "infra", tiersFixture, "") {
		t.Error("expected entitled")
	}
	if isEntitled("alpha-org/some-random-repo", "infra", tiersFixture, "") {
		t.Error("expected not entitled")
	}
}

func TestIsEntitledDeniesForeignOwnerDespiteWildcard(t *testing.T) {
	if !isEntitled("alpha-org/infra", "build", tiersFixture, "alpha-org") {
		t.Error("expected entitled")
	}
	if isEntitled("alpha-org/infra", "build", tiersFixture, "juno") {
		t.Error("expected not entitled for foreign owner")
	}
}

func TestIsEntitledOwnerMatchCaseInsensitive(t *testing.T) {
	if !isEntitled("Juno/app", "build", tiersFixture, "juno") {
		t.Error("expected entitled (case-insensitive)")
	}
}

func TestOwnerOfLowercasesAndTakesFirstSegment(t *testing.T) {
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
}

// Owner and repo names are ASCII; folding anything else could map a mangled
// full_name onto a registered owner or repo that Python would not match.
func TestOwnerAndRepoFoldingIsASCIIOnly(t *testing.T) {
	if got := ownerOf("İstanbul/x"); got == "istanbul" {
		t.Errorf("ownerOf folded U+0130 to %q", got)
	}
	userTenant(t)
	repos = []string{"safe"}
	if !repoIsRegistered("Juno/SAFE", "") {
		t.Error("ASCII case folding must still work")
	}
	if repoIsRegistered("Juno/ſafe", "") { // U+017F folds to "s" under EqualFold
		t.Error("a non-ASCII spelling matched a registered repo")
	}
}

// ---- target path ----

func TestTargetPathIsKindTyped(t *testing.T) {
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	got, err := targetPath("alpha-org/app")
	if err != nil || got != "/orgs/alpha-org" {
		t.Errorf("got (%q, %v), want /orgs/alpha-org", got, err)
	}

	userTenant(t)
	got, err = targetPath("Juno/some-repo")
	if err != nil || got != "/repos/Juno/some-repo" {
		t.Errorf("got (%q, %v), want /repos/Juno/some-repo", got, err)
	}
}

func TestTargetPathRefusesUnregisteredUserRepo(t *testing.T) {
	userTenant(t)
	if _, err := targetPath("Juno/other"); err == nil {
		t.Error("expected error")
	}
}

func TestTargetPathRaisesOnUnknownKind(t *testing.T) {
	swap(t, &targetKind, "repo") // the pre-bartixcloud kind
	if _, err := targetPath("Juno/some-repo"); err == nil {
		t.Error("expected error")
	}
}

// ---- capacity and admission ----

func TestTryReserveBlocksAtCapacity(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	swap(t, &memFloorBytes, 0)
	tiers := map[string]tier{"build": {MaxRunners: 2, Mem: "4GiB"}}

	if !tryReserve("build", tiers) {
		t.Error("first reserve should succeed")
	}
	if !tryReserve("build", tiers) {
		t.Error("second reserve should succeed")
	}
	if tryReserve("build", tiers) {
		t.Error("third reserve should fail (at capacity)")
	}
}

func TestReleaseReservationFreesSlot(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	swap(t, &memFloorBytes, 0)
	tiers := map[string]tier{"build": {MaxRunners: 1, Mem: "4GiB"}}

	if !tryReserve("build", tiers) {
		t.Fatal("first reserve should succeed")
	}
	if tryReserve("build", tiers) {
		t.Fatal("second reserve should fail")
	}
	releaseReservation("build")
	if !tryReserve("build", tiers) {
		t.Fatal("reserve after release should succeed")
	}
}

func TestTryReserveCountsExistingInstances(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return []string{"ci-build-1-aaa", "ci-build-2-bbb"}, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	tiers := map[string]tier{"build": {MaxRunners: 2, Mem: "4GiB"}}
	if tryReserve("build", tiers) {
		t.Error("should be refused (already at capacity)")
	}
}

func TestTryReserveAdmitsWhenHeadroomClearsFloor(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &memFloorBytes, 6<<30)
	// 4GiB tier + 6GiB floor = 10GiB needed; 12GiB available.
	swap(t, &availableBytes, func() (uint64, error) { return 12 << 30, nil })
	if !tryReserve("build", tiersFixture) {
		t.Error("should admit")
	}
}

func TestTryReserveRefusesBelowFloorAndLogsIt(t *testing.T) {
	freshState(t)
	logs := captureLog(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &memFloorBytes, 6<<30)
	// 4GiB tier + 6GiB floor = 10GiB needed; only 9GiB available.
	swap(t, &availableBytes, func() (uint64, error) { return 9 << 30, nil })
	if tryReserve("build", tiersFixture) {
		t.Error("should refuse below floor")
	}
	// infra/drift.sh greps the token; journald alerts key on the level.
	if !strings.Contains(logs.String(), "WARNING ADMISSION REFUSED tier=build tenant=alpha-org") {
		t.Errorf("refusal not logged: %q", logs.String())
	}
	// The refusal must not consume a slot: the job is queued, not spent.
	if reserved["build"] != 0 {
		t.Errorf("reserved should be 0, got %d", reserved["build"])
	}
}

// need+floor in uint64 wrapped for a huge floor, admitting everything.
func TestTryReserveDoesNotWrapAroundOnAHugeFloor(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &memFloorBytes, math.MaxUint64)
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	if tryReserve("build", tiersFixture) {
		t.Error("a floor above the available memory must refuse")
	}
	swap(t, &memFloorBytes, 1<<60)
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 30, nil }) // need (4GiB) > avail
	if tryReserve("build", tiersFixture) {
		t.Error("a tier larger than the available memory must refuse")
	}
}

func TestTryReserveRefusesWhenMemoryCannotBeMeasured(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 0, &testErr{"no MemAvailable"} })
	if tryReserve("build", tiersFixture) {
		t.Error("should refuse when memory unmeasurable")
	}
}

// At capacity is a cheaper, more specific refusal; it must not be reported as a
// memory refusal or it would falsely signal "rent burst capacity".
func TestTryReserveChecksCapacityBeforeMemory(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) {
		return []string{"ci-build-0-x", "ci-build-1-x", "ci-build-2-x", "ci-build-3-x"}, nil
	})
	memoryConsulted := false
	swap(t, &availableBytes, func() (uint64, error) { memoryConsulted = true; return 0, nil })
	if tryReserve("build", tiersFixture) {
		t.Error("should refuse at capacity")
	}
	if memoryConsulted {
		t.Error("memory must not be consulted when at capacity")
	}
}

func TestTryReserveRefusesWhenRunningRunnersFails(t *testing.T) {
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, &testErr{"daemon not reachable"} })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	if tryReserve("build", tiersFixture) {
		t.Error("should refuse when count unknown")
	}
}

// ---- injection script ----

// mustIndexOfLine fails the test when line is absent: a missing line must not
// satisfy an ordering assertion (-1 sorts before everything).
func mustIndexOfLine(t *testing.T, script, line string) int {
	t.Helper()
	for i, l := range strings.Split(script, "\n") {
		if l == line {
			return i
		}
	}
	t.Fatalf("script lacks the line %q:\n%s", line, script)
	return -1
}

// firstLineWithPrefix returns the index of the first line starting with prefix,
// or fails the test.
func firstLineWithPrefix(t *testing.T, script, prefix string) int {
	t.Helper()
	for i, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	t.Fatalf("script has no line starting %q:\n%s", prefix, script)
	return -1
}

func TestBuildInjectionScriptJitconfigOnly(t *testing.T) {
	script := buildInjectionScript("JIT-PAYLOAD", map[string]string{})
	if strings.Contains(script, "JIT-PAYLOAD") {
		t.Error("payload must be base64-encoded, not raw")
	}
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte("JIT-PAYLOAD"))) {
		t.Error("missing base64 payload")
	}
	if !strings.Contains(script, "/etc/actions-runner/jitconfig") {
		t.Error("missing jitconfig path")
	}
	if strings.Contains(script, "credentials.env") {
		t.Error("build tier must not inject credentials")
	}
	// incus exec runs as root; without the chown the unprivileged runner user
	// cannot read a root:root jitconfig and the JIT run silently never starts.
	chown := mustIndexOfLine(t, script, "chown runner:runner /etc/actions-runner/jitconfig")
	chmod := mustIndexOfLine(t, script, "chmod 600 /etc/actions-runner/jitconfig")
	if chown >= chmod {
		t.Error("chown must precede chmod")
	}
}

func TestBuildInjectionScriptIncludesTierSecrets(t *testing.T) {
	script := buildInjectionScript("JIT-PAYLOAD", map[string]string{"INFRA_HCLOUD_TOKEN": "tok123"})
	if !strings.Contains(script, "/etc/infra-runner/credentials.env") {
		t.Error("missing credentials path")
	}
	if strings.Contains(script, "INFRA_HCLOUD_TOKEN=tok123") {
		t.Error("secret must be base64-encoded, not raw")
	}
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte("INFRA_HCLOUD_TOKEN=tok123\n"))) {
		t.Error("missing base64 env content")
	}
	chown := mustIndexOfLine(t, script, "chown runner:runner /etc/infra-runner/credentials.env")
	chmod := mustIndexOfLine(t, script, "chmod 600 /etc/infra-runner/credentials.env")
	if chown >= chmod {
		t.Error("chown must precede chmod")
	}
}

func TestBuildInjectionScriptSurvivesDelimiterCollidingSecretValue(t *testing.T) {
	script := buildInjectionScript("JIT-PAYLOAD", map[string]string{"INFRA_TOKEN": "ENVEOF"})
	mustIndexOfLine(t, script, "chmod 600 /etc/infra-runner/credentials.env")
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte("INFRA_TOKEN=ENVEOF\n"))) {
		t.Error("missing base64 env content")
	}
	count := 0
	for _, line := range strings.Split(script, "\n") {
		if line == "ENVEOF" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ENVEOF delimiter appears %d times, want 1", count)
	}
}

func TestBuildInjectionScriptWritesSecretsInASortedDeterministicOrder(t *testing.T) {
	script := buildInjectionScript("J", map[string]string{"B_KEY": "2", "A_KEY": "1", "C_KEY": "3"})
	want := base64.StdEncoding.EncodeToString([]byte("A_KEY=1\nB_KEY=2\nC_KEY=3\n"))
	if !strings.Contains(script, want) {
		t.Errorf("env content not sorted: %s", script)
	}
}

// The files used to be created under the guest's default umask and only
// chmod'ed afterwards, leaving the credentials briefly world-readable.
func TestBuildInjectionScriptCreatesFilesUnderARestrictiveUmask(t *testing.T) {
	for name, secrets := range map[string]map[string]string{
		"jitconfig only": {},
		"with secrets":   {"INFRA_HCLOUD_TOKEN": "tok"},
	} {
		t.Run(name, func(t *testing.T) {
			script := buildInjectionScript("JIT", secrets)
			if first := strings.Split(script, "\n")[0]; first != "set -e" {
				t.Errorf("first line = %q, want set -e", first)
			}
			umaskAt := mustIndexOfLine(t, script, "umask 077")
			if firstWrite := firstLineWithPrefix(t, script, "base64 -d >"); umaskAt >= firstWrite {
				t.Error("umask must be set before the first file is created")
			}
			if len(secrets) > 0 {
				// A 0700 /etc/infra-runner would hide credentials.env from the
				// runner user, so the directory is made before the umask tightens.
				if mkdir := mustIndexOfLine(t, script, "mkdir -p /etc/infra-runner"); mkdir >= umaskAt {
					t.Error("the credentials directory must be created before umask 077")
				}
			}
		})
	}
}

// shellQuote feeds a sourced KEY=value line: whatever it emits must come back
// out of sh byte for byte, and safe values pass through unquoted like
// shlex.quote.
func TestShellQuoteRoundTripsThroughSh(t *testing.T) {
	for _, v := range []string{
		"", "plain", "with space", "$HOME", "`id`", "$(id)", "it's", "'", "a\nb", `"dq"`, `back\slash`,
		"a;b", "a&b", "*", "~", "!", "#", "tab\tx", "é", "a'b'c",
	} {
		script := "KEY=" + shellQuote(v) + "\nprintf %s \"$KEY\"\n"
		out, err := exec.Command("sh", "-c", script).Output()
		if err != nil || string(out) != v {
			t.Errorf("value %q came back as %q (%v) from %q", v, out, err, script)
		}
	}
	for _, safe := range []string{"abc", "A_b-1.2/3:4,5@6%7+8=9"} {
		if got := shellQuote(safe); got != safe {
			t.Errorf("shellQuote(%q) = %q, want it unquoted", safe, got)
		}
	}
	if got := shellQuote(""); got != "''" {
		t.Errorf("shellQuote(\"\") = %q", got)
	}
}

// ---- decideJobAction ----

func TestDecideJobAction(t *testing.T) {
	d := decideJobAction([]string{"self-hosted", "linux", "x64"}, "anyone/anything", tiersFixture, "")
	if d.action != actionIgnore || d.tier != "" || d.reason != "" {
		t.Errorf("ignore: got %+v", d)
	}

	d = decideJobAction([]string{"self-hosted", "build", "infra"}, "anyone/anything", tiersFixture, "")
	if d.action != actionDeny || d.reason != "label_vocabulary" {
		t.Errorf("label vocab: got %+v", d)
	}

	d = decideJobAction([]string{"self-hosted", "infra"}, "alpha-org/some-random-repo", tiersFixture, "")
	if d.action != actionDeny || d.tier != "infra" || d.reason != "not_entitled" {
		t.Errorf("not entitled: got %+v", d)
	}

	d = decideJobAction([]string{"self-hosted", "build"}, "alpha-org/anything", tiersFixture, "")
	if d.action != actionSpawn || d.tier != "build" || d.reason != "" {
		t.Errorf("spawn build: got %+v", d)
	}

	d = decideJobAction([]string{"self-hosted", "infra"}, "alpha-org/platform", tiersFixture, "")
	if d.action != actionSpawn || d.tier != "infra" || d.reason != "" {
		t.Errorf("spawn infra: got %+v", d)
	}
}

func TestDecideJobActionDeniesWrongOwnerBeforeAllowlist(t *testing.T) {
	d := decideJobAction([]string{"self-hosted", "build"}, "alpha-org/infra", tiersFixture, "juno")
	if d.action != actionDeny || d.tier != "build" || d.reason != "wrong_owner" {
		t.Errorf("got %+v", d)
	}
}

func TestDecideJobActionSpawnsForTenantsOwnRepo(t *testing.T) {
	d := decideJobAction([]string{"self-hosted", "build"}, "Juno/app", tiersFixture, "juno")
	if d.action != actionSpawn || d.tier != "build" || d.reason != "" {
		t.Errorf("got %+v", d)
	}
}

func TestDecideJobActionIgnoresBeforeCheckingOwner(t *testing.T) {
	d := decideJobAction([]string{"self-hosted", "linux", "x64"}, "alpha-org/infra", tiersFixture, "juno")
	if d.action != actionIgnore || d.tier != "" || d.reason != "" {
		t.Errorf("got %+v", d)
	}
}

func TestDecideJobActionDeniesUnregisteredUserRepo(t *testing.T) {
	userTenant(t)
	d := decideJobAction([]string{"self-hosted", "build"}, "Juno/other", tiersFixture, "juno")
	if d.action != actionDeny || d.tier != "build" || d.reason != "not_registered" {
		t.Errorf("got %+v", d)
	}
}

func TestDecideJobActionSpawnsForRegisteredUserRepo(t *testing.T) {
	userTenant(t)
	d := decideJobAction([]string{"self-hosted", "build"}, "Juno/Some-Repo", tiersFixture, "juno")
	if d.action != actionSpawn || d.tier != "build" || d.reason != "" {
		t.Errorf("got %+v", d)
	}
}

// ---- startup secret policy (spec §5.1) ----

func TestUnmetTierSecrets(t *testing.T) {
	if got := unmetTierSecrets(tiersFixture, map[string]string{}, "juno"); len(got) != 0 {
		t.Errorf("juno should demand nothing, got %v", got)
	}
	got := unmetTierSecrets(tiersFixture, map[string]string{}, "alpha-org")
	if len(got) != 1 || len(got["infra"]) != 1 || got["infra"][0] != "INFRA_HCLOUD_TOKEN" {
		t.Errorf("alpha-org should demand INFRA_HCLOUD_TOKEN, got %v", got)
	}
	// wildcard tier demands its secrets for any tenant
	wildcard := map[string]tier{"infra": {Allowlist: []string{"*"}, Secrets: []string{"INFRA_HCLOUD_TOKEN"}}}
	got = unmetTierSecrets(wildcard, map[string]string{}, "juno")
	if len(got) != 1 || got["infra"][0] != "INFRA_HCLOUD_TOKEN" {
		t.Errorf("wildcard should demand, got %v", got)
	}
	// present secrets not missing
	if got := unmetTierSecrets(tiersFixture, map[string]string{"INFRA_HCLOUD_TOKEN": "x"}, "alpha-org"); len(got) != 0 {
		t.Errorf("present secrets should not be missing, got %v", got)
	}
}

func TestTierIsUsableOwnerTypedAndCaseInsensitive(t *testing.T) {
	if !tierIsUsable("infra", tiersFixture, "alpha-org") {
		t.Error("alpha-org should be usable")
	}
	if tierIsUsable("infra", tiersFixture, "juno") {
		t.Error("juno should not be usable")
	}
	tiers := map[string]tier{"infra": {Allowlist: []string{"Juno/app"}}}
	if !tierIsUsable("infra", tiers, "juno") {
		t.Error("case-insensitive owner should be usable")
	}
}

func TestCheckTierSecretsFailsForTheOwningTenant(t *testing.T) {
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &secretsAge, "/etc/bcx-broker/alpha-org.age")
	err := checkTierSecrets(tiersFixture, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "INFRA_HCLOUD_TOKEN") || !strings.Contains(err.Error(), "alpha-org.age") {
		t.Errorf("err = %v, want one naming the missing secret and the age file", err)
	}
	if err := checkTierSecrets(tiersFixture, map[string]string{"INFRA_HCLOUD_TOKEN": "x"}); err != nil {
		t.Errorf("present secret: %v", err)
	}
}

// The juno broker must start with no tier secrets at all: demanding the infra
// tier's would put alpha-org's credentials in juno's age file.
func TestCheckTierSecretsPassesForAForeignOwner(t *testing.T) {
	swap(t, &tenantOwner, "juno")
	if err := checkTierSecrets(tiersFixture, map[string]string{}); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

// ---- helpers ----

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
