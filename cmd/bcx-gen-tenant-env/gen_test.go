package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

// setUmask returns the previous umask.
func setUmask(mask int) int { return syscall.Umask(mask) }

// goodDoc mirrors test_gen_tenant_env.py's GOOD.
func goodDoc() map[string]any {
	return map[string]any{
		"mem_floor": "6GiB",
		"platform": map[string]any{
			"incus_project": "platform",
			"github":        map[string]any{"kind": "org", "target": "bartixcloud", "installation_id": 100},
		},
		"tenants": []any{
			map[string]any{
				"name":          "kappa-org",
				"github":        map[string]any{"kind": "org", "target": "kappa-org", "installation_id": 101},
				"incus_project": "beta", "capacity": map[string]any{"mem": "20GiB"},
			},
			map[string]any{
				"name":          "alpha-org",
				"github":        map[string]any{"kind": "org", "target": "alpha-org", "installation_id": 102},
				"incus_project": "alpha-org", "capacity": map[string]any{"mem": "16GiB"},
			},
			map[string]any{
				"name":          "juno",
				"github":        map[string]any{"kind": "user", "target": "Juno", "installation_id": 103, "repos": []any{}},
				"incus_project": "juno", "capacity": map[string]any{"mem": "20GiB"},
			},
		},
		"vms": []any{
			map[string]any{"name": "ingress", "role": "edge"},
			map[string]any{"name": "registry", "role": "registry"},
			map[string]any{"name": "devbox-a", "role": "ops"},
			map[string]any{"name": "bartix-db", "role": "data"},
			map[string]any{"name": "alpha-org-app", "tenant": "alpha-org", "role": "app"},
			map[string]any{"name": "dev-main", "tenant": "juno", "role": "devbox"},
			map[string]any{"name": "beta-app", "tenant": "kappa-org", "role": "app"},
			map[string]any{"name": "beta-db", "tenant": "kappa-org", "role": "data"},
		},
	}
}

var pokes = map[string][][2]string{
	"bartixcloud/infra": {
		{"platform", "ingress"}, {"platform", "registry"}, {"platform", "ops"},
		{"platform", "bartix-db"}, {"alpha-org", "alpha-org-app"},
		{"beta", "beta-app"}, {"beta", "beta-db"},
	},
	"alpha-org/infra": {
		{"platform", "ingress"}, {"platform", "bartix-db"}, {"alpha-org", "alpha-org-app"},
	},
	"juno/infra": {
		{"platform", "ingress"}, {"platform", "bartix-db"},
	},
	"kappa-org/infra": {
		{"platform", "ingress"}, {"beta", "beta-app"}, {"beta", "beta-db"},
	},
}

// stubPokeTargets replaces pokeTargets with the POKES table for the duration of
// a test. The table is keyed lower-case; the generator asks in the owner's own
// spelling.
func stubPokeTargets(t *testing.T) {
	t.Helper()
	orig := pokeTargets
	pokeTargets = func(_, repo string) ([][2]string, error) {
		return pokes[strings.ToLower(repo)], nil
	}
	t.Cleanup(func() { pokeTargets = orig })
}

// ---- document helpers: tests build documents as Python-style maps, but every
// document goes through real YAML text before the code under test sees it ----

// at walks path (string = map key, int = list index) through a decoded document.
func at(t *testing.T, doc any, path ...any) any {
	t.Helper()
	cur := doc
	for _, seg := range path {
		switch k := seg.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %v is not a mapping", path, cur)
			}
			cur = m[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("path %v: %v is not a list with index %d", path, cur, k)
			}
			cur = l[k]
		default:
			t.Fatalf("path %v: bad segment %v", path, seg)
		}
	}
	return cur
}

// set stores value at path, creating nothing: the parent must exist.
func set(t *testing.T, doc map[string]any, value any, path ...any) {
	t.Helper()
	parent := at(t, doc, path[:len(path)-1]...)
	switch k := path[len(path)-1].(type) {
	case string:
		m, ok := parent.(map[string]any)
		if !ok {
			t.Fatalf("path %v: parent is not a mapping", path)
		}
		m[k] = value
	case int:
		l, ok := parent.([]any)
		if !ok || k >= len(l) {
			t.Fatalf("path %v: parent is not a list with index %d", path, k)
		}
		l[k] = value
	default:
		t.Fatalf("path %v: bad final segment", path)
	}
}

// del removes the mapping key at path.
func del(t *testing.T, doc map[string]any, path ...any) {
	t.Helper()
	parent, ok := at(t, doc, path[:len(path)-1]...).(map[string]any)
	key, keyOK := path[len(path)-1].(string)
	if !ok || !keyOK {
		t.Fatalf("path %v: not a mapping key", path)
	}
	delete(parent, key)
}

// push appends value to the list stored at path.
func push(t *testing.T, doc map[string]any, value any, path ...any) {
	t.Helper()
	list, ok := at(t, doc, path...).([]any)
	if !ok {
		t.Fatalf("path %v: not a list", path)
	}
	set(t, doc, append(list, value), path...)
}

