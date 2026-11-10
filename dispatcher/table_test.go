package dispatcher

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tableJSON is testTable() in the generator's wire shape (contracts C5/G4).
const tableJSON = `{
  "installations": {
    "100": {"owner": "bartixcloud", "tenant": null},
    "102": {"owner": "alpha-org", "tenant": "alpha-org"},
    "103": {"owner": "Juno", "tenant": "juno"}
  },
  "pokes": {
    "bartixcloud/infra": [["platform", "ingress"], ["alpha-org", "alpha-org-app"]],
    "alpha-org/infra": [["platform", "ingress"], ["platform", "bartix-db"], ["alpha-org", "alpha-org-app"]],
    "juno/infra": [["platform", "ingress"], ["platform", "bartix-db"]]
  }
}`

func loadTableText(t *testing.T, text string) (Table, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatcher.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
	return LoadTable(path)
}

func TestLoadTableReadsTheGeneratorShape(t *testing.T) {
	got, err := loadTableText(t, tableJSON)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if want := testTable(); !reflect.DeepEqual(got, want) {
		t.Errorf("table = %+v, want %+v", got, want)
	}
}

func TestLoadTableAcceptsEmptyMappingsAndEmptyTargetLists(t *testing.T) {
	for _, text := range []string{
		`{"installations":{},"pokes":{}}`,
		`{"installations":{"1":{"owner":"o","tenant":null}},"pokes":{"o/infra":[]}}`,
	} {
		if _, err := loadTableText(t, text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
}

func TestLoadTableMissingFileIsAnErrorNamingThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := LoadTable(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
}

func TestLoadTableFailsClosed(t *testing.T) {
	const okInst = `"installations":{"1":{"owner":"o","tenant":"t"}}`
	const okPokes = `"pokes":{"o/infra":[["p","vm"]]}`
	cases := map[string]string{
		"empty file":                 ``,
		"not json":                   `installations: {}`,
		"not an object":              `[]`,
		"null document":              `null`,
		"trailing data":              `{"installations":{},"pokes":{}} {}`,
		"missing installations":      `{` + okPokes + `}`,
		"missing pokes":              `{` + okInst + `}`,
		"both keys missing":          `{}`,
		"installations null":         `{"installations":null,` + okPokes + `}`,
		"pokes null":                 `{` + okInst + `,"pokes":null}`,
		"installations is a list":    `{"installations":[],` + okPokes + `}`,
		"unknown top-level field":    `{` + okInst + `,` + okPokes + `,"extra":1}`,
		"unknown installation field": `{"installations":{"1":{"owner":"o","tenant":"t","admin":true}},` + okPokes + `}`,
		"installation not an object": `{"installations":{"1":"o"},` + okPokes + `}`,
		"installation null":          `{"installations":{"1":null},` + okPokes + `}`,
		"missing tenant key":         `{"installations":{"1":{"owner":"o"}},` + okPokes + `}`,
		"missing owner key":          `{"installations":{"1":{"tenant":"t"}},` + okPokes + `}`,
		"empty owner":                `{"installations":{"1":{"owner":"","tenant":"t"}},` + okPokes + `}`,
		"owner is not a string":      `{"installations":{"1":{"owner":7,"tenant":"t"}},` + okPokes + `}`,
		"tenant is not a string":     `{"installations":{"1":{"owner":"o","tenant":7}},` + okPokes + `}`,
		"tenant empty":               `{"installations":{"1":{"owner":"o","tenant":""}},` + okPokes + `}`,
		"tenant with slash":          `{"installations":{"1":{"owner":"o","tenant":"a/b"}},` + okPokes + `}`,
		"tenant path traversal":      `{"installations":{"1":{"owner":"o","tenant":"../etc"}},` + okPokes + `}`,
		"tenant leading dot":         `{"installations":{"1":{"owner":"o","tenant":".hidden"}},` + okPokes + `}`,
		"tenant leading dash":        `{"installations":{"1":{"owner":"o","tenant":"-x"}},` + okPokes + `}`,
		"tenant with space":          `{"installations":{"1":{"owner":"o","tenant":"a b"}},` + okPokes + `}`,
		"tenant with newline":        `{"installations":{"1":{"owner":"o","tenant":"a\nb"}},` + okPokes + `}`,
		"tenant with nul":            `{"installations":{"1":{"owner":"o","tenant":"a\u0000b"}},` + okPokes + `}`,
		"tenant non-ascii":           `{"installations":{"1":{"owner":"o","tenant":"tü"}},` + okPokes + `}`,
		"target of length 1":         `{` + okInst + `,"pokes":{"o/infra":[["p"]]}}`,
		"target of length 3":         `{` + okInst + `,"pokes":{"o/infra":[["p","vm","x"]]}}`,
		"target empty list":          `{` + okInst + `,"pokes":{"o/infra":[[]]}}`,
		"target is an object":        `{` + okInst + `,"pokes":{"o/infra":[{"project":"p","vm":"vm"}]}}`,
		"target is a string":         `{` + okInst + `,"pokes":{"o/infra":["p/vm"]}}`,
		"target element not string":  `{` + okInst + `,"pokes":{"o/infra":[["p",7]]}}`,
		"target element null":        `{` + okInst + `,"pokes":{"o/infra":[["p",null]]}}`,
		"target list is a string":    `{` + okInst + `,"pokes":{"o/infra":"p/vm"}}`,
		"project empty":              `{` + okInst + `,"pokes":{"o/infra":[["","vm"]]}}`,
		"vm empty":                   `{` + okInst + `,"pokes":{"o/infra":[["p",""]]}}`,
		"project looks like a flag":  `{` + okInst + `,"pokes":{"o/infra":[["--help","vm"]]}}`,
		"vm looks like a flag":       `{` + okInst + `,"pokes":{"o/infra":[["p","-x"]]}}`,
		"vm with space":              `{` + okInst + `,"pokes":{"o/infra":[["p","v m"]]}}`,
		"vm with shell syntax":       `{` + okInst + `,"pokes":{"o/infra":[["p","vm;reboot"]]}}`,
		"poke key not lower-case":    `{` + okInst + `,"pokes":{"O/infra":[["p","vm"]]}}`,
		"poke key without slash":     `{` + okInst + `,"pokes":{"infra":[["p","vm"]]}}`,
		"poke key empty":             `{` + okInst + `,"pokes":{"":[["p","vm"]]}}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if table, err := loadTableText(t, text); err == nil {
				t.Errorf("expected an error, got table %+v", table)
			}
		})
	}
}

func TestLoadTableErrorsNameWhatIsWrong(t *testing.T) {
	cases := map[string]struct{ text, want string }{
		"missing installations": {`{"pokes":{}}`, "installations"},
		"missing pokes":         {`{"installations":{}}`, "pokes"},
		"missing tenant key":    {`{"installations":{"7":{"owner":"o"}},"pokes":{}}`, `missing "tenant"`},
		"empty owner":           {`{"installations":{"7":{"owner":"","tenant":null}},"pokes":{}}`, "owner"},
		"bad tenant":            {`{"installations":{"7":{"owner":"o","tenant":"a/b"}},"pokes":{}}`, "a/b"},
		"bad vm":                {`{"installations":{},"pokes":{"o/infra":[["p","v m"]]}}`, "v m"},
		"short target":          {`{"installations":{},"pokes":{"o/infra":[["p"]]}}`, "2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadTableText(t, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
