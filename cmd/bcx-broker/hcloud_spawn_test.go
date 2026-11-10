package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func hcloudSpawnEnv(t *testing.T, b backend) {
	t.Helper()
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &backends, map[backendKind]backend{backendHcloud: b, backendIncus: b})
}

func TestHcloudSpawnDeletesTheJitRunnerWhenLaunchFails(t *testing.T) {
	logs := captureLog(t)
	fake := &fakeBackend{admit: true, launchErr: errors.New("create failed")}
	hcloudSpawnEnv(t, fake)
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "e2e-ephemeral", 42), nil
		}
		return []byte(`{"runner":{"id":321},"encoded_jit_config":"JIT"}`), nil
	})

	_, err := spawn("tok", 7, "e2e", map[string]tier{"e2e": hcloudFixture()}, map[string]string{}, "alpha-org/app")
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("err = %v, want the launch error", err)
	}
	paths := ghPaths(*calls)
	if len(paths) != 3 || paths[0] != "GET /orgs/alpha-org/actions/runner-groups" ||
		paths[1] != "POST /orgs/alpha-org/actions/runners/generate-jitconfig" ||
		paths[2] != "DELETE /orgs/alpha-org/actions/runners/321" {
		t.Errorf("gh calls = %v", paths)
	}
	if strings.Contains(logs.String(), "tok") {
		t.Errorf("the GitHub token leaked into the log: %q", logs.String())
	}
}

func TestDeleteRunnerUsesTheRepositoryPathForAUserTenant(t *testing.T) {
	userTenant(t)
	calls := ghStub(t, func(ghCall) ([]byte, error) { return nil, nil })
	deleteRunner("tok", "Juno/some-repo", 321)
	if paths := ghPaths(*calls); len(paths) != 1 || paths[0] != "DELETE /repos/Juno/some-repo/actions/runners/321" {
		t.Errorf("gh calls = %v", paths)
	}
}

func TestHcloudSpawnKeepsTheJitRunnerOnSuccess(t *testing.T) {
	fake := &fakeBackend{admit: true}
	hcloudSpawnEnv(t, fake)
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "e2e-ephemeral", 42), nil
		}
		return []byte(`{"runner":{"id":321},"encoded_jit_config":"JIT"}`), nil
	})

	name, err := spawn("tok", 7, "e2e", map[string]tier{"e2e": hcloudFixture()}, map[string]string{}, "alpha-org/app")
	if err != nil || !strings.HasPrefix(name, "ci-e2e-7-") {
		t.Fatalf("got (%q, %v)", name, err)
	}
	if paths := ghPaths(*calls); len(paths) != 2 || paths[1] != "POST /orgs/alpha-org/actions/runners/generate-jitconfig" {
		t.Errorf("gh calls = %v, want no cleanup on success", paths)
	}
	_, _, launches := fake.snapshot()
	if len(launches) != 1 || launches[0].Tier != "e2e" || launches[0].JobID != 7 || launches[0].Remint == nil {
		t.Errorf("launch spec = %+v", launches)
	}
}

// Incus launch failures keep their old behaviour: the ephemeral VM (or its
// half-built remains) is the backend's problem, not a GitHub runner delete.
func TestIncusSpawnDoesNotDeleteTheJitRunner(t *testing.T) {
	fake := &fakeBackend{admit: true, launchErr: errors.New("incus down")}
	freshState(t)
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	swap(t, &backends, map[backendKind]backend{backendIncus: fake})
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "build-ephemeral", 42), nil
		}
		return []byte(`{"runner":{"id":321},"encoded_jit_config":"JIT"}`), nil
	})

	if _, err := spawn("tok", 7, "build", tiersFixture, map[string]string{}, "alpha-org/app"); err == nil {
		t.Fatal("expected the launch error")
	}
	for _, p := range ghPaths(*calls) {
		if strings.HasPrefix(p, "DELETE ") {
			t.Errorf("Incus spawn deleted a runner: %v", ghPaths(*calls))
		}
	}
}

// A Hetzner uniqueness collision renames the server. The JIT config embeds the
// runner name, so spawn must remint: the returned name, the VM name and the
// registration have to stay in sync, and the superseded registration must go.
func TestHcloudSpawnRemintsWhenTheBackendRenames(t *testing.T) {
	const newName = "ci-e2e-7-aaaaaa"
	fake := &fakeBackend{admit: true}
	fake.launchFunc = func(spec launchSpec) error {
		if spec.Remint == nil {
			t.Fatal("hcloud launch spec has no Remint callback")
		}
		jit, err := spec.Remint(newName)
		if err != nil {
			return err
		}
		if jit != "JIT-2" {
			t.Errorf("reminted jit = %q", jit)
		}
		return nil
	}
	hcloudSpawnEnv(t, fake)
	mints := 0
	calls := ghStub(t, func(c ghCall) ([]byte, error) {
		if strings.HasSuffix(c.path, "/runner-groups") {
			return groupsReply(t, "e2e-ephemeral", 42), nil
		}
		if c.method == "POST" {
			mints++
		}
		return []byte(fmt.Sprintf(`{"runner":{"id":%d},"encoded_jit_config":"JIT-%d"}`, mints*100+21, mints)), nil
	})

	name, err := spawn("tok", 7, "e2e", map[string]tier{"e2e": hcloudFixture()}, map[string]string{}, "alpha-org/app")
	if err != nil || name != newName {
		t.Fatalf("got (%q, %v), want the reminted name", name, err)
	}
	var minted []string
	for _, c := range *calls {
		if c.method != "POST" {
			continue
		}
		var b jitBody
		if err := json.Unmarshal(c.body, &b); err != nil {
			t.Fatal(err)
		}
		minted = append(minted, b.Name)
	}
	if len(minted) != 2 || !strings.HasPrefix(minted[0], "ci-e2e-7-") || minted[1] != newName {
		t.Errorf("minted names = %v", minted)
	}
	paths := ghPaths(*calls)
	foundDelete := false
	for _, p := range paths {
		foundDelete = foundDelete || p == "DELETE /orgs/alpha-org/actions/runners/121"
	}
	if !foundDelete {
		t.Errorf("superseded runner was not deleted: %v", paths)
	}
}
