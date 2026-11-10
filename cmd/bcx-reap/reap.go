// Command reap (deployed as bcx-reap) sweeps orphaned ephemeral CI runner VMs.
//
// Security control, not capacity hygiene: an orphaned `infra` VM holds the
// hcloud token, S3 state credentials, and an age key indefinitely. Run by
// bcx-broker-reap@<tenant>.timer, one instance per tenant, as a oneshot with no
// arguments.
//
// TTL policy: an instance not currently registered as a busy GitHub runner is
// reaped after idleTTLSeconds; any instance at all is reaped after
// hardCeilingSeconds regardless of busy state (GitHub's documented max job
// runtime — nothing legitimate runs longer).
//
// The pass fails closed: a GitHub answer that is not a well-formed 2xx runners
// page aborts it before any delete, because treating an error as "no runner is
// busy" would kill live CI VMs. It exits 0 only when every step succeeded.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
	"github.com/bartixcloud/runner-broker/hcloud"
)

const (
	idleTTLSeconds     = 15 * 60
	hardCeilingSeconds = 6 * 3600

	// hcloudOffGraceSeconds is spec §6's "off and age > 2 min": a powered-off
	// VM has finished (job done or runner died) and is still billed; the grace
	// absorbs the poweroff-to-API lag.
	hcloudOffGraceSeconds = 2 * 60
	// hcloudIdleSeconds is spec §6's third rule. It is shorter than the Incus
	// idle TTL because an hcloud VM exists for exactly one job.
	hcloudIdleSeconds = 10 * 60

	// maxLogChars bounds attacker- or error-controlled text (incus stderr, a
	// GitHub error body) echoed into the journal.
	maxLogChars = 200

	exitFailed = 1
	exitUsage  = 2
)

// ownerRE and repoRE are GitHub's owner and repository name charsets. They keep
// BROKER_TARGET / BROKER_REPOS from steering the API path ("..", "?", "#", " ",
// "%2F"). A repository may start with "." (".github"); "." and ".." are
// excluded separately.
var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// targetKind is the closed set of tenant scopes a reaper can target.
type targetKind int

const (
	kindUnknown targetKind = iota
	kindOrg
	kindUser
)

// instance is a normalized CI VM for the reaping decision: name, creation time
// in unix seconds, and — for hcloud — the backend identity the delete needs.
// The Incus fields are all an Incus instance has; Backend's zero value is
// backendIncus, so an Incus-only instance literal stays unchanged.
type instance struct {
	Backend   backendKind
	Tier      string // hcloud tier name; empty for Incus
	ID        int64  // hcloud server id; zero for Incus
	Name      string
	CreatedAt float64
	Status    string        // hcloud server status ("off", "running", ...)
	MaxAge    time.Duration // hcloud tier ceiling; zero applies no ceiling
}

// incusInstance is the raw element of `incus list --format json`.
type incusInstance struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// runnersWire is the wire form of the /actions/runners reply. Required fields
// are pointers or nil-able so an absent or null field is distinguishable from a
// real zero (total_count 0, busy false) and rejected instead of defaulted.
type runnersWire struct {
	TotalCount *int         `json:"total_count"`
	Runners    []runnerWire `json:"runners"`
}

type runnerWire struct {
	Name *string `json:"name"`
	Busy *bool   `json:"busy"`
}

// runnerPage is a validated page of the runners endpoint.
type runnerPage struct {
	total int
	busy  []string
	link  string // raw Link header
}

// incusResult is the outcome of an incus invocation: exit code and captured
// output. Mirrors Python's subprocess.CompletedProcess fields.
type incusResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// deps are the collaborators run() needs, injected so tests drive the whole
// pass without incus, GitHub, Hetzner, a clock or process-global state. The
// hcloud seams are nil in tests that do not exercise a Hetzner tenant; a nil
// loadTiers means "no hcloud tier", exactly like a deployment whose tiers.yml
// has none.
type deps struct {
	now       func() time.Time
	client    *http.Client
	ghAPI     string
	mintToken func(installationID int) (string, error)
	execIncus func(argv []string) incusResult
	log       *log.Logger

	loadTiers       func(path string) (map[string]tier, error)
	readToken       func(path string) (string, error)
	newHcloudClient func(token string) hcloudOps
}

