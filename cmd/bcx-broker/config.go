package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/bartixcloud/runner-broker/hcloud"
	"github.com/bartixcloud/runner-broker/mem"
)

// Config is the validated process configuration, read from the environment gen
// writes (BROKER_*). Nothing about the tenant identity, the Incus project or
// the socket has a default: a broker that silently assumed one would mint
// runners for the wrong owner, reap another tenant's instances, or take over a
// neighbour's socket.
type Config struct {
	Tenant          string
	TargetKind      string // "org" | "user"
	Target          string // bare owner name
	Repos           []string
	InstallationID  int
	IncusProject    string
	RunnerGroupID   int
	MemFloor        uint64
	CapacityMem     uint64 // validated only; admission uses MemFloor plus the tier's mem
	TiersFile       string
	Socket          string
	AgeKey          string
	SecretsAge      string
	HcloudTokenFile string
}

// projectRe keeps BROKER_INCUS_PROJECT a plain name: a leading dash would be
// read by incus as a flag, and whitespace or slashes are never project names.
var projectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// loadConfig reads and validates the environment through lookup (os.LookupEnv
// in production). It reports every problem at once, each naming its variable,
// and returns the zero Config on failure.
func loadConfig(lookup func(string) (string, bool)) (Config, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	get := func(name, def string) string {
		if v, ok := lookup(name); ok {
			return v
		}
		return def
	}
	required := func(name string) string {
		v := get(name, "")
		if v == "" {
			fail("%s is required", name)
		}
		return v
	}

	cfg := Config{
		Tenant:     required("BROKER_TENANT"),
		TargetKind: get("BROKER_TARGET_KIND", "org"),
		Target:     get("BROKER_TARGET", ""),
		TiersFile:  get("BROKER_TIERS_FILE", "/etc/bcx-broker/tiers.yml"),
		AgeKey:     get("BROKER_AGE_KEY", "/etc/bartixcloud/age.key"),
		SecretsAge: get("BROKER_SECRETS_AGE", "/etc/bcx-broker/tier-secrets.age"),
	}
	cfg.HcloudTokenFile = get("BROKER_HCLOUD_TOKEN_FILE", "")
	if cfg.HcloudTokenFile == "" {
		cfg.HcloudTokenFile = "/etc/bcx-broker/" + cfg.Tenant + ".hcloud-token"
	}
	for _, r := range strings.Split(get("BROKER_REPOS", ""), ",") {
		if r != "" {
			cfg.Repos = append(cfg.Repos, r)
		}
	}

	kind := parseTargetKind(cfg.TargetKind)
	if kind == kindUnknown {
		fail("BROKER_TARGET_KIND must be 'org' or 'user', got %q", cfg.TargetKind)
	}
	if cfg.Target == "" || strings.Contains(cfg.Target, "/") {
		fail("BROKER_TARGET=%q must be a bare owner name", cfg.Target)
	}
	if kind == kindUser && len(cfg.Repos) == 0 {
		fail("BROKER_REPOS must list at least one repo for kind user")
	}
	if kind == kindOrg && len(cfg.Repos) > 0 {
		fail("BROKER_REPOS is only valid for kind user")
	}

	iid, _ := lookup("BROKER_INSTALLATION_ID")
	if n, err := parsePositiveInt(iid); err != nil {
		fail("BROKER_INSTALLATION_ID must be a positive integer, got %q", iid)
	} else {
		cfg.InstallationID = n
	}

	cfg.IncusProject = required("BROKER_INCUS_PROJECT")
	if cfg.IncusProject != "" && !projectRe.MatchString(cfg.IncusProject) {
		fail("BROKER_INCUS_PROJECT=%q is not a valid project name", cfg.IncusProject)
	}

	cfg.Socket = required("BROKER_SOCKET")
	if cfg.Socket != "" && cfg.Tenant != "" {
		if want := cfg.Tenant + ".sock"; !filepath.IsAbs(cfg.Socket) || filepath.Base(cfg.Socket) != want {
			fail("BROKER_SOCKET=%q must be an absolute path ending in /%s", cfg.Socket, want)
		}
	}

	cfg.RunnerGroupID = 1 // kind=user only: repo runners join the default group
	if v, ok := lookup("BROKER_RUNNER_GROUP_ID"); ok {
		if n, err := parsePositiveInt(v); err != nil {
			fail("BROKER_RUNNER_GROUP_ID must be a positive integer, got %q", v)
		} else {
			cfg.RunnerGroupID = n
		}
	}

	// The floor is the admission guard that makes tier overcommit safe; a value
	// that does not parse must stop the unit, not read as 0 (admit everything).
	var err error
	if cfg.MemFloor, err = mem.ParseMemBytes(get("BROKER_MEM_FLOOR", "6GiB")); err != nil {
		fail("BROKER_MEM_FLOOR: %v", err)
	}
	if v, ok := lookup("BROKER_CAPACITY_MEM"); ok {
		if cfg.CapacityMem, err = mem.ParseMemBytes(v); err != nil {
			fail("BROKER_CAPACITY_MEM: %v", err)
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// Seams over the hcloud client so the token-file and startup-proof tests never
// open a socket.
var (
	newHcloudClient = func(token string) *hcloud.Client { return hcloud.New(token) }
	proveHcloud     = func(c *hcloud.Client) error { return c.Prove() }
)

// hcloudAccessRequired reports whether this tenant owns a repo that can use an
// hcloud tier. A tier that names another owner never makes this broker read the
// token file, let alone call Hetzner.
func hcloudAccessRequired(cfg Config, tiers map[string]tier) bool {
	for name, t := range tiers {
		if t.Backend == backendHcloud && tierIsUsable(name, tiers, cfg.Target) {
			return true
		}
	}
	return false
}

// loadHcloudClient reads, validates and proves the tenant's Hetzner token when
// (and only when) a usable hcloud tier exists. Every problem is collected into
// one startup error: a broker that cannot talk to Hetzner must not start and
// then fail every e2e job silently. The token itself never enters an error.
func loadHcloudClient(cfg Config, tiers map[string]tier) (*hcloud.Client, error) {
	if !hcloudAccessRequired(cfg, tiers) {
		return nil, nil
	}
	path := cfg.HcloudTokenFile
	var errs []error
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("hcloud token file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		errs = append(errs, fmt.Errorf("hcloud token file %s is not a regular file (%s)", path, fi.Mode().Type()))
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		errs = append(errs, fmt.Errorf("hcloud token file %s has mode %04o; group and other must have no access (0600)", path, mode))
	}
	// The unit runs as root; a non-root owner could swap the token under it.
	// Tests run unprivileged, where this check would only test their own uid.
	if os.Geteuid() == 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
			errs = append(errs, fmt.Errorf("hcloud token file %s is owned by uid %d, not root", path, st.Uid))
		}
	}
	data, readErr := os.ReadFile(path)
	token := ""
	if readErr != nil {
		errs = append(errs, fmt.Errorf("hcloud token file %s: %w", path, readErr))
	} else {
		token = strings.TrimSpace(string(data))
		if token == "" {
			errs = append(errs, fmt.Errorf("hcloud token file %s is empty", path))
		}
	}
	var client *hcloud.Client
	if token != "" {
		client = newHcloudClient(token)
		if proofErr := proveHcloud(client); proofErr != nil {
			errs = append(errs, fmt.Errorf("hcloud token from %s: startup proof GET /servers?per_page=1 failed: %w", path, proofErr))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return client, nil
}

// parsePositiveInt accepts ASCII digits only (no sign, no other scripts) and a
// value of at least 1 that fits an int.
func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a positive integer", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a positive integer", s)
	}
	return n, nil
}

// apply installs cfg as the process's identity and scope. The decision core
// reads these globals; they are written once before serving and only read after.
func (c Config) apply() {
	tenant, targetKind, target, repos = c.Tenant, c.TargetKind, c.Target, c.Repos
	tenantOwner = ownerOf(c.Target)
	installationID = c.InstallationID
	incusProject = c.IncusProject
	runnerGroupDefault = c.RunnerGroupID
	memFloorBytes = c.MemFloor
	tiersFile, socketPath = c.TiersFile, c.Socket
	ageKey, secretsAge = c.AgeKey, c.SecretsAge
}
