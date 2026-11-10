package dispatcher

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// orgTestTable is an org-mode table: installation 5 is the org (no broker), 6 a
// tenant installation of another owner. ekd-akd/infra is a poke source.
func orgTestTable() Table {
	return Table{
		Installations: map[string]Installation{
			"5": {Owner: "ekd-akd", Tenant: nil},
			"6": {Owner: "other", Tenant: strPtr("other")},
		},
		Pokes: map[string][]PokeTarget{"ekd-akd/infra": {{"platform", "ingress"}}},
		Org: &OrgConfig{
			Owner: "ekd-akd", SpiegelRepo: "ekd-akd/spiegel", SpiegelWorkflow: "fakten.yml",
			SpiegelOrgWorkflow: "org.yml", SpiegelRef: "main", SpiegelInstallation: 77,
			SicherungDir: "/usr/local/lib/sicherung", SicherungWurzel: "/srv/sicherung",
			BefundDatei: "/var/lib/sicherung/befunde.jsonl", Schwellen: DefaultSchwellen(),
			SicherungTimeout: defaultSicherungTimeout, ResticTimeout: defaultResticTimeout,
		},
	}
}

// orgBody builds a payload for repo `name` in org ekd-akd; extra is a JSON object
// body fragment (without braces) merged in, e.g. `"action":"opened"`.
func orgBody(name, extra string) []byte {
	repo := fmt.Sprintf(`"repository":{"name":%q,"full_name":%q,"default_branch":"main"}`, name, "ekd-akd/"+name)
	if extra != "" {
		extra = "," + extra
	}
	return []byte(`{"installation":{"id":5},` + repo + extra + `}`)
}

func routeOrgFor(t *testing.T, event string, body []byte) Decision {
	t.Helper()
	p, err := ParsePayload(body)
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	return RouteOrg(event, body, p, orgTestTable())
}

var (
	jobsSE  = []OrgJob{OrgJobSicherung, OrgJobErkennung}
	jobsSEF = []OrgJob{OrgJobSicherung, OrgJobErkennung, OrgJobSpiegelFakten}
	jobsE   = []OrgJob{OrgJobErkennung}
	jobsEF  = []OrgJob{OrgJobErkennung, OrgJobSpiegelFakten}
	jobsEO  = []OrgJob{OrgJobErkennung, OrgJobSpiegelOrg}
	jobsF   = []OrgJob{OrgJobSpiegelFakten}
)

