package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func nodeOf(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	if len(n.Content) == 0 {
		return &yaml.Node{}
	}
	return n.Content[0]
}

func TestReprMatchesPythonRepr(t *testing.T) {
	cases := map[string]string{
		"org":                                   "'org'",
		"'it''s'":                               `"it's"`,
		`"say \"hi\" it's"`:                     `'say "hi" it\'s'`,
		`"a\nb\tc\\d"`:                          `'a\nb\tc\\d'`,
		`"\u0007"`:                              `'\x07'`,
		"\"\\" + "u200b\"":                      "'\\" + "u200b'",
		`"ü٦"`:                                  `'ü٦'`,
		"7":                                     "7",
		"1.5":                                   "1.5",
		"true":                                  "True",
		"False":                                 "False",
		"yes":                                   "True",
		"Off":                                   "False",
		"null":                                  "None",
		"~":                                     "None",
		"[org, 7, null, true]":                  "['org', 7, None, True]",
		"{a: 1, b: [x]}":                        "{'a': 1, 'b': ['x']}",
		"[[[[[[[deep]]]]]]]":                    "[[[[[...]]]]]",
		"&a [x]":                                "['x']",
		"- x\n- y":                              "['x', 'y']",
		"{1: one, ~: nothing}":                  "{1: 'one', None: 'nothing'}",
		"[]":                                    "[]",
		"{}":                                    "{}",
		"2001-02-03":                            "'2001-02-03'",
		"0x1F":                                  "0x1F",
		"''":                                    "''",
		"\"don't\\\"quote\"":                    `'don\'t"quote'`,
		"[" + strings.Repeat("a, ", 400) + "a]": "",
	}
	for text, want := range cases {
		got := repr(nodeOf(t, text))
		if want == "" { // the budget case: bounded, and says so
			if len(got) > reprBudget+16 || !strings.HasSuffix(got, "...]") {
				t.Errorf("unbounded repr: %d bytes", len(got))
			}
			continue
		}
		if got != want {
			t.Errorf("repr(%s) = %s, want %s", text, got, want)
		}
	}
	if got := repr(&yaml.Node{}); got != "None" {
		t.Errorf("absent key = %s, want None", got)
	}
}

func TestReprFollowsAliasesAndSurvivesSelfReference(t *testing.T) {
	n := nodeOf(t, "base: &b [x, y]\nref: *b\n")
	if got := repr(n); got != "{'base': ['x', 'y'], 'ref': ['x', 'y']}" {
		t.Errorf("repr = %s", got)
	}
	// An anchor that contains its own alias: the node graph is cyclic.
	cyc := nodeOf(t, "&a [*a]")
	if got := repr(cyc); len(got) > reprBudget+16 {
		t.Errorf("cyclic repr not bounded: %d bytes", len(got))
	}
}

func TestPyStrList(t *testing.T) {
	if got := pyStrList([]string{"org", "user"}); got != "['org', 'user']" {
		t.Errorf("got %s", got)
	}
	if got := pyStrList(nil); got != "[]" {
		t.Errorf("got %s", got)
	}
}

func TestStringOfAcceptsOnlyYAMLStrings(t *testing.T) {
	strs := []string{"org", "'7'", `"no"`, "'true'", "'null'", "'~'", "|\n  block\n", "'1e3'", "!!str 7", "!!str no"}
	for _, text := range strs {
		if _, ok := stringOf(nodeOf(t, text)); !ok {
			t.Errorf("%q is a string and was refused", text)
		}
	}
	notStrs := []string{"7", "1.5", "true", "null", "~", "", "[a]", "{a: b}", "2001-02-03", "no", "yes", "On", "OFF"}
	for _, text := range notStrs {
		if s, ok := stringOf(nodeOf(t, text)); ok {
			t.Errorf("%q accepted as the string %q", text, s)
		}
	}
	if s, ok := stringOf(nodeOf(t, "a: &x hello\nb: *x\n").Content[3]); !ok || s != "hello" {
		t.Errorf("alias to a string = %q, %v", s, ok)
	}
}

