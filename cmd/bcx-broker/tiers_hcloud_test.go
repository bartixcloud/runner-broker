package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// hcloudTierYAML renders one hcloud tier. It deliberately omits the Incus-only
// keys, and "-" omits any key so a test can prove it is required.
func hcloudTierYAML(name string, fields map[string]string) string {
	defaults := map[string]string{
		"backend":      "hcloud",
		"image":        "bcx-e2e-base",
		"server_type":  "cpx42",
		"locations":    "[fsn1, nbg1, hel1]",
		"runner_group": "e2e-ephemeral",
		"max_runners":  "2",
		"allowlist":    `["alpha-org/e2e"]`,
		"secrets":      "[]",
		"max_age":      "-",
	}
	for k, v := range fields {
		defaults[k] = v
	}
	known := []string{"backend", "image", "server_type", "locations", "runner_group", "max_runners", "allowlist", "secrets", "max_age"}
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
		if defaults[k] != "-" {
			b.WriteString("    " + k + ": " + defaults[k] + "\n")
		}
	}
	return b.String()
}

func TestLoadTiersAcceptsAnHcloudTier(t *testing.T) {
	tiers, err := loadTierYAML(t, hcloudTierYAML("e2e", nil))
	if err != nil {
		t.Fatal(err)
	}
	got := tiers["e2e"]
	if got.Backend != backendHcloud || got.Image != "bcx-e2e-base" || got.ServerType != "cpx42" ||
		len(got.Locations) != 3 || got.Locations[0] != "fsn1" || got.RunnerGroup != "e2e-ephemeral" ||
		got.MaxRunners != 2 || got.MaxAge != 2*time.Hour || got.Network != "" || got.CPU != "" || got.Mem != "" {
		t.Errorf("tier = %+v", got)
	}
}

func TestLoadTiersParsesHcloudMaxAge(t *testing.T) {
	tiers, err := loadTierYAML(t, hcloudTierYAML("e2e", map[string]string{"max_age": "90m"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := tiers["e2e"].MaxAge; got != 90*time.Minute {
		t.Errorf("max_age = %s, want 90m", got)
	}
}

func TestLoadTiersRejectsInvalidHcloudTiers(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"missing image", map[string]string{"image": "-"}, "image"},
		{"missing server_type", map[string]string{"server_type": "-"}, "server_type"},
		{"missing locations", map[string]string{"locations": "-"}, "locations"},
		{"missing runner_group", map[string]string{"runner_group": "-"}, "runner_group"},
		{"missing max_runners", map[string]string{"max_runners": "-"}, "max_runners"},
		{"missing allowlist", map[string]string{"allowlist": "-"}, "allowlist"},
		{"missing secrets", map[string]string{"secrets": "-"}, "secrets"},
		{"incus network", map[string]string{"network": "nat-ci"}, "network"},
		{"incus cpu", map[string]string{"cpu": `"2"`}, "cpu"},
		{"incus mem", map[string]string{"mem": `"4GiB"`}, "mem"},
		{"unknown key", map[string]string{"foo": "bar"}, "foo"},
		{"wildcard allowlist", map[string]string{"allowlist": `["*"]`}, "allowlist"},
		{"empty locations", map[string]string{"locations": "[]"}, "locations"},
		{"scalar locations", map[string]string{"locations": "fsn1"}, "locations"},
		{"non-string location", map[string]string{"locations": "[1]"}, "locations"},
		{"empty location", map[string]string{"locations": `["fsn1", ""]`}, "locations"},
		{"closed location set", map[string]string{"locations": "[fsn1, nyc1]"}, "nyc1"},
		{"empty server_type", map[string]string{"server_type": `""`}, "server_type"},
		{"null server_type", map[string]string{"server_type": "~"}, "server_type"},
		{"bad max_age", map[string]string{"max_age": "soon"}, "max_age"},
		{"zero max_age", map[string]string{"max_age": "0"}, "max_age"},
		{"negative max_age", map[string]string{"max_age": "-1h"}, "max_age"},
		{"bool max_age", map[string]string{"max_age": "true"}, "max_age"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTierYAML(t, hcloudTierYAML("e2e", c.fields))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "e2e") {
				t.Errorf("error %q should name the tier and %q", err, c.want)
			}
		})
	}
}

func TestLoadTiersRejectsHcloudKeysOnAnIncusTier(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"server_type", map[string]string{"server_type": "cpx42"}, "server_type"},
		{"locations", map[string]string{"locations": "[fsn1]"}, "locations"},
		{"max_age", map[string]string{"max_age": "2h"}, "max_age"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadTierYAML(t, tierYAML("build", c.fields))
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "incus") {
				t.Errorf("err = %v, want one naming %q and incus", err, c.want)
			}
		})
	}
}

func TestLoadTiersHcloudBackendSpelling(t *testing.T) {
	_, err := loadTierYAML(t, tierYAML("e2e", map[string]string{"backend": "HCloud"}))
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("err = %v, want an error naming backend", err)
	}
}