func marshal(t *testing.T, d map[string]any) []byte {
	t.Helper()
	data, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func docOf(t *testing.T, d map[string]any) *tenantsDoc {
	t.Helper()
	return docFromText(t, string(marshal(t, d)))
}

func docFromText(t *testing.T, text string) *tenantsDoc {
	t.Helper()
	doc, err := parseDoc([]byte(text))
	if err != nil {
		t.Fatalf("parseDoc: %v\n%s", err, text)
	}
	return doc
}

func errsOf(t *testing.T, d map[string]any, incus map[string]struct{}) []string {
	t.Helper()
	_, errs := validate(docOf(t, d), incus)
	return errs
}

func renderOf(t *testing.T, d map[string]any) rendered {
	t.Helper()
	files, err := render(docOf(t, d), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func renderErr(t *testing.T, d map[string]any) error {
	t.Helper()
	_, err := render(docOf(t, d), nil, "")
	return err
}

func fileText(t *testing.T, files rendered, name string) string {
	t.Helper()
	text, ok := files.text(name)
	if !ok {
		t.Fatalf("%s not rendered; have %v", name, fileNames(files))
	}
	return text
}

func fileNames(files rendered) []string {
	names := []string{}
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

func envLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "BROKER_") {
			out = append(out, line)
		}
	}
	return out
}

func writeYAMLIn(t *testing.T, dir string, doc map[string]any) string {
	t.Helper()
	return writeTextIn(t, dir, string(marshal(t, doc)))
}

func writeTextIn(t *testing.T, dir, text string) string {
	t.Helper()
	p := filepath.Join(dir, "tenants.yml")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mustReplace edits YAML text and fails if the anchor is missing, so a
// mutation can never silently turn into a no-op.
func mustReplace(t *testing.T, text, old, replacement string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("anchor %q not in:\n%s", old, text)
	}
	return strings.Replace(text, old, replacement, 1)
}

// mustDropLine deletes the one line holding anchor.
func mustDropLine(t *testing.T, text, anchor string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(line, anchor) {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
		}
	}
	t.Fatalf("anchor %q not in:\n%s", anchor, text)
	return ""
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(argv ...string) result {
	var stdout, stderr bytes.Buffer
	code := run(argv, &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

// fakeBin makes PATH resolve to the given executables first (and only /bin and
// /usr/bin after them, for sh's own tools). It replaces the real python3/incus.
func fakeBin(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return dir
}

func anyContains(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func anyContainsTwo(errs []string, a, b string) bool {
	for _, e := range errs {
		if strings.Contains(e, a) && strings.Contains(e, b) {
			return true
		}
	}
	return false
}

func anyHasPrefix(errs []string, prefix string) bool {
	for _, e := range errs {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s should not exist (stat err = %v)", path, err)
	}
}

// ------------------------------------------------------------ validation ----

func TestValidDocumentHasNoErrors(t *testing.T) {
	if errs := errsOf(t, goodDoc(), nil); len(errs) != 0 {
		t.Errorf("validate = %v", errs)
	}
}

func TestRejectsPreBartixcloudFields(t *testing.T) {
	fields := map[string]any{
		"webhook_host": "webhook.example.org", "listen_host": "172.17.0.1",
		"runner_project": "default", "runner_group_id": 1, "listen_port": 9010,
		"port": 9010, "secrets_age": "/x.age",
	}
	for key, value := range fields {
		d := goodDoc()
		set(t, d, value, "tenants", 1, key)
		errs := errsOf(t, d, nil)
		if !anyContainsTwo(errs, key, "pre-bartixcloud") {
			t.Errorf("key %q: no pre-bartixcloud error in %v", key, errs)
		}
		// Presence is enough: a null value is still a legacy key.
		set(t, d, nil, "tenants", 1, key)
		if !anyContainsTwo(errsOf(t, d, nil), key, "pre-bartixcloud") {
			t.Errorf("key %q: null value not rejected", key)
		}
	}
}

func TestRejectsTopLevelAgeKey(t *testing.T) {
	for _, value := range []any{"/etc/example/age.key", nil} {
		d := goodDoc()
		d["age_key"] = value
		if errs := errsOf(t, d, nil); !anyContains(errs, "age_key") {
			t.Errorf("age_key=%v: expected age_key error, got %v", value, errs)
		}
	}
}

func TestRejectsKindRepo(t *testing.T) {
	d := goodDoc()
	set(t, d, map[string]any{"kind": "repo", "target": "Juno/some-repo", "installation_id": 103},
		"tenants", 2, "github")
	errs := errsOf(t, d, nil)
	if !anyContains(errs, "github.kind") {
		t.Errorf("missing github.kind error: %v", errs)
	}
	if !anyContains(errs, "bare owner") {
		t.Errorf("missing bare owner error: %v", errs)
	}
}

func TestUserTenantRequiresBareRepoNames(t *testing.T) {
	d := goodDoc()
	del(t, d, "tenants", 2, "github", "repos")
	if !anyContains(errsOf(t, d, nil), "github.repos") {
		t.Error("expected github.repos error when repos missing")
	}
	set(t, d, []any{"Juno/some-repo"}, "tenants", 2, "github", "repos")
	if !anyContains(errsOf(t, d, nil), "github.repos") {
		t.Error("expected github.repos error for non-bare repo")
	}
}

// A kind: user tenant whose repos is not a list used to validate as an empty
// list, render no env, and make the write run delete the tenant's live env.
func TestUserReposMustBeAList(t *testing.T) {
	stubPokeTargets(t)
	cases := map[string]any{
		"string": "ci-repo", "null": nil, "mapping": map[string]any{"a": "b"},
		"int": 5, "bool": true, "list-of-int": []any{1}, "list-of-null": []any{nil},
		"nested-list": []any{[]any{"a"}},
	}
	for name, repos := range cases {
		t.Run(name, func(t *testing.T) {
			d := goodDoc()
			set(t, d, repos, "tenants", 2, "github", "repos")
			if errs := errsOf(t, d, nil); !anyContains(errs, "github.repos must be a list of bare repo names") {
				t.Errorf("validate accepted repos=%v: %v", repos, errs)
			}
			if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Errorf("render accepted repos=%v: %v", repos, err)
			}
		})
	}
}

func TestUserReposTypoNeitherDeletesLiveEnvNorReportsDrift(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	good := goodDoc()
	set(t, good, []any{"ci-repo"}, "tenants", 2, "github", "repos")
	if r := runCLI(writeYAMLIn(t, dir, good), "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	before, err := os.ReadFile(filepath.Join(out, "juno.env"))
	if err != nil {
		t.Fatal(err)
	}

	typo := goodDoc()
	set(t, typo, "ci-repo", "tenants", 2, "github", "repos")
	yml := writeYAMLIn(t, dir, typo)
	for _, argv := range [][]string{
		{yml, "--out", out, "--no-incus-check"},
		{yml, "--out", out, "--check", "--no-incus-check"},
	} {
		r := runCLI(argv...)
		if r.code != 2 {
			t.Errorf("%v = %d, want 2 (invalid document, never drift)", argv, r.code)
		}
		if !strings.Contains(r.stderr, "github.repos must be a list") {
			t.Errorf("%v: stderr = %q", argv, r.stderr)
		}
		if strings.Contains(r.stdout, "removed") {
			t.Errorf("%v: deleted a file: %q", argv, r.stdout)
		}
	}
	after, err := os.ReadFile(filepath.Join(out, "juno.env"))
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("juno.env was lost or changed: %v", err)
	}
}

func TestOrgTenantMustNotDeclareRepos(t *testing.T) {
	for name, repos := range map[string]any{
		"list": []any{"app"}, "empty-list": []any{}, "string": "app", "mapping": map[string]any{"a": 1},
	} {
		d := goodDoc()
		set(t, d, repos, "tenants", 1, "github", "repos")
		if !anyContains(errsOf(t, d, nil), "only valid for kind: user") {
			t.Errorf("%s: expected repos-only-valid-for-user error", name)
		}
	}
}

// Python only rejects a non-null repos on an org tenant (`elif repos is not
// None`), so an explicit `repos: null` is the same as no key.
func TestOrgTenantMayHaveNullRepos(t *testing.T) {
	d := goodDoc()
	set(t, d, nil, "tenants", 1, "github", "repos")
	if errs := errsOf(t, d, nil); len(errs) != 0 {
		t.Errorf("org tenant with repos: null rejected: %v", errs)
	}
}

func TestPlatformMustBeOrgInstallation(t *testing.T) {
	d := goodDoc()
	set(t, d, "user", "platform", "github", "kind")
	errs := errsOf(t, d, nil)
	if !anyHasPrefix(errs, "platform: github.kind must be one of ['org'], got 'user'") {
		t.Errorf("expected platform github.kind error: %v", errs)
	}
}

func TestRejectsPlaceholderInstallationID(t *testing.T) {
	d := goodDoc()
	set(t, d, "<id>", "tenants", 0, "github", "installation_id")
	if !anyContains(errsOf(t, d, nil), "installation_id must be a positive int, got '<id>'") {
		t.Error("expected installation_id error")
	}
}

func TestInstallationIDMustBeAPlainPositiveInt(t *testing.T) {
	const anchor = "installation_id: 101"
	text := string(marshal(t, goodDoc()))
	bad := []string{
		"0", "-5", "true", "1.5", "6.0", "'7'", "null", "~", "[1]", "0o146", "0x10", "010", "1_0",
		"9223372036854775808", "99999999999999999999", "1e3",
	}
	for _, v := range bad {
		doc := docFromText(t, mustReplace(t, text, anchor, "installation_id: "+v))
		if _, errs := validate(doc, nil); !anyContains(errs, "installation_id must be a positive int") {
			t.Errorf("installation_id %s accepted: %v", v, errs)
		}
	}
	for _, v := range []string{"7", "9223372036854775807"} {
		doc := docFromText(t, mustReplace(t, text, anchor, "installation_id: "+v))
		if _, errs := validate(doc, nil); len(errs) != 0 {
			t.Errorf("installation_id %s rejected: %v", v, errs)
		}
	}
	absent := docFromText(t, mustDropLine(t, text, "installation_id: 101"))
	if _, errs := validate(absent, nil); !anyContains(errs, "installation_id must be a positive int, got None") {
		t.Errorf("absent installation_id accepted: %v", errs)
	}
}

func TestRejectsInstallationIDUsedTwice(t *testing.T) {
	d := goodDoc()
	set(t, d, 100, "tenants", 0, "github", "installation_id")
	if !anyContains(errsOf(t, d, nil), "installation_id(s) [100] used more than once") {
		t.Error("expected duplicate installation_id error")
	}
}

func TestRejectsMissingIncusProjectWhenChecked(t *testing.T) {
	errs := errsOf(t, goodDoc(), map[string]struct{}{"platform": {}, "alpha-org": {}})
	if !anyContains(errs, "'beta'") {
		t.Errorf("missing beta error: %v", errs)
	}
	if !anyContains(errs, "'juno'") {
		t.Errorf("missing juno error: %v", errs)
	}
	if !anyContains(errs, "(have: ['alpha-org', 'platform'])") {
		t.Errorf("have-list not Python-formatted: %v", errs)
	}
}

func TestRejectsSharedIncusProjectOrGitHubTarget(t *testing.T) {
	stubPokeTargets(t)
	cases := []struct {
		name   string
		mutate func(t *testing.T, d map[string]any)
		field  string
	}{
		{"project-twice", func(t *testing.T, d map[string]any) {
			set(t, d, at(t, d, "tenants", 0, "incus_project"), "tenants", 1, "incus_project")
		}, "incus_project"},
		{"project-is-platforms", func(t *testing.T, d map[string]any) {
			set(t, d, "platform", "tenants", 1, "incus_project")
		}, "incus_project"},
		{"target-twice-other-case", func(t *testing.T, d map[string]any) {
			set(t, d, "Kappa-Org", "tenants", 1, "github", "target")
		}, "github.target"},
		{"target-is-platforms-other-case", func(t *testing.T, d map[string]any) {
			set(t, d, "BartixCloud", "tenants", 1, "github", "target")
		}, "github.target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodDoc()
			c.mutate(t, d)
			if errs := errsOf(t, d, nil); !anyContainsTwo(errs, c.field, "already used") {
				t.Errorf("expected %q already-used error: %v", c.field, errs)
			}
			if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Errorf("expected render to fail with invalid, got %v", err)
			}
		})
	}
}

// Python registers every string, the empty one included, in the uniqueness pass.
func TestEmptyStringsAreStillCheckedForUniqueness(t *testing.T) {
	d := goodDoc()
	set(t, d, "", "tenants", 0, "incus_project")
	set(t, d, "", "tenants", 1, "incus_project")
	if !anyContainsTwo(errsOf(t, d, nil), "incus_project ''", "already used") {
		t.Error("two empty incus_project values not reported as shared")
	}
}

func TestUniquenessCheckSurvivesGarbledRows(t *testing.T) {
	d := goodDoc()
	d["platform"] = "not-a-mapping"
	set(t, d, []any{"not", "a", "mapping"}, "tenants", 0, "github")
	set(t, d, []any{"list"}, "tenants", 1, "incus_project")
	push(t, d, "not-a-mapping", "tenants")
	errs := errsOf(t, d, nil)
	for _, want := range []string{"platform must be a mapping", "tenants[0]: github must be a mapping",
		"tenants[1]: incus_project must match", "tenants[3]: not a mapping"} {
		if !anyContains(errs, want) {
			t.Errorf("missing %q in %v", want, errs)
		}
	}
}

func TestTrailingNewlineIsNotAValidName(t *testing.T) {
	stubPokeTargets(t)
	cases := []struct {
		name   string
		mutate func(t *testing.T, d map[string]any)
	}{
		{"tenant-name", func(t *testing.T, d map[string]any) { renameTenant(t, d, 1, "alpha-org\n") }},
		{"owner", func(t *testing.T, d map[string]any) { set(t, d, "alpha-org\n", "tenants", 1, "github", "target") }},
		{"incus-project", func(t *testing.T, d map[string]any) { set(t, d, "alpha-org\n", "tenants", 1, "incus_project") }},
		{"repo", func(t *testing.T, d map[string]any) {
			set(t, d, []any{"some-repo\n"}, "tenants", 2, "github", "repos")
		}},
		{"vm-name", func(t *testing.T, d map[string]any) { set(t, d, "ingress\n", "vms", 0, "name") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodDoc()
			c.mutate(t, d)
			errs := errsOf(t, d, nil)
			if !anyContains(errs, "must match") && !anyContains(errs, "bare") {
				t.Errorf("expected name error: %v", errs)
			}
			if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Errorf("expected render invalid error, got %v", err)
			}
		})
	}
}

