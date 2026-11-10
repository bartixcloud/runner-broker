package main

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// spawnEnv wires the seams spawn touches for an org tenant: capacity and memory
// are plentiful, GitHub answers with a group and a jitconfig, and incus
// succeeds. Tests override the pieces they exercise.
func spawnEnv(t *testing.T, reply func(c incusCall) (string, string, int)) *incusRecorder {
	t.Helper()
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &incusProject, "alpha-org")
	swap(t, &memFloorBytes, 0)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 60, nil })
	swap(t, &pause, func(time.Duration) {})
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return mustJSON(t, map[string]any{"runner_groups": []any{
				map[string]any{"name": "build-ephemeral", "id": 42},
				map[string]any{"name": "infra-ephemeral", "id": 99},
			}}), nil
		}
		return []byte(`{"encoded_jit_config":"JIT-PAYLOAD"}`), nil
	})
	if reply == nil {
		reply = incusOK
	}
	return incusStub(t, reply)
}

func singleSlotTiers() map[string]tier {
	cfg := tiersFixture["build"]
	cfg.MaxRunners = 1
	return map[string]tier{"build": cfg}
}

func TestSpawnLaunchesAndInjectsWithOneExecSh(t *testing.T) {
	rec := spawnEnv(t, nil)
	name, err := spawn("tok", 7, "build", tiersFixture, map[string]string{}, "alpha-org/app")
	if err != nil || !strings.HasPrefix(name, "ci-build-7-") {
		t.Fatalf("got (%q, %v)", name, err)
	}
	launch := rec.withVerb("launch")
	if len(launch) != 1 {
		t.Fatalf("launch calls = %v", launch)
	}
	wantLaunch := []string{"incus", "--project", "alpha-org", "launch", "runner-base", name, "--ephemeral",
		"--network", "nat-ci", "-c", "limits.cpu=2", "-c", "limits.memory=4GiB"}
	if !reflect.DeepEqual(launch[0].argv, wantLaunch) {
		t.Errorf("launch argv = %v, want %v", launch[0].argv, wantLaunch)
	}
	// The injection used to be preceded by a dead `exec -- sh` with no stdin
	// whose (empty) stderr was what the failure log printed.
	var sh []incusCall
	for _, c := range rec.withVerb("exec") {
		if c.argv[len(c.argv)-1] == "sh" {
			sh = append(sh, c)
		}
	}
	if len(sh) != 1 {
		t.Fatalf("want exactly one `exec -- sh`, got %v", sh)
	}
	if sh[0].stdin != buildInjectionScript("JIT-PAYLOAD", map[string]string{}) {
		t.Errorf("stdin = %q", sh[0].stdin)
	}
}

func TestSpawnInjectsTheTiersOwnSecretsOnly(t *testing.T) {
	rec := spawnEnv(t, nil)
	secrets := map[string]string{"INFRA_HCLOUD_TOKEN": "tok123", "UNRELATED": "nope"}
	name, err := spawn("tok", 7, "infra", tiersFixture, secrets, "alpha-org/platform")
	if err != nil || name == "" {
		t.Fatalf("got (%q, %v)", name, err)
	}
	var stdin string
	for _, c := range rec.withVerb("exec") {
		stdin += c.stdin
	}
	if !strings.Contains(stdin, base64.StdEncoding.EncodeToString([]byte("INFRA_HCLOUD_TOKEN=tok123\n"))) {
		t.Errorf("tier secret not injected: %q", stdin)
	}
	if strings.Contains(stdin, base64.StdEncoding.EncodeToString([]byte("UNRELATED=nope\n"))) {
		t.Error("another tier's secret leaked into the VM")
	}
}

// One flag scopes launch, list, exec and delete. Drive a spawn whose injection
// fails so every verb is exercised, then require the prefix on all of them.
func TestSpawnScopesEveryIncusCallToTheProject(t *testing.T) {
	rec := spawnEnv(t, func(c incusCall) (string, string, int) {
		if c.stdin != "" {
			return "", "boom", 1
		}
		return "", "", 0
	})
	swap(t, &incusProject, "juno")
	if name, err := spawn("tok", 1, "build", tiersFixture, map[string]string{}, "alpha-org/app"); err != nil || name != "" {
		t.Fatalf("got (%q, %v), want an abandoned spawn", name, err)
	}
	verbs := map[string]bool{}
	for _, c := range rec.all() {
		if !reflect.DeepEqual(c.argv[:3], []string{"incus", "--project", "juno"}) {
			t.Errorf("unscoped incus call: %v", c.argv)
		}
		verbs[c.argv[3]] = true
	}
	for _, v := range []string{"launch", "exec", "delete"} {
		if !verbs[v] {
			t.Errorf("verb %q never exercised: %v", v, verbs)
		}
	}
}

func TestSpawnLogsTheInjectionFailureReasonAndDeletesTheVM(t *testing.T) {
	logs := captureLog(t)
	rec := spawnEnv(t, func(c incusCall) (string, string, int) {
		if c.stdin != "" {
			return "", "guest agent exploded", 1
		}
		return "", "", 0
	})
	name, err := spawn("tok", 1, "build", tiersFixture, map[string]string{}, "alpha-org/app")
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v)", name, err)
	}
	if !strings.Contains(logs.String(), "guest agent exploded") {
		t.Errorf("log lacks the reason: %q", logs.String())
	}
	del := rec.withVerb("delete")
	if len(del) != 1 || del[0].argv[len(del[0].argv)-1] != "--force" {
		t.Errorf("delete calls = %v", del)
	}
}