// The rows are the decision table of spec K1, one line each.
func TestRouteOrgDecisionTable(t *testing.T) {
	cases := []struct {
		name  string
		event string
		body  []byte
		want  []OrgJob // nil: the action is ActionIgnore
	}{
		// push
		{"push default branch", "push", orgBody("demo", `"ref":"refs/heads/main","deleted":false`), jobsSEF},
		{"push default branch forced", "push", orgBody("demo", `"ref":"refs/heads/main","forced":true,"deleted":false`), jobsSEF},
		{"push feature branch", "push", orgBody("demo", `"ref":"refs/heads/feature","deleted":false`), jobsSE},
		{"push deleting the default branch", "push", orgBody("demo", `"ref":"refs/heads/main","deleted":true`), jobsSE},
		{"push deleting a branch", "push", orgBody("demo", `"ref":"refs/heads/feature","deleted":true`), jobsSE},
		{"push new tag", "push", orgBody("demo", `"ref":"refs/tags/v1.0.0","deleted":false`), jobsSE},
		{"push v tag on a non-poke repo", "push", orgBody("demo", `"ref":"refs/tags/v2","deleted":false`), jobsSE},
		{"push deleted v tag on a poke repo", "push", orgBody("infra", `"ref":"refs/tags/v2","deleted":true`), jobsSE},
		{"push non-v tag on a poke repo", "push", orgBody("infra", `"ref":"refs/tags/x2","deleted":false`), jobsSE},
		{"push to a poke repo's default branch", "push", orgBody("infra", `"ref":"refs/heads/main","deleted":false`), jobsSEF},
		{"push without default_branch", "push",
			[]byte(`{"installation":{"id":5},"repository":{"name":"demo","full_name":"ekd-akd/demo"},"ref":"refs/heads/main"}`), jobsSE},
		{"push of a branch merely named like the default", "push", orgBody("demo", `"ref":"refs/heads/main2"`), jobsSE},
		// delete
		{"delete branch", "delete", orgBody("demo", `"ref":"x","ref_type":"branch"`), jobsSE},
		{"delete tag", "delete", orgBody("demo", `"ref":"v1","ref_type":"tag"`), jobsSE},
		// repository
		{"repository created", "repository", orgBody("demo", `"action":"created"`), jobsEF},
		{"repository archived", "repository", orgBody("demo", `"action":"archived"`), jobsEF},
		{"repository unarchived", "repository", orgBody("demo", `"action":"unarchived"`), jobsEF},
		{"repository renamed", "repository", orgBody("demo", `"action":"renamed"`), jobsEF},
		{"repository edited", "repository", orgBody("demo", `"action":"edited"`), jobsEF},
		{"repository privatized", "repository", orgBody("demo", `"action":"privatized"`), jobsEF},
		{"repository publicized", "repository", orgBody("demo", `"action":"publicized"`), jobsEF},
		{"repository transferred", "repository", orgBody("demo", `"action":"transferred"`), jobsEF},
		{"repository deleted", "repository", orgBody("demo", `"action":"deleted"`), jobsEO},
		{"repository unknown action", "repository", orgBody("demo", `"action":"frobnicated"`), jobsE},
		{"repository without action", "repository", orgBody("demo", ``), jobsE},
		{"repository action of the wrong type", "repository", orgBody("demo", `"action":7`), jobsE},
		// pull_request
		{"pull_request opened", "pull_request", orgBody("demo", `"action":"opened"`), jobsF},
		{"pull_request closed", "pull_request", orgBody("demo", `"action":"closed"`), jobsF},
		{"pull_request reopened", "pull_request", orgBody("demo", `"action":"reopened"`), jobsF},
		{"pull_request synchronize", "pull_request", orgBody("demo", `"action":"synchronize"`), jobsF},
		{"pull_request ready_for_review", "pull_request", orgBody("demo", `"action":"ready_for_review"`), jobsF},
		{"pull_request converted_to_draft", "pull_request", orgBody("demo", `"action":"converted_to_draft"`), jobsF},
		{"pull_request labeled", "pull_request", orgBody("demo", `"action":"labeled"`), nil},
		{"pull_request without action", "pull_request", orgBody("demo", ``), nil},
		// check_suite / workflow_run
		{"check_suite completed", "check_suite", orgBody("demo", `"action":"completed"`), jobsF},
		{"check_suite requested", "check_suite", orgBody("demo", `"action":"requested"`), nil},
		{"workflow_run completed", "workflow_run", orgBody("demo", `"action":"completed"`), jobsF},
		{"workflow_run requested", "workflow_run", orgBody("demo", `"action":"requested"`), nil},
		{"workflow_run in_progress", "workflow_run", orgBody("demo", `"action":"in_progress"`), nil},
		// release and the rest
		{"release published", "release", orgBody("demo", `"action":"published"`), jobsF},
		{"release deleted", "release", orgBody("demo", `"action":"deleted"`), jobsF},
		{"workflow_job has no broker here", "workflow_job", orgBody("demo", `"action":"queued"`), nil},
		{"star", "star", orgBody("demo", `"action":"created"`), nil},
		{"empty event", "", orgBody("demo", ``), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := routeOrgFor(t, tc.event, tc.body)
			if tc.want == nil {
				if dec.Action != ActionIgnore || dec.Reason == "" {
					t.Fatalf("decision = %+v, want ignore with a reason", dec)
				}
				return
			}
			if dec.Action != ActionOrg {
				t.Fatalf("action = %v (%s), want org", dec.Action, dec.Reason)
			}
			if !slices.Equal(dec.Org.Jobs, tc.want) {
				t.Errorf("jobs = %v, want %v", dec.Org.Jobs, tc.want)
			}
			if dec.Org.Repo == "" || dec.Org.BadName || dec.Org.Event != tc.event && tc.event != "" {
				t.Errorf("plan = %+v", dec.Org)
			}
			if string(dec.Org.Body) != string(tc.body) {
				t.Errorf("plan lost the verbatim body")
			}
		})
	}
}

