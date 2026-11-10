package dispatcher

import (
	"strings"
	"testing"
	"time"
)

const orgMinimal = `{"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77}`

func orgTableText(org string) string {
	return `{"installations":{"5":{"owner":"ekd-akd","tenant":null}},"pokes":{},"org":` + org + `}`
}

func TestOrgSectionAbsentOrNullMeansOrgModeOff(t *testing.T) {
	for _, text := range []string{
		`{"installations":{},"pokes":{}}`,
		`{"installations":{},"pokes":{},"org":null}`,
	} {
		tbl, err := loadTableText(t, text)
		if err != nil || tbl.Org != nil {
			t.Errorf("%s: org = %+v, err = %v, want nil and no error", text, tbl.Org, err)
		}
	}
}

func TestOrgMinimalSectionGetsTheDocumentedDefaults(t *testing.T) {
	tbl, err := loadTableText(t, orgTableText(orgMinimal))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	want := OrgConfig{
		Owner: "ekd-akd", SpiegelRepo: "ekd-akd/spiegel", SpiegelWorkflow: "fakten.yml",
		SpiegelOrgWorkflow: "org.yml", SpiegelRef: "main", SpiegelInstallation: 77,
		SicherungDir: "/usr/local/lib/sicherung", SicherungWurzel: "/srv/sicherung",
		BefundDatei:      "/var/lib/sicherung/befunde.jsonl",
		Schwellen:        Schwellen{GeloeschteDateien: 50, GeloeschteZeilen: 5000},
		SicherungTimeout: 10 * time.Minute, ResticTimeout: 6 * time.Hour,
	}
	if tbl.Org == nil || *tbl.Org != want {
		t.Errorf("org = %+v, want %+v", tbl.Org, want)
	}
}

func TestOrgSectionOverrides(t *testing.T) {
	tbl, err := loadTableText(t, orgTableText(`{"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77,
	  "spiegel_workflow":"f.yaml","spiegel_org_workflow":"o.yml","spiegel_ref":"release/1",
	  "sicherung_dir":"/opt/s","sicherung_wurzel":"/data/s","befund_datei":"/data/b.jsonl",
	  "schwellen":{"geloeschte_dateien":3,"geloeschte_zeilen":0},"sicherung_timeout":"90s","restic_timeout":"2h"}`))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	o := tbl.Org
	if o.SpiegelWorkflow != "f.yaml" || o.SpiegelOrgWorkflow != "o.yml" || o.SpiegelRef != "release/1" ||
		o.SicherungDir != "/opt/s" || o.SicherungWurzel != "/data/s" || o.BefundDatei != "/data/b.jsonl" ||
		o.Schwellen != (Schwellen{3, 0}) || o.SicherungTimeout != 90*time.Second || o.ResticTimeout != 2*time.Hour {
		t.Errorf("org = %+v", o)
	}
}

// Every one of these is a config error that must stop the start, not become a zero
// value or a default.
func TestOrgSectionRejectsBadValues(t *testing.T) {
	const base = `"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77`
	cases := map[string]struct{ org, wantErr string }{
		"not an object":          {`[]`, "org"},
		"missing owner":          {`{"spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77}`, "owner"},
		"empty owner":            {`{"owner":"","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77}`, "owner"},
		"bad owner":              {`{"owner":"-x","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":77}`, "owner"},
		"missing spiegel_repo":   {`{"owner":"ekd-akd","spiegel_installation":77}`, "spiegel_repo"},
		"spiegel_repo no slash":  {`{"owner":"ekd-akd","spiegel_repo":"spiegel","spiegel_installation":77}`, "spiegel_repo"},
		"spiegel_repo traversal": {`{"owner":"ekd-akd","spiegel_repo":"ekd-akd/..","spiegel_installation":77}`, "spiegel_repo"},
		"spiegel_repo foreign":   {`{"owner":"ekd-akd","spiegel_repo":"other/spiegel","spiegel_installation":77}`, "spiegel_repo"},
		"missing installation":   {`{"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel"}`, "spiegel_installation"},
		"zero installation":      {`{"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":0}`, "spiegel_installation"},
		"string installation":    {`{"owner":"ekd-akd","spiegel_repo":"ekd-akd/spiegel","spiegel_installation":"77"}`, "spiegel_installation"},
		"unknown key":            {`{` + base + `,"spiegle_ref":"x"}`, "spiegle_ref"},
		"empty workflow":         {`{` + base + `,"spiegel_workflow":""}`, "spiegel_workflow"},
		"workflow with slash":    {`{` + base + `,"spiegel_workflow":"a/b.yml"}`, "spiegel_workflow"},
		"workflow not yaml":      {`{` + base + `,"spiegel_org_workflow":"org.sh"}`, "spiegel_org_workflow"},
		"ref with space":         {`{` + base + `,"spiegel_ref":"a b"}`, "spiegel_ref"},
		"ref dash first":         {`{` + base + `,"spiegel_ref":"-x"}`, "spiegel_ref"},
		"relative dir":           {`{` + base + `,"sicherung_dir":"lib"}`, "sicherung_dir"},
		"relative wurzel":        {`{` + base + `,"sicherung_wurzel":"srv"}`, "sicherung_wurzel"},
		"relative befund":        {`{` + base + `,"befund_datei":"b.jsonl"}`, "befund_datei"},
		"negative schwelle":      {`{` + base + `,"schwellen":{"geloeschte_dateien":-1,"geloeschte_zeilen":5}}`, "schwellen"},
		"half schwellen":         {`{` + base + `,"schwellen":{"geloeschte_dateien":1}}`, "geloeschte_zeilen"},
		"unparsable timeout":     {`{` + base + `,"sicherung_timeout":"soon"}`, "sicherung_timeout"},
		"zero timeout":           {`{` + base + `,"restic_timeout":"0s"}`, "restic_timeout"},
		"numeric timeout":        {`{` + base + `,"restic_timeout":3600}`, "restic_timeout"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadTableText(t, orgTableText(tc.org))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

func TestOrgOwnerNeedsAnInstallation(t *testing.T) {
	text := `{"installations":{"5":{"owner":"someone","tenant":null}},"pokes":{},"org":` + orgMinimal + `}`
	_, err := loadTableText(t, text)
	if err == nil || !strings.Contains(err.Error(), "no installation") {
		t.Errorf("err = %v, want one saying the owner has no installation", err)
	}
}

func TestOrgOwnerMatchesInstallationsCaseInsensitively(t *testing.T) {
	text := `{"installations":{"5":{"owner":"EKD-AKD","tenant":null}},"pokes":{},"org":` + orgMinimal + `}`
	if _, err := loadTableText(t, text); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestSpiegelNameIsTheLowerCasedRepoPart(t *testing.T) {
	o := OrgConfig{SpiegelRepo: "ekd-akd/Spiegel"}
	if got := o.spiegelFullName(); got != "ekd-akd/spiegel" {
		t.Errorf("spiegelFullName = %q", got)
	}
}
