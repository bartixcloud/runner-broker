package main

import (
	"net/http"
	"testing"
)

func completedEnv(t *testing.T) (*server, *fakeBackend) {
	t.Helper()
	swap(t, &tenant, "alpha-org")
	swap(t, &targetKind, "org")
	swap(t, &target, "alpha-org")
	swap(t, &tenantOwner, "alpha-org")
	fake := &fakeBackend{}
	swap(t, &backends, map[backendKind]backend{backendHcloud: fake, backendIncus: fake})
	srv := &server{
		tokens: fakeTokens{token: "tok"},
		tiers: map[string]tier{
			"build": tiersFixture["build"],
			"e2e":   hcloudFixture(),
		},
		secrets: map[string]string{},
	}
	return srv, fake
}

func completedEvent(runnerName, repo string) map[string]any {
	return map[string]any{
		"action": "completed",
		"workflow_job": map[string]any{
			"id": 1, "run_id": 2, "runner_name": runnerName,
		},
		"repository": map[string]any{"full_name": repo},
	}
}

func TestCompletedDeletesAHcloudRunnerByItsName(t *testing.T) {
	srv, fake := completedEnv(t)
	code, body := postJob(t, srv, completedEvent("ci-e2e-7-abcdef", "alpha-org/app"))
	srv.wg.Wait()
	if code != http.StatusAccepted || body != `{"status":"deleting"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "ci-e2e-7-abcdef" {
		t.Errorf("deleted = %v", fake.deleted)
	}
}

func TestCompletedIgnoresAHcloudTierThisTenantCannotUse(t *testing.T) {
	srv, fake := completedEnv(t)
	cfg := hcloudFixture()
	cfg.Allowlist = []string{"juno/e2e"}
	srv.tiers["e2e"] = cfg
	code, body := postJob(t, srv, completedEvent("ci-e2e-7-abcdef", "alpha-org/app"))
	srv.wg.Wait()
	if code != http.StatusAccepted || body != `{"status":"ignored (not queued)"}` {
		t.Errorf("got (%d, %s)", code, body)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("deleted = %v", fake.deleted)
	}
}

func TestCompletedIgnoresEverythingElse(t *testing.T) {
	cases := []struct {
		name   string
		event  map[string]any
		reason string
	}{
		{"other tenant's repo", completedEvent("ci-e2e-7-abcdef", "juno/app"), "other tenant"},
		{"bad shape", completedEvent("ci-e2e-7-abc", "alpha-org/app"), "bad shape"},
		{"uppercase hex", completedEvent("ci-e2e-7-ABCDEF", "alpha-org/app"), "bad shape"},
		{"empty runner_name", completedEvent("", "alpha-org/app"), "empty"},
		{"unknown tier", completedEvent("ci-ghost-7-abcdef", "alpha-org/app"), "unknown tier"},
		{"Incus tier", completedEvent("ci-build-7-abcdef", "alpha-org/app"), "incus"},
		{"in_progress", map[string]any{
			"action": "in_progress",
			"workflow_job": map[string]any{
				"id": 1, "run_id": 2, "runner_name": "ci-e2e-7-abcdef",
			},
			"repository": map[string]any{"full_name": "alpha-org/app"},
		}, "other action"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, fake := completedEnv(t)
			code, body := postJob(t, srv, c.event)
			srv.wg.Wait()
			if code != http.StatusAccepted || body != `{"status":"ignored (not queued)"}` {
				t.Errorf("got (%d, %s), want the unchanged ignored reply", code, body)
			}
			if len(fake.deleted) != 0 {
				t.Errorf("%s: deleted %v", c.reason, fake.deleted)
			}
		})
	}
}