func defaultDeps() deps {
	return deps{
		now:             time.Now,
		client:          &http.Client{Timeout: 20 * time.Second},
		ghAPI:           appauth.GHAPI,
		mintToken:       newMintToken(appauth.APPEnv, appauth.APPKey),
		execIncus:       defaultExecIncus,
		log:             log.New(os.Stderr, "", log.LstdFlags),
		loadTiers:       loadTiers,
		readToken:       readHcloudToken,
		newHcloudClient: func(token string) hcloudOps { return hcloud.New(token) },
	}
}

// targetPaths returns the API prefixes whose runners belong to this tenant:
// the org, or each registered repo of a user installation. It fails closed on
// an empty or non-name target, empty or non-name BROKER_REPOS entries for kind
// user, or an unknown kind. BROKER_TARGET_KIND defaults to org only when unset.
func targetPaths(env map[string]string) ([]string, error) {
	target := env["BROKER_TARGET"]
	if !ownerRE.MatchString(target) {
		return nil, fmt.Errorf("BROKER_TARGET=%q must be a bare owner name", target)
	}

	kind, ok := env["BROKER_TARGET_KIND"]
	if !ok {
		kind = "org"
	}

	switch parseTargetKind(kind) {
	case kindOrg:
		return []string{"/orgs/" + target}, nil
	case kindUser:
		repos := splitRepos(env["BROKER_REPOS"])
		if len(repos) == 0 {
			return nil, errors.New("BROKER_REPOS is empty for kind user")
		}
		paths := make([]string, 0, len(repos))
		for _, r := range repos {
			if r == "." || r == ".." || !repoRE.MatchString(r) {
				return nil, fmt.Errorf("BROKER_REPOS entry %q is not a repository name", r)
			}
			paths = append(paths, "/repos/"+target+"/"+r)
		}
		return paths, nil
	default:
		return nil, fmt.Errorf("unknown BROKER_TARGET_KIND %q", kind)
	}
}

func parseTargetKind(s string) targetKind {
	switch s {
	case "org":
		return kindOrg
	case "user":
		return kindUser
	default:
		return kindUnknown
	}
}

// splitRepos splits a comma-separated BROKER_REPOS value, dropping empty
// segments (matching the Python's `if r` filter).
func splitRepos(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseInstallationID tolerates surrounding whitespace and a leading "+" like
// Python's int(), but rejects what GitHub could never have issued: non-numeric,
// non-positive and overflowing values.
func parseInstallationID(s string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(s))
	switch {
	case errors.Is(err, strconv.ErrRange):
		return 0, fmt.Errorf("BROKER_INSTALLATION_ID=%q is out of range", s)
	case err != nil || id <= 0:
		return 0, fmt.Errorf("BROKER_INSTALLATION_ID=%q is not a positive integer", s)
	}
	return id, nil
}

// newMintToken mints an installation token from the App credentials. A missing
// App id or key surfaces here, before any network call, as a plain error.
func newMintToken(appEnvFile, keyPath string) func(installationID int) (string, error) {
	return func(installationID int) (string, error) {
		appID, err := appauth.AppID(appEnvFile)
		if err != nil {
			return "", err
		}
		return appauth.NewInstallationToken(appID, installationID, keyPath).Get()
	}
}

// reapReason is the closed set of rules that can doom an instance. The zero
// value means "keep".
type reapReason int

const (
	reasonNone reapReason = iota
	reasonStatusOff
	reasonMaxAge
	reasonCeiling
	reasonIdle
)

