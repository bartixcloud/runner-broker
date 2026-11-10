package dispatcher

import (
	"slices"
	"strings"
	"testing"
)

// testTable mirrors the generator's dispatcher.json shape (contracts C5/G4).
func testTable() Table {
	return Table{
		Installations: map[string]Installation{
			"100": {Owner: "bartixcloud", Tenant: nil},
			"102": {Owner: "alpha-org", Tenant: strPtr("alpha-org")},
			"103": {Owner: "Juno", Tenant: strPtr("juno")},
		},
		Pokes: map[string][]PokeTarget{
			"bartixcloud/infra": {{"platform", "ingress"}, {"alpha-org", "alpha-org-app"}},
			"alpha-org/infra":   {{"platform", "ingress"}, {"platform", "bartix-db"}, {"alpha-org", "alpha-org-app"}},
			"juno/infra":        {{"platform", "ingress"}, {"platform", "bartix-db"}},
		},
	}
}

// payloadOf builds a payload the way GitHub's push/workflow_job events carry it.
func payloadOf(inst, repo string) Payload {
	return Payload{
		Installation: InstallationRef{ID: InstallationID(inst)},
		Repository:   RepositoryRef{FullName: repo},
	}
}

func pushOf(inst, repo, ref string, deleted bool) Payload {
	p := payloadOf(inst, repo)
	p.Ref, p.Deleted = ref, deleted
	return p
}

func sameTargets(t *testing.T, got, want []PokeTarget) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- Action ----

func TestActionZeroValueIsNotAValidAction(t *testing.T) {
	var zero Action
	if zero != actionUnset {
		t.Fatalf("zero Action = %d, want actionUnset", zero)
	}
	for a := Action(1); a < actionCount; a++ {
		if a == actionUnset {
			t.Errorf("action %d collides with actionUnset", a)
		}
	}
}

func TestActionString(t *testing.T) {
	want := map[Action]string{
		ActionPong: "pong", ActionReject: "reject", ActionIgnore: "ignore",
		ActionPoke: "poke", ActionForward: "forward", ActionPin: "pin", ActionOrg: "org",
	}
	for a := Action(1); a < actionCount; a++ {
		name, ok := want[a]
		if !ok {
			t.Fatalf("action %d has no expected name: extend this test", a)
		}
		if got := a.String(); got != name {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, name)
		}
	}
	if got := actionUnset.String(); !strings.Contains(got, "invalid") {
		t.Errorf("actionUnset.String() = %q, want it to say invalid", got)
	}
	if got := actionCount.String(); !strings.Contains(got, "invalid") {
		t.Errorf("actionCount.String() = %q, want it to say invalid", got)
	}
}

// ---------------------------------------------------------- ParsePayload ----

func TestParsePayloadReadsTheRoutingFields(t *testing.T) {
	body := `{"action":"queued","installation":{"id":102,"node_id":"x"},
	  "repository":{"full_name":"alpha-org/infra","private":true},
	  "ref":"refs/tags/v1.2.0","deleted":true,"unrelated":{"deep":[1,2,3]}}`
	p, err := ParsePayload([]byte(body))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	want := Payload{
		Installation: InstallationRef{ID: "102"},
		Repository:   RepositoryRef{FullName: "alpha-org/infra"},
		Ref:          "refs/tags/v1.2.0",
		Deleted:      true,
	}
	if p != want {
		t.Errorf("payload = %+v, want %+v", p, want)
	}
}

func TestParsePayloadInstallationIDNumberOrString(t *testing.T) {
	cases := map[string]string{
		`{"installation":{"id":102}}`:                  "102",
		`{"installation":{"id":"102"}}`:                "102",
		`{"installation":{"id":12345678901234567890}}`: "12345678901234567890", // no float rounding
		`{"installation":{"id":102.0}}`:                "102.0",                // never equals "102": fails closed
		`{"installation":{"id":null}}`:                 "",
		`{"installation":{"id":""}}`:                   "",
		`{"installation":{}}`:                          "",
		`{"installation":null}`:                        "",
		`{}`:                                           "",
	}
	for body, want := range cases {
		p, err := ParsePayload([]byte(body))
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if p.Installation.ID != InstallationID(want) {
			t.Errorf("%s: id = %q, want %q", body, p.Installation.ID, want)
		}
	}
}