func renameTenant(t *testing.T, d map[string]any, i int, newName string) {
	t.Helper()
	old := at(t, d, "tenants", i, "name")
	set(t, d, newName, "tenants", i, "name")
	vms, ok := at(t, d, "vms").([]any)
	if !ok {
		t.Fatal("vms is not a list")
	}
	for j := range vms {
		if at(t, d, "vms", j, "tenant") == old {
			set(t, d, newName, "vms", j, "tenant")
		}
	}
}

func TestRejectsDotRepoNames(t *testing.T) {
	for _, repos := range [][]any{{"."}, {".."}, {"ok", ".."}} {
		d := goodDoc()
		set(t, d, repos, "tenants", 2, "github", "repos")
		if !anyContains(errsOf(t, d, nil), "github.repos") {
			t.Errorf("expected github.repos error for %v", repos)
		}
	}
}

func TestMemorySizesMustBeOneEnvFileToken(t *testing.T) {
	stubPokeTargets(t)
	bads := []string{"6\nGiB", "6 GiB", " 6GiB", "6GiB\n", "٦GiB"}
	for _, where := range []string{"mem_floor", "capacity.mem"} {
		for _, bad := range bads {
			d := goodDoc()
			if where == "mem_floor" {
				d["mem_floor"] = bad
			} else {
				set(t, d, bad, "tenants", 1, "capacity", "mem")
			}
			if errs := errsOf(t, d, nil); !anyContains(errs, where) {
				t.Errorf("%s=%q: no error mentioning %s: %v", where, bad, where, errs)
			}
			if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Errorf("expected invalid render for %q", bad)
			}
		}
	}
}

// Anything that is not a string or a plain decimal integer either renders
// differently to YAML 1.1 and 1.2 readers or is not a size at all.
func TestMemorySizesOfTheWrongTypeAreRejected(t *testing.T) {
	text := string(marshal(t, goodDoc()))
	bads := map[string]string{
		"6.0": "fractional", "1.5": "fractional", "6.": "fractional", "1e3": "fractional",
		"true": "not a memory size: True", "false": "not a memory size: False",
		"yes": "not a memory size: True", "no": "not a memory size: False",
		"null": "not a memory size: None", "~": "not a memory size: None",
		"[6]": "not a memory size: [6]", "{a: 1}": "not a memory size: {'a': 1}",
		"0x10": "plain decimal", "010": "plain decimal", "0o10": "plain decimal", "1_000": "plain decimal",
		"-5": "not a memory size", "'GiB'": "not a memory size", "'6XiB'": "unknown memory unit",
	}
	for v, want := range bads {
		for _, c := range []struct{ anchor, where string }{
			{"mem_floor: 6GiB", "mem_floor"}, {"mem: 16GiB", "capacity.mem"},
		} {
			key, _, _ := strings.Cut(c.anchor, ":")
			doc := docFromText(t, mustReplace(t, text, c.anchor, key+": "+v))
			_, errs := validate(doc, nil)
			if !anyContainsTwo(errs, c.where+" — ", want) {
				t.Errorf("%s: %s accepted or wrong reason (want %q): %v", c.where, v, want, errs)
			}
		}
	}
	absent := docFromText(t, mustDropLine(t, text, "mem_floor: 6GiB"))
	if _, errs := validate(absent, nil); !anyContains(errs, "mem_floor — not a memory size: None") {
		t.Errorf("absent mem_floor accepted: %v", errs)
	}
}

func TestMemorySizesOneTokenRenderVerbatim(t *testing.T) {
	stubPokeTargets(t)
	oks := map[string]string{
		"6GiB": "6GiB", "6GB": "6GB", "6144MiB": "6144MiB", "1.5GiB": "1.5GiB",
		"'6442450944'": "6442450944", "6442450944": "6442450944", "+7": "7",
	}
	for v, want := range oks {
		text := string(marshal(t, goodDoc()))
		text = mustReplace(t, text, "mem_floor: 6GiB", "mem_floor: "+v)
		text = mustReplace(t, text, "mem: 16GiB", "mem: "+v)
		files, err := render(docFromText(t, text), nil, "")
		if err != nil {
			t.Errorf("%s: %v", v, err)
			continue
		}
		lines := envLines(fileText(t, files, "alpha-org.env"))
		if !contains(lines, "BROKER_CAPACITY_MEM="+want) || !contains(lines, "BROKER_MEM_FLOOR="+want) {
			t.Errorf("%s: missing verbatim lines: %v", v, lines)
		}
	}
}

func TestRejectsBadVMs(t *testing.T) {
	d := goodDoc()
	push(t, d, map[string]any{"name": "x-app", "tenant": "nobody", "role": "app"}, "vms")
	push(t, d, map[string]any{"name": "ingress", "role": "router"}, "vms")
	errs := errsOf(t, d, nil)
	if !anyContains(errs, "'nobody'") {
		t.Errorf("missing nobody error: %v", errs)
	}
	if !anyContains(errs, "duplicate name 'ingress'") {
		t.Errorf("missing duplicate error: %v", errs)
	}
	if !anyContains(errs, "role must be one of ['app', 'data', 'devbox', 'edge', 'ops', 'registry'], got 'router'") {
		t.Errorf("missing role error: %v", errs)
	}
}

func TestVMsMustBeANonEmptyList(t *testing.T) {
	for name, vms := range map[string]any{"missing": nil, "empty": []any{}, "mapping": map[string]any{"a": 1}, "string": "x"} {
		d := goodDoc()
		if name == "missing" {
			del(t, d, "vms")
		} else {
			d["vms"] = vms
		}
		if !anyContains(errsOf(t, d, nil), "vms must be a non-empty list") {
			t.Errorf("%s: vms accepted", name)
		}
	}
}

func TestVMsAcceptC2ShapeWithNullTenant(t *testing.T) {
	d := goodDoc()
	vms, ok := at(t, d, "vms").([]any)
	if !ok {
		t.Fatal("vms is not a list")
	}
	for i := range vms {
		if at(t, d, "vms", i, "tenant") == nil {
			set(t, d, nil, "vms", i, "tenant")
		}
	}
	if errs := errsOf(t, d, nil); len(errs) != 0 {
		t.Errorf("validate = %v", errs)
	}
}

func TestC2FieldsNotConsumedAreNeitherRejectedNorRendered(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	set(t, d, map[string]any{"name": "incusbr0", "cidr": "10.0.10.0/24"}, "platform", "bridge")
	set(t, d, "bartixcloud", "platform", "signer_principal")
	set(t, d, map[string]any{"name": "br-alpha-org", "cidr": "10.0.11.0/24"}, "tenants", 1, "bridge")
	set(t, d, "shared", "tenants", 1, "edge")
	set(t, d, []any{"hello.example"}, "tenants", 1, "domains")
	set(t, d, []any{8080}, "tenants", 1, "service_ports")
	set(t, d, "alpha-org", "tenants", 1, "signer_principal")
	set(t, d, "age1recipient", "tenants", 1, "age_recipient")
	set(t, d, map[string]any{"instance": "beta-db"}, "tenants", 0, "database")
	vms, ok := at(t, d, "vms").([]any)
	if !ok {
		t.Fatal("vms is not a list")
	}
	for i := range vms {
		set(t, d, "10.0.10.99", "vms", i, "ip")
		set(t, d, 1, "vms", i, "cpu")
		set(t, d, "2GiB", "vms", i, "mem")
		set(t, d, "running", "vms", i, "boot")
	}
	if errs := errsOf(t, d, nil); len(errs) != 0 {
		t.Fatalf("validate = %v", errs)
	}
	for _, f := range renderOf(t, d) {
		for _, leaked := range []string{"hello.example", "age1recipient", "10.0.11.0", "10.0.10.99"} {
			if strings.Contains(f.Text, leaked) {
				t.Errorf("%s leaked %q", f.Name, leaked)
			}
		}
	}
}