func (r reapReason) String() string {
	switch r {
	case reasonNone:
		return "none"
	case reasonStatusOff:
		return "status-off"
	case reasonMaxAge:
		return "max-age"
	case reasonCeiling:
		return "ceiling"
	case reasonIdle:
		return "idle"
	default:
		panic(fmt.Sprintf("unknown reap reason %d", r))
	}
}

// reapCandidate is one instance to delete and the rule that fired, so the log
// line can name both.
type reapCandidate struct {
	instance
	Reason reapReason
}

// reapReasonFor applies one instance's rules. It is pure and total: every
// backend has an exhaustive branch.
func reapReasonFor(inst instance, busy bool, now float64, trustBusyCheck bool) reapReason {
	age := now - inst.CreatedAt
	switch inst.Backend {
	case backendIncus:
		// Incus rules unchanged: any instance past the hard ceiling, and an
		// idle instance when the GitHub busy check is trusted.
		if age > hardCeilingSeconds {
			return reasonCeiling
		}
		if trustBusyCheck && !busy && age > idleTTLSeconds {
			return reasonIdle
		}
	case backendHcloud:
		// Spec §6, in order: a powered-off VM is done and still billed; a tier
		// ceiling that does not depend on GitHub at all; and the idle rule,
		// which the zero-runner guard may disable (an empty runner list is the
		// normal post-job state for hcloud).
		if inst.Status == hcloudStatusOff && age > hcloudOffGraceSeconds {
			return reasonStatusOff
		}
		// A zero MaxAge would reap every server the moment it exists, so a
		// missing duration keeps rather than deletes (hcloudSweep always fills
		// it from the tier, default 2h).
		if inst.MaxAge > 0 && age > inst.MaxAge.Seconds() {
			return reasonMaxAge
		}
		if trustBusyCheck && !busy && age > hcloudIdleSeconds {
			return reasonIdle
		}
	default:
		panic(fmt.Sprintf("unknown instance backend %d", inst.Backend))
	}
	return reasonNone
}

// reapCandidates is the pure decision function: which instances to delete, the
// rule that fired for each, given their creation times and the set of names
// GitHub currently shows as busy.
//
// trustBusyCheck: if false (GitHub reported zero runners while Incus instances
// actually exist), idle-TTL reaping is skipped for this pass — for hcloud that
// disables only the idle rule, never off or max_age. The hard ceiling still
// applies regardless, since it does not depend on the busy check.
func reapCandidates(instances []instance, busyNames map[string]struct{}, now float64, trustBusyCheck bool) []reapCandidate {
	out := make([]reapCandidate, 0)
	for _, inst := range instances {
		_, busy := busyNames[inst.Name]
		if reason := reapReasonFor(inst, busy, now, trustBusyCheck); reason != reasonNone {
			out = append(out, reapCandidate{instance: inst, Reason: reason})
		}
	}
	return out
}

// instancesToReap is reapCandidates reduced to the names, the shape the
// Incus-only reaper always had.
func instancesToReap(instances []instance, busyNames map[string]struct{}, now float64, trustBusyCheck bool) []string {
	candidates := reapCandidates(instances, busyNames, now, trustBusyCheck)
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.Name)
	}
	return out
}

// incus runs a project-scoped incus command: the other tenant's instances are
// not visible to this reaper, so it cannot delete them even by mistake
// (spec 1 section 7.1).
func (d deps) incus(project string, args ...string) incusResult {
	argv := append([]string{"incus", "--project", project}, args...)
	return d.execIncus(argv)
}

