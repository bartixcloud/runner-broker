package main

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAdmit records one admission question so a test can assert both the tier
// and the reservations the caller already held.
type fakeAdmit struct {
	tier     string
	cfg      tier
	reserved int
}

// fakeBackend stands in for a VM host. It records every seam call so a test can
// prove the broker went through the backend instead of talking to Incus.
type fakeBackend struct {
	mu sync.Mutex

	running    []string
	runningErr error
	runningArg []string

	admit      bool
	admits     []fakeAdmit
	launches   []launchSpec
	launchFunc func(launchSpec) error
	launchErr  error
	deleted    []string
}

func (f *fakeBackend) Running(t string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runningArg = append(f.runningArg, t)
	return append([]string(nil), f.running...), f.runningErr
}

func (f *fakeBackend) Admit(t string, cfg tier, reserved int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admits = append(f.admits, fakeAdmit{tier: t, cfg: cfg, reserved: reserved})
	return f.admit
}

func (f *fakeBackend) Launch(spec launchSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, spec)
	if f.launchFunc != nil {
		return f.launchFunc(spec)
	}
	return f.launchErr
}

func (f *fakeBackend) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeBackend) setAdmit(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admit = v
}

func (f *fakeBackend) snapshot() (runningArgs []string, admits []fakeAdmit, launches []launchSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runningArg...),
		append([]fakeAdmit(nil), f.admits...),
		append([]launchSpec(nil), f.launches...)
}

// useBackend installs b as the implementation of every Incus tier. Restored
// through swap, so no test inherits a fake host.
func useBackend(t *testing.T, b backend) {
	t.Helper()
	swap(t, &backends, map[backendKind]backend{backendIncus: b})
}

// backendEnv wires the pieces spawn and reconcileOnce read without touching
// Incus: identity, GitHub (runner groups + jitconfig) and the guest-agent pause.
func backendEnv(t *testing.T) {
	t.Helper()
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &incusProject, "alpha-org")
	swap(t, &memFloorBytes, 0)
	swap(t, &pause, func(time.Duration) {})
	ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"encoded_jit_config":"JIT-PAYLOAD"}`), nil
	})
}

// spawn must take its launch path from the tier's backend. A single Incus call
// here would mean the seam is bypassed.
func TestSpawnGoesThroughTheTierBackend(t *testing.T) {
	backendEnv(t)
	fake := &fakeBackend{admit: true}
	useBackend(t, fake)
	incusCalls := 0
	swap(t, &execCommand, func([]string, string) (string, string, int) {
		incusCalls++
		return "", "must not run incus", 1
	})

	name, err := spawn("tok", 7, "build", tiersFixture, map[string]string{}, "alpha-org/app")
	if err != nil || !strings.HasPrefix(name, "ci-build-7-") {
		t.Fatalf("got (%q, %v), want a ci-build-7- name", name, err)
	}
	if incusCalls != 0 {
		t.Errorf("spawn made %d incus call(s), want the backend only", incusCalls)
	}

	runningArgs, admits, launches := fake.snapshot()
	if len(runningArgs) != 0 {
		t.Errorf("Running calls = %v, want none during a spawn", runningArgs)
	}
	if len(admits) != 1 || admits[0].tier != "build" || admits[0].reserved != 0 {
		t.Errorf("admits = %+v, want one build admission with 0 reserved", admits)
	}
	if len(launches) != 1 {
		t.Fatalf("launches = %+v, want exactly one", launches)
	}
	got := launches[0]
	if got.Name != name || got.JIT != "JIT-PAYLOAD" || got.Config.Image != "runner-base" ||
		got.Config.Network != "nat-ci" || got.Config.CPU != "2" || got.Config.Mem != "4GiB" {
		t.Errorf("launch spec = %+v, want the tier's config and the minted JIT", got)
	}
}

// tryReserve must ask the backend under launchLock and keep the reservation
// count itself: a refusal may not consume a slot.
func TestTryReserveAsksTheTierBackend(t *testing.T) {
	freshState(t)
	fake := &fakeBackend{admit: true}
	useBackend(t, fake)
	incusCalls, memCalls := 0, 0
	swap(t, &execCommand, func([]string, string) (string, string, int) {
		incusCalls++
		return "", "", 1
	})
	swap(t, &availableBytes, func() (uint64, error) {
		memCalls++
		return 0, nil
	})
	tiers := map[string]tier{"build": {Backend: backendIncus, MaxRunners: 2, Mem: "4GiB"}}

	for i := 0; i < 2; i++ {
		if !tryReserve("build", tiers) {
			t.Fatalf("reservation %d should be admitted by the fake", i+1)
		}
	}
	_, admits, _ := fake.snapshot()
	if len(admits) != 2 || admits[0].reserved != 0 || admits[1].reserved != 1 {
		t.Fatalf("admits = %+v, want reservations 0 then 1", admits)
	}
	if admits[0].cfg.MaxRunners != 2 || admits[0].cfg.Mem != "4GiB" {
		t.Errorf("admits[0].cfg = %+v, want the tier's config", admits[0].cfg)
	}

	fake.setAdmit(false)
	if tryReserve("build", tiers) {
		t.Error("a backend refusal must not be overridden")
	}
	if reserved["build"] != 2 {
		t.Errorf("reserved = %d, want 2: a refusal must not consume a slot", reserved["build"])
	}
	if incusCalls != 0 || memCalls != 0 {
		t.Errorf("tryReserve touched incus (%d) or host memory (%d) directly", incusCalls, memCalls)
	}
}

// The reconciler's covered-job lookup must list through the backends: the names
// a backend reports are what stops a second VM for an already-covered job.
func TestReconcileFindsCoveredJobsThroughTheBackend(t *testing.T) {
	backendEnv(t)
	rec := incusStub(t, incusOK)
	fake := &fakeBackend{running: []string{"ci-build-7-abc123"}, admit: true}
	useBackend(t, fake)
	reconcileStub(t, map[string][]int{"alpha-org/app": {100}}, oneQueuedJob(7, []string{"self-hosted", "build"}))

	srv := newServer(fakeTokens{token: "tok"})
	srv.reconcileOnce(context.Background(), "tok")
	srv.wg.Wait()

	if n := len(rec.withVerb("launch")); n != 0 {
		t.Errorf("launch calls = %d, want 0: job 7 is already covered", n)
	}
	runningArgs, admits, launches := fake.snapshot()
	if !reflect.DeepEqual(runningArgs, []string{""}) {
		t.Errorf("Running calls = %v, want one all-tier lookup", runningArgs)
	}
	if len(admits) != 0 || len(launches) != 0 {
		t.Errorf("a covered job must not admit or launch: admits=%v launches=%v", admits, launches)
	}
}
