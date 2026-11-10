package dispatcher

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// newOrgHarness is the live Server of newHarness switched into org mode over the
// fakes of orgRig: no process starts, no request leaves the machine.
func newOrgHarness(t *testing.T, tweak ...func(*Server, *orgRig)) (*harness, *orgRig) {
	t.Helper()
	rig := newOrgRig(t)
	h := newHarness(t, func(s *Server) {
		s.Table = rig.table
		s.Org = rig.runner
		for _, f := range tweak {
			f(s, rig)
		}
	})
	return h, rig
}

func TestOrgServerAnswers202BeforeTheJobsFinishAndRunsThemInTheBackground(t *testing.T) {
	gate := make(chan struct{})
	h, rig := newOrgHarness(t, func(_ *Server, r *orgRig) { r.out.gate = gate })
	start := time.Now()
	code, msg := h.post("push", string(orgBody("demo", `"ref":"refs/heads/main","before":"`+strings.Repeat("1", 40)+
		`","after":"`+strings.Repeat("2", 40)+`","forced":false,"deleted":false,"commits":[]`)))
	if code != 202 || msg != "org: 3 job(s)" {
		t.Fatalf("got %d %q, want 202 org: 3 job(s)", code, msg)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the reply took %v: it must not wait for the jobs", time.Since(start))
	}
	waitFor(t, "the backup to start", func() bool { return len(rig.events.all()) == 1 })
	if got := rig.events.all(); len(got) != 1 {
		t.Fatalf("events while the backup is held = %v", got)
	}
	close(gate)
	h.srv.Wait()
	if got := rig.events.all(); !slices.Equal(got, []string{"sichern.sh demo", "dispatch fakten.yml demo"}) {
		t.Errorf("events = %v", got)
	}
}

func TestOrgServerJobsRanBeforeCloseReturns(t *testing.T) {
	h, rig := newOrgHarness(t)
	if code, _ := h.post("pull_request", string(orgBody("demo", `"action":"opened"`))); code != 202 {
		t.Fatalf("code = %d", code)
	}
	h.srv.Wait()
	h.srv.Close()
	if got := rig.events.all(); !slices.Equal(got, []string{"dispatch fakten.yml demo"}) {
		t.Errorf("events = %v", got)
	}
}

func TestOrgServerBadSignatureRunsNothing(t *testing.T) {
	h, rig := newOrgHarness(t)
	body := orgBody("demo", `"action":"opened"`)
	hdr := signedHeaders(t, "pull_request", body)
	hdr["X-Hub-Signature-256"] = sign(t, "another-secret-another-secret", body)
	code, _, _ := h.do("POST", "/github", strings.NewReader(string(body)), hdr)
	h.srv.Wait()
	if code != 401 || len(rig.events.all()) != 0 {
		t.Errorf("code = %d, events = %v", code, rig.events.all())
	}
}

func TestOrgServerAfterCloseIs503AndRunsNothing(t *testing.T) {
	h, rig := newOrgHarness(t)
	h.srv.Close()
	code, msg := h.post("pull_request", string(orgBody("demo", `"action":"opened"`)))
	if code != 503 || msg != "shutting down" {
		t.Errorf("got %d %q, want 503 shutting down so GitHub can redeliver", code, msg)
	}
	if len(rig.events.all()) != 0 {
		t.Errorf("events = %v", rig.events.all())
	}
}

func TestOrgServerLoopProtectionOverHTTP(t *testing.T) {
	h, rig := newOrgHarness(t)
	cases := []struct{ event, extra string }{
		{"workflow_run", `"action":"completed","workflow_run":{"name":"fakten"}`},
		{"check_suite", `"action":"completed"`},
		{"pull_request", `"action":"opened"`},
		{"release", `"action":"published"`},
	}
	for _, tc := range cases {
		code, msg := h.post(tc.event, string(orgBody("spiegel", tc.extra)))
		if code != 202 || !strings.HasPrefix(msg, "ignored") || !strings.Contains(msg, "loop protection") {
			t.Errorf("%s: got %d %q, want an ignore naming the loop protection", tc.event, code, msg)
		}
	}
	// A push to the mirror's default branch is backed up and rated, never mirrored.
	if code, _ := h.post("push", string(orgBody("spiegel", `"ref":"refs/heads/main","deleted":false,"commits":[],"forced":false`))); code != 202 {
		t.Fatalf("push code = %d", code)
	}
	h.srv.Wait()
	for _, e := range rig.events.all() {
		if strings.HasPrefix(e, "dispatch") {
			t.Errorf("the mirror's own events dispatched: %s", e)
		}
	}
	if got := rig.events.all(); !slices.Equal(got, []string{"sichern.sh spiegel"}) {
		t.Errorf("events = %v", got)
	}
}

