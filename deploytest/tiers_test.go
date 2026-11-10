package deploytest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// dTiersFile is the shape of the shipped tiers.yml. Decoding is strict
// (decodeStrict), so a key added to the file but not modelled here fails the
// test instead of slipping through.
type dTiersFile struct {
	Tiers map[string]dTier `yaml:"tiers"`
}

type dTier struct {
	Image       string   `yaml:"image"`
	Network     string   `yaml:"network"`
	RunnerGroup string   `yaml:"runner_group"`
	CPU         dQuoted  `yaml:"cpu"`
	Mem         dQuoted  `yaml:"mem"`
	MaxRunners  int      `yaml:"max_runners"`
	Allowlist   []string `yaml:"allowlist"`
	Secrets     []string `yaml:"secrets"`
}

// dQuoted only accepts a YAML string scalar. yaml.v3 would also decode an
// unquoted `cpu: 2` into a string field, but the broker hands cpu and mem to
// Incus verbatim, so the file must say them as strings.
type dQuoted string

func (q *dQuoted) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("line %d: want a quoted string, got %s", node.Line, node.Tag)
	}
	*q = dQuoted(node.Value)
	return nil
}

// The infra tier was retired: the platform installation has no broker, so no
// runner can ever serve `runs-on: [self-hosted, infra]`. tiers.yml holds only
// tiers that carry no tier secrets (build, dependabot).
func TestShippedTiersYml(t *testing.T) {
	got := decodeStrict[dTiersFile](t, filepath.Join(repoRoot(t), "tiers.yml"))
	want := dTiersFile{Tiers: map[string]dTier{
		"build": {
			Image:       "bcx-runner-base",
			Network:     "nat-ci",
			RunnerGroup: "build-ephemeral",
			CPU:         "2",
			Mem:         "4GiB",
			MaxRunners:  4,
			Allowlist:   []string{"*"},
			Secrets:     []string{},
		},
		"dependabot": {
			Image:       "bcx-runner-base",
			Network:     "nat-ci",
			RunnerGroup: "build-ephemeral",
			CPU:         "2",
			Mem:         "4GiB",
			MaxRunners:  2,
			Allowlist:   []string{"*"},
			Secrets:     []string{},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tiers.yml mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// Sizing is no longer a hand-computed sum against the metal's free RAM; the
// tenant's Incus project cap bounds it, so the header must say where the cap
// comes from.
func TestTiersHeaderCitesTheProjectCap(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "tiers.yml"))
	if err != nil {
		t.Fatalf("read tiers.yml: %v", err)
	}
	header, _, ok := bytes.Cut(data, []byte("\ntiers:"))
	if !ok {
		t.Fatal("tiers.yml has no top-level tiers: key")
	}
	for _, token := range []string{"BROKER_INCUS_PROJECT", "limits.memory"} {
		if !bytes.Contains(header, []byte(token)) {
			t.Errorf("tiers.yml header does not mention %s", token)
		}
	}
}

func TestInfraImageBuilderIsGone(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"images/runner-infra-provision.sh", "images"} {
		_, err := os.Stat(filepath.Join(root, rel))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s must not exist (stat err: %v)", rel, err)
		}
	}
}

// infraRemnant matches what a surviving infra tier leaves behind: its image,
// its runner group, its secrets, the tier key and prose naming it. Plain
// "infra" is not matched: the <owner>/infra repo and infra/tenants.yml are
// still real.
var infraRemnant = regexp.MustCompile(`runner-infra|infra-ephemeral|(?i:infra[ -]tier)|INFRA_[A-Z]|(?m:^[ \t]*infra:)`)

// pendingRewrite is a file owned by someone else that still describes the
// retired tier. It is exempt only until its owner rewrites it; a stale
// exemption fails the test so this list cannot outlive the problem.
type pendingRewrite struct {
	Path   string
	Reason string
}

var infraRemnantExempt = []pendingRewrite{}

// This file names the patterns it forbids, so it cannot scan itself.
const tiersTestPath = "deploytest/tiers_test.go"

func TestNoInfraTierRemnantsOutsideTestsAndPython(t *testing.T) {
	root := repoRoot(t)
	hits := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// bin/ is untracked build output.
			if rel == ".git" || rel == "bin" {
				return fs.SkipDir
			}
			return nil
		}
		// Go tests keep tier fixtures with secrets (the secrets path is generic);
		// they are the tests this scan exempts.
		if !d.Type().IsRegular() || strings.HasSuffix(rel, "_test.go") || rel == tiersTestPath {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for _, m := range infraRemnant.FindAllString(string(data), -1) {
			hits[rel] = append(hits[rel], strings.TrimSpace(m))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	for _, ex := range infraRemnantExempt {
		found, ok := hits[ex.Path]
		if !ok {
			t.Errorf("%s no longer mentions the infra tier: delete its exemption (%s)", ex.Path, ex.Reason)
			continue
		}
		t.Logf("exempt until rewritten: %s (%d hits): %s", ex.Path, len(found), ex.Reason)
		delete(hits, ex.Path)
	}
	for rel, found := range hits {
		t.Errorf("%s still mentions the retired infra tier: %q", rel, found)
	}
}
