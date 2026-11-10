package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsAcceptsTheArgparseContract(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want cliArgs
	}{
		{"positional-first", []string{"t.yml", "--out", "D"}, cliArgs{tenantsYml: "t.yml", out: "D"}},
		{"positional-last", []string{"--out", "D", "t.yml"}, cliArgs{tenantsYml: "t.yml", out: "D"}},
		{"out-equals", []string{"t.yml", "--out=D"}, cliArgs{tenantsYml: "t.yml", out: "D"}},
		{"out-equals-dash-value", []string{"t.yml", "--out=-weird"}, cliArgs{tenantsYml: "t.yml", out: "-weird"}},
		{"all-flags", []string{"t.yml", "--out", "D", "--check", "--no-incus-check"},
			cliArgs{tenantsYml: "t.yml", out: "D", check: true, noIncusCheck: true}},
		{"install-sh-order", []string{"t.yml", "--out", "D", "--no-incus-check"},
			cliArgs{tenantsYml: "t.yml", out: "D", noIncusCheck: true}},
		{"last-out-wins", []string{"t.yml", "--out", "A", "--out=B"}, cliArgs{tenantsYml: "t.yml", out: "B"}},
		{"double-dash-ends-options", []string{"--out", "D", "--", "-t.yml"}, cliArgs{tenantsYml: "-t.yml", out: "D"}},
		{"lone-dash-is-positional", []string{"-", "--out", "D"}, cliArgs{tenantsYml: "-", out: "D"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseArgs(c.argv)
			if err != nil || got != c.want {
				t.Errorf("parseArgs(%q) = %+v, %v; want %+v", c.argv, got, err, c.want)
			}
		})
	}
}

func TestParseArgsRejectsEverythingElse(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"no-args", nil, "the following arguments are required: tenants_yml, --out"},
		{"missing-out", []string{"t.yml"}, "the following arguments are required: --out"},
		{"missing-out-with-check", []string{"t.yml", "--check"}, "the following arguments are required: --out"},
		{"missing-positional", []string{"--out", "D"}, "the following arguments are required: tenants_yml"},
		{"trailing-out", []string{"t.yml", "--out"}, "argument --out: expected one argument"},
		{"out-then-flag", []string{"t.yml", "--out", "--check"}, "argument --out: expected one argument"},
		{"out-empty", []string{"t.yml", "--out="}, "expected a directory"},
		{"out-empty-arg", []string{"t.yml", "--out", ""}, "expected a directory"},
		{"unknown-long", []string{"t.yml", "--out", "D", "--chek"}, "unrecognized arguments: --chek"},
		{"abbreviation", []string{"t.yml", "--out", "D", "--chec"}, "unrecognized arguments: --chec"},
		{"single-dash-long", []string{"t.yml", "--out", "D", "-check"}, "unrecognized arguments: -check"},
		{"unknown-short", []string{"t.yml", "--out", "D", "-x"}, "unrecognized arguments: -x"},
		{"flag-with-value", []string{"t.yml", "--out", "D", "--check=1"}, "unrecognized arguments: --check=1"},
		{"two-positionals", []string{"a.yml", "b.yml", "--out", "D"}, "unrecognized arguments: b.yml"},
		{"positionals-after-double-dash", []string{"--out", "D", "--", "a.yml", "b.yml"}, "unrecognized arguments: b.yml"},
		{"several-unrecognized", []string{"t.yml", "--out", "D", "--a", "x.yml", "--b"}, "unrecognized arguments: --a --b x.yml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseArgs(c.argv)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parseArgs(%q) = %+v, %v; want error containing %q", c.argv, got, err, c.wantErr)
			}
			if errors.Is(err, errHelp) {
				t.Error("an error must not look like a help request")
			}
			if !reflect.DeepEqual(got, cliArgs{}) {
				t.Errorf("a failed parse must return no arguments, got %+v", got)
			}
		})
	}
}

func TestParseArgsHelpWinsOverLaterProblems(t *testing.T) {
	for _, argv := range [][]string{
		{"-h"}, {"--help"}, {"t.yml", "--out", "D", "-h"}, {"--bogus", "-h"}, {"t.yml", "--help", "--out"},
	} {
		if _, err := parseArgs(argv); !errors.Is(err, errHelp) {
			t.Errorf("parseArgs(%q) err = %v, want errHelp", argv, err)
		}
	}
	// ...but an option missing its value stops argparse before it sees -h.
	if _, err := parseArgs([]string{"--out", "-h"}); errors.Is(err, errHelp) || err == nil {
		t.Errorf("--out -h: err = %v, want the missing-value error", err)
	}
}

