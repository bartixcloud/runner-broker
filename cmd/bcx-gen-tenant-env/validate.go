package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bartixcloud/runner-broker/mem"
	"gopkg.in/yaml.v3"
)

// Regexes. Go's `$` matches only at the end of the text, so unlike Python's
// .fullmatch() workaround these need no extra guard against 'name\n'.
var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	memRe   = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[A-Za-z]*$`)
	// nameRePattern is the literal Python regex source, used verbatim inside
	// error messages (the Python interpolates NAME_RE.pattern).
	nameRePattern = "[a-z0-9][a-z0-9-]*"
)

// targetKind is the closed set of GitHub installation kinds.
type targetKind string

const (
	kindOrg  targetKind = "org"
	kindUser targetKind = "user"
)

func (k targetKind) valid() bool {
	switch k {
	case kindOrg, kindUser:
		return true
	}
	return false
}

// vmRole is the closed set of platform VM roles.
type vmRole string

const (
	roleApp      vmRole = "app"
	roleData     vmRole = "data"
	roleDevbox   vmRole = "devbox"
	roleEdge     vmRole = "edge"
	roleOps      vmRole = "ops"
	roleRegistry vmRole = "registry"
)

// vmRoleNames lists the roles in the sorted order the error message shows.
var vmRoleNames = []string{
	string(roleApp), string(roleData), string(roleDevbox),
	string(roleEdge), string(roleOps), string(roleRegistry),
}

func (r vmRole) valid() bool {
	switch r {
	case roleApp, roleData, roleDevbox, roleEdge, roleOps, roleRegistry:
		return true
	}
	return false
}

// github is a validated `github:` block.
type github struct {
	Kind           targetKind
	Target         string
	Repos          []string // kind user only; empty means no CI repos yet
	InstallationID int64
}

// tenant is a validated tenants[] row, reduced to what the broker layer renders.
type tenant struct {
	Name         string
	GitHub       github
	IncusProject string
	CapacityMem  string
}

// platformCI is a validated `platform.ci` block (G24): the platform org runs its
// own CI, in its own Incus project, served by a broker like any tenant's. Its
// tenant name is the platform owner lower-cased, and its GitHub App installation
// is the platform's own — one App installs once per org, so the block names no
// second installation.
type platformCI struct {
	Name         string
	IncusProject string
	CapacityMem  string
}

// config is a whole validated tenants.yml. It exists only when validation found
// nothing wrong, so render never has to second-guess a field.
type config struct {
	MemFloor   string
	Platform   github
	PlatformCI *platformCI // nil: the platform org runs no CI broker
	Tenants    []tenant
}

// The *Row structs are tenants.yml as written: every value a yaml.Node so a
// wrong type becomes an error naming the field. Unknown keys are ignored, as
// the fields this layer does not consume belong to P3's tenants.py.

type tenantsDoc struct {
	MemFloor yaml.Node `yaml:"mem_floor"`
	AgeKey   yaml.Node `yaml:"age_key"`
	Platform yaml.Node `yaml:"platform"`
	Tenants  yaml.Node `yaml:"tenants"`
	VMs      yaml.Node `yaml:"vms"`

	// notMapping is set when the document's top level is not a mapping.
	notMapping bool
}

type platformRow struct {
	IncusProject yaml.Node `yaml:"incus_project"`
	GitHub       yaml.Node `yaml:"github"`
	CI           yaml.Node `yaml:"ci"`
}

// platformCIRow is the platform's optional `ci:` block: the platform org's own
// CI capacity, in an Incus project of its own (never `platform`'s — that would
// put the platform's VMs and its CI VMs under one reaper).
type platformCIRow struct {
	IncusProject yaml.Node `yaml:"incus_project"`
	Capacity     yaml.Node `yaml:"capacity"`
}

type githubRow struct {
	Kind           yaml.Node `yaml:"kind"`
	Target         yaml.Node `yaml:"target"`
	Repos          yaml.Node `yaml:"repos"`
	InstallationID yaml.Node `yaml:"installation_id"`
}

type tenantRow struct {
	Name         yaml.Node `yaml:"name"`
	GitHub       yaml.Node `yaml:"github"`
	IncusProject yaml.Node `yaml:"incus_project"`
	Capacity     yaml.Node `yaml:"capacity"`
	Legacy       legacyRow `yaml:",inline"`
}

type capacityRow struct {
	Mem yaml.Node `yaml:"mem"`
}

type vmRow struct {
	Name   yaml.Node `yaml:"name"`
	Role   yaml.Node `yaml:"role"`
	Tenant yaml.Node `yaml:"tenant"`
}

// legacyRow holds the fields of the pre-bartixcloud per-tenant-webhook schema. A
// row still carrying one was written for the old world and must not be
// half-understood.
type legacyRow struct {
	WebhookHost   yaml.Node `yaml:"webhook_host"`
	ListenHost    yaml.Node `yaml:"listen_host"`
	RunnerProject yaml.Node `yaml:"runner_project"`
	RunnerGroupID yaml.Node `yaml:"runner_group_id"`
	ListenPort    yaml.Node `yaml:"listen_port"`
	Port          yaml.Node `yaml:"port"`
	SecretsAge    yaml.Node `yaml:"secrets_age"`
}

// keys returns the legacy keys the row carries, in the Python's reporting order.
func (l *legacyRow) keys() []string {
	var found []string
	for _, f := range []struct {
		key  string
		node *yaml.Node
	}{
		{"webhook_host", &l.WebhookHost}, {"listen_host", &l.ListenHost},
		{"runner_project", &l.RunnerProject}, {"runner_group_id", &l.RunnerGroupID},
		{"listen_port", &l.ListenPort}, {"port", &l.Port}, {"secrets_age", &l.SecretsAge},
	} {
		if present(f.node) {
			found = append(found, f.key)
		}
	}
	return found
}

// parseDoc decodes tenants.yml. An empty file is an empty mapping (PyYAML's
// `or {}`); a second YAML document is refused where PyYAML refused it.
func parseDoc(data []byte) (*tenantsDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return &tenantsDoc{}, nil
		}
		return nil, err
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("expected a single YAML document, found more than one")
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	if len(root.Content) == 0 {
		return &tenantsDoc{}, nil
	}
	top := follow(root.Content[0])
	if isNull(top) {
		return &tenantsDoc{}, nil
	}
	doc, err := decodeRow[tenantsDoc](top)
	if errors.Is(err, errNotMapping) {
		return &tenantsDoc{notMapping: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return doc, nil
}

// uniqueRow is what the uniqueness pass needs from a row that parsed as a mapping.
type uniqueRow struct {
	where      string
	project    string
	hasProject bool
	target     string
	hasTarget  bool
}

type validator struct {
	incus map[string]struct{} // nil skips the project-existence rule
	errs  []string
	rows  []uniqueRow
}

func (v *validator) errf(format string, args ...any) {
	v.errs = append(v.errs, fmt.Sprintf(format, args...))
}

// memSize returns the text a memory size renders as in an env file, or the
// reason it cannot (for "<where> — <reason>"). The text must be exactly one
// token: parse_mem_bytes tolerates whitespace and non-ASCII digits that would
// split the env line or change what the broker reads.
func memSize(n *yaml.Node) (text, problem string) {
	r := follow(n)
	if isNull(r) {
		return "", "not a memory size: None"
	}
	if r.Kind != yaml.ScalarNode {
		return "", "not a memory size: " + repr(r)
	}
	if v, ok := yaml11Bool(r); ok {
		return "", "not a memory size: " + pyBool(v)
	}
	switch r.ShortTag() {
	case tagBool:
		return "", "not a memory size: " + repr(r)
	case tagFloat:
		return "", fmt.Sprintf("%s is a fractional number; write a quoted size with a unit "+
			"(e.g. \"6GiB\") or a whole number of bytes", r.Value)
	case tagInt:
		v, ok := decimalInt(r)
		if !ok {
			return "", fmt.Sprintf("%s must be written as a plain decimal number of bytes", r.Value)
		}
		text = strconv.FormatInt(v, 10)
	case tagStr:
		text = r.Value
	default:
		return "", "not a memory size: " + repr(r)
	}
	if _, err := mem.ParseMemBytes(text); err != nil {
		return "", err.Error()
	}
	if !memRe.MatchString(text) {
		return "", fmt.Sprintf("%s must be a number directly followed by an optional unit "+
			"(e.g. 6GiB), with no whitespace", pyQuote(text))
	}
	return text, ""
}

func repoNames(n *yaml.Node) ([]string, bool) {
	items, ok := itemsOf(n)
	if !ok {
		return nil, false
	}
	repos := make([]string, 0, len(items))
	for _, item := range items {
		s, isStr := stringOf(item)
		if !isStr || !isRepoName(s) {
			return nil, false
		}
		repos = append(repos, s)
	}
	return repos, true
}

// isRepoName rejects '.' and '..', which fit the character class but would be
// path traversal in the /repos/<owner>/<repo>/... URLs the broker builds.
func isRepoName(name string) bool {
	return repoRe.MatchString(name) && name != "." && name != ".."
}

// github validates a `github:` block. The result is meaningful only when no
// error was recorded; InstallationID is set whenever that one field is valid,
// so duplicate-id detection still sees it.
func (v *validator) github(where string, n *yaml.Node, kinds []targetKind) github {
	var gh github
	row, err := decodeRow[githubRow](n)
	switch {
	case errors.Is(err, errNotMapping):
		v.errf("%s: github must be a mapping", where)
		return gh
	case err != nil:
		v.errf("%s: github: %v", where, err)
		return gh
	}

	kindText, kindIsStr := stringOf(&row.Kind)
	kind := targetKind(kindText)
	if kindIsStr && kind.valid() && slices.Contains(kinds, kind) {
		gh.Kind = kind
	} else {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = string(k)
		}
		v.errf("%s: github.kind must be one of %s, got %s", where, pyStrList(names), repr(&row.Kind))
	}

	target, targetIsStr := stringOf(&row.Target)
	if targetIsStr && ownerRe.MatchString(target) {
		gh.Target = target
	} else {
		v.errf("%s: github.target must be a bare owner name, got %s", where, repr(&row.Target))
	}

	// The repos key is required for kind: user, but the list may be empty: no CI
	// repos yet, and render emits no broker env for such a tenant (C2).
	if kindIsStr && kind == kindUser {
		repos, ok := repoNames(&row.Repos)
		if !ok {
			v.errf("%s: github.repos must be a list of bare repo names "+
				"for kind: user (an empty list means: no CI repos yet)", where)
		}
		gh.Repos = repos
	} else if !isNull(&row.Repos) {
		v.errf("%s: github.repos is only valid for kind: user", where)
	}

	iid, ok := decimalInt(&row.InstallationID)
	if !ok || iid < 1 {
		v.errf("%s: github.installation_id must be a positive int, got %s", where, repr(&row.InstallationID))
		return gh
	}
	gh.InstallationID = iid
	return gh
}

func (v *validator) project(where string, n *yaml.Node) string {
	project, isStr := stringOf(n)
	switch {
	case !isStr || !nameRe.MatchString(project):
		v.errf("%s: incus_project must match %s, got %s", where, nameRePattern, repr(n))
		return ""
	case v.incus != nil:
		if _, ok := v.incus[project]; !ok {
			have := make([]string, 0, len(v.incus))
			for name := range v.incus {
				have = append(have, name)
			}
			sort.Strings(have)
			v.errf("%s: incus_project %s does not exist as an Incus project (have: %s)",
				where, pyQuote(project), pyStrList(have))
		}
	}
	return project
}

// name validates a tenants[] or vms[] name against the names in seen.
func (v *validator) name(where string, n *yaml.Node, seen map[string]struct{}) string {
	name, isStr := stringOf(n)
	switch {
	case !isStr || !nameRe.MatchString(name):
		v.errf("%s: name must match %s, got %s", where, nameRePattern, repr(n))
		return ""
	case hasKey(seen, name):
		v.errf("%s: duplicate name %s", where, pyQuote(name))
		return ""
	}
	seen[name] = struct{}{}
	return name
}

func hasKey(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// tenant validates one tenants[] row; garbled rows are reported, never panicked on.
func (v *validator) tenant(i int, n *yaml.Node, seenNames map[string]struct{}) tenant {
	var t tenant
	where := fmt.Sprintf("tenants[%d]", i)
	row, err := decodeRow[tenantRow](n)
	switch {
	case errors.Is(err, errNotMapping):
		v.errf("%s: not a mapping", where)
		return t
	case err != nil:
		v.errf("%s: %v", where, err)
		return t
	}

	t.Name = v.name(where, &row.Name, seenNames)
	t.GitHub = v.github(where, &row.GitHub, []targetKind{kindOrg, kindUser})
	t.IncusProject = v.project(where, &row.IncusProject)

	var capMem yaml.Node // absent unless capacity is a mapping that has mem
	capacity, err := decodeRow[capacityRow](&row.Capacity)
	switch {
	case err == nil:
		capMem = capacity.Mem
	case !errors.Is(err, errNotMapping):
		v.errf("%s: capacity: %v", where, err)
	}
	text, problem := memSize(&capMem)
	if problem != "" {
		v.errf("%s: capacity.mem — %s", where, problem)
	}
	t.CapacityMem = text

	for _, key := range row.Legacy.keys() {
		v.errf("%s: %s belongs to the pre-bartixcloud schema — remove it (C2)", where, key)
	}

	v.noteUnique(where, &row.IncusProject, &row.GitHub)
	return t
}

// platformCIBlock validates the platform's optional `ci:` block. Absent or null
// means the platform org runs no CI broker: no env, and its installation entry
// keeps tenant: null. gh is the already-validated platform github block, whose
// owner and installation the CI broker reuses.
func (v *validator) platformCIBlock(n *yaml.Node, gh github) *platformCI {
	if isNull(n) {
		return nil
	}
	where := "platform.ci"
	row, err := decodeRow[platformCIRow](n)
	switch {
	case errors.Is(err, errNotMapping):
		v.errf("%s: must be a mapping", where)
		return nil
	case err != nil:
		v.errf("%s: %v", where, err)
		return nil
	}

	ci := &platformCI{Name: strings.ToLower(gh.Target)}
	ci.IncusProject = v.project(where, &row.IncusProject)

	var capMem yaml.Node // absent unless capacity is a mapping that has mem
	capacity, err := decodeRow[capacityRow](&row.Capacity)
	switch {
	case err == nil:
		capMem = capacity.Mem
	case !errors.Is(err, errNotMapping):
		v.errf("%s: capacity: %v", where, err)
	}
	text, problem := memSize(&capMem)
	if problem != "" {
		v.errf("%s: capacity.mem — %s", where, problem)
	}
	ci.CapacityMem = text

	v.noteProject(where, &row.IncusProject)
	return ci
}

// noteProject records a row that has an Incus project but no github block of its
// own (the platform's `ci:`): only the project joins the uniqueness pass, so its
// sharing the platform's owner is not reported as a duplicate target.
func (v *validator) noteProject(where string, project *yaml.Node) {
	row := uniqueRow{where: where}
	row.project, row.hasProject = stringOf(project)
	v.rows = append(v.rows, row)
}

// noteUnique records a mapping row's project and owner for the uniqueness pass.
// Garbled values are skipped there; their own validators already report them.
func (v *validator) noteUnique(where string, project, gh *yaml.Node) {
	row := uniqueRow{where: where}
	row.project, row.hasProject = stringOf(project)
	if ghRow, err := decodeRow[githubRow](gh); err == nil {
		row.target, row.hasTarget = stringOf(&ghRow.Target)
	}
	v.rows = append(v.rows, row)
}

// unique rejects any two rows (platform included) sharing an Incus project or a
// GitHub owner. A shared project puts two brokers' reapers on one set of ci-*
// VMs, and each would kill the other's running jobs. GitHub owners compare
// case-insensitively and the dispatcher's poke table is keyed lower-case, so
// 'Foo' and 'foo' would merge.
func (v *validator) unique() {
	seenProject := map[string]string{}
	seenOwner := map[string]string{}
	for _, r := range v.rows {
		if r.hasProject {
			if first, dup := seenProject[r.project]; dup {
				v.errf("%s: incus_project %s is already used by %s", r.where, pyQuote(r.project), first)
			} else {
				seenProject[r.project] = r.where
			}
		}
		if r.hasTarget {
			key := strings.ToLower(r.target)
			if first, dup := seenOwner[key]; dup {
				v.errf("%s: github.target %s is already used by %s", r.where, pyQuote(r.target), first)
			} else {
				seenOwner[key] = r.where
			}
		}
	}
}

func (v *validator) vms(n *yaml.Node, tenantNames map[string]struct{}) {
	items, ok := itemsOf(n)
	if !ok || len(items) == 0 {
		v.errf("tenants.yml: vms must be a non-empty list")
		return
	}
	seen := map[string]struct{}{}
	for i, item := range items {
		where := fmt.Sprintf("vms[%d]", i)
		row, err := decodeRow[vmRow](item)
		switch {
		case errors.Is(err, errNotMapping):
			v.errf("%s: not a mapping", where)
			continue
		case err != nil:
			v.errf("%s: %v", where, err)
			continue
		}
		v.name(where, &row.Name, seen)
		if role, isStr := stringOf(&row.Role); !isStr || !vmRole(role).valid() {
			v.errf("%s: role must be one of %s, got %s", where, pyStrList(vmRoleNames), repr(&row.Role))
		}
		// C2 writes platform VMs as `tenant: null`: only a named tenant is checked.
		if !isNull(&row.Tenant) {
			if tenant, isStr := stringOf(&row.Tenant); !isStr || !hasKey(tenantNames, tenant) {
				v.errf("%s: tenant %s is not a tenants[] name", where, repr(&row.Tenant))
			}
		}
	}
}

// validate returns the validated configuration, or the human-readable errors
// (config nil). incusProjects nil skips the project-existence rule (tests, and
// install.sh at L0 before Terraform has created the projects).
func validate(doc *tenantsDoc, incusProjects map[string]struct{}) (*config, []string) {
	if doc.notMapping {
		return nil, []string{"tenants.yml: top level is not a mapping"}
	}
	v := &validator{incus: incusProjects}
	cfg := &config{}

	memFloor, problem := memSize(&doc.MemFloor)
	if problem != "" {
		v.errf("tenants.yml: mem_floor — %s", problem)
	}
	cfg.MemFloor = memFloor
	if present(&doc.AgeKey) {
		v.errf("tenants.yml: age_key belongs to the pre-bartixcloud schema — the broker uses %s", ageKey)
	}

	var ids []int64
	platform, err := decodeRow[platformRow](&doc.Platform)
	switch {
	case errors.Is(err, errNotMapping):
		v.errf("tenants.yml: platform must be a mapping")
	case err != nil:
		v.errf("tenants.yml: platform: %v", err)
	default:
		cfg.Platform = v.github("platform", &platform.GitHub, []targetKind{kindOrg})
		ids = append(ids, cfg.Platform.InstallationID)
		v.project("platform", &platform.IncusProject)
		v.noteUnique("platform", &platform.IncusProject, &platform.GitHub)
		// After the platform row, so a project shared with platform is reported
		// on the ci row and names platform as the first user.
		cfg.PlatformCI = v.platformCIBlock(&platform.CI, cfg.Platform)
	}

	items, ok := itemsOf(&doc.Tenants)
	if !ok || len(items) == 0 {
		v.errf("tenants.yml: tenants must be a non-empty list")
		return nil, v.errs
	}
	seenNames := map[string]struct{}{}
	for i, item := range items {
		t := v.tenant(i, item, seenNames)
		cfg.Tenants = append(cfg.Tenants, t)
		ids = append(ids, t.GitHub.InstallationID)
	}
	// The platform CI broker is named after the platform owner, so it can only
	// collide with a tenants[] row by name — which would make both render
	// <name>.env, one silently overwriting the other.
	if cfg.PlatformCI != nil && hasKey(seenNames, cfg.PlatformCI.Name) {
		v.errf("platform.ci: name %s is already used by a tenants[] row "+
			"(the platform CI broker is named after platform.github.target, lower-cased)",
			pyQuote(cfg.PlatformCI.Name))
	}

	counts := map[int64]int{}
	for _, id := range ids {
		counts[id]++
	}
	var dups []int64
	for id, c := range counts {
		if id > 0 && c > 1 {
			dups = append(dups, id)
		}
	}
	if len(dups) > 0 {
		sort.Slice(dups, func(i, j int) bool { return dups[i] < dups[j] })
		strs := make([]string, len(dups))
		for i, d := range dups {
			strs[i] = strconv.FormatInt(d, 10)
		}
		v.errf("tenants.yml: installation_id(s) [%s] used more than once", strings.Join(strs, ", "))
	}

	v.unique()
	v.vms(&doc.VMs, seenNames)
	if len(v.errs) > 0 {
		return nil, v.errs
	}
	return cfg, nil
}
