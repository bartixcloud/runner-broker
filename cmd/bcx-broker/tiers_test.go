package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bartixcloud/runner-broker/mem"
)

// tierYAML renders one tier mapping, indented under `tiers:`; fields override
// the valid defaults and a nil-string field is omitted entirely.
func tierYAML(name string, fields map[string]string) string {
	defaults := map[string]string{
		"image": `runner-base`, "network": `nat-ci`, "runner_group": `build-ephemeral`,
		"cpu": `"2"`, "mem": `"4GiB"`, "max_runners": `4`, "allowlist": `["*"]`, "secrets": `[]`,
		"backend": "-",
	}
	for k, v := range fields {
		defaults[k] = v
	}
	known := []string{"image", "network", "runner_group", "cpu", "mem", "max_runners", "allowlist", "secrets", "backend"}
	var extra []string
	for k := range defaults {
		if !slices.Contains(known, k) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	var b strings.Builder
	b.WriteString("tiers:\n  " + name + ":\n")
	for _, k := range append(known, extra...) {
		if defaults[k] != "-" { // "-" omits the key
			b.WriteString("    " + k + ": " + defaults[k] + "\n")
		}
	}
	return b.String()
}

func loadTierYAML(t *testing.T, content string) (map[string]tier, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tiers.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadTiers(p)
}

func TestLoadTiersParsesKnownTiers(t *testing.T) {
	tiers, err := loadTierYAML(t, tierYAML("build", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := tier{Image: "runner-base", Network: "nat-ci", RunnerGroup: "build-ephemeral", CPU: "2", Mem: "4GiB",
		MaxRunners: 4, Allowlist: []string{"*"}, Secrets: []string{}}
	if len(tiers) != 1 || !reflect.DeepEqual(tiers["build"], want) {
		t.Errorf("got %+v, want %+v", tiers, want)
	}
}

func TestLoadTiersParsesTheRealTiersFile(t *testing.T) {
	tiers, err := loadTiers("../../tiers.yml")
	if err != nil {
		t.Fatal(err)
	}
	// build + dependabot: the infra tier was retired with its image builder.
	b, ok := tiers["build"]
	if _, dep := tiers["dependabot"]; len(tiers) != 2 || !dep || !ok || b.MaxRunners != 4 || len(b.Allowlist) != 1 || b.Allowlist[0] != "*" || len(b.Secrets) != 0 {
		t.Errorf("got %+v", tiers)
	}
}

// Python rendered YAML numbers through an f-string, so `cpu: 2` meant
// limits.cpu=2; Go dropped the value and launched with an empty limit, and
// `mem: 4096` (bytes) became a mem the parser refused on every reservation.
func TestLoadTiersRendersYAMLNumbersLikePython(t *testing.T) {
	cases := []struct {
		name, cpu, mem, wantCPU, wantMem string
	}{
		{"int cpu", `2`, `"4GiB"`, "2", "4GiB"},
		{"float cpu", `1.5`, `"4GiB"`, "1.5", "4GiB"},
		{"integral float cpu", `2.0`, `"4GiB"`, "2.0", "4GiB"},
		{"int mem is bytes", `"2"`, `4096`, "2", "4096"},
		{"float mem", `"2"`, `4.5`, "2", "4.5"},
		{"quoted number stays a string", `"2"`, `"4096"`, "2", "4096"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tiers, err := loadTierYAML(t, tierYAML("build", map[string]string{"cpu": c.cpu, "mem": c.mem}))
			if err != nil {
				t.Fatal(err)
			}
			if got := tiers["build"]; got.CPU != c.wantCPU || got.Mem != c.wantMem {
				t.Errorf("cpu=%q mem=%q, want cpu=%q mem=%q", got.CPU, got.Mem, c.wantCPU, c.wantMem)
			}
		})
	}
}

// test_parse_mem_bytes_bare_number_is_bytes at the tiers.yml level.
func TestLoadTiersIntMemIsBytesEndToEnd(t *testing.T) {
	tiers, err := loadTierYAML(t, tierYAML("build", map[string]string{"mem": "4096"}))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := mem.ParseMemBytes(tiers["build"].Mem); err != nil || n != 4096 {
		t.Errorf("ParseMemBytes(%q) = (%d, %v), want 4096", tiers["build"].Mem, n, err)
	}
	freshState(t)
	swap(t, &runningRunners, func(string) ([]string, error) { return nil, nil })
	swap(t, &availableBytes, func() (uint64, error) { return 1 << 40, nil })
	swap(t, &memFloorBytes, 0)
	if !tryReserve("build", tiers) {
		t.Error("an int mem must be admitted, not silently refused")
	}
}

func TestLoadTiersRejectsMalformedTiers(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string // substring of the error
	}{
		{"missing key", map[string]string{"network": "-"}, "network"},
		{"scalar allowlist", map[string]string{"allowlist": `"alpha-org/platform"`}, "allowlist"},
		{"empty allowlist", map[string]string{"allowlist": `[]`}, "allowlist"},
		{"non-string allowlist entry", map[string]string{"allowlist": `[1]`}, "allowlist"},
		{"scalar secrets", map[string]string{"secrets": `INFRA_HCLOUD_TOKEN`}, "secrets"},
		// secrets: [1] used to panic on an unchecked s.(string) at startup.
		{"int secret", map[string]string{"secrets": `[1]`}, "secrets"},
		{"null secret", map[string]string{"secrets": `[~]`}, "secrets"},
		{"nested secret", map[string]string{"secrets": `[[a]]`}, "secrets"},
		{"mapping secrets", map[string]string{"secrets": `{a: b}`}, "secrets"},
		{"null secrets", map[string]string{"secrets": `~`}, "secrets"},
		{"string max_runners", map[string]string{"max_runners": `"4"`}, "max_runners"},
		{"zero max_runners", map[string]string{"max_runners": `0`}, "max_runners"},
		{"negative max_runners", map[string]string{"max_runners": `-2`}, "max_runners"},
		{"float max_runners", map[string]string{"max_runners": `4.5`}, "max_runners"},
		{"bool max_runners", map[string]string{"max_runners": `true`}, "max_runners"},
		{"null image", map[string]string{"image": `~`}, "image"},
		{"empty image", map[string]string{"image": `""`}, "image"},
		{"list image", map[string]string{"image": `[a]`}, "image"},
		{"bool cpu", map[string]string{"cpu": `true`}, "cpu"},
		{"null cpu", map[string]string{"cpu": `~`}, "cpu"},
		{"empty cpu", map[string]string{"cpu": `""`}, "cpu"},
		{"mapping mem", map[string]string{"mem": `{a: b}`}, "mem"},
		{"unparseable mem", map[string]string{"mem": `"lots"`}, "mem"},
		{"unknown mem unit", map[string]string{"mem": `"4GIBB"`}, "mem"},
		{"empty runner_group", map[string]string{"runner_group": `""`}, "runner_group"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTierYAML(t, tierYAML("build", c.fields))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "build") {
				t.Errorf("error %q should name the tier and %q", err, c.want)
			}
		})
	}
}