func TestSpawnWaitsForTheGuestAgentBeforeInjecting(t *testing.T) {
	probes := 0
	rec := spawnEnv(t, func(c incusCall) (string, string, int) {
		if c.argv[len(c.argv)-1] == "/etc/actions-runner" {
			probes++
			if probes < 4 {
				return "", "", 1
			}
		}
		return "", "", 0
	})
	if name, err := spawn("tok", 1, "build", tiersFixture, map[string]string{}, "alpha-org/app"); err != nil || name == "" {
		t.Fatalf("got (%q, %v)", name, err)
	}
	if probes != 4 {
		t.Errorf("probes = %d, want 4", probes)
	}
	if len(rec.withVerb("exec")) != 5 { // 4 probes + 1 injection
		t.Errorf("exec calls = %d", len(rec.withVerb("exec")))
	}
}

func TestSpawnGivesUpWaitingAfterSixtyProbesAndStillTriesToInject(t *testing.T) {
	probes := 0
	spawnEnv(t, func(c incusCall) (string, string, int) {
		if c.argv[len(c.argv)-1] == "/etc/actions-runner" {
			probes++
			return "", "", 1
		}
		return "", "", 0
	})
	if _, err := spawn("tok", 1, "build", tiersFixture, map[string]string{}, "alpha-org/app"); err != nil {
		t.Fatal(err)
	}
	if probes != 60 {
		t.Errorf("probes = %d, want 60", probes)
	}
}

// A leaked slot per failed spawn would leave the tier permanently "at capacity".
func TestSpawnReleasesTheReservationWhenMintFails(t *testing.T) {
	spawnEnv(t, nil)
	ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("github api unreachable") })
	tiers := singleSlotTiers()
	if _, err := spawn("tok", 1, "build", tiers, map[string]string{}, "alpha-org/app"); err == nil {
		t.Fatal("expected the mint error")
	}
	if !tryReserve("build", tiers) {
		t.Error("the failed attempt leaked its reservation")
	}
}

func TestSpawnReleasesTheReservationWhenLaunchFails(t *testing.T) {
	rec := spawnEnv(t, func(c incusCall) (string, string, int) {
		if c.argv[3] == "launch" {
			return "", "launch failed", 1
		}
		return "", "", 0
	})
	tiers := singleSlotTiers()
	name, err := spawn("tok", 1, "build", tiers, map[string]string{}, "alpha-org/app")
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v), want an abandoned spawn", name, err)
	}
	if !tryReserve("build", tiers) {
		t.Error("the failed launch leaked its reservation")
	}
	if len(rec.withVerb("exec")) != 0 {
		t.Errorf("nothing to inject into after a failed launch: %v", rec.all())
	}
}

func TestSpawnReleasesTheReservationOnSuccess(t *testing.T) {
	spawnEnv(t, nil)
	tiers := singleSlotTiers()
	if name, err := spawn("tok", 1, "build", tiers, map[string]string{}, "alpha-org/app"); err != nil || name == "" {
		t.Fatalf("got (%q, %v)", name, err)
	}
	if reserved["build"] != 0 {
		t.Errorf("reserved = %d: a running VM is counted by incus list, not by the reservation", reserved["build"])
	}
}

// Python indexed the jitconfig reply, so a reply without one aborted before
// any VM existed.
func TestSpawnLaunchesNothingWhenNoJitconfigCanBeMinted(t *testing.T) {
	rec := spawnEnv(t, nil)
	ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{}`), nil })
	swap(t, &targetKind, "user")
	swap(t, &target, "Juno")
	swap(t, &tenantOwner, "juno")
	swap(t, &repos, []string{"some-repo"})
	tiers := singleSlotTiers()
	if _, err := spawn("tok", 1, "build", tiers, map[string]string{}, "Juno/some-repo"); err == nil {
		t.Fatal("expected an error")
	}
	if len(rec.all()) != 0 {
		t.Errorf("no incus call may precede a minted jitconfig: %v", rec.all())
	}
	if !tryReserve("build", tiers) {
		t.Error("reservation leaked")
	}
}

// Python's {k: secrets[k]} raised KeyError; Go injected KEY=” and started the
// runner with blank credentials.
func TestSpawnFailsBeforeLaunchWhenATierSecretIsMissing(t *testing.T) {
	rec := spawnEnv(t, nil)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return []byte(`{"encoded_jit_config":"J"}`), nil })
	tiers := map[string]tier{"infra": tiersFixture["infra"]}
	if _, err := spawn("tok", 1, "infra", tiers, map[string]string{}, "alpha-org/platform"); err == nil ||
		!strings.Contains(err.Error(), "INFRA_HCLOUD_TOKEN") {
		t.Fatalf("err = %v, want one naming the missing secret", err)
	}
	if len(rec.all()) != 0 || len(*calls) != 0 {
		t.Errorf("nothing may be minted or launched: incus=%v gh=%v", rec.all(), *calls)
	}
}

func TestSpawnReturnsEmptyWhenTheTierIsAtCapacity(t *testing.T) {
	rec := spawnEnv(t, nil)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, errors.New("must not be called") })
	swap(t, &runningRunners, func(string) ([]string, error) { return []string{"ci-build-0-x"}, nil })
	name, err := spawn("tok", 1, "build", singleSlotTiers(), map[string]string{}, "alpha-org/app")
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v), want a quiet refusal", name, err)
	}
	if len(*calls) != 0 || len(rec.all()) != 0 {
		t.Errorf("a refused reservation must not touch GitHub or incus")
	}
}
