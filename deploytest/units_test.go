package deploytest

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// bShippedUnits is the set install.sh puts under /etc/systemd/system.
var bShippedUnits = []string{
	"bcx-dispatcher.service",
	"bcx-broker@.service",
	"bcx-broker-reap@.service",
	"bcx-broker-reap@.timer",
	"broker-firewall.service",
	"nat-ci-firewall.service",
}

// bInstallDir is where binaries and broker-firewall.sh land on metal.
const bInstallDir = "/opt/bcx-broker"

func bUnit(t *testing.T, name string) unitFile {
	t.Helper()
	return parseUnit(t, filepath.Join(repoRoot(t), name))
}

// bInstalledPath is the on-metal path of the binary with the given manifest role,
// so a unit test follows binaries.txt instead of repeating a name.
func bInstalledPath(t *testing.T, role string) string {
	t.Helper()
	for _, b := range loadManifest(t) {
		if b.Role == role {
			return bInstallDir + "/" + b.Name
		}
	}
	t.Fatalf("manifest has no %s role", role)
	return ""
}

// bExpect asserts every value of key in section, in file order; no want means absent.
func bExpect(t *testing.T, u unitFile, section, key string, want ...string) {
	t.Helper()
	if got := u.values(section, key); !reflect.DeepEqual(got, want) {
		t.Errorf("%s [%s] %s = %q, want %q", filepath.Base(u.Path), section, key, got, want)
	}
}

func TestDispatcherUnit(t *testing.T) {
	u := bUnit(t, "bcx-dispatcher.service")
	bExpect(t, u, "Service", "ExecStart", bInstalledPath(t, "dispatcher"))
	bExpect(t, u, "Service", "EnvironmentFile", "/etc/bartixcloud/dispatcher.env")
	bExpect(t, u, "Service", "Type", "simple")
	bExpect(t, u, "Service", "Restart", "on-failure")
	bExpect(t, u, "Service", "NoNewPrivileges", "yes")
	bExpect(t, u, "Service", "PrivateTmp", "yes")
	bExpect(t, u, "Service", "ProtectHome", "yes")
	// 172.17.0.1 is docker0's address: it exists only once dockerd is up.
	bExpect(t, u, "Unit", "After", "network-online.target docker.service incus.service")
	bExpect(t, u, "Install", "WantedBy", "multi-user.target")
}

func TestBrokerTemplateReadsAppThenTenantEnvAndKeepsSharedSocketDir(t *testing.T) {
	u := bUnit(t, "bcx-broker@.service")
	bExpect(t, u, "Service", "ExecStart", bInstalledPath(t, "broker"))
	bExpect(t, u, "Service", "EnvironmentFile", "/etc/bartixcloud/app.env", "/etc/bcx-broker/%i.env")
	bExpect(t, u, "Service", "RuntimeDirectory", "bcx-broker")
	bExpect(t, u, "Service", "RuntimeDirectoryMode", "0700")
	bExpect(t, u, "Service", "RuntimeDirectoryPreserve", "yes")
	bExpect(t, u, "Service", "Type", "simple")
	bExpect(t, u, "Service", "Restart", "on-failure")
	bExpect(t, u, "Install", "WantedBy", "multi-user.target")
}

func TestReaperTemplateAndTimer(t *testing.T) {
	s := bUnit(t, "bcx-broker-reap@.service")
	bExpect(t, s, "Service", "ExecStart", bInstalledPath(t, "reaper"))
	bExpect(t, s, "Service", "EnvironmentFile", "/etc/bartixcloud/app.env", "/etc/bcx-broker/%i.env")
	bExpect(t, s, "Service", "Type", "oneshot")

	tm := bUnit(t, "bcx-broker-reap@.timer")
	bExpect(t, tm, "Timer", "Unit", "bcx-broker-reap@%i.service")
	bExpect(t, tm, "Timer", "OnBootSec", "2min")
	bExpect(t, tm, "Timer", "OnUnitActiveSec", "5min")
	bExpect(t, tm, "Install", "WantedBy", "timers.target")
}

func TestFirewallUnitPointsAtTheNewLayout(t *testing.T) {
	u := bUnit(t, "broker-firewall.service")
	bExpect(t, u, "Service", "ExecStart", bInstallDir+"/broker-firewall.sh")
	bExpect(t, u, "Service", "Type", "oneshot")
	bExpect(t, u, "Service", "RemainAfterExit", "yes")
	// The rule's source is the live edge subnet: a dockerd restart must re-run it.
	bExpect(t, u, "Unit", "After", "docker.service")
	bExpect(t, u, "Unit", "Requires", "docker.service")
	bExpect(t, u, "Unit", "PartOf", "docker.service")
	bExpect(t, u, "Install", "WantedBy", "multi-user.target")
}

// nat-ci-firewall is CI-ISOLATION, not a broker unit, but it lives in the same
// install dir and was hand-installed from the retired Python tree until install.sh
// took it over (2026-10-07). Its ExecStart is the point of that move: a unit
// pointing into the removed directory silently loses the egress lockdown.
func TestNatCiFirewallUnitPointsAtTheInstallDir(t *testing.T) {
	u := bUnit(t, "nat-ci-firewall.service")
	bExpect(t, u, "Service", "ExecStart", bInstallDir+"/nat-ci-firewall.sh")
	bExpect(t, u, "Service", "Type", "oneshot")
	bExpect(t, u, "Service", "RemainAfterExit", "yes")
	// dockerd rebuilds DOCKER-USER from scratch, so the rules must be re-applied with it.
	bExpect(t, u, "Unit", "After", "docker.service")
	bExpect(t, u, "Unit", "Requires", "docker.service")
	bExpect(t, u, "Unit", "PartOf", "docker.service")
	bExpect(t, u, "Install", "WantedBy", "multi-user.target")
}

func TestShippedUnitsHaveNoOldLayoutPythonOrUv(t *testing.T) {
	root := repoRoot(t)
	old, err := filepath.Glob(filepath.Join(root, "nuroot-runner-broker*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("old-layout units still present: %v", old)
	}
	stale := regexp.MustCompile(`nuroot-runner-broker|GH_RUNNER_PAT|python|\.py\b|\buv\b`)
	for _, name := range bShippedUnits {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if m := stale.Find(raw); m != nil {
			t.Errorf("%s mentions %q", name, m)
		}
	}
}

// The Python runtime is gone from metal: no shipped unit or script may name its
// directories again, or a fresh install would point at a path that no longer exists.
func TestNoShippedFileNamesTheRetiredPythonDirs(t *testing.T) {
	root := repoRoot(t)
	stale := regexp.MustCompile(`/opt/nuroot-runner-broker|/etc/nuroot-runner-broker`)
	files := append([]string{"broker-firewall.sh", "nat-ci-firewall.sh"}, bShippedUnits...)
	// The firewall scripts may keep the historical reference in a comment only if it
	// is not a path a unit would exec; both are cheap to keep clean, so forbid the
	// path outright.
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if m := stale.Find(raw); m != nil {
			t.Errorf("%s still names the retired Python dir: %q", name, m)
		}
	}
}