func TestParsePayloadRejectsWhatIsNotAnObject(t *testing.T) {
	bad := []string{
		``, `   `, `null`, `[]`, `[{"installation":{"id":1}}]`, `"x"`, `1`, `true`,
		`not json`, `{"installation":`, `{} trailing`, `{}{}`,
		`{"installation":"102"}`,         // installation must be an object
		`{"installation":[]}`,            // ...not an array
		`{"installation":{"id":true}}`,   // id is a number or a string
		`{"installation":{"id":{}}}`,     // ...never a container
		`{"installation":{"id":[102]}}`,  // ...
		`{"repository":"alpha-org/app"}`, // repository must be an object
		`{"repository":{"full_name":7}}`,
		`{"ref":7}`,
		`{"deleted":"yes"}`,
	}
	for _, body := range bad {
		if _, err := ParsePayload([]byte(body)); err == nil {
			t.Errorf("%q: expected an error", body)
		}
	}
}

func TestParsePayloadAcceptsNullForOptionalFields(t *testing.T) {
	p, err := ParsePayload([]byte(`{"repository":null,"ref":null,"deleted":null}`))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	if p != (Payload{}) {
		t.Errorf("payload = %+v, want zero", p)
	}
}

// ----------------------------------------------------------------- Route ----

func TestRoutePingIsPong(t *testing.T) {
	d := Route("ping", Payload{}, testTable())
	if d.Action != ActionPong {
		t.Fatalf("action = %v, want pong", d.Action)
	}
}

func TestRoutePingComesBeforeTheInstallationLookup(t *testing.T) {
	// A signed ping for an unknown installation is still a pong: GitHub's "Test
	// delivery" must succeed before the table knows the installation.
	if d := Route("ping", payloadOf("999", "x/y"), testTable()); d.Action != ActionPong {
		t.Fatalf("action = %v, want pong", d.Action)
	}
}

func TestRouteRejectsUnknownOrMissingInstallation(t *testing.T) {
	d := Route("workflow_job", payloadOf("999", ""), testTable())
	if d.Action != ActionReject || !strings.Contains(d.Reason, "999") {
		t.Errorf("decision = %+v, want reject naming 999", d)
	}
	d = Route("workflow_job", Payload{}, testTable())
	if d.Action != ActionReject || !strings.Contains(d.Reason, "unknown installation -") {
		t.Errorf("decision = %+v, want reject of installation -", d)
	}
}

func TestRouteForwardsWorkflowJobToItsTenant(t *testing.T) {
	d := Route("workflow_job", payloadOf("102", "alpha-org/app"), testTable())
	if d.Action != ActionForward || d.Tenant != "alpha-org" {
		t.Errorf("decision = %+v, want forward to alpha-org", d)
	}
}

func TestRouteForwardsAnyNonPushEventOfATenantInstallation(t *testing.T) {
	for _, event := range []string{"workflow_job", "workflow_run", "check_run", ""} {
		d := Route(event, payloadOf("102", "alpha-org/app"), testTable())
		if d.Action != ActionForward || d.Tenant != "alpha-org" {
			t.Errorf("event %q: decision = %+v, want forward to alpha-org", event, d)
		}
	}
}

func TestRouteForwardsWhenThePayloadHasNoRepository(t *testing.T) {
	d := Route("workflow_job", payloadOf("102", ""), testTable())
	if d.Action != ActionForward || d.Tenant != "alpha-org" {
		t.Errorf("decision = %+v, want forward to alpha-org", d)
	}
}

func TestRouteRejectsARepoOfAnotherOwner(t *testing.T) {
	d := Route("workflow_job", payloadOf("102", "kappa-org/infra"), testTable())
	if d.Action != ActionReject {
		t.Fatalf("action = %v, want reject", d.Action)
	}
	for _, want := range []string{"kappa-org/infra", "102", "alpha-org"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason %q does not mention %q", d.Reason, want)
		}
	}
}

func TestRouteOwnerCheckPrecedesPushHandling(t *testing.T) {
	// A v* tag on a foreign owner's infra repo must not poke through this installation.
	p := pushOf("102", "kappa-org/infra", "refs/tags/v1", false)
	if d := Route("push", p, testTable()); d.Action != ActionReject {
		t.Errorf("action = %v, want reject", d.Action)
	}
}

func TestRouteOwnerMatchIsCaseInsensitiveBothWays(t *testing.T) {
	d := Route("workflow_job", payloadOf("103", "juno/app"), testTable())
	if d.Action != ActionForward || d.Tenant != "juno" {
		t.Errorf("decision = %+v, want forward to juno", d)
	}
	d = Route("workflow_job", payloadOf("102", "Alpha-Org/app"), testTable())
	if d.Action != ActionForward || d.Tenant != "alpha-org" {
		t.Errorf("decision = %+v, want forward to alpha-org", d)
	}
}

