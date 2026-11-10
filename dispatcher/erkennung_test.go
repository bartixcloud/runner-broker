package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenCase is one entry of testdata/sicherung/cases.json, the single source for
// this test and for regen.py, which produces the goldens from the Python reference.
type goldenCase struct {
	Name     string `json:"name"`
	Fixture  string `json:"fixture"`
	Event    string `json:"event"`
	Diffstat *struct {
		DateienGeloescht int `json:"dateien_geloescht"`
		ZeilenGeloescht  int `json:"zeilen_geloescht"`
	} `json:"diffstat"`
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sicherung", name))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return data
}

// TestBewerteMatchesThePythonGoldens is the shared-vector contract: byte-identical
// JSON lines to what the reference (erkennung.py) prints for the same payload.
func TestBewerteMatchesThePythonGoldens(t *testing.T) {
	var cases []goldenCase
	if err := json.Unmarshal(readTestdata(t, "cases.json"), &cases); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	if len(cases) < 40 {
		t.Fatalf("only %d cases: cases.json looks truncated", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var diff *DiffStat
			if tc.Diffstat != nil {
				diff = &DiffStat{DateienGeloescht: tc.Diffstat.DateienGeloescht, ZeilenGeloescht: tc.Diffstat.ZeilenGeloescht}
			}
			got, err := EncodeBefunde(Bewerte(tc.Event, readTestdata(t, tc.Fixture), DefaultSchwellen(), diff))
			if err != nil {
				t.Fatalf("EncodeBefunde: %v", err)
			}
			want := readTestdata(t, tc.Name+".golden.jsonl")
			if string(got) != string(want) {
				t.Errorf("befunde differ from the Python reference\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestBewerteNeverPanicsOnHostileInput(t *testing.T) {
	for _, event := range []string{"push", "delete", "repository", "", "star"} {
		for _, body := range []string{"", "{", "null", "[]", `{"repository":{"name":null}}`, `{"commits":[null]}`,
			`{"ref":"x","forced":true,"deleted":false,"repository":{"name":"d"},"commits":[{"removed":[null]}],"size":1e400}`} {
			_ = Bewerte(event, []byte(body), DefaultSchwellen(), nil)
		}
	}
}

func TestSchwellenBoundaryIsStrictlyGreater(t *testing.T) {
	body := readTestdata(t, "push_normal.json")
	s := Schwellen{GeloeschteDateien: 2, GeloeschteZeilen: 10}
	cases := []struct {
		d    DiffStat
		want int
	}{{DiffStat{2, 10}, 0}, {DiffStat{3, 0}, 1}, {DiffStat{0, 11}, 1}}
	for _, tc := range cases {
		if got := len(Bewerte("push", body, s, &tc.d)); got != tc.want {
			t.Errorf("diffstat %+v: %d befunde, want %d", tc.d, got, tc.want)
		}
	}
}
