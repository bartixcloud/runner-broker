package deploytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The module's go.mod is owned by another agent and keeps yaml.v3 marked "// indirect"
// until its own code imports it; this proves tests may import it regardless.
func TestYAMLImportable(t *testing.T) {
	var got struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte("name: bcx\n"), &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if got.Name != "bcx" {
		t.Fatalf("name = %q, want bcx", got.Name)
	}
}

func TestManifestIsWellFormed(t *testing.T) {
	roles := map[string]bool{}
	names := map[string]bool{}
	for _, b := range loadManifest(t) {
		if roles[b.Role] || names[b.Name] {
			t.Errorf("duplicate role or name in manifest: %+v", b)
		}
		roles[b.Role], names[b.Name] = true, true
		if !strings.HasPrefix(b.Name, "bcx-") {
			t.Errorf("%s: installed name must start with bcx-", b.Name)
		}
		if !strings.HasPrefix(b.Package, "./cmd/") {
			t.Errorf("%s: package %q must live under ./cmd/", b.Name, b.Package)
		}
	}
	for _, want := range []string{"dispatcher", "broker", "reaper", "generator", "appauth"} {
		if !roles[want] {
			t.Errorf("manifest has no %s role", want)
		}
	}
}

func TestUnitParserKeepsRepeatedKeys(t *testing.T) {
	path := filepath.Join(shortTmp(t), "x.service")
	body := "[Unit]\nDescription=x\n\n[Service]\nEnvironmentFile=/a\nEnvironmentFile=/b\nExecStart=/bin/true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	u := parseUnit(t, path)
	got := u.values("Service", "EnvironmentFile")
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("EnvironmentFile = %v, want [/a /b]", got)
	}
	if v := u.values("Unit", "Description"); len(v) != 1 || v[0] != "x" {
		t.Fatalf("Description = %v", v)
	}
}

func TestRunCmdReportsExitCodeAndStreams(t *testing.T) {
	res := runCmd(t, shortTmp(t), []string{"BCX_T=1"}, "bash", "-c", "echo out; echo err >&2; test \"$BCX_T\" = 1; exit 3")
	if res.Code != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("got %+v", res)
	}
}