func TestRouteOrgPingAndGatesAreTheSharedOnes(t *testing.T) {
	if dec := routeOrgFor(t, "ping", []byte(`{"zen":"x"}`)); dec.Action != ActionPong {
		t.Errorf("ping = %+v", dec)
	}
	unknown := []byte(`{"installation":{"id":999},"repository":{"name":"demo","full_name":"ekd-akd/demo"}}`)
	if dec := routeOrgFor(t, "push", unknown); dec.Action != ActionReject {
		t.Errorf("unknown installation = %+v, want reject", dec)
	}
	foreign := []byte(`{"installation":{"id":5},"repository":{"name":"demo","full_name":"other/demo"},"ref":"refs/heads/main"}`)
	if dec := routeOrgFor(t, "push", foreign); dec.Action != ActionReject {
		t.Errorf("foreign owner = %+v, want reject", dec)
	}
}

func TestRouteOrgNewVTagOnAPokeRepoPokesAndNothingElse(t *testing.T) {
	dec := routeOrgFor(t, "push", orgBody("infra", `"ref":"refs/tags/v1.2.0","deleted":false`))
	if dec.Action != ActionPoke {
		t.Fatalf("decision = %+v, want poke (takes precedence over backup)", dec)
	}
	sameTargets(t, dec.Targets, []PokeTarget{{"platform", "ingress"}})
	if len(dec.Org.Jobs) != 0 {
		t.Errorf("a poke decision must carry no org jobs: %v", dec.Org.Jobs)
	}
}

func TestRouteOrgPokeTargetsAreCloned(t *testing.T) {
	tbl := orgTestTable()
	body := orgBody("infra", `"ref":"refs/tags/v1.2.0"`)
	p, _ := ParsePayload(body)
	dec := RouteOrg("push", body, p, tbl)
	dec.Targets[0].VM = "mutated"
	if tbl.Pokes["ekd-akd/infra"][0].VM != "ingress" {
		t.Error("the decision aliases the table")
	}
}

// Loop protection: the facts generator commits into the mirror, and the mirror's
// state hash would otherwise produce new runs forever.
func TestRouteOrgLoopProtectionForTheMirrorRepo(t *testing.T) {
	cases := []struct {
		name  string
		event string
		extra string
		want  []OrgJob // nil: ignore
	}{
		{"push default branch", "push", `"ref":"refs/heads/main","deleted":false`, jobsSE},
		{"push tag", "push", `"ref":"refs/tags/v1","deleted":false`, jobsSE},
		{"forced push", "push", `"ref":"refs/heads/main","forced":true`, jobsSE},
		{"delete", "delete", `"ref":"x","ref_type":"branch"`, jobsSE},
		{"pull_request opened", "pull_request", `"action":"opened"`, nil},
		{"check_suite completed", "check_suite", `"action":"completed"`, nil},
		{"workflow_run completed", "workflow_run", `"action":"completed","workflow_run":{"name":"fakten"}`, nil},
		{"workflow_run completed of org.yml", "workflow_run", `"action":"completed","workflow_run":{"name":"org"}`, nil},
		{"release published", "release", `"action":"published"`, nil},
		{"repository edited", "repository", `"action":"edited"`, jobsE},
		{"repository renamed", "repository", `"action":"renamed"`, jobsE},
		{"repository deleted does not start org.yml", "repository", `"action":"deleted"`, jobsE},
	}
	for _, name := range []string{"spiegel", "Spiegel", "SPIEGEL"} {
		for _, tc := range cases {
			t.Run(name+" "+tc.name, func(t *testing.T) {
				body := orgBody("spiegel", tc.extra)
				body = []byte(strings.Replace(string(body), `"full_name":"ekd-akd/spiegel"`, `"full_name":"EKD-AKD/`+name+`"`, 1))
				dec := routeOrgFor(t, tc.event, body)
				if tc.want == nil {
					if dec.Action != ActionIgnore {
						t.Fatalf("decision = %+v, want ignore", dec)
					}
					return
				}
				if dec.Action != ActionOrg || !slices.Equal(dec.Org.Jobs, tc.want) {
					t.Errorf("decision = %+v, want jobs %v", dec, tc.want)
				}
				for _, j := range dec.Org.Jobs {
					if j == OrgJobSpiegelFakten || j == OrgJobSpiegelOrg {
						t.Errorf("the mirror's own event started %v", j)
					}
				}
			})
		}
	}
}