func TestValidateReportsGarbledRowsInsteadOfRaising(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, d map[string]any)
		want   string
	}{
		{"capacity-str", func(t *testing.T, d map[string]any) { set(t, d, "16GiB", "tenants", 1, "capacity") },
			"capacity.mem — not a memory size: None"},
		{"tenant-kind-list", func(t *testing.T, d map[string]any) { set(t, d, []any{"org"}, "tenants", 1, "github", "kind") },
			"github.kind must be one of ['org', 'user'], got ['org']"},
		{"tenant-kind-int", func(t *testing.T, d map[string]any) { set(t, d, 7, "tenants", 1, "github", "kind") },
			"github.kind must be one of ['org', 'user'], got 7"},
		{"tenant-kind-null", func(t *testing.T, d map[string]any) { set(t, d, nil, "tenants", 1, "github", "kind") },
			"github.kind must be one of ['org', 'user'], got None"},
		{"platform-kind-dict", func(t *testing.T, d map[string]any) {
			set(t, d, map[string]any{"x": 1}, "platform", "github", "kind")
		}, "got {'x': 1}"},
		{"vm-role-list", func(t *testing.T, d map[string]any) { set(t, d, []any{"edge"}, "vms", 0, "role") },
			"got ['edge']"},
		{"vm-tenant-list", func(t *testing.T, d map[string]any) { set(t, d, []any{"alpha-org"}, "vms", 4, "tenant") },
			"tenant ['alpha-org'] is not a tenants[] name"},
		{"name-int", func(t *testing.T, d map[string]any) { set(t, d, 7, "tenants", 1, "name") },
			"name must match [a-z0-9][a-z0-9-]*, got 7"},
		{"target-null", func(t *testing.T, d map[string]any) { set(t, d, nil, "tenants", 1, "github", "target") },
			"github.target must be a bare owner name, got None"},
		{"project-bool", func(t *testing.T, d map[string]any) { set(t, d, true, "tenants", 1, "incus_project") },
			"incus_project must match [a-z0-9][a-z0-9-]*, got True"},
		{"tenant-row-string", func(t *testing.T, d map[string]any) { set(t, d, "x", "tenants", 1) },
			"tenants[1]: not a mapping"},
		{"vm-row-string", func(t *testing.T, d map[string]any) { set(t, d, "x", "vms", 1) },
			"vms[1]: not a mapping"},
		{"tenants-mapping", func(t *testing.T, d map[string]any) { d["tenants"] = map[string]any{"a": 1} },
			"tenants must be a non-empty list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodDoc()
			c.mutate(t, d)
			if errs := errsOf(t, d, nil); !anyContains(errs, c.want) {
				t.Errorf("want %q in %v", c.want, errs)
			}
		})
	}
}

func TestRejectsYAML11BooleansWherePythonSawABool(t *testing.T) {
	text := string(marshal(t, goodDoc()))
	for _, v := range []string{"no", "yes", "on", "off", "No", "ON"} {
		for _, c := range []struct{ anchor, want string }{
			{"incus_project: beta", "incus_project must match"},
			{"target: kappa-org", "github.target must be a bare owner name"},
		} {
			key, _, _ := strings.Cut(c.anchor, ":")
			doc := docFromText(t, mustReplace(t, text, c.anchor, key+": "+v))
			if _, errs := validate(doc, nil); !anyContains(errs, c.want) {
				t.Errorf("%s: %s accepted: %v", c.anchor, v, errs)
			}
			// Quoted, it is a string, and `no` is a valid name.
			if v != strings.ToLower(v) {
				continue
			}
			quoted := docFromText(t, mustReplace(t, text, c.anchor, key+": \""+v+"\""))
			if _, errs := validate(quoted, nil); anyContains(errs, c.want) {
				t.Errorf("%s: quoted %q rejected: %v", c.anchor, v, errs)
			}
		}
	}
}

// ---------------------------------------------------------------- render ----

const (
	wantAlphaEnv = `# /etc/bcx-broker/alpha-org.env
# GENERATED by bcx-gen-tenant-env from bartixcloud/infra/tenants.yml.
# Do not edit — re-run the generator; --check reports any difference.
BROKER_TENANT=alpha-org
BROKER_TARGET_KIND=org
BROKER_TARGET=alpha-org
BROKER_INSTALLATION_ID=102
BROKER_INCUS_PROJECT=alpha-org
BROKER_SOCKET=/run/bcx-broker/alpha-org.sock
BROKER_TIERS_FILE=/etc/bcx-broker/tiers.yml
BROKER_AGE_KEY=/etc/bartixcloud/age.key
BROKER_SECRETS_AGE=/etc/bcx-broker/alpha-org.age
BROKER_CAPACITY_MEM=16GiB
BROKER_MEM_FLOOR=6GiB
`
	wantBetaEnv = `# /etc/bcx-broker/kappa-org.env
# GENERATED by bcx-gen-tenant-env from bartixcloud/infra/tenants.yml.
# Do not edit — re-run the generator; --check reports any difference.
BROKER_TENANT=kappa-org
BROKER_TARGET_KIND=org
BROKER_TARGET=kappa-org
BROKER_INSTALLATION_ID=101
BROKER_INCUS_PROJECT=beta
BROKER_SOCKET=/run/bcx-broker/kappa-org.sock
BROKER_TIERS_FILE=/etc/bcx-broker/tiers.yml
BROKER_AGE_KEY=/etc/bartixcloud/age.key
BROKER_SECRETS_AGE=/etc/bcx-broker/kappa-org.age
BROKER_CAPACITY_MEM=20GiB
BROKER_MEM_FLOOR=6GiB
`
	wantJunoEnv = `# /etc/bcx-broker/juno.env
# GENERATED by bcx-gen-tenant-env from bartixcloud/infra/tenants.yml.
# Do not edit — re-run the generator; --check reports any difference.
BROKER_TENANT=juno
BROKER_TARGET_KIND=user
BROKER_TARGET=Juno
BROKER_REPOS=ci-repo,other.repo
BROKER_INSTALLATION_ID=103
BROKER_INCUS_PROJECT=juno
BROKER_SOCKET=/run/bcx-broker/juno.sock
BROKER_TIERS_FILE=/etc/bcx-broker/tiers.yml
BROKER_AGE_KEY=/etc/bartixcloud/age.key
BROKER_SECRETS_AGE=/etc/bcx-broker/juno.age
BROKER_CAPACITY_MEM=20GiB
BROKER_MEM_FLOOR=6GiB
`
)

// wantDispatcherJSON is what the Python generator's json.dumps(indent=2,
// sort_keys=True) emits for goodDoc and the stubbed poke table, byte for byte.
const wantDispatcherJSON = `{
  "installations": {
    "100": {
      "owner": "bartixcloud",
      "tenant": null
    },
    "101": {
      "owner": "kappa-org",
      "tenant": "kappa-org"
    },
    "102": {
      "owner": "alpha-org",
      "tenant": "alpha-org"
    },
    "103": {
      "owner": "Juno",
      "tenant": "juno"
    }
  },
  "pokes": {
    "alpha-org/infra": [
      [
        "platform",
        "ingress"
      ],
      [
        "platform",
        "bartix-db"
      ],
      [
        "alpha-org",
        "alpha-org-app"
      ]
    ],
    "bartixcloud/infra": [
      [
        "platform",
        "ingress"
      ],
      [
        "platform",
        "registry"
      ],
      [
        "platform",
        "ops"
      ],
      [
        "platform",
        "bartix-db"
      ],
      [
        "alpha-org",
        "alpha-org-app"
      ],
      [
        "beta",
        "beta-app"
      ],
      [
        "beta",
        "beta-db"
      ]
    ],
    "juno/infra": [
      [
        "platform",
        "ingress"
      ],
      [
        "platform",
        "bartix-db"
      ]
    ],
    "kappa-org/infra": [
      [
        "platform",
        "ingress"
      ],
      [
        "beta",
        "beta-app"
      ],
      [
        "beta",
        "beta-db"
      ]
    ]
  }
}
`

// ------------------------------------------------------- platform.ci (G24) ----
// The platform org runs its own CI in an Incus project of its own, served by a
// broker built from the platform's own App installation. Everything below pins
// that the block renders a broker, keeps one installation entry, and never
// reaches across into a tenants[] row's scope.

// goodDocWithCI is goodDoc plus a platform `ci:` block in its own project.
func goodDocWithCI(t *testing.T) map[string]any {
	t.Helper()
	d := goodDoc()
	set(t, d, map[string]any{
		"incus_project": "bartixcloud",
		"capacity":      map[string]any{"mem": "16GiB"},
	}, "platform", "ci")
	return d
}

func TestPlatformCIRendersItsOwnBrokerEnv(t *testing.T) {
	stubPokeTargets(t)
	files := renderOf(t, goodDocWithCI(t))
	lines := envLines(fileText(t, files, "bartixcloud.env"))
	for _, want := range []string{
		"BROKER_TENANT=bartixcloud",
		"BROKER_TARGET_KIND=org",
		"BROKER_TARGET=bartixcloud",
		"BROKER_INSTALLATION_ID=100", // the platform's own installation, not a tenant's
		"BROKER_INCUS_PROJECT=bartixcloud",
		"BROKER_SOCKET=/run/bcx-broker/bartixcloud.sock",
		"BROKER_SECRETS_AGE=/etc/bcx-broker/bartixcloud.age",
		"BROKER_CAPACITY_MEM=16GiB",
		"BROKER_MEM_FLOOR=6GiB",
	} {
		if !contains(lines, want) {
			t.Errorf("bartixcloud.env missing %q: %v", want, lines)
		}
	}
}

