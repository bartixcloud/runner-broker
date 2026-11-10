package deploytest

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func bDeployDoc(t *testing.T) string {
	t.Helper()
	return string(bReadFile(t, filepath.Join(repoRoot(t), "DEPLOY.md")))
}

func TestDeployDocNamesEveryInstalledBinaryAndUnit(t *testing.T) {
	doc := bDeployDoc(t)
	for _, b := range loadManifest(t) {
		if !strings.Contains(doc, "`"+b.Name+"`") {
			t.Errorf("DEPLOY.md does not name the installed binary `%s`", b.Name)
		}
	}
	names := append([]string{"install.sh", "build.sh", "broker-firewall.sh", "nat-ci-firewall.sh", "tiers.yml", "binaries.txt"}, bShippedUnits...)
	for _, name := range names {
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("DEPLOY.md does not name `%s`", name)
		}
	}
}

// The 10-03 plan cited a spec section that does not exist; the canonical spec puts
// CI/dispatcher in 3.3 and the L0 layer in 2.2.
func TestDeployDocCitesSpecSectionsThatExist(t *testing.T) {
	doc := bDeployDoc(t)
	for _, want := range []string{"§3.3", "§2.2"} {
		if !strings.Contains(doc, want) {
			t.Errorf("DEPLOY.md does not cite %s", want)
		}
	}
	if strings.Contains(doc, "§9.2") {
		t.Errorf("DEPLOY.md cites §9.2, which does not exist in the spec")
	}
}

func TestDeployDocHasNoPythonInstallInstructions(t *testing.T) {
	stale := regexp.MustCompile(`\b(dispatcher|broker|reap|mem|app_auth|gen_tenant_env)\.py\b|uv run`)
	if m := stale.FindString(bDeployDoc(t)); m != "" {
		t.Errorf("DEPLOY.md still refers to the Python layout: %q", m)
	}
}