func defaultExecIncus(argv []string) incusResult {
	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	res := incusResult{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.exitCode = ee.ExitCode()
		} else {
			// The binary never ran (missing, not executable): without the cause the
			// journal would show an empty reason for every failure.
			res.exitCode = -1
			stderr.WriteString(err.Error())
		}
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func (d deps) listInstances(project string) ([]instance, error) {
	res := d.incus(project, "list", "^ci-", "--format", "json")
	if res.exitCode != 0 {
		return nil, fmt.Errorf("incus list failed: %s", strings.TrimSpace(res.stderr))
	}
	// Truly empty output means no instances (Python: `stdout or "[]"`); anything
	// else, whitespace included, must be a JSON array.
	if res.stdout == "" {
		return nil, nil
	}
	var raw []incusInstance
	if err := json.Unmarshal([]byte(res.stdout), &raw); err != nil {
		return nil, fmt.Errorf("incus list: undecodable output: %w", err)
	}
	if raw == nil {
		return nil, errors.New("incus list: output is not a JSON array")
	}
	out := make([]instance, 0, len(raw))
	for _, i := range raw {
		// The incus filter already restricts to ^ci-; re-checking keeps a bad
		// record (or a changed filter) from reaching the destructive step.
		if !strings.HasPrefix(i.Name, "ci-") {
			return nil, fmt.Errorf("incus list returned instance %q, want only ci-* names", i.Name)
		}
		ts, err := parseCreated(i.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("instance %s: %w", i.Name, err)
		}
		out = append(out, instance{Name: i.Name, CreatedAt: ts})
	}
	return out, nil
}

// parseCreated converts an incus created_at timestamp to fractional unix
// seconds, like Python's datetime.timestamp().
func parseCreated(ts string) (float64, error) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, fmt.Errorf("created_at %q: %w", ts, err)
	}
	return unixSeconds(t), nil
}

func unixSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// busyRunnerNames fetches the names of currently-busy runners for the given
// target path, following RFC 5988 Link-header pagination. Any failing page
// fails the whole call: a partial busy set is a wrong busy set.
func (d deps) busyRunnerNames(targetPath, token string) (map[string]struct{}, error) {
	busy := make(map[string]struct{})
	pageURL := d.ghAPI + targetPath + "/actions/runners?per_page=100"
	for pageURL != "" {
		page, err := d.fetchPage(pageURL, token)
		if err != nil {
			return nil, err
		}
		for _, name := range page.busy {
			busy[name] = struct{}{}
		}
		if pageURL, err = d.nextPageURL(page.link); err != nil {
			return nil, err
		}
	}
	return busy, nil
}

// runnerTotal reports the tenant's own total_count (not len(busy)) — a
// sanity check on the busy-check result itself. per_page=1 keeps it cheap.
func (d deps) runnerTotal(targetPath, token string) (int, error) {
	page, err := d.fetchPage(d.ghAPI+targetPath+"/actions/runners?per_page=1", token)
	if err != nil {
		return 0, err
	}
	return page.total, nil
}

// nextPageURL returns the validated rel=next URL, or "" on the last page. The
// installation token rides along on the follow-up request, so a URL that leaves
// the configured API origin is an error rather than something to ignore (which
// would silently drop that page's busy runners).
func (d deps) nextPageURL(link string) (string, error) {
	next := parseLinkHeader(link, "next")
	if next == "" {
		return "", nil
	}
	nextURL, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("unparseable Link rel=next %q: %w", next, err)
	}
	api, err := url.Parse(d.ghAPI)
	if err != nil {
		return "", fmt.Errorf("unparseable API base %q: %w", d.ghAPI, err)
	}
	if nextURL.Scheme != api.Scheme || nextURL.Host != api.Host {
		return "", fmt.Errorf("refusing Link rel=next %q: not on %s://%s", next, api.Scheme, api.Host)
	}
	return next, nil
}