func TestRouteOwnerIsTheSegmentBeforeTheFirstSlash(t *testing.T) {
	// "alpha-org-evil/x" must not pass for owner "alpha-org": no prefix matching.
	if d := Route("workflow_job", payloadOf("102", "alpha-org-evil/x"), testTable()); d.Action != ActionReject {
		t.Errorf("action = %v, want reject", d.Action)
	}
	if d := Route("workflow_job", payloadOf("102", "alpha-org"), testTable()); d.Action != ActionForward {
		t.Errorf("owner-only full_name: action = %v, want forward (no slash, same owner)", d.Action)
	}
}

func TestRouteIgnoresWorkflowJobOfThePlatformInstallation(t *testing.T) {
	d := Route("workflow_job", payloadOf("100", "bartixcloud/nuk"), testTable())
	if d.Action != ActionIgnore || !strings.Contains(d.Reason, "platform installation") {
		t.Errorf("decision = %+v, want ignore (platform installation)", d)
	}
}

func TestRoutePokesOnAVTagOfAnInfraRepo(t *testing.T) {
	d := Route("push", pushOf("102", "alpha-org/infra", "refs/tags/v1.2.0", false), testTable())
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke (reason %q)", d.Action, d.Reason)
	}
	sameTargets(t, d.Targets, []PokeTarget{{"platform", "ingress"}, {"platform", "bartix-db"}, {"alpha-org", "alpha-org-app"}})
}

func TestRoutePokeIsCaseInsensitive(t *testing.T) {
	d := Route("push", pushOf("103", "Juno/infra", "refs/tags/v1.2.0", false), testTable())
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke (reason %q)", d.Action, d.Reason)
	}
	sameTargets(t, d.Targets, []PokeTarget{{"platform", "ingress"}, {"platform", "bartix-db"}})
}

func TestRoutePokeComesBeforeThePlatformNoBrokerCheck(t *testing.T) {
	// The platform installation has no broker, yet bartixcloud/infra must still poke.
	d := Route("push", pushOf("100", "bartixcloud/infra", "refs/tags/v2", false), testTable())
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke (reason %q)", d.Action, d.Reason)
	}
	sameTargets(t, d.Targets, []PokeTarget{{"platform", "ingress"}, {"alpha-org", "alpha-org-app"}})
}

func TestRouteIgnoresEverythingElseOnPush(t *testing.T) {
	cases := map[string]Payload{
		"branch push":         pushOf("102", "alpha-org/infra", "refs/heads/main", false),
		"deleted tag":         pushOf("102", "alpha-org/infra", "refs/tags/v1.2.0", true),
		"non-v tag":           pushOf("102", "alpha-org/infra", "refs/tags/release-1", false),
		"not a poke source":   pushOf("100", "bartixcloud/nuk", "refs/tags/v1.2.0", false),
		"uppercase V tag":     pushOf("102", "alpha-org/infra", "refs/tags/V1.2.0", false),
		"v inside the tag":    pushOf("102", "alpha-org/infra", "refs/tags/rcv1", false),
		"empty ref":           pushOf("102", "alpha-org/infra", "", false),
		"no repository":       pushOf("102", "", "refs/tags/v1", false),
		"platform other repo": pushOf("100", "bartixcloud/nuk", "refs/tags/v1", false),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			d := Route("push", payload, testTable())
			if d.Action != ActionIgnore {
				t.Errorf("decision = %+v, want ignore", d)
			}
			if d.Reason == "" {
				t.Error("an ignore must say why")
			}
		})
	}
}

func TestRoutePushWithAnUnknownInstallationIsRejected(t *testing.T) {
	d := Route("push", pushOf("999", "alpha-org/infra", "refs/tags/v1", false), testTable())
	if d.Action != ActionReject {
		t.Errorf("action = %v, want reject", d.Action)
	}
}