func TestOrgServerKeepsGatesAndPokePrecedence(t *testing.T) {
	h, rig := newOrgHarness(t)
	if code, msg := h.post("ping", `{"zen":"x"}`); code != 200 || msg != "pong" {
		t.Errorf("ping = %d %q", code, msg)
	}
	if code, _ := h.post("push", `{"installation":{"id":999},"repository":{"name":"demo","full_name":"ekd-akd/demo"}}`); code != 403 {
		t.Errorf("unknown installation = %d, want 403", code)
	}
	if code, _ := h.post("push", `{"installation":{"id":5},"repository":{"name":"demo","full_name":"other/demo"},"ref":"refs/heads/main"}`); code != 403 {
		t.Errorf("foreign owner = %d, want 403", code)
	}
	code, msg := h.post("workflow_job", string(orgBody("demo", `"action":"queued"`)))
	if code != 202 || !strings.Contains(msg, "no broker") {
		t.Errorf("workflow_job = %d %q", code, msg)
	}
	code, msg = h.post("push", string(orgBody("infra", `"ref":"refs/tags/v1.2.0","deleted":false`)))
	if code != 202 || msg != "poking 1 vm(s)" {
		t.Errorf("v tag on a poke repo = %d %q", code, msg)
	}
	h.srv.Wait()
	if got := h.runner.argvs(); len(got) != 1 || got[0][0] != "incus" {
		t.Errorf("pokes = %v", got)
	}
	if len(rig.events.all()) != 0 {
		t.Errorf("a poke must not start org jobs: %v", rig.events.all())
	}
}

func TestOrgServerPlansForOneRepoRunOneAfterAnother(t *testing.T) {
	gate := make(chan struct{})
	h, rig := newOrgHarness(t, func(_ *Server, r *orgRig) { r.out.gate = gate })
	push := string(orgBody("demo", `"ref":"refs/heads/feature","deleted":false,"commits":[],"forced":false`))
	for range 2 {
		if code, _ := h.post("push", push); code != 202 {
			t.Fatalf("code = %d", code)
		}
	}
	waitFor(t, "the first backup", func() bool { return len(rig.events.all()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := rig.events.all(); len(got) != 1 {
		t.Errorf("events = %v: the second plan for the same repo started early", got)
	}
	close(gate)
	h.srv.Wait()
	if got := rig.events.all(); !slices.Equal(got, []string{"sichern.sh demo", "sichern.sh demo"}) {
		t.Errorf("events = %v", got)
	}
}

func TestOrgServerLimitsParallelPlans(t *testing.T) {
	gate := make(chan struct{})
	h, rig := newOrgHarness(t, func(s *Server, r *orgRig) { s.OrgParallel = 2; r.out.gate = gate })
	for _, repo := range []string{"a", "b", "c"} {
		body := string(orgBody(repo, `"ref":"refs/heads/feature","deleted":false,"commits":[],"forced":false`))
		if code, _ := h.post("push", body); code != 202 {
			t.Fatalf("code = %d", code)
		}
	}
	waitFor(t, "two backups", func() bool { return len(rig.events.all()) >= 2 })
	time.Sleep(100 * time.Millisecond)
	if got := len(rig.events.all()); got != 2 {
		t.Errorf("%d plans started with a limit of 2", got)
	}
	close(gate)
	h.srv.Wait()
	if got := len(rig.events.all()); got != 3 {
		t.Errorf("%d plans ran in the end, want 3", got)
	}
}

func TestOrgServerCloseCancelsRunningAndRecordsDroppedPlans(t *testing.T) {
	gate := make(chan struct{}) // never closed: only the context can free the first job
	h, rig := newOrgHarness(t, func(_ *Server, r *orgRig) { r.out.gate = gate })
	push := string(orgBody("demo", `"ref":"refs/heads/feature","deleted":false,"commits":[],"forced":false`))
	for range 2 {
		if code, _ := h.post("push", push); code != 202 {
			t.Fatalf("code = %d", code)
		}
	}
	waitFor(t, "the first backup", func() bool { return len(rig.events.all()) >= 1 })
	done := make(chan struct{})
	go func() { h.srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the held job")
	}
	var grunds []string
	for _, b := range rig.befunde() {
		grunds = append(grunds, b.Grund)
	}
	joined := strings.Join(grunds, " | ")
	if !strings.Contains(joined, "Sicherung fehlgeschlagen") || !strings.Contains(joined, "nicht verarbeitet") {
		t.Errorf("befunde = %q: both the killed backup and the dropped plan must be on record", joined)
	}
}

func TestOrgPlanWithoutARunnerFailsClosedWith500(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.srv.respond(t.Context(), rec, delivery{id: "d", event: "push"}, Decision{Action: ActionOrg, Org: OrgPlan{Repo: "demo", Jobs: jobsSE}})
	if rec.Code != 500 || !strings.Contains(h.logs.String(), "level=ERROR") {
		t.Errorf("status = %d, logs:\n%s", rec.Code, h.logs.String())
	}
}

func TestOrgModeIsOffWithoutAnOrgSection(t *testing.T) {
	// The bartixcloud default: the same delivery that org mode would back up is
	// ignored as "not a new v* tag", exactly as before.
	h := newHarness(t)
	code, msg := h.post("push", `{"installation":{"id":102},"repository":{"full_name":"alpha-org/app"},"ref":"refs/heads/main"}`)
	if code != 202 || !strings.Contains(msg, "not a new v* tag") {
		t.Errorf("got %d %q", code, msg)
	}
}