func TestLoadTiersRejectsUnusableFiles(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":       "",
		"no tiers key":     "other: 1\n",
		"empty tiers":      "tiers: {}\n",
		"null tiers":       "tiers:\n",
		"tiers is a list":  "tiers: [build]\n",
		"tier is null":     "tiers:\n  build:\n",
		"tier is a scalar": "tiers:\n  build: yes\n",
		"not yaml":         "tiers: [unclosed\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := loadTierYAML(t, content); err == nil {
				t.Fatalf("got %+v, want an error", got)
			}
		})
	}
	if _, err := loadTiers(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestLoadTiersReportsEveryMissingKeyDeterministically(t *testing.T) {
	_, err := loadTierYAML(t, tierYAML("build", map[string]string{"network": "-", "cpu": "-", "image": "-"}))
	if err == nil || !strings.Contains(err.Error(), "[cpu image network]") {
		t.Errorf("err = %v, want the sorted key list [cpu image network]", err)
	}
}

// The backend key is optional and its default is the host every tier used
// before the seam existed.
func TestLoadTiersBackendDefaultsToIncus(t *testing.T) {
	absent, err := loadTierYAML(t, tierYAML("build", nil))
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := loadTierYAML(t, tierYAML("build", map[string]string{"backend": "incus"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := absent["build"].Backend; got != backendIncus {
		t.Errorf("absent backend = %v, want incus", got)
	}
	if !reflect.DeepEqual(absent, explicit) {
		t.Errorf("backend: incus decoded %+v, absent decoded %+v; want identical", explicit, absent)
	}
}

func TestLoadTiersRejectsAnUnknownBackend(t *testing.T) {
	_, err := loadTierYAML(t, tierYAML("build", map[string]string{"backend": "foo"}))
	if err == nil || !strings.Contains(err.Error(), "backend") || !strings.Contains(err.Error(), `"foo"`) {
		t.Fatalf("err = %v, want one naming backend and foo", err)
	}
}