// fetchPage GETs one runners page and returns it validated. Non-2xx statuses,
// transport errors, undecodable bodies and missing required fields are all
// errors; the Authorization header never appears in them.
func (d deps) fetchPage(pageURL, token string) (runnerPage, error) {
	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return runnerPage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.client.Do(req)
	if err != nil {
		return runnerPage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return runnerPage{}, fmt.Errorf("GET %s: reading body: %w", pageURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return runnerPage{}, fmt.Errorf("GET %s: HTTP %d: %s", pageURL, resp.StatusCode, truncate(string(body), maxLogChars))
	}
	page, err := decodeRunners(body)
	if err != nil {
		return runnerPage{}, fmt.Errorf("GET %s: %w", pageURL, err)
	}
	page.link = resp.Header.Get("Link")
	return page, nil
}

func decodeRunners(body []byte) (runnerPage, error) {
	var wire runnersWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return runnerPage{}, fmt.Errorf("undecodable runners reply: %w", err)
	}
	if wire.TotalCount == nil {
		return runnerPage{}, errors.New("runners reply lacks total_count")
	}
	if wire.Runners == nil {
		return runnerPage{}, errors.New("runners reply lacks runners")
	}
	page := runnerPage{total: *wire.TotalCount}
	for i, r := range wire.Runners {
		if r.Name == nil || *r.Name == "" {
			return runnerPage{}, fmt.Errorf("runner #%d lacks a name", i)
		}
		if r.Busy == nil {
			return runnerPage{}, fmt.Errorf("runner %q lacks busy", *r.Name)
		}
		if *r.Busy {
			page.busy = append(page.busy, *r.Name)
		}
	}
	return page, nil
}

// parseLinkHeader extracts the URL for the given rel from an RFC 5988 Link
// header, returning "" if absent.
func parseLinkHeader(linkHeader, rel string) string {
	if linkHeader == "" {
		return ""
	}
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, `rel="`+rel+`"`) || strings.Contains(part, "rel='"+rel+"'") {
			start := strings.Index(part, "<")
			end := strings.Index(part, ">")
			if start >= 0 && end > start {
				return part[start+1 : end]
			}
		}
	}
	return ""
}