func TestPlatformCIRoutesItsInstallationToOneEntry(t *testing.T) {
	stubPokeTargets(t)
	inst, pk := decodeTable(t, fileText(t, renderOf(t, goodDocWithCI(t)), "dispatcher.json"))
	if len(inst) != 4 {
		t.Errorf("installations = %+v, want 4 (platform + 3 tenants)", inst)
	}
	got := inst["100"]
	if got.Owner != "bartixcloud" || got.Tenant == nil || *got.Tenant != "bartixcloud" {
		t.Errorf("installations[100] = %+v, want owner bartixcloud, tenant bartixcloud", got)
	}
	// The CI block adds no second poke: <owner>/infra is the platform's own.
	if !reflect.DeepEqual(pk["bartixcloud/infra"], pokes["bartixcloud/infra"]) {
		t.Errorf("pokes[bartixcloud/infra] = %v, want %v", pk["bartixcloud/infra"], pokes["bartixcloud/infra"])
	}
}

func TestPlatformCIWithoutTheBlockRendersNoBroker(t *testing.T) {
	stubPokeTargets(t)
	files := renderOf(t, goodDoc())
	if _, ok := files.text("bartixcloud.env"); ok {
		t.Errorf("no platform ci block, but bartixcloud.env was rendered: %v", fileNames(files))
	}
	inst, _ := decodeTable(t, fileText(t, files, "dispatcher.json"))
	if got := inst["100"]; got.Owner != "bartixcloud" || got.Tenant != nil {
		t.Errorf("installations[100] = %+v, want owner bartixcloud, no tenant", got)
	}
}

func TestPlatformCINameIsTheTargetLowerCased(t *testing.T) {
	stubPokeTargets(t)
	d := goodDocWithCI(t)
	set(t, d, "BartixCloud", "platform", "github", "target")
	files := renderOf(t, d)
	if _, ok := files.text("bartixcloud.env"); !ok {
		t.Fatalf("no bartixcloud.env: %v", fileNames(files))
	}
	lines := envLines(fileText(t, files, "bartixcloud.env"))
	if !contains(lines, "BROKER_TARGET=BartixCloud") {
		t.Errorf("BROKER_TARGET must keep the owner's spelling: %v", lines)
	}
	if !contains(lines, "BROKER_TENANT=bartixcloud") {
		t.Errorf("BROKER_TENANT must be the lower-cased target: %v", lines)
	}
}

// The platform env is the first file: the platform block is first in the
// document, and dispatcher.json stays last.
func TestPlatformCIEnvComesFirstInRenderOrder(t *testing.T) {
	stubPokeTargets(t)
	// No juno.env: its repos are [] in goodDoc, so it has no broker (C2).
	want := []string{"bartixcloud.env", "kappa-org.env", "alpha-org.env", "dispatcher.json"}
	for i := 0; i < 25; i++ {
		if got := fileNames(renderOf(t, goodDocWithCI(t))); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: order = %v, want %v", i, got, want)
		}
	}
}

func TestRejectsBadPlatformCI(t *testing.T) {
	stubPokeTargets(t)
	cases := []struct {
		name   string
		mutate func(t *testing.T, d map[string]any)
		want   string
	}{
		{"not-a-mapping", func(t *testing.T, d map[string]any) {
			set(t, d, []any{"incus_project"}, "platform", "ci")
		}, "platform.ci: must be a mapping"},
		{"shares-platforms-project", func(t *testing.T, d map[string]any) {
			set(t, d, "platform", "platform", "ci", "incus_project")
		}, "platform.ci: incus_project 'platform' is already used by platform"},
		// The ci row is noted before the tenants, so the tenant is the row the
		// pass flags; the message still names both parties.
		{"shares-a-tenants-project", func(t *testing.T, d map[string]any) {
			set(t, d, "alpha-org", "platform", "ci", "incus_project")
		}, "tenants[1]: incus_project 'alpha-org' is already used by platform.ci"},
		{"name-is-a-tenants-name", func(t *testing.T, d map[string]any) {
			renameTenant(t, d, 1, "bartixcloud")
		}, "platform.ci: name 'bartixcloud' is already used by a tenants[] row"},
		{"project-name-invalid", func(t *testing.T, d map[string]any) {
			set(t, d, "BartiXCloud", "platform", "ci", "incus_project")
		}, "platform.ci: incus_project must match [a-z0-9][a-z0-9-]*"},
		{"capacity-garbled", func(t *testing.T, d map[string]any) {
			set(t, d, map[string]any{"incus_project": "bartixcloud", "capacity": map[string]any{"mem": true}},
				"platform", "ci")
		}, "platform.ci: capacity.mem"},
		{"capacity-is-a-list", func(t *testing.T, d map[string]any) {
			set(t, d, map[string]any{"incus_project": "bartixcloud", "capacity": []any{"16GiB"}},
				"platform", "ci")
		}, "platform.ci: capacity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := goodDocWithCI(t)
			c.mutate(t, d)
			if errs := errsOf(t, d, nil); !anyContains(errs, c.want) {
				t.Errorf("missing %q in %v", c.want, errs)
			}
			if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Errorf("expected render to fail with invalid, got %v", err)
			}
		})
	}
}

func TestRejectsMissingPlatformCIProjectWhenChecked(t *testing.T) {
	incus := map[string]struct{}{"platform": {}, "beta": {}, "alpha-org": {}, "juno": {}}
	errs := errsOf(t, goodDocWithCI(t), incus)
	if !anyContains(errs, "platform.ci: incus_project 'bartixcloud' does not exist as an Incus project") {
		t.Errorf("missing platform.ci project error: %v", errs)
	}
}

func TestGoodDocWithCIValidates(t *testing.T) {
	if errs := errsOf(t, goodDocWithCI(t), nil); len(errs) != 0 {
		t.Fatalf("goodDocWithCI = %v", errs)
	}
}

func TestRenderOrgTenantEnv(t *testing.T) {
	stubPokeTargets(t)
	files := renderOf(t, goodDoc())
	got := envLines(fileText(t, files, "alpha-org.env"))
	want := envLines(wantAlphaEnv)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRenderedFilesAreByteIdenticalToThePythonGenerator(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	set(t, d, []any{"ci-repo", "other.repo"}, "tenants", 2, "github", "repos")
	files := renderOf(t, d)
	for name, want := range map[string]string{
		"alpha-org.env": wantAlphaEnv,
		"kappa-org.env": wantBetaEnv,
		"juno.env":      wantJunoEnv,
	} {
		if got := fileText(t, files, name); got != want {
			t.Errorf("%s differs:\n got: %q\nwant: %q", name, got, want)
		}
	}
	if got := fileText(t, renderOf(t, goodDoc()), "dispatcher.json"); got != wantDispatcherJSON {
		t.Errorf("dispatcher.json differs:\n got: %q\nwant: %q", got, wantDispatcherJSON)
	}
}

func TestRenderOrderIsTenantsInFileOrderThenDispatcherJSON(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	set(t, d, []any{"ci-repo"}, "tenants", 2, "github", "repos")
	want := []string{"kappa-org.env", "alpha-org.env", "juno.env", "dispatcher.json"}
	for i := 0; i < 25; i++ {
		if got := fileNames(renderOf(t, d)); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: order = %v, want %v", i, got, want)
		}
	}
}

func TestRenderUserTenantEnvCarriesRepos(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	set(t, d, []any{"some-repo"}, "tenants", 2, "github", "repos")
	lines := envLines(fileText(t, renderOf(t, d), "juno.env"))
	want := []string{"BROKER_TENANT=juno", "BROKER_TARGET_KIND=user",
		"BROKER_TARGET=Juno", "BROKER_REPOS=some-repo", "BROKER_INSTALLATION_ID=103"}
	if len(lines) < len(want) || !reflect.DeepEqual(lines[:len(want)], want) {
		t.Errorf("lines = %v, want prefix %v", lines, want)
	}
}

func decodeTable(t *testing.T, text string) (map[string]installation, map[string][][2]string) {
	t.Helper()
	var table dispatcherTable
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&table); err != nil {
		t.Fatalf("dispatcher.json: %v", err)
	}
	return table.Installations, table.Pokes
}

func TestEmptyReposUserTenantRendersNoEnvButStaysInMap(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	if errs := errsOf(t, d, nil); len(errs) != 0 {
		t.Fatalf("validate = %v", errs)
	}
	files := renderOf(t, d)
	if _, present := files.text("juno.env"); present {
		t.Error("juno.env should not be rendered")
	}
	inst, pk := decodeTable(t, fileText(t, files, "dispatcher.json"))
	got := inst["103"]
	if got.Owner != "Juno" || got.Tenant == nil || *got.Tenant != "juno" {
		t.Errorf("installations[103] = %+v", got)
	}
	want := [][2]string{{"platform", "ingress"}, {"platform", "bartix-db"}}
	if !reflect.DeepEqual(pk["juno/infra"], want) {
		t.Errorf("pokes[juno/infra] = %v, want %v", pk["juno/infra"], want)
	}
}