func TestRouteOrgRepoNameValidation(t *testing.T) {
	long := strings.Repeat("a", 101)
	bad := []struct{ name, repo string }{
		{"dash first", "-rf"},
		{"space", "a b"},
		{"slash", "a/b"},
		{"semicolon", "a;b"},
		{"newline", "a\nb"},
		{"too long", long},
		{"unicode", "démo"},
	}
	for _, tc := range bad {
		for _, event := range []string{"push", "pull_request", "delete", "repository", "release", "check_suite"} {
			t.Run(tc.name+" "+event, func(t *testing.T) {
				action := "completed"
				if event == "pull_request" {
					action = "opened"
				}
				extra := `"action":"` + action + `","ref":"refs/heads/main"`
				body := []byte(fmt.Sprintf(`{"installation":{"id":5},"repository":{"name":%q,"full_name":"ekd-akd/x","default_branch":"main"},%s}`, tc.repo, extra))
				dec := routeOrgFor(t, event, body)
				if dec.Action != ActionOrg {
					t.Fatalf("decision = %+v, want an org plan that records the finding", dec)
				}
				if !dec.Org.BadName || dec.Org.Repo != "" || !slices.Equal(dec.Org.Jobs, jobsE) {
					t.Errorf("plan = %+v, want BadName, no repo, only the detection job", dec.Org)
				}
			})
		}
	}
	// Exactly 100 characters is still fine.
	ok := routeOrgFor(t, "pull_request", orgBody(strings.Repeat("a", 100), `"action":"opened"`))
	if ok.Action != ActionOrg || ok.Org.BadName {
		t.Errorf("100-char name = %+v", ok)
	}
}

func TestRouteOrgMissingRepositoryName(t *testing.T) {
	noRepo := []byte(`{"installation":{"id":5},"ref":"refs/heads/main","action":"opened"}`)
	// push/delete/repository: the detection reports the broken payload itself.
	for _, event := range []string{"push", "delete", "repository"} {
		dec := routeOrgFor(t, event, noRepo)
		if dec.Action != ActionOrg || dec.Org.BadName || dec.Org.Repo != "" || !slices.Equal(dec.Org.Jobs, jobsE) {
			t.Errorf("%s without repository: %+v", event, dec)
		}
	}
	// Everything else needs a name to act on: record it as a finding.
	for _, event := range []string{"pull_request", "release"} {
		dec := routeOrgFor(t, event, noRepo)
		if dec.Action != ActionOrg || !dec.Org.BadName || !slices.Equal(dec.Org.Jobs, jobsE) {
			t.Errorf("%s without repository: %+v", event, dec)
		}
	}
	// Events that would be ignored anyway stay ignored.
	if dec := routeOrgFor(t, "star", noRepo); dec.Action != ActionIgnore {
		t.Errorf("star: %+v", dec)
	}
}

func TestRouteOrgMalformedFieldsNeverPanic(t *testing.T) {
	bodies := []string{
		`{"installation":{"id":5},"repository":null}`,
		`{"installation":{"id":5},"repository":"x"}`,
		`{"installation":{"id":5},"repository":{"name":5}}`,
		`{"installation":{"id":5},"repository":{"name":"d","default_branch":7},"ref":"refs/heads/main"}`,
		`{"installation":{"id":5},"repository":{"name":"d"},"action":{"x":1}}`,
	}
	for _, event := range []string{"push", "delete", "repository", "pull_request", "check_suite", "workflow_run", "release"} {
		for _, b := range bodies {
			p, err := ParsePayload([]byte(b))
			if err != nil {
				continue // the HMAC-verified body must be valid for Route's own Payload
			}
			_ = RouteOrg(event, []byte(b), p, orgTestTable())
		}
	}
}

func TestOrgJobString(t *testing.T) {
	for j := OrgJob(1); j < orgJobCount; j++ {
		if s := j.String(); s == "" || strings.HasPrefix(s, "invalid") {
			t.Errorf("job %d has no name: %q", int(j), s)
		}
	}
	for _, j := range []OrgJob{orgJobUnset, orgJobCount, OrgJob(-1)} {
		if !strings.HasPrefix(j.String(), "invalid org job") {
			t.Errorf("job %d: %q", int(j), j.String())
		}
	}
}
