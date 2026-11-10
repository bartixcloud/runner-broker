package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// dispatcherNameRe is the dispatcher's own rule for owner, tenant, project and
// vm: a tenant becomes a socket file name and project/vm reach `incus exec`
// argv. The dispatcher rejects its whole table on one bad name, so gen must be
// at least as strict. The narrower nameRe and ownerRe are subsets of it.
var dispatcherNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const dispatcherNamePattern = "^[A-Za-z0-9][A-Za-z0-9._-]*$"

func checkDispatcherName(what, value string) error {
	if !dispatcherNameRe.MatchString(value) {
		return fmt.Errorf("%s %q is not a valid name (want %s)", what, value, dispatcherNamePattern)
	}
	return nil
}

// renderedFile is one artifact of a render.
type renderedFile struct {
	Name string
	Text string
}

// rendered is the render result in the order files are reported and written:
// tenants in file order, then dispatcher.json.
type rendered []renderedFile

func (r rendered) text(name string) (string, bool) {
	for _, f := range r {
		if f.Name == name {
			return f.Text, true
		}
	}
	return "", false
}

// isLineBreak is the set of separators Python's str.splitlines() honours.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// pokeTargets returns [(incus_project, vm)] to poke for a v* tag on
// repoFullName, by shelling out to the authoritative `fleet/tenants.py
// poke-targets` CLI (P3 owns it) next to tenants.yml. Every failure is an
// error, which main maps to exit 2: a poke table baked from a half-run CLI
// would silently drop or mangle targets. It is a var so tests can stub the
// subprocess.
var pokeTargets = func(tenantsYml, repoFullName string) ([][2]string, error) {
	if tenantsYml == "" {
		return nil, errors.New("poke targets need the tenants_yml path (tenants.py sits next to it)")
	}
	// Absolute so a relative tenants.yml containing '..' cannot clean into a
	// script path that python3 would read as an option (-c CODE).
	abs, err := filepath.Abs(tenantsYml)
	if err != nil {
		return nil, fmt.Errorf("tenants.py poke-targets %s: %w", repoFullName, err)
	}
	tenantsPy := filepath.Join(filepath.Dir(abs), "fleet", "tenants.py")
	cmd := exec.Command("python3", tenantsPy, "poke-targets", tenantsYml, repoFullName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = exit.Error()
			}
			return nil, fmt.Errorf("tenants.py poke-targets %s: %s", repoFullName, detail)
		}
		return nil, fmt.Errorf("tenants.py poke-targets %s: cannot run: %w", repoFullName, err)
	}
	var targets [][2]string
	for _, line := range strings.FieldsFunc(stdout.String(), isLineBreak) {
		parts := strings.Fields(line)
		switch len(parts) {
		case 0:
		case 2:
			targets = append(targets, [2]string{parts[0], parts[1]})
		default:
			return nil, fmt.Errorf("tenants.py poke-targets %s: expected '<project> <vm>', got %q",
				repoFullName, strings.Join(parts, " "))
		}
	}
	return targets, nil
}

// renderEnv renders one broker env. A tenants[] row and the platform's own CI
// broker (G24) render identically — a name, a GitHub App installation, an Incus
// project and a memory cap — so both go through this one function and neither
// can drift from the other.
func renderEnv(name string, gh github, incusProject, capacityMem, memFloor string) string {
	lines := []string{
		fmt.Sprintf("# %s/%s.env", envDir, name),
		generatedMark + " from bartixcloud/infra/tenants.yml.",
		"# Do not edit — re-run the generator; --check reports any difference.",
		"BROKER_TENANT=" + name,
		"BROKER_TARGET_KIND=" + string(gh.Kind),
		"BROKER_TARGET=" + gh.Target,
	}
	if gh.Kind == kindUser {
		lines = append(lines, "BROKER_REPOS="+strings.Join(gh.Repos, ","))
	}
	lines = append(lines,
		"BROKER_INSTALLATION_ID="+strconv.FormatInt(gh.InstallationID, 10),
		"BROKER_INCUS_PROJECT="+incusProject,
		fmt.Sprintf("BROKER_SOCKET=%s/%s.sock", socketDir, name),
		"BROKER_TIERS_FILE="+tiersFile,
		"BROKER_AGE_KEY="+ageKey,
		fmt.Sprintf("BROKER_SECRETS_AGE=%s/%s.age", envDir, name),
		"BROKER_CAPACITY_MEM="+capacityMem,
		"BROKER_MEM_FLOOR="+memFloor,
	)
	return strings.Join(lines, "\n") + "\n"
}