func TestDecimalInt(t *testing.T) {
	ok := map[string]int64{"0": 0, "7": 7, "+7": 7, "-7": -7, "9223372036854775807": 9223372036854775807}
	for text, want := range ok {
		if got, isInt := decimalInt(nodeOf(t, text)); !isInt || got != want {
			t.Errorf("%s = %d, %v; want %d", text, got, isInt, want)
		}
	}
	for _, text := range []string{"010", "0o10", "0x10", "0b1", "1_0", "1.0", "1e3", "'7'", "true", "null", "[7]",
		"9223372036854775808", "-9223372036854775809", "99999999999999999999", ".5", "1:30"} {
		if got, isInt := decimalInt(nodeOf(t, text)); isInt {
			t.Errorf("%s accepted as %d", text, got)
		}
	}
}

func TestParseDocShapes(t *testing.T) {
	good := string(marshal(t, goodDoc()))
	cases := []struct {
		name    string
		text    string
		wantErr string // parseDoc error
		invalid string // validation error, when parseDoc succeeds
	}{
		{"empty-file", "", "", "mem_floor — not a memory size: None"},
		{"comment-only", "# nothing\n", "", "mem_floor — not a memory size: None"},
		{"explicit-null", "null\n", "", "platform must be a mapping"},
		{"top-level-list", "- a\n- b\n", "", "top level is not a mapping"},
		{"top-level-scalar", "just text\n", "", "top level is not a mapping"},
		{"top-level-number", "7\n", "", "top level is not a mapping"},
		{"syntax-error", "tenants: [unclosed\n", "yaml:", ""},
		{"two-documents", good + "---\n" + good, "more than one", ""},
		{"second-document-broken", good + "---\n[unclosed\n", "yaml:", ""},
		{"duplicate-key-in-a-row", strings.Replace(good, "incus_project: platform", "incus_project: platform\n    incus_project: other", 1), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := parseDoc([]byte(c.text))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				if c.name == "duplicate-key-in-a-row" {
					return // refused at decode: stricter than PyYAML's last-wins, and fails closed
				}
				t.Fatal(err)
			}
			if c.name == "duplicate-key-in-a-row" {
				if _, errs := validate(doc, nil); len(errs) == 0 {
					t.Error("duplicate key silently accepted")
				}
				return
			}
			if _, errs := validate(doc, nil); !anyContains(errs, c.invalid) {
				t.Errorf("validate = %v, want %q", errs, c.invalid)
			}
		})
	}
}

func TestParseDocFollowsAnchorsAndMergeKeys(t *testing.T) {
	text := `
mem_floor: 6GiB
platform:
  incus_project: platform
  github: &org {kind: org, target: bartixcloud, installation_id: 100}
common: &common
  capacity: {mem: 16GiB}
tenants:
  - <<: *common
    name: alpha-org
    github: {kind: org, target: alpha-org, installation_id: 102}
    incus_project: alpha-org
vms:
  - {name: ingress, role: edge}
`
	cfg, errs := validate(docFromText(t, text), nil)
	if len(errs) != 0 || cfg == nil {
		t.Fatalf("validate = %v", errs)
	}
	if got := cfg.Tenants[0].CapacityMem; got != "16GiB" {
		t.Errorf("merged capacity.mem = %q", got)
	}
}

// Keys the generator does not consume belong to P3's tenants.py and may hold any
// type, including non-string keys.
func TestUnknownFieldsOfAnyShapeAreTolerated(t *testing.T) {
	d := goodDoc()
	set(t, d, map[string]any{"deep": []any{1, nil, map[string]any{"x": true}}}, "tenants", 0, "database")
	text := string(marshal(t, d)) + "extra: 1\n7: seven\n"
	if _, errs := validate(docFromText(t, text), nil); len(errs) != 0 {
		t.Errorf("validate = %v", errs)
	}
}