func TestRoutePokeCarriesTheReportJob(t *testing.T) {
	push := pushOf("103", "Juno/infra", "refs/tags/v1.2.0", false)
	push.HeadCommit = HeadCommitRef{ID: testCommit}
	d := Route("push", push, testTable())
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke", d.Action)
	}
	want := Report{Repo: "juno/infra", Commit: testCommit, Source: "juno", Installation: 103}
	if d.Report == nil || *d.Report != want {
		t.Errorf("report = %+v, want %+v", d.Report, want)
	}

	// The platform installation has no tenant, so its reports name bartixcloud.
	push = pushOf("100", "bartixcloud/infra", "refs/tags/v2", false)
	push.HeadCommit = HeadCommitRef{ID: otherCommit}
	d = Route("push", push, testTable())
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke", d.Action)
	}
	want = Report{Repo: "bartixcloud/infra", Commit: otherCommit, Source: "bartixcloud", Installation: 100}
	if d.Report == nil || *d.Report != want {
		t.Errorf("report = %+v, want %+v", d.Report, want)
	}
}

func TestRoutePokeWithoutACommitStillPokesButDoesNotReport(t *testing.T) {
	push := func(commit string) Payload {
		p := pushOf("102", "alpha-org/infra", "refs/tags/v1", false)
		p.HeadCommit = HeadCommitRef{ID: commit}
		return p
	}
	cases := map[string]Payload{
		"no head commit":   push(""),
		"short commit":     push(testCommit[:39]),
		"long commit":      push(testCommit + "0"),
		"non-hex commit":   push(strings.Repeat("g", 40)),
		"uppercase commit": push(strings.ToUpper(testCommit)),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			d := Route("push", payload, testTable())
			if d.Action != ActionPoke {
				t.Fatalf("action = %v, want poke (reason %q)", d.Action, d.Reason)
			}
			if len(d.Targets) == 0 {
				t.Error("a poke without a report must still carry its targets")
			}
			if d.Report != nil {
				t.Errorf("report = %+v, want nil", d.Report)
			}
		})
	}
}

func TestRoutePokeWithANonIntegerInstallationStillPokesButDoesNotReport(t *testing.T) {
	table := testTable()
	// A key that is not an integer routes, but no token can be minted for it.
	table.Installations["102.0"] = Installation{Owner: "alpha-org", Tenant: strPtr("alpha-org")}
	push := pushOf("102.0", "alpha-org/infra", "refs/tags/v1", false)
	push.HeadCommit = HeadCommitRef{ID: testCommit}

	d := Route("push", push, table)
	if d.Action != ActionPoke {
		t.Fatalf("action = %v, want poke (reason %q)", d.Action, d.Reason)
	}
	if d.Report != nil {
		t.Errorf("report = %+v, want nil", d.Report)
	}
}

func TestRoutePinsOnAVTagOfAnAppRepo(t *testing.T) {
	d := Route("push", pushOf("102", "alpha-org/app", "refs/tags/v1.2.0", false), testTable())
	if d.Action != ActionPin {
		t.Fatalf("action = %v, want pin (reason %q)", d.Action, d.Reason)
	}
	want := PinJob{Owner: "alpha-org", InfraRepo: "infra", SourceRepo: "app", Tag: "v1.2.0", Installation: 102}
	if d.PinJob != want {
		t.Errorf("pin job = %+v, want %+v", d.PinJob, want)
	}
}

func TestRoutePinIgnoresPreReleaseTag(t *testing.T) {
	d := Route("push", pushOf("102", "alpha-org/app", "refs/tags/v1.2.0-rc1", false), testTable())
	if d.Action == ActionPin {
		t.Fatalf("pre-release tag pinned: %+v", d.PinJob)
	}
}

func TestRoutePinIgnoresPlatformInstallation(t *testing.T) {
	d := Route("push", pushOf("100", "bartixcloud/app", "refs/tags/v1.2.0", false), testTable())
	if d.Action == ActionPin {
		t.Fatalf("platform installation pinned: %+v", d.PinJob)
	}
}

func TestRoutePinIgnoresAppRepoWithoutInfraPokeEntry(t *testing.T) {
	table := testTable()
	delete(table.Pokes, "alpha-org/infra")
	d := Route("push", pushOf("102", "alpha-org/app", "refs/tags/v1.2.0", false), table)
	if d.Action == ActionPin {
		t.Fatalf("pinned without an infra poke entry: %+v", d.PinJob)
	}
}

func TestRouteDoesNotMutateItsTable(t *testing.T) {
	table := testTable()
	d := Route("push", pushOf("102", "alpha-org/infra", "refs/tags/v1", false), table)
	if len(d.Targets) == 0 {
		t.Fatal("expected targets")
	}
	d.Targets[0] = PokeTarget{"evil", "evil"}
	if got := table.Pokes["alpha-org/infra"][0]; got != (PokeTarget{"platform", "ingress"}) {
		t.Errorf("mutating the decision changed the table: %v", got)
	}
}