func TestRenderNeverEmitsPortsOrWebhookHosts(t *testing.T) {
	stubPokeTargets(t)
	for _, f := range renderOf(t, goodDoc()) {
		if strings.HasSuffix(f.Name, ".env") {
			if strings.Contains(f.Text, "PORT") || strings.Contains(f.Text, "WEBHOOK") || strings.Contains(f.Text, "LISTEN") {
				t.Errorf("%s leaked PORT/WEBHOOK/LISTEN", f.Name)
			}
		}
	}
}

func TestRenderRefusesInvalidDocument(t *testing.T) {
	stubPokeTargets(t)
	d := goodDoc()
	set(t, d, "team", "tenants", 0, "github", "kind")
	if err := renderErr(t, d); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("expected invalid error, got %v", err)
	}
}

func TestDispatcherMap(t *testing.T) {
	stubPokeTargets(t)
	inst, pk := decodeTable(t, fileText(t, renderOf(t, goodDoc()), "dispatcher.json"))
	owner := func(s string) *string { return &s }
	want := map[string]installation{
		"100": {Owner: "bartixcloud"},
		"101": {Owner: "kappa-org", Tenant: owner("kappa-org")},
		"102": {Owner: "alpha-org", Tenant: owner("alpha-org")},
		"103": {Owner: "Juno", Tenant: owner("juno")},
	}
	if !reflect.DeepEqual(inst, want) {
		t.Errorf("installations = %+v, want %+v", inst, want)
	}
	if !reflect.DeepEqual(pk, pokes) {
		t.Errorf("pokes = %v, want %v", pk, pokes)
	}
}

// ---------------------------------------------------------- poke_targets ----

