package main

import (
	"reflect"
	"strings"
	"testing"
)

// lookupOf serves a fixed environment to loadConfig.
func lookupOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// orgEnv is a complete, valid environment for the org tenant alpha-org, shaped
// like the file gen writes.
func orgEnv() map[string]string {
	return map[string]string{
		"BROKER_TENANT":          "alpha-org",
		"BROKER_TARGET_KIND":     "org",
		"BROKER_TARGET":          "alpha-org",
		"BROKER_INSTALLATION_ID": "42",
		"BROKER_INCUS_PROJECT":   "alpha-org",
		"BROKER_SOCKET":          "/run/bcx-broker/alpha-org.sock",
		"BROKER_TIERS_FILE":      "/etc/bcx-broker/tiers.yml",
		"BROKER_AGE_KEY":         "/etc/bartixcloud/age.key",
		"BROKER_SECRETS_AGE":     "/etc/bcx-broker/alpha-org.age",
		"BROKER_CAPACITY_MEM":    "24GiB",
		"BROKER_MEM_FLOOR":       "6GiB",
	}
}

func userEnv() map[string]string {
	e := orgEnv()
	e["BROKER_TENANT"], e["BROKER_TARGET_KIND"], e["BROKER_TARGET"] = "juno", "user", "Juno"
	e["BROKER_REPOS"] = "some-repo,other-repo"
	e["BROKER_INCUS_PROJECT"] = "juno"
	e["BROKER_SOCKET"] = "/run/bcx-broker/juno.sock"
	return e
}