func TestRunHelpPrintsUsageToStdoutAndExitsZero(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		r := runCLI(flag)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("%s: run = %+v, want exit 0 and an empty stderr", flag, r)
		}
		for _, want := range []string{"usage: bcx-gen-tenant-env", "--out", "--check", "--no-incus-check", "tenants_yml"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("%s: help lacks %q:\n%s", flag, want, r.stdout)
			}
		}
	}
}

// A mistyped flag must stop the run before it reads, renders, or writes
// anything: a mistyped --check used to fall through to a write run.
func TestRunUsageErrorsExit2WithUsageOnStderrAndTouchNothing(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	cases := map[string][]string{
		"no-args":         nil,
		"unknown-flag":    {yml, "--out", out, "--no-incus-check", "--chek"},
		"mistyped-check":  {yml, "-check", "--out", out, "--no-incus-check"},
		"abbreviation":    {yml, "--out", out, "--no-incus-check", "--chec"},
		"trailing-out":    {yml, "--out"},
		"out-then-flag":   {yml, "--out", "--check"},
		"empty-out":       {yml, "--out="},
		"missing-out":     {yml, "--no-incus-check"},
		"check-no-out":    {yml, "--check", "--no-incus-check"},
		"two-positionals": {yml, yml, "--out", out, "--no-incus-check"},
		"no-positional":   {"--out", out, "--no-incus-check"},
	}
	cwd := t.TempDir()
	t.Chdir(cwd) // a --check with no --out used to compare files in the current directory
	for name, argv := range cases {
		t.Run(name, func(t *testing.T) {
			r := runCLI(argv...)
			if r.code != 2 {
				t.Errorf("run(%q) = %d, want 2", argv, r.code)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			if !strings.HasPrefix(r.stderr, "usage: bcx-gen-tenant-env") || !strings.Contains(r.stderr, "error: ") {
				t.Errorf("stderr = %q, want a usage message and an error line", r.stderr)
			}
			assertAbsent(t, out)
			if got := listDir(t, cwd); len(got) != 0 {
				t.Errorf("the current directory was touched: %v", got)
			}
		})
	}
}

func TestRunNeverPanicsOnAnyArgumentShape(t *testing.T) {
	shapes := [][]string{
		nil, {""}, {"--out"}, {"--out", ""}, {"--"}, {"--", "--"}, {"-"}, {"--out=", "--out"},
		{"-h", "--out"}, {"--out", "--out", "--out"}, {"x", "--out", "y", "--"}, {"--check", "--check"},
		{"--out=\x00"}, {strings.Repeat("a", 1<<16)},
	}
	for _, argv := range shapes {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("run(%q) panicked: %v", argv, r)
				}
			}()
			if r := runCLI(argv...); r.code != 0 && r.code != 2 {
				t.Errorf("run(%q) = %d, want 0 or 2", argv, r.code)
			}
		}()
	}
}

func TestRunOutEqualsFormWritesIntoThatDirectory(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out="+out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("run = %+v", r)
	}
	if got := listDir(t, out); len(got) != 3 {
		t.Errorf("files = %v", got)
	}
	if r := runCLI("--out="+out, yml, "--check", "--no-incus-check"); r.code != 0 {
		t.Errorf("--out=DIR before the positional, --check = %+v", r)
	}
}

func TestExitCodes(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("write = %+v, want 0", r)
	}
	if r := runCLI(yml, "--out", out, "--check", "--no-incus-check"); r.code != 0 {
		t.Errorf("clean check = %+v, want 0", r)
	}
	if err := os.WriteFile(filepath.Join(out, "alpha-org.env"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(yml, "--out", out, "--check", "--no-incus-check"); r.code != 1 {
		t.Errorf("drift = %+v, want 1", r)
	}
	if r := runCLI(filepath.Join(dir, "absent.yml"), "--out", out, "--check"); r.code != 2 {
		t.Errorf("refused = %+v, want 2", r)
	}
}