// fakePython3 puts a python3 on PATH that records its argv, one argument per
// line, and prints the given poke-targets output.
func fakePython3(t *testing.T, output string) (argvFile string) {
	t.Helper()
	argvFile = filepath.Join(t.TempDir(), "argv")
	fixture := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(fixture, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ARGV_FILE", argvFile)
	t.Setenv("FAKE_STDOUT_FILE", fixture)
	fakeBin(t, map[string]string{"python3": `printf '%s\n' "$@" > "$FAKE_ARGV_FILE"
cat "$FAKE_STDOUT_FILE"
`})
	return argvFile
}

func readArgv(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("python3 was never run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestPokeTargetsShellsOutToC2CLI(t *testing.T) {
	dir := t.TempDir()
	argvFile := fakePython3(t, "platform ingress\nplatform bartix-db\n")
	yml := filepath.Join(dir, "tenants.yml")

	got, err := pokeTargets(yml, "Juno/infra")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"platform", "ingress"}, {"platform", "bartix-db"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
	// Verbatim repo: P3's CLI matches the owner's own spelling case-sensitively.
	wantArgv := []string{filepath.Join(dir, "fleet", "tenants.py"), "poke-targets", yml, "Juno/infra"}
	if gotArgv := readArgv(t, argvFile); !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Errorf("python3 argv = %q, want %q", gotArgv, wantArgv)
	}
}

// A relative tenants.yml with '..' cleaned into a leading '-' made python3 read
// the directory name as `-c CODE`.
func TestPokeTargetsScriptPathIsAbsoluteSoItCanNeverBeAnOption(t *testing.T) {
	argvFile := fakePython3(t, "platform ingress\n")
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const rel = "x/../-cprint(1)#/tenants.yml"
	if _, err := pokeTargets(rel, "x/infra"); err != nil {
		t.Fatal(err)
	}
	argv := readArgv(t, argvFile)
	if want := filepath.Join(cwd, "-cprint(1)#", "fleet", "tenants.py"); argv[0] != want {
		t.Errorf("script argument = %q, want %q", argv[0], want)
	}
	if argv[2] != rel {
		t.Errorf("tenants.yml argument = %q, want it verbatim %q", argv[2], rel)
	}
}

func TestPokeTargetsParsesLikePythonSplitlines(t *testing.T) {
	// CR, U+2028 and blank/whitespace-only lines, as str.splitlines() sees them.
	fakePython3(t, "platform ingress\rplatform ops\u2028platform registry\n\n   \nbeta\tbeta-db\r\n")
	got, err := pokeTargets(filepath.Join(t.TempDir(), "tenants.yml"), "x/infra")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"platform", "ingress"}, {"platform", "ops"}, {"platform", "registry"}, {"beta", "beta-db"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestPokeTargetsRefusesBadLine(t *testing.T) {
	for _, line := range []string{"platform\n", "platform ingress extra\n"} {
		fakePython3(t, line)
		_, err := pokeTargets(filepath.Join(t.TempDir(), "tenants.yml"), "x/infra")
		if err == nil || !strings.Contains(err.Error(), "poke-targets") || !strings.Contains(err.Error(), strings.TrimSpace(line)) {
			t.Errorf("%q: expected poke-targets error naming the line, got %v", line, err)
		}
	}
}

func TestPokeTargetsReportsTheCLIsOwnFailure(t *testing.T) {
	fakeBin(t, map[string]string{"python3": "echo 'unknown repo x/infra' >&2\nexit 2\n"})
	_, err := pokeTargets(filepath.Join(t.TempDir(), "tenants.yml"), "x/infra")
	if err == nil || !strings.Contains(err.Error(), "x/infra: unknown repo x/infra") {
		t.Errorf("stderr not surfaced: %v", err)
	}
	fakeBin(t, map[string]string{"python3": "exit 3\n"})
	_, err = pokeTargets(filepath.Join(t.TempDir(), "tenants.yml"), "x/infra")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || strings.Contains(err.Error(), "cannot run") {
		t.Errorf("silent failure not reported as an exit status: %v", err)
	}
}

func TestPokeTargetsRaisesWhenCLICannotRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no python3 anywhere
	_, err := pokeTargets(filepath.Join(t.TempDir(), "tenants.yml"), "x/infra")
	if err == nil || !strings.Contains(err.Error(), "cannot run") || !strings.Contains(err.Error(), "python3") {
		t.Errorf("expected 'cannot run ... python3', got %v", err)
	}
}

func TestPokeTargetsRefusesMissingTenantsYmlPath(t *testing.T) {
	if _, err := pokeTargets("", "x/infra"); err == nil || !strings.Contains(err.Error(), "tenants_yml") {
		t.Errorf("expected tenants_yml error, got %v", err)
	}
}

// The dispatcher rejects its whole table on one bad name, which would drop every
// webhook after install.sh restarts it, so gen must refuse first.
func TestRenderRefusesPokeNamesTheDispatcherWouldReject(t *testing.T) {
	cases := map[string]struct{ project, vm, offender string }{
		"leading-dash-project": {"-platform", "ingress", `project "-platform"`},
		"leading-dash-vm":      {"platform", "-x", `vm "-x"`},
		"dotdot-vm":            {"p", "../x", `vm "../x"`},
		"leading-dot-project":  {".hidden", "vm", `project ".hidden"`},
		"slash-vm":             {"p", "v/m", `vm "v/m"`},
		"non-ascii-project":    {"pläform", "ingress", `project "pläform"`},
		"shell-meta-vm":        {"platform", "in;gress", `vm "in;gress"`},
		"html-vm":              {"platform", "in<gr>ess", `vm "in<gr>ess"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			orig := pokeTargets
			pokeTargets = func(_, repo string) ([][2]string, error) {
				if repo == "alpha-org/infra" {
					return [][2]string{{"platform", "ingress"}, {c.project, c.vm}}, nil
				}
				return pokes[strings.ToLower(repo)], nil
			}
			t.Cleanup(func() { pokeTargets = orig })
			err := renderErr(t, goodDoc())
			if err == nil {
				t.Fatalf("%q %q accepted", c.project, c.vm)
			}
			for _, want := range []string{"alpha-org/infra", c.offender, "is not a valid name"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestRenderAcceptsEveryNameTheDispatcherAccepts(t *testing.T) {
	orig := pokeTargets
	pokeTargets = func(_, _ string) ([][2]string, error) {
		return [][2]string{{"Platform.1", "vm_2-a.x"}, {"0", "9"}}, nil
	}
	t.Cleanup(func() { pokeTargets = orig })
	_, pk := decodeTable(t, fileText(t, renderOf(t, goodDoc()), "dispatcher.json"))
	if want := [][2]string{{"Platform.1", "vm_2-a.x"}, {"0", "9"}}; !reflect.DeepEqual(pk["alpha-org/infra"], want) {
		t.Errorf("pokes = %v, want %v", pk["alpha-org/infra"], want)
	}
}

func TestRenderDispatcherMapChecksTenantAndOwnerNamesToo(t *testing.T) {
	stubPokeTargets(t)
	cfg := &config{
		Platform: github{Kind: kindOrg, Target: "bartixcloud", InstallationID: 100},
		Tenants:  []tenant{{Name: "-t", GitHub: github{Kind: kindOrg, Target: "owner", InstallationID: 101}}},
	}
	if _, err := renderDispatcherMap(cfg, ""); err == nil || !strings.Contains(err.Error(), `tenant "-t"`) {
		t.Errorf("bad tenant accepted: %v", err)
	}
	cfg.Tenants[0] = tenant{Name: "t", GitHub: github{Kind: kindOrg, Target: "-owner", InstallationID: 101}}
	if _, err := renderDispatcherMap(cfg, ""); err == nil || !strings.Contains(err.Error(), `owner "-owner"`) {
		t.Errorf("bad owner accepted: %v", err)
	}
	cfg.Platform.Target = ".x"
	if _, err := renderDispatcherMap(cfg, ""); err == nil || !strings.Contains(err.Error(), `platform owner ".x"`) {
		t.Errorf("bad platform owner accepted: %v", err)
	}
}

func TestMainBakesPokeTableThroughRealCLIWrapper(t *testing.T) {
	dir := t.TempDir()
	fleet := filepath.Join(dir, "fleet")
	if err := os.MkdirAll(fleet, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "import sys\n" +
		"assert sys.argv[1] == 'poke-targets', sys.argv\n" +
		"print('platform ingress')\n" +
		"print(sys.argv[3].split('/')[0] + ' ' + sys.argv[3].split('/')[0] + '-app')\n"
	if err := os.WriteFile(filepath.Join(fleet, "tenants.py"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())

	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("run = %+v", r)
	}
	data, err := os.ReadFile(filepath.Join(out, "dispatcher.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, pk := decodeTable(t, string(data))
	want := [][2]string{{"platform", "ingress"}, {"alpha-org", "alpha-org-app"}}
	if !reflect.DeepEqual(pk["alpha-org/infra"], want) {
		t.Errorf("pokes[alpha-org/infra] = %v, want %v", pk["alpha-org/infra"], want)
	}
	keys := []string{}
	for k := range pk {
		keys = append(keys, k)
	}
	if len(keys) != 4 {
		t.Errorf("poke keys = %v, want the four <owner>/infra repos", keys)
	}
	for _, k := range []string{"bartixcloud/infra", "kappa-org/infra", "alpha-org/infra", "juno/infra"} {
		if _, ok := pk[k]; !ok {
			t.Errorf("missing poke key %s in %v", k, keys)
		}
	}
}

func writeFakeTenantsPy(t *testing.T, dir, body string) {
	t.Helper()
	fleet := filepath.Join(dir, "fleet")
	if err := os.MkdirAll(fleet, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fleet, "tenants.py"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestMainExits2AndWritesNothingWhenPokeCLIFails(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	// No fleet/tenants.py -> python3 cannot open it.
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 2 {
		t.Errorf("run = %+v, want 2", r)
	}
	assertAbsent(t, out)
}

func TestMainBakesMixedCaseOwnerThroughCaseSensitiveCLI(t *testing.T) {
	dir := t.TempDir()
	writeFakeTenantsPy(t, dir, "import sys\n"+
		"KNOWN = {'bartixcloud/infra', 'kappa-org/infra', 'alpha-org/infra', 'Juno/infra'}\n"+
		"if sys.argv[3] in KNOWN:\n    print('platform ingress')\n")
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("run = %+v", r)
	}
	data, err := os.ReadFile(filepath.Join(out, "dispatcher.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, pk := decodeTable(t, string(data))
	if want := [][2]string{{"platform", "ingress"}}; !reflect.DeepEqual(pk["juno/infra"], want) {
		t.Errorf("pokes[juno/infra] = %v, want %v", pk["juno/infra"], want)
	}
	for k, v := range pk {
		if len(v) == 0 {
			t.Errorf("pokes[%s] is empty", k)
		}
	}
}

func TestMainExits2WhenPokeCLIReturnsNoTargets(t *testing.T) {
	dir := t.TempDir()
	writeFakeTenantsPy(t, dir, "pass\n")
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 2 {
		t.Errorf("run = %+v, want 2", r)
	}
	assertAbsent(t, out)
}

func TestMainExits2AndWritesNothingOnAPokeNameTheDispatcherWouldReject(t *testing.T) {
	dir := t.TempDir()
	writeFakeTenantsPy(t, dir, "print('-platform ingress')\n")
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	r := runCLI(yml, "--out", out, "--no-incus-check")
	if r.code != 2 || !strings.Contains(r.stderr, `project "-platform" is not a valid name`) {
		t.Errorf("run = %+v", r)
	}
	assertAbsent(t, out)
}

func TestRenderRefusesEmptyPokeListNamingRepo(t *testing.T) {
	orig := pokeTargets
	pokeTargets = func(_, repo string) ([][2]string, error) {
		if repo == "Juno/infra" {
			return nil, nil
		}
		return pokes[strings.ToLower(repo)], nil
	}
	t.Cleanup(func() { pokeTargets = orig })
	if err := renderErr(t, goodDoc()); err == nil || !strings.Contains(err.Error(), "Juno/infra") {
		t.Errorf("expected Juno/infra error, got %v", err)
	}
}

// ------------------------------------------------------------------ main ----

func TestMainWritesEveryFile0600(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	r := runCLI(yml, "--out", out, "--no-incus-check")
	if r.code != 0 {
		t.Fatalf("run = %+v", r)
	}
	want := []string{"alpha-org.env", "dispatcher.json", "kappa-org.env"}
	if got := listDir(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want exactly %v (no temp files left behind)", got, want)
	}
	for _, name := range want {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
	wantOut := "wrote " + filepath.Join(out, "kappa-org.env") + "\n" +
		"wrote " + filepath.Join(out, "alpha-org.env") + "\n" +
		"wrote " + filepath.Join(out, "dispatcher.json") + "\n"
	if r.stdout != wantOut {
		t.Errorf("stdout = %q, want %q (tenants in file order, dispatcher.json last)", r.stdout, wantOut)
	}
}

func TestEveryRunNormalisesTheModeToZeroSixHundred(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	for name, mode := range map[string]os.FileMode{
		"alpha-org.env": 0o644, "kappa-org.env": 0o666, "dispatcher.json": 0o640,
	} {
		if err := os.Chmod(filepath.Join(out, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("second run = %+v", r)
	}
	for _, name := range []string{"alpha-org.env", "kappa-org.env", "dispatcher.json"} {
		info, err := os.Stat(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o after a re-write, want 600", name, info.Mode().Perm())
		}
	}
}

func TestFilesAreWrittenAtZeroSixHundredWhateverTheUmask(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := writeYAMLIn(t, dir, goodDoc())
	old := setUmask(0o277)
	defer setUmask(old)
	for i := 0; i < 2; i++ { // the second run must be able to rewrite what the first wrote
		if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
			t.Fatalf("run %d = %+v", i, r)
		}
	}
	info, err := os.Stat(filepath.Join(out, "dispatcher.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o under umask 277, want 600", info.Mode().Perm())
	}
}

func TestOutputIsByteDeterministic(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	d := goodDoc()
	set(t, d, []any{"ci-repo", "other.repo"}, "tenants", 2, "github", "repos")
	yml := writeYAMLIn(t, dir, d)
	var first map[string]string
	for i := 0; i < 5; i++ {
		out := filepath.Join(dir, "out", string(rune('a'+i)))
		if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
			t.Fatalf("run = %+v", r)
		}
		got := map[string]string{}
		for _, name := range listDir(t, out) {
			b, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			got[name] = string(b)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs from run 0", i)
		}
	}
	if first["juno.env"] != wantJunoEnv || first["dispatcher.json"] == "" {
		t.Errorf("unexpected artifacts: %v", first)
	}
}

func TestAFailedWriteLeavesNoTempFileAndKeepsTheOldContent(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	// A directory where dispatcher.json should go makes the final rename fail
	// after the temp file was written.
	blocker := filepath.Join(out, "dispatcher.json")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(blocker, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runCLI(yml, "--out", out, "--no-incus-check")
	if r.code != 2 {
		t.Errorf("run = %+v, want 2", r)
	}
	for _, name := range listDir(t, out) {
		if strings.HasSuffix(name, ".tmp") {
			t.Errorf("temp file %s left behind", name)
		}
	}
}

func TestWriteIsAtomicAgainstAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	before, err := os.ReadFile(filepath.Join(out, "alpha-org.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(out, 0o700) })
	changed := goodDoc()
	set(t, changed, "32GiB", "tenants", 1, "capacity", "mem")
	if r := runCLI(writeYAMLIn(t, dir, changed), "--out", out, "--no-incus-check"); r.code != 2 {
		t.Errorf("run = %+v, want 2", r)
	}
	after, err := os.ReadFile(filepath.Join(out, "alpha-org.env"))
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("alpha-org.env changed by a failed run: %v", err)
	}
}

func TestMainCheckModeDetectsHandEdit(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	if r := runCLI(yml, "--out", out, "--check", "--no-incus-check"); r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatalf("check clean run = %+v", r)
	}
	path := filepath.Join(out, "dispatcher.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runCLI(yml, "--out", out, "--check", "--no-incus-check")
	if r.code != 1 || r.stderr != path+": differs from generated content\n" {
		t.Errorf("check dirty run = %+v", r)
	}
}

func TestCheckNeverWrites(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())

	// Nothing there yet: drift (exit 1), and --check must not create --out.
	r := runCLI(yml, "--out", out, "--check", "--no-incus-check")
	if r.code != 1 {
		t.Errorf("check on a missing out dir = %+v, want 1", r)
	}
	assertAbsent(t, out)
	for _, name := range []string{"kappa-org.env", "alpha-org.env", "dispatcher.json"} {
		if !strings.Contains(r.stderr, filepath.Join(out, name)) {
			t.Errorf("stderr does not name %s: %q", name, r.stderr)
		}
	}

	// A drifted, stale, partly missing directory is reported and left untouched.
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(out, "old.env")
	if err := os.WriteFile(stale, []byte(generatedMark+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(out, "alpha-org.env")
	if err := os.WriteFile(drifted, []byte("hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		got := map[string]string{}
		for _, name := range listDir(t, out) {
			info, err := os.Stat(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			got[name] = info.Mode().String() + info.ModTime().String() + string(b)
		}
		return got
	}
	before := snapshot()
	if r := runCLI(yml, "--out", out, "--check", "--no-incus-check"); r.code != 1 {
		t.Errorf("check = %+v, want 1", r)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("--check changed the directory:\nbefore %v\nafter  %v", before, after)
	}
}

func withJunoRepos(t *testing.T, repos []any) map[string]any {
	t.Helper()
	d := goodDoc()
	set(t, d, repos, "tenants", 2, "github", "repos")
	return d
}

func TestCheckReportsEnvRenderNoLongerProduces(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	with := writeYAMLIn(t, dir, withJunoRepos(t, []any{"ci-repo"}))
	if r := runCLI(with, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(out, "juno.env")); err != nil {
		t.Fatal("juno.env should exist")
	}
	without := writeYAMLIn(t, dir, withJunoRepos(t, []any{}))
	r := runCLI(without, "--out", out, "--check", "--no-incus-check")
	want := filepath.Join(out, "juno.env") + ": not produced by this render (stale)\n"
	if r.code != 1 || r.stderr != want {
		t.Errorf("check = %+v, want code 1 and stderr %q", r, want)
	}
}

func TestWriteRemovesStaleGeneratedEnvThenCheckClean(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	with := writeYAMLIn(t, dir, withJunoRepos(t, []any{"ci-repo"}))
	if r := runCLI(with, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	without := writeYAMLIn(t, dir, withJunoRepos(t, []any{}))
	r := runCLI(without, "--out", out, "--no-incus-check")
	if r.code != 0 {
		t.Fatalf("write run = %+v", r)
	}
	gone := filepath.Join(out, "juno.env")
	assertAbsent(t, gone)
	if !strings.Contains(r.stdout, "removed "+gone+"\n") {
		t.Errorf("stdout lacks the removed line: %q", r.stdout)
	}
	if !strings.HasSuffix(r.stdout, "removed "+gone+"\n") {
		t.Errorf("removals must come after every write: %q", r.stdout)
	}
	if c := runCLI(without, "--out", out, "--check", "--no-incus-check"); c.code != 0 {
		t.Errorf("check clean run = %+v", c)
	}
}

func TestRemovedTenantLeavesNoEnvBehind(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(out, "kappa-org.env")); err != nil {
		t.Fatal("kappa-org.env should exist")
	}
	d := goodDoc()
	d["tenants"] = []any{at(t, d, "tenants", 1), at(t, d, "tenants", 2)}
	d["vms"] = []any{
		at(t, d, "vms", 0), at(t, d, "vms", 1), at(t, d, "vms", 2), at(t, d, "vms", 3),
		at(t, d, "vms", 4), at(t, d, "vms", 5),
	}
	smaller := writeYAMLIn(t, dir, d)
	if r := runCLI(smaller, "--out", out, "--check", "--no-incus-check"); r.code != 1 {
		t.Errorf("check run = %+v, want 1", r)
	}
	if r := runCLI(smaller, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("write run = %+v", r)
	}
	if got, want := listDir(t, out), []string{"alpha-org.env", "dispatcher.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestWriteRefusesEnvItDidNotGenerate(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(out, "legacy.env")
	if err := os.WriteFile(foreign, []byte("TOKEN=hand-written\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--check", "--no-incus-check"); r.code != 1 {
		t.Errorf("check run = %+v, want 1", r)
	}
	r := runCLI(yml, "--out", out, "--no-incus-check")
	if r.code != 2 || !strings.Contains(r.stderr, foreign) {
		t.Errorf("write run = %+v, want 2 naming %s", r, foreign)
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "TOKEN=hand-written\n" {
		t.Errorf("foreign file was modified: %v", err)
	}
	if got := listDir(t, out); !reflect.DeepEqual(got, []string{"legacy.env"}) {
		t.Errorf("out contains %v, want only legacy.env", got)
	}
}

func TestMainExits2OnInvalidDocument(t *testing.T) {
	d := goodDoc()
	set(t, d, "<id>", "tenants", 0, "github", "installation_id")
	dir := t.TempDir()
	yml := writeYAMLIn(t, dir, d)
	out := filepath.Join(dir, "o")
	for _, extra := range [][]string{{}, {"--check"}} {
		r := runCLI(append([]string{yml, "--out", out, "--no-incus-check"}, extra...)...)
		if r.code != 2 || !strings.Contains(r.stderr, "tenants.yml is invalid") {
			t.Errorf("%v: run = %+v, want 2", extra, r)
		}
	}
	assertAbsent(t, out)
}

func TestMainExits2OnUnparsableOrMissingTenantsYml(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(bad, []byte("tenants: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "o")
	multi := filepath.Join(dir, "multi.yml")
	if err := os.WriteFile(multi, append(append(marshal(t, goodDoc()), "---\n"...), marshal(t, goodDoc())...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, yml := range []string{bad, filepath.Join(dir, "absent.yml"), multi} {
		for _, extra := range [][]string{{}, {"--check"}} {
			r := runCLI(append([]string{yml, "--out", out, "--no-incus-check"}, extra...)...)
			if r.code != 2 || !strings.Contains(r.stderr, yml) {
				t.Errorf("%s %v: run = %+v, want 2", yml, extra, r)
			}
		}
	}
	assertAbsent(t, out)
}

// install.sh and drift.sh read exit 1 as drift, so an --out that cannot be
// listed must be a refusal, not "no stale files".
func TestAnOutPathThatCannotBeListedIsRefusedNotIgnored(t *testing.T) {
	stubPokeTargets(t)
	dir := t.TempDir()
	notADir := filepath.Join(dir, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := writeYAMLIn(t, dir, goodDoc())
	for _, extra := range [][]string{{}, {"--check"}} {
		r := runCLI(append([]string{yml, "--out", notADir, "--no-incus-check"}, extra...)...)
		if r.code != 2 || !strings.Contains(r.stderr, notADir) {
			t.Errorf("%v: run = %+v, want 2 naming the path", extra, r)
		}
	}
}

func TestAnUnreadableOutDirectoryHidesNoStaleEnv(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	stubPokeTargets(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", out, "--no-incus-check"); r.code != 0 {
		t.Fatalf("first run = %+v", r)
	}
	if err := os.WriteFile(filepath.Join(out, "legacy.env"), []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(out, 0o700) })
	for _, extra := range [][]string{{}, {"--check"}} {
		if r := runCLI(append([]string{yml, "--out", out, "--no-incus-check"}, extra...)...); r.code != 2 {
			t.Errorf("%v: run = %+v, want 2", extra, r)
		}
	}
}

// ----------------------------------------------------------------- incus ----

func TestIncusProjectRule(t *testing.T) {
	stubPokeTargets(t)
	projects := `[{"name":"platform","x":1},{"name":"beta"},{"name":"alpha-org"},{"name":"juno"}]`
	cases := []struct {
		name   string
		script string
		want   int
	}{
		{"all-present", "echo '" + projects + "'\n", 0},
		{"one-missing", `echo '[{"name":"platform"},{"name":"beta"},{"name":"alpha-org"}]'` + "\n", 2},
		{"empty-list", "echo '[]'\n", 2},
		{"empty-output", "exit 0\n", 2},
		{"object-not-list", "echo '{}'\n", 2},
		{"not-json", "echo 'not json'\n", 2},
		{"project-without-name", `echo '[{"x":1}]'` + "\n", 2},
		{"json-null", "echo null\n", 2},
		{"incus-fails-rule-skipped", "echo boom >&2\nexit 1\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeBin(t, map[string]string{"incus": c.script})
			dir := t.TempDir()
			out := filepath.Join(dir, "out")
			r := runCLI(writeYAMLIn(t, dir, goodDoc()), "--out", out)
			if r.code != c.want {
				t.Errorf("run = %+v, want %d", r, c.want)
			}
			if c.want == 2 {
				assertAbsent(t, out)
			}
		})
	}
}

func TestIncusNotInstalledSkipsTheRuleButNoIncusCheckAlwaysDoes(t *testing.T) {
	stubPokeTargets(t)
	t.Setenv("PATH", t.TempDir()) // no incus
	dir := t.TempDir()
	yml := writeYAMLIn(t, dir, goodDoc())
	if r := runCLI(yml, "--out", filepath.Join(dir, "a")); r.code != 0 {
		t.Errorf("incus missing: run = %+v, want 0", r)
	}
	fakeBin(t, map[string]string{"incus": "echo '[]'\n"})
	if r := runCLI(yml, "--out", filepath.Join(dir, "b"), "--no-incus-check"); r.code != 0 {
		t.Errorf("--no-incus-check: run = %+v, want 0", r)
	}
}