// truncate trims s and cuts it to at most n characters (runes, like Python's
// slicing), so a multi-byte character is never split into invalid UTF-8.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// sweep is one reaping pass. It returns an error for anything that makes the
// pass untrustworthy or incomplete. A failed Hetzner listing skips every hcloud
// delete but does not stop the Incus sweep: the Incus list, guard and delete
// logic is independent of tiers.yml and of Hetzner, and an outage there is
// exactly the class that once filled the shared Incus pool. Both failures are
// reported in the returned error.
func sweep(env map[string]string, d deps) error {
	project := env["BROKER_INCUS_PROJECT"]
	if project == "" {
		return errors.New("BROKER_INCUS_PROJECT is required: every incus call is scoped to it")
	}
	tpaths, err := targetPaths(env)
	if err != nil {
		return err
	}
	installationID, err := parseInstallationID(env["BROKER_INSTALLATION_ID"])
	if err != nil {
		return err
	}
	token, err := d.mintToken(installationID)
	if err != nil {
		return err
	}

	instances, err := d.listInstances(project)
	if err != nil {
		return err
	}

	busy := make(map[string]struct{})
	for _, p := range tpaths {
		names, err := d.busyRunnerNames(p, token)
		if err != nil {
			return err
		}
		for n := range names {
			busy[n] = struct{}{}
		}
	}

	hc, hcloudErr := d.hcloudSweep(env)
	if hcloudErr != nil {
		// No hcloud server is deleted unless the whole listing succeeded; the
		// Incus part of the pass still runs below.
		d.log.Printf("ERROR hcloud sweep failed, skipping every hcloud delete this pass: %v", hcloudErr)
		hc = hcloudPass{}
	}

	// The zero-runner guard covers both backends: a wrong empty busy set must
	// not let the idle rule kill a live VM. It disables only the idle rule (spec
	// §6); the Incus hard ceiling, hcloud status==off and hcloud max_age still
	// apply.
	trustBusyCheck := true
	if len(instances)+len(hc.instances) > 0 {
		total := 0
		for _, p := range tpaths {
			c, err := d.runnerTotal(p, token)
			if err != nil {
				// The busy check cannot be trusted either way; report the
				// hcloud failure captured above with it.
				return errors.Join(err, hcloudErr)
			}
			total += c
		}
		if total == 0 {
			if len(instances) > 0 {
				d.log.Printf("ERROR GitHub reports 0 total runner(s) under %q but %d ci-* instance(s) exist in "+
					"incus project %s — the installation may lack runner read access, or a transient "+
					"API issue; skipping idle-TTL reaping this pass (hard ceiling still enforced)",
					tpaths, len(instances), project)
			} else {
				d.log.Printf("INFO GitHub reports 0 total runner(s) under %q but %d hcloud server(s) exist — "+
					"an empty runner list is normal after a job; skipping the hcloud idle rule this pass "+
					"(off and max_age still enforced)", tpaths, len(hc.instances))
			}
			trustBusyCheck = false
		}
	}

	all := make([]instance, 0, len(instances)+len(hc.instances))
	all = append(all, instances...)
	all = append(all, hc.instances...)

	now := unixSeconds(d.now())
	doomed := reapCandidates(all, busy, now, trustBusyCheck)
	failed := 0
	for _, c := range doomed {
		// A Hetzner VM is not in an Incus project, and naming one would send
		// incident triage to the wrong host.
		projectField := project
		if c.Backend == backendHcloud {
			projectField = "-"
		}
		d.log.Printf("WARNING reaping orphaned instance %s (tenant=%s project=%s) backend=%s tier=%s server=%s age=%s status=%s rule=%s",
			logSafe(c.Name), logSafe(env["BROKER_TENANT"]), projectField, c.Backend, logSafe(dashIfEmpty(c.Tier)), logSafe(c.Name),
			time.Duration((now-c.CreatedAt)*float64(time.Second)).Truncate(time.Second),
			logSafe(dashIfEmpty(c.Status)), c.Reason)
		// Best-effort per instance (the timer retries), but the pass still fails
		// below so a VM that survives its own reaper is visible in the unit state.
		switch c.Backend {
		case backendIncus:
			if res := d.incus(project, "delete", c.Name, "--force"); res.exitCode != 0 {
				failed++
				d.log.Printf("ERROR failed to delete instance %s: %s", logSafe(c.Name), truncate(res.stderr, maxLogChars))
			}
		case backendHcloud:
			if err := hc.api.DeleteServer(c.ID); err != nil {
				failed++
				d.log.Printf("ERROR failed to delete hcloud server %s (id=%d): %s", logSafe(c.Name), c.ID, truncate(err.Error(), maxLogChars))
			}
		default:
			panic(fmt.Sprintf("unknown instance backend %d", c.Backend))
		}
	}
	if len(doomed) == 0 {
		d.log.Printf("INFO reap: nothing to do (%d instance(s) checked in project %s)", len(instances), project)
		if len(hc.tiers) > 0 {
			d.log.Printf("INFO reap: hcloud: nothing to do (%d server(s) checked in tiers %v)", len(hc.instances), hc.tiers)
		}
	}
	var errs []error
	if hcloudErr != nil {
		errs = append(errs, fmt.Errorf("hcloud sweep: %w", hcloudErr))
	}
	if failed > 0 {
		errs = append(errs, fmt.Errorf("%d of %d orphaned instance(s) could not be deleted", failed, len(doomed)))
	}
	return errors.Join(errs...)
}

// dashIfEmpty keeps a log field present when the backend has no value for it
// (an Incus instance has no hcloud tier or status).
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// logSafe replaces control characters so an API- or config-controlled value
// cannot start a forged journal line. Ordinary names pass through unchanged.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// run is the testable entry point: it returns the process exit status, 0 only
// for a fully successful pass.
func run(args []string, env map[string]string, d deps) int {
	if len(args) > 0 {
		d.log.Printf("ERROR reap: unexpected arguments %q; bcx-reap takes none", args)
		return exitUsage
	}
	if err := sweep(env, d); err != nil {
		d.log.Printf("ERROR reap: %v", err)
		return exitFailed
	}
	return 0
}

// envMap turns os.Environ() into a lookup that distinguishes unset from empty.
func envMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func main() {
	os.Exit(run(os.Args[1:], envMap(os.Environ()), defaultDeps()))
}