func with(m map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func without(m map[string]string, keys ...string) map[string]string {
	out := with(m)
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func TestLoadConfigAcceptsTheOrgShape(t *testing.T) {
	cfg, err := loadConfig(lookupOf(orgEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Tenant: "alpha-org", TargetKind: "org", Target: "alpha-org", InstallationID: 42,
		IncusProject: "alpha-org", RunnerGroupID: 1, MemFloor: 6 << 30, CapacityMem: 24 << 30,
		TiersFile: "/etc/bcx-broker/tiers.yml", Socket: "/run/bcx-broker/alpha-org.sock",
		AgeKey: "/etc/bartixcloud/age.key", SecretsAge: "/etc/bcx-broker/alpha-org.age",
		HcloudTokenFile: "/etc/bcx-broker/alpha-org.hcloud-token",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigAcceptsTheUserShape(t *testing.T) {
	cfg, err := loadConfig(lookupOf(with(userEnv(), "BROKER_RUNNER_GROUP_ID", "7", "BROKER_MEM_FLOOR", "512MiB")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetKind != "user" || cfg.Target != "Juno" || !reflect.DeepEqual(cfg.Repos, []string{"some-repo", "other-repo"}) ||
		cfg.RunnerGroupID != 7 || cfg.MemFloor != 512<<20 {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadConfigDefaultsOnlyWhatPythonDefaulted(t *testing.T) {
	cfg, err := loadConfig(lookupOf(without(orgEnv(), "BROKER_TARGET_KIND", "BROKER_TIERS_FILE", "BROKER_AGE_KEY",
		"BROKER_SECRETS_AGE", "BROKER_MEM_FLOOR", "BROKER_CAPACITY_MEM")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetKind != "org" || cfg.RunnerGroupID != 1 || cfg.MemFloor != 6<<30 || cfg.CapacityMem != 0 ||
		cfg.TiersFile != "/etc/bcx-broker/tiers.yml" || cfg.AgeKey != "/etc/bartixcloud/age.key" ||
		cfg.SecretsAge != "/etc/bcx-broker/tier-secrets.age" {
		t.Errorf("got %+v", cfg)
	}
}

// Ports test_check_target_config_exits_on_bad_identity, and names the variable.
func TestLoadConfigRejectsABadIdentity(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string // the variable the message must name
	}{
		{"kind repo is gone", with(userEnv(), "BROKER_TARGET_KIND", "repo"), "BROKER_TARGET_KIND"},
		{"empty kind", with(orgEnv(), "BROKER_TARGET_KIND", ""), "BROKER_TARGET_KIND"},
		{"target must be a bare owner", with(orgEnv(), "BROKER_TARGET", "alpha-org/infra"), "BROKER_TARGET"},
		{"no default owner", with(orgEnv(), "BROKER_TARGET", ""), "BROKER_TARGET"},
		{"unset owner", without(orgEnv(), "BROKER_TARGET"), "BROKER_TARGET"},
		{"no default tenant", with(orgEnv(), "BROKER_TENANT", ""), "BROKER_TENANT"},
		{"unset tenant", without(orgEnv(), "BROKER_TENANT"), "BROKER_TENANT"},
		{"user needs repos", without(userEnv(), "BROKER_REPOS"), "BROKER_REPOS"},
		{"user repos all empty", with(userEnv(), "BROKER_REPOS", ",,"), "BROKER_REPOS"},
		{"org must not carry repos", with(orgEnv(), "BROKER_REPOS", "x"), "BROKER_REPOS"},
		{"installation id must be numeric", with(orgEnv(), "BROKER_INSTALLATION_ID", "<id>"), "BROKER_INSTALLATION_ID"},
		{"installation id unset", without(orgEnv(), "BROKER_INSTALLATION_ID"), "BROKER_INSTALLATION_ID"},
		{"installation id zero", with(orgEnv(), "BROKER_INSTALLATION_ID", "0"), "BROKER_INSTALLATION_ID"},
		{"installation id signed", with(orgEnv(), "BROKER_INSTALLATION_ID", "+42"), "BROKER_INSTALLATION_ID"},
		{"installation id negative", with(orgEnv(), "BROKER_INSTALLATION_ID", "-4"), "BROKER_INSTALLATION_ID"},
		{"installation id overflows", with(orgEnv(), "BROKER_INSTALLATION_ID", "99999999999999999999"), "BROKER_INSTALLATION_ID"},
		{"installation id non-ASCII digit", with(orgEnv(), "BROKER_INSTALLATION_ID", "٣"), "BROKER_INSTALLATION_ID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := loadConfig(lookupOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want one naming %s", err, c.want)
			}
		})
	}
}

// A garbled value used to be swallowed: parseFloor returned 0 (admission floor
// silently off, so the host could be driven into the OOM killer) and atoiOr
// returned 1. Python raised at import and the unit refused to start.
func TestLoadConfigRejectsUnparseableNumbers(t *testing.T) {
	for _, c := range []struct{ name, key, val string }{
		{"floor typo", "BROKER_MEM_FLOOR", "6Gib/"},
		{"floor word", "BROKER_MEM_FLOOR", "six"},
		{"floor bad unit", "BROKER_MEM_FLOOR", "6GiBB"},
		{"floor G is not a unit", "BROKER_MEM_FLOOR", "6G"},
		{"floor empty", "BROKER_MEM_FLOOR", ""},
		{"floor negative", "BROKER_MEM_FLOOR", "-1"},
		{"group id word", "BROKER_RUNNER_GROUP_ID", "abc"},
		{"group id empty", "BROKER_RUNNER_GROUP_ID", ""},
		{"group id zero", "BROKER_RUNNER_GROUP_ID", "0"},
		{"group id negative", "BROKER_RUNNER_GROUP_ID", "-1"},
		{"group id fractional", "BROKER_RUNNER_GROUP_ID", "1.5"},
		{"group id signed", "BROKER_RUNNER_GROUP_ID", "+1"},
		{"capacity word", "BROKER_CAPACITY_MEM", "banana"},
		{"capacity empty", "BROKER_CAPACITY_MEM", ""},
		{"capacity negative", "BROKER_CAPACITY_MEM", "-5GiB"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadConfig(lookupOf(with(orgEnv(), c.key, c.val)))
			if err == nil || !strings.Contains(err.Error(), c.key) {
				t.Errorf("err = %v, want one naming %s", err, c.key)
			}
		})
	}
}

// The project and socket used to default to "default" and a shared
// broker.sock. A broker missing them would reap another tenant's instances
// (an empty --project is the default project) or steal a neighbour's socket.
func TestLoadConfigHasNoSilentFallbackForScopeAndSocket(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"project unset", without(orgEnv(), "BROKER_INCUS_PROJECT"), "BROKER_INCUS_PROJECT"},
		{"project empty", with(orgEnv(), "BROKER_INCUS_PROJECT", ""), "BROKER_INCUS_PROJECT"},
		{"project leading dash would parse as a flag", with(orgEnv(), "BROKER_INCUS_PROJECT", "-x"), "BROKER_INCUS_PROJECT"},
		{"project with a space", with(orgEnv(), "BROKER_INCUS_PROJECT", "a b"), "BROKER_INCUS_PROJECT"},
		{"project with a slash", with(orgEnv(), "BROKER_INCUS_PROJECT", "a/b"), "BROKER_INCUS_PROJECT"},
		{"socket unset", without(orgEnv(), "BROKER_SOCKET"), "BROKER_SOCKET"},
		{"socket empty", with(orgEnv(), "BROKER_SOCKET", ""), "BROKER_SOCKET"},
		{"socket relative", with(orgEnv(), "BROKER_SOCKET", "alpha-org.sock"), "BROKER_SOCKET"},
		{"socket is the shared name", with(orgEnv(), "BROKER_SOCKET", "/run/bcx-broker/broker.sock"), "BROKER_SOCKET"},
		{"socket is another tenant's", with(orgEnv(), "BROKER_SOCKET", "/run/bcx-broker/juno.sock"), "BROKER_SOCKET"},
		{"socket is the shared dir", with(orgEnv(), "BROKER_SOCKET", "/run/bcx-broker"), "BROKER_SOCKET"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := loadConfig(lookupOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want one naming %s", err, c.want)
			}
		})
	}
}

// Mirrors test_tenant_identity_has_no_defaults: nothing about the tenant may be
// assumed from an empty environment.
func TestTenantIdentityHasNoDefaults(t *testing.T) {
	cfg, err := loadConfig(lookupOf(map[string]string{}))
	if err == nil {
		t.Fatalf("an empty environment produced a config: %+v", cfg)
	}
	for _, v := range []string{"BROKER_TENANT", "BROKER_TARGET", "BROKER_INSTALLATION_ID", "BROKER_INCUS_PROJECT", "BROKER_SOCKET"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error should name %s: %v", v, err)
		}
	}
	if cfg.Tenant != "" || cfg.Target != "" || cfg.Socket != "" {
		t.Errorf("failed config must be the zero value, got %+v", cfg)
	}
}

func TestConfigApplySetsTheProcessGlobals(t *testing.T) {
	cfg, err := loadConfig(lookupOf(with(userEnv(), "BROKER_RUNNER_GROUP_ID", "7")))
	if err != nil {
		t.Fatal(err)
	}
	// apply overwrites every global; snapshot them so the test cleans up after itself.
	swap(t, &tenant, tenant)
	swap(t, &targetKind, targetKind)
	swap(t, &target, target)
	swap(t, &tenantOwner, tenantOwner)
	swap(t, &incusProject, incusProject)
	swap(t, &tiersFile, tiersFile)
	swap(t, &socketPath, socketPath)
	swap(t, &ageKey, ageKey)
	swap(t, &secretsAge, secretsAge)
	swap(t, &repos, repos)
	swap(t, &installationID, installationID)
	swap(t, &runnerGroupDefault, runnerGroupDefault)
	swap(t, &memFloorBytes, memFloorBytes)
	cfg.apply()
	if tenant != "juno" || targetKind != "user" || target != "Juno" || tenantOwner != "juno" ||
		incusProject != "juno" || socketPath != "/run/bcx-broker/juno.sock" || installationID != 42 ||
		runnerGroupDefault != 7 || memFloorBytes != 6<<30 || !reflect.DeepEqual(repos, []string{"some-repo", "other-repo"}) {
		t.Errorf("globals not applied: tenant=%q kind=%q target=%q owner=%q project=%q socket=%q iid=%d group=%d floor=%d repos=%v",
			tenant, targetKind, target, tenantOwner, incusProject, socketPath, installationID, runnerGroupDefault, memFloorBytes, repos)
	}
}

func TestLoadConfigReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadConfig(lookupOf(with(orgEnv(), "BROKER_MEM_FLOOR", "x", "BROKER_RUNNER_GROUP_ID", "y", "BROKER_CAPACITY_MEM", "z")))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, v := range []string{"BROKER_MEM_FLOOR", "BROKER_RUNNER_GROUP_ID", "BROKER_CAPACITY_MEM"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error should name %s: %v", v, err)
		}
	}
}