// installation is one dispatcher.json installations entry; a nil Tenant is a
// platform installation that runs no CI broker (G24).
type installation struct {
	Owner  string  `json:"owner"`
	Tenant *string `json:"tenant"`
}

// dispatcherTable is the dispatcher's whole config. Field and map-key order is
// what makes the bytes identical to the Python's sort_keys output.
type dispatcherTable struct {
	Installations map[string]installation `json:"installations"`
	Pokes         map[string][][2]string  `json:"pokes"`
}

func installationKey(gh github) string { return strconv.FormatInt(gh.InstallationID, 10) }

// infraRepo is the owner's own spelling: P3's CLI matches the repo URL
// case-sensitively against tenants.yml. Only the dispatcher's lookup key is
// lower-cased.
func infraRepo(gh github) string { return gh.Target + "/infra" }

func renderDispatcherMap(cfg *config, tenantsYml string) (string, error) {
	table := dispatcherTable{
		Installations: map[string]installation{},
		Pokes:         map[string][][2]string{},
	}
	if err := checkDispatcherName("platform owner", cfg.Platform.Target); err != nil {
		return "", err
	}
	// One installation id, one entry: the platform's CI broker routes through the
	// same App installation as the platform's own poke, so the entry names the
	// CI tenant instead of appearing twice (a second entry would just overwrite
	// this one in the map).
	platformInstall := installation{Owner: cfg.Platform.Target}
	if ci := cfg.PlatformCI; ci != nil {
		if err := checkDispatcherName("platform CI tenant", ci.Name); err != nil {
			return "", err
		}
		platformInstall.Tenant = &ci.Name
	}
	table.Installations[installationKey(cfg.Platform)] = platformInstall
	repos := []string{infraRepo(cfg.Platform)}
	for _, t := range cfg.Tenants {
		if err := errors.Join(
			checkDispatcherName("tenant", t.Name),
			checkDispatcherName("owner", t.GitHub.Target),
		); err != nil {
			return "", err
		}
		table.Installations[installationKey(t.GitHub)] = installation{Owner: t.GitHub.Target, Tenant: &t.Name}
		repos = append(repos, infraRepo(t.GitHub))
	}

	for _, r := range repos {
		targets, err := pokeTargets(tenantsYml, r)
		if err != nil {
			return "", err
		}
		// C3: ingress reads every tenant's routes, so a configured <owner>/infra
		// never has zero targets. An empty answer means the CLI matched nothing;
		// baked as [], the dispatcher would ignore every v* tag without a trace.
		if len(targets) == 0 {
			return "", fmt.Errorf("tenants.py poke-targets %s: no targets, but every "+
				"<owner>/infra reaches at least ingress (C3)", r)
		}
		for _, target := range targets {
			if err := errors.Join(
				checkDispatcherName("project", target[0]),
				checkDispatcherName("vm", target[1]),
			); err != nil {
				return "", fmt.Errorf("tenants.py poke-targets %s: %w", r, err)
			}
		}
		table.Pokes[strings.ToLower(r)] = targets
	}

	b, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// renderConfig renders a validated config: the platform's own CI env when it has
// one, then one <tenant>.env per tenant, then dispatcher.json. A kind:user tenant
// with empty repos renders no env (no broker, C2) but keeps its installation
// entry and <owner>/infra poke mapping. The platform CI env comes first because
// the platform block is the first thing in the document.
func renderConfig(cfg *config, tenantsYml string) (rendered, error) {
	var out rendered
	if ci := cfg.PlatformCI; ci != nil {
		out = append(out, renderedFile{Name: ci.Name + ".env",
			Text: renderEnv(ci.Name, cfg.Platform, ci.IncusProject, ci.CapacityMem, cfg.MemFloor)})
	}
	for _, t := range cfg.Tenants {
		if t.GitHub.Kind == kindUser && len(t.GitHub.Repos) == 0 {
			continue
		}
		out = append(out, renderedFile{Name: t.Name + ".env",
			Text: renderEnv(t.Name, t.GitHub, t.IncusProject, t.CapacityMem, cfg.MemFloor)})
	}
	dispatcher, err := renderDispatcherMap(cfg, tenantsYml)
	if err != nil {
		return nil, err
	}
	return append(out, renderedFile{Name: dispatcherMap, Text: dispatcher}), nil
}

// render validates doc and renders it; an invalid document renders nothing for
// anyone.
func render(doc *tenantsDoc, incusProjects map[string]struct{}, tenantsYml string) (rendered, error) {
	cfg, errs := validate(doc, incusProjects)
	if len(errs) > 0 {
		return nil, fmt.Errorf("tenants.yml is invalid:\n  %s", strings.Join(errs, "\n  "))
	}
	return renderConfig(cfg, tenantsYml)
}
