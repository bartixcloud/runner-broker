// Command broker runs ephemeral CI runners on Incus for ONE tenant (spec §3.3).
//
// bcx-dispatcher verifies the GitHub App webhook's HMAC and forwards this
// tenant's deliveries to this process's unix socket (BROKER_SOCKET, 0600 in a
// 0700 root dir — the trust boundary that replaces a per-tenant HMAC). For each
// `queued` workflow_job that resolves to exactly one tier, the broker mints a
// single-use JIT runner registration with the App's installation token (no PAT
// exists) and launches an ephemeral VM in the tenant's Incus project on nat-ci,
// which runs that one job and self-destructs.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bartixcloud/runner-broker/mem"
)

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// Process identity and scope, installed once by Config.apply before serving
// (config.go) and only read afterwards. Nothing is defaulted: a broker that
// silently assumed a tenant would mint runners for the wrong owner. The
// fail-closed assertions are loadConfig (startup) and targetPath (use).
var (
	tenant, targetKind, target string // targetKind: org | user; target: bare owner name
	tenantOwner                string // ownerOf(target)
	repos                      []string
	installationID             int
	incusProject               string
	runnerGroupDefault         int // kind=user only: repo runners join the default group
	memFloorBytes              uint64
	tiersFile                  string
	socketPath                 string
	ageKey, secretsAge         string

	webhookPath = "/github" // the dispatcher always forwards to this path
)

var (
	ghAPI          = "https://api.github.com"
	launchLock     sync.Mutex
	reserved       = map[string]int{} // tier -> int, guarded by launchLock
	claimed        = map[int]string{} // job id -> its tier, being launched here, guarded by launchLock
	groupIDMu      sync.Mutex
	groupIDCache   = map[[2]string]int{} // guarded by groupIDMu
	reservedLabels = map[string]struct{}{
		"self-hosted": {}, "linux": {}, "x64": {}, "x86": {}, "arm": {}, "arm64": {},
	}
)

// logf logs with the severity word Python's formatter printed, so journal greps
// and alerts keyed on WARNING/ERROR (infra/drift.sh greps ADMISSION REFUSED)
// keep working. Attacker-influenced values must be passed through %q.
func logf(level, format string, args ...any) {
	log.Printf(level+" "+format, args...)
}

// ownerOf returns "Juno/app" -> "juno". A bare owner returns
// itself. GitHub owner names are case-insensitive, so both sides fold.
func ownerOf(fullName string) string {
	if i := strings.IndexByte(fullName, '/'); i >= 0 {
		return asciiLower(fullName[:i])
	}
	return asciiLower(fullName)
}

// asciiLower folds A-Z only. Python's str.lower() and GitHub's own matching
// differ from Unicode folding for names like "İstanbul" or "ſafe"; owners,
// repos and labels are ASCII, so anything else must stay distinct.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// ---------------------------------------------------------------------------
// tier / target kind
// ---------------------------------------------------------------------------

// tierKind is the closed set of tenant target kinds.
type tierKind int

const (
	kindUnknown tierKind = iota
	kindOrg
	kindUser
)

func parseTargetKind(s string) tierKind {
	switch s {
	case "org":
		return kindOrg
	case "user":
		return kindUser
	default:
		return kindUnknown
	}
}

// targetPath returns the API prefix under which a JIT runner for repoFullName
// registers. Fails closed on anything else.
func targetPath(repoFullName string) (string, error) {
	switch parseTargetKind(targetKind) {
	case kindOrg:
		return "/orgs/" + target, nil
	case kindUser:
		if !repoIsRegistered(repoFullName, "") {
			return "", fmt.Errorf("%q is not a registered repo of user %s", repoFullName, target)
		}
		return "/repos/" + repoFullName, nil
	default:
		return "", fmt.Errorf("unknown BROKER_TARGET_KIND %q", targetKind)
	}
}

// ---------------------------------------------------------------------------
// decision core
// ---------------------------------------------------------------------------

// verdict is the closed set of resolveTier outcomes.
type verdict int

const (
	verdictIgnore verdict = iota
	verdictResolved
	verdictDeny
)

// resolveTier returns (verdict, tierName). Exactly one known tier and nothing
// else resolves; anything touching a known tier without matching exactly is a
// DENY.
func resolveTier(labels []string, knownTiers map[string]struct{}, tenantArg string) (verdict, string) {
	own := asciiLower(tenant)
	if tenantArg != "" {
		own = asciiLower(tenantArg)
	}
	want := make(map[string]struct{})
	for _, l := range labels {
		ll := asciiLower(l)
		if _, reserved := reservedLabels[ll]; reserved {
			continue
		}
		if ll == own {
			continue
		}
		want[ll] = struct{}{}
	}
	// Does any wanted label touch a known tier?
	intersection := false
	for w := range want {
		if _, known := knownTiers[w]; known {
			intersection = true
		}
	}
	if !intersection {
		return verdictIgnore, ""
	}
	if len(want) == 1 {
		for w := range want {
			if _, known := knownTiers[w]; known {
				return verdictResolved, w
			}
		}
	}
	return verdictDeny, ""
}

// resolveOwner returns the owner this process answers for, folded to lower case.
func resolveOwner(tenantOwnerArg string) string {
	if tenantOwnerArg == "" {
		return tenantOwner
	}
	return asciiLower(tenantOwnerArg)
}

// isEntitled reports whether repoFullName is entitled to t for this process's
// tenant. The owner anchor comes first and no allowlist entry overrides it.
func isEntitled(repoFullName string, t string, tiers map[string]tier, tenantOwnerArg string) bool {
	if ownerOf(repoFullName) != resolveOwner(tenantOwnerArg) {
		return false
	}
	for _, a := range tiers[t].Allowlist {
		if a == "*" || a == repoFullName {
			return true
		}
	}
	return false
}

// repoIsRegistered reports whether this tenant can register a runner for
// repoFullName. org: any repo of the owner; user: only the C2 repos.
func repoIsRegistered(repoFullName, tenantOwnerArg string) bool {
	owner, repo, _ := strings.Cut(repoFullName, "/")
	if asciiLower(owner) != resolveOwner(tenantOwnerArg) || repo == "" {
		return false
	}
	if parseTargetKind(targetKind) == kindOrg {
		return true
	}
	for _, r := range repos {
		if asciiLower(r) == asciiLower(repo) {
			return true
		}
	}
	return false
}

// tierIsUsable reports whether ANY repo owned by this tenant can ever use t.
func tierIsUsable(t string, tiers map[string]tier, tenantOwnerArg string) bool {
	owner := resolveOwner(tenantOwnerArg)
	for _, a := range tiers[t].Allowlist {
		if a == "*" {
			return true
		}
		if ownerOf(a) == owner {
			return true
		}
	}
	return false
}

// unmetTierSecrets returns {tier: [missing secret names]} for every tier this
// tenant can use.
func unmetTierSecrets(tiers map[string]tier, secrets map[string]string, tenantOwnerArg string) map[string][]string {
	out := map[string][]string{}
	for name, cfg := range tiers {
		if !tierIsUsable(name, tiers, tenantOwnerArg) {
			continue
		}
		var missing []string
		for _, k := range cfg.Secrets {
			if _, ok := secrets[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			out[name] = missing
		}
	}
	return out
}

// checkTierSecrets fails closed when a usable tier is missing a secret.
func checkTierSecrets(tiers map[string]tier, secrets map[string]string) error {
	if unmet := unmetTierSecrets(tiers, secrets, ""); len(unmet) > 0 {
		return fmt.Errorf("tier(s) %v need secret(s) not present in %s", unmet, secretsAge)
	}
	return nil
}

// secretLineRe is broker.py's ^(\w+)\s*[:=]\s*(.*)$: a line whose key is not a
// bare word is skipped rather than stored under a bogus name.
var secretLineRe = regexp.MustCompile(`^(\w+)\s*[:=]\s*(.*)$`)

// loadSecrets decrypts this tenant's tier credentials in-memory only. A missing
// file means no tier secrets; checkTierSecrets decides whether that is
// acceptable.
func loadSecrets() (map[string]string, error) {
	if _, err := os.Stat(secretsAge); os.IsNotExist(err) {
		logf("INFO", "no %s — this tenant holds no tier secrets", secretsAge)
		return map[string]string{}, nil
	}
	// stdout and stderr stay apart: age's warnings must not parse as secrets, and
	// a failed run's stdout (possibly plaintext) must not reach the journal.
	stdout, stderr, code := execCommand([]string{"age", "-d", "-i", ageKey, secretsAge}, "")
	if code != 0 {
		return nil, fmt.Errorf("cannot decrypt %s: %s", secretsAge, strings.TrimSpace(stderr))
	}
	secrets := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := secretLineRe.FindStringSubmatch(line); m != nil {
			secrets[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return secrets, nil
}

// ---------------------------------------------------------------------------
// github api
// ---------------------------------------------------------------------------

// gh does an authenticated GitHub API call and returns the raw reply body;
// callers decode it into a typed struct and validate the fields they need. It
// is a var so tests can stub it.
var gh = ghHTTP

func ghHTTP(method, path, token string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ghAPI+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &httpError{code: resp.StatusCode, body: string(data)}
	}
	return data, nil
}

// httpError is an HTTP error from the GitHub API.
type httpError struct {
	code int
	body string
}

// The body is quoted so a reply carrying newlines cannot forge log lines.
func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %q", e.code, truncate(e.body, 200)) }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// runnerGroupID resolves a tier's runner_group NAME to its id — org tenants
// only. For kind=user there is nothing to resolve; runnerGroupID is used.
func runnerGroupID(token, groupName string) (int, error) {
	if parseTargetKind(targetKind) != kindOrg {
		return runnerGroupDefault, nil
	}
	key := [2]string{target, groupName}
	// Held across the GET on purpose: N jobs queued at once on a cold cache make
	// one API call instead of N, and a concurrent map write is a fatal error
	// that no recover() can catch.
	groupIDMu.Lock()
	defer groupIDMu.Unlock()
	if id, ok := groupIDCache[key]; ok {
		return id, nil
	}
	raw, err := gh("GET", "/orgs/"+target+"/actions/runner-groups", token, nil)
	if err != nil {
		return 0, err
	}
	var reply runnerGroupsReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return 0, fmt.Errorf("runner-groups reply is not the expected JSON: %w", err)
	}
	for _, g := range reply.RunnerGroups {
		if g.Name != groupName {
			continue
		}
		// A group without a usable id would otherwise be cached as 0 for the
		// life of the process and every jitconfig request would carry it.
		if g.ID < 1 {
			return 0, fmt.Errorf("runner group %q in org %s has no positive id (got %d)", groupName, target, g.ID)
		}
		groupIDCache[key] = g.ID
		return g.ID, nil
	}
	return 0, fmt.Errorf("runner group %q not found in org %s", groupName, target)
}

// runnerGroupsReply is the part of GET /orgs/{org}/actions/runner-groups the
// broker reads. A missing field decodes to its zero value, so callers validate.
type runnerGroupsReply struct {
	RunnerGroups []struct {
		Name string `json:"name"`
		ID   int    `json:"id"`
	} `json:"runner_groups"`
}

// jitRequest is the generate-jitconfig request body.
type jitRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

// jitReply is the part of the generate-jitconfig reply the broker reads.
type jitReply struct {
	Runner struct {
		ID int `json:"id"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// mintJitconfig POSTs generate-jitconfig with exactly [self-hosted, tenant,
// tier]. It keeps the old two-value shape for callers that do not need the
// runner id; spawn uses mintRunner so it can clean up a failed launch.
func mintJitconfig(token, name, t string, tiers map[string]tier, repoFullName string) (string, error) {
	jit, _, err := mintRunner(token, name, t, tiers, repoFullName)
	return jit, err
}

// mintRunner is mintJitconfig plus GitHub's runner id, which spawn needs to
// delete the registration when the VM never comes up.
func mintRunner(token, name, t string, tiers map[string]tier, repoFullName string) (string, int, error) {
	groupID, err := runnerGroupID(token, tiers[t].RunnerGroup)
	if err != nil {
		return "", 0, err
	}
	path, err := targetPath(repoFullName)
	if err != nil {
		return "", 0, err
	}
	body, err := json.Marshal(jitRequest{
		Name:          name,
		RunnerGroupID: groupID,
		Labels:        []string{"self-hosted", tenant, t},
		WorkFolder:    "_work",
	})
	if err != nil {
		return "", 0, err
	}
	raw, err := gh("POST", path+"/actions/runners/generate-jitconfig", token, body)
	if err != nil {
		return "", 0, err
	}
	var reply jitReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", 0, fmt.Errorf("generate-jitconfig reply is not the expected JSON: %w", err)
	}
	// An empty config would launch a VM that can never register and holds a
	// capacity slot (and the tier's secrets) until the reaper.
	if reply.EncodedJITConfig == "" {
		return "", 0, errors.New("generate-jitconfig reply has no encoded_jit_config")
	}
	return reply.EncodedJITConfig, reply.Runner.ID, nil
}

// deleteRunner is the best-effort cleanup of a registration whose VM never
// came up. Source: docs.github.com/rest/actions/self-hosted-runners — DELETE
// /orgs/{org}/actions/runners/{id} and
// DELETE /repos/{owner}/{repo}/actions/runners/{id}. It uses the same App
// permission and target path as the mint. The caller logs and keeps going; a
// leaked registration is reaped by GitHub's offline-runner cleanup, not allowed
// to fail the job twice.
func deleteRunner(token, repoFullName string, id int) {
	if id < 1 {
		return
	}
	path, err := targetPath(repoFullName)
	if err != nil {
		logf("ERROR", "runner cleanup skipped for id %d: %v", id, err)
		return
	}
	if _, err := gh("DELETE", fmt.Sprintf("%s/actions/runners/%d", path, id), token, nil); err != nil {
		logf("ERROR", "github runner cleanup failed for id %d (%q): %v", id, repoFullName, err)
	}
}

// ---------------------------------------------------------------------------
// incus
// ---------------------------------------------------------------------------

// incus runs an Incus command scoped to the tenant's project.
func incus(args ...string) incusResult { return incusInput("", args...) }

// incusInput is the one place that builds an incus argv, so --project is
// injected into launch, list, exec, delete and the stdin-fed injection alike
// (spec 1 §7.1): another tenant's instances are not visible to enumerate,
// rather than hidden by a predicate some call site might forget.
func incusInput(stdin string, args ...string) incusResult {
	argv := append([]string{"incus", "--project", incusProject}, args...)
	stdout, stderr, code := execCommand(argv, stdin)
	return incusResult{exitCode: code, stdout: stdout, stderr: stderr}
}

type incusResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// backend is what the broker needs from a VM host: one seam between the job
// decision and the host, so a second host can be added without a branch at
// every call site (spec §5.1). Implementations own the host's naming and
// lifecycle details; callers only ever see names of the shape
// ci-<tier>-<jobID>-<hex6>.
type backend interface {
	// Running lists the names of live runners for a tier; "" means every tier.
	// A host failure is an error, never an empty list: "we could not ask" must
	// not read as "none running" (that would bypass the per-tier ceiling).
	Running(t string) ([]string, error)
	// Admit reports whether one more runner for t fits, given the number of
	// in-process reservations the caller already holds. It is called with
	// launchLock held and must only report; the caller owns the reservation
	// count, so a refusal can never leak a slot.
	Admit(t string, cfg tier, reserved int) bool
	// Launch creates the runner and hands it its JIT config. A host-side
	// failure that leaves the job waiting is reported as errLaunchAbandoned
	// after logging; any other error aborts the spawn loudly.
	Launch(spec launchSpec) error
	// Delete removes a runner by name. Callers decide whether a failure is
	// fatal or best-effort.
	Delete(name string) error
}

// launchSpec is everything a backend needs to create one runner, so Launch
// takes no broker globals and a fake can be driven from a test.
type launchSpec struct {
	Name    string            // ci-<tier>-<jobID>-<hex6>, the reaper's key
	Tier    string            // tier name, for the hcloud bcx-tier label
	JobID   int               // job id, for the hcloud bcx-job label
	Config  tier              // image, network, cpu and mem for the VM
	JIT     string            // single-use runner registration config
	Secrets map[string]string // this tier's secrets, already filtered
	// Remint is optional and used only by a backend that must rename the runner
	// after creation (Hetzner name collisions). It returns a new JIT config and
	// updates the caller's name through its closure.
	Remint func(name string) (string, error)
}

// errLaunchAbandoned means the backend already logged why the runner never
// came up and the job stays queued for the reconciler. spawn must not log it a
// second time.
var errLaunchAbandoned = errors.New("launch abandoned")

// backends is the one implementation per kind. A var so tests can swap a fake
// host and prove the broker calls the seam rather than Incus.
var backends = map[backendKind]backend{backendIncus: incusBackend{}, backendHcloud: hcloudBackend{}}

// backendOf resolves a tier's backend. The switch is exhaustive: a kind added
// without an implementation must fail loudly, not fall back to Incus.
func backendOf(kind backendKind) backend {
	switch kind {
	case backendIncus:
		b, ok := backends[kind]
		if !ok {
			panic("backend seam has no incus implementation")
		}
		return b
	case backendHcloud:
		b, ok := backends[kind]
		if !ok {
			panic("backend seam has no hcloud implementation")
		}
		return b
	default:
		panic(fmt.Sprintf("unknown tier backend %d", kind))
	}
}

// runningRunners returns the names of running ci-* instances for a tier (or all
// tiers). It returns an error on incus failure rather than []. It is a var so
// tests can stub it; incusBackend.Running is its production caller.
var runningRunners = func(t string) ([]string, error) {
	prefix := "ci-"
	if t != "" {
		prefix = "ci-" + t + "-"
	}
	res := incus("list", "^"+prefix, "--format", "csv", "-c", "n")
	if res.exitCode != 0 {
		return nil, fmt.Errorf("incus list failed: %s", strings.TrimSpace(res.stderr))
	}
	var out []string
	for _, n := range strings.Split(res.stdout, "\n") {
		if strings.TrimSpace(n) != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// incusBackend is the original Incus code behind the backend seam; its argv,
// log lines and error text are unchanged so a deploy behaves exactly as before.
type incusBackend struct{}

func (incusBackend) Running(t string) ([]string, error) { return runningRunners(t) }

func (incusBackend) Admit(t string, cfg tier, reserved int) bool {
	current, err := runningRunners(t)
	if err != nil {
		logf("ERROR", "try_reserve(%s): cannot determine current count, refusing reservation: %s", t, err)
		return false
	}
	if len(current)+reserved >= cfg.MaxRunners {
		return false
	}
	avail, err := availableBytes()
	if err != nil {
		logf("ERROR", "try_reserve(%s): cannot measure memory headroom, refusing reservation: %s", t, err)
		return false
	}
	need, err := mem.ParseMemBytes(cfg.Mem)
	if err != nil {
		logf("ERROR", "try_reserve(%s): cannot measure memory headroom, refusing reservation: %s", t, err)
		return false
	}
	// Written as two comparisons: need+floor can wrap uint64 for a huge floor
	// and would then admit everything.
	if avail < need || avail-need < memFloorBytes {
		logf("WARNING", "ADMISSION REFUSED tier=%s tenant=%s: %.1f GiB available - %.1f GiB tier "+
			"would leave less than the %.1f GiB floor — the job waits for a free slot; "+
			"the reconciler retries every %s; add burst capacity (spec 1 §8.2)",
			t, tenant, float64(avail)/(1<<30), float64(need)/(1<<30), float64(memFloorBytes)/(1<<30), reconcileInterval)
		return false
	}
	return true
}

func (incusBackend) Launch(spec launchSpec) error {
	res := incus("launch", spec.Config.Image, spec.Name, "--ephemeral", "--network", spec.Config.Network,
		"-c", "limits.cpu="+spec.Config.CPU, "-c", "limits.memory="+spec.Config.Mem)
	if res.exitCode != 0 {
		logf("ERROR", "launch %s failed: %s", spec.Name, strings.TrimSpace(res.stderr))
		return errLaunchAbandoned
	}
	for i := 0; i < 60; i++ {
		if incus("exec", spec.Name, "--", "test", "-d", "/etc/actions-runner").exitCode == 0 {
			break
		}
		pause(1 * time.Second)
	}
	inj := incusInput(buildInjectionScript(spec.JIT, spec.Secrets), "exec", spec.Name, "--", "sh")
	if inj.exitCode != 0 {
		logf("ERROR", "injection into %s failed: %s — deleting", spec.Name, strings.TrimSpace(inj.stderr))
		_ = incusBackend{}.Delete(spec.Name)
		return errLaunchAbandoned
	}
	return nil
}

func (incusBackend) Delete(name string) error {
	res := incus("delete", name, "--force")
	if res.exitCode != 0 {
		return fmt.Errorf("incus delete of %s failed: %s", name, strings.TrimSpace(res.stderr))
	}
	return nil
}

// runningRunnersAll lists the runners of every backend a configured tier uses.
// "" means all tiers, and a name must be visible to the reconciler whichever
// host created it: a per-host list would let the reconciler double-spawn when
// two backends are in use (spec §5.1). Today that is one Incus call.
func runningRunnersAll(tiers map[string]tier) ([]string, error) {
	var names []string
	seen := make(map[backendKind]struct{}, len(tiers))
	for name, cfg := range tiers {
		// An hcloud tier this tenant cannot use has no token (config.go), so it
		// must not force a list the tenant is not allowed to make.
		if cfg.Backend == backendHcloud && !tierIsUsable(name, tiers, "") {
			continue
		}
		if _, ok := seen[cfg.Backend]; ok {
			continue
		}
		seen[cfg.Backend] = struct{}{}
		got, err := backendOf(cfg.Backend).Running("")
		if err != nil {
			return nil, err
		}
		names = append(names, got...)
	}
	return names, nil
}

// tryReserve atomically claims one capacity slot for a tier. The tier's host
// decides admission under launchLock; the reservation count stays here so
// release is symmetric and a refused admit never leaves a slot behind.
func tryReserve(t string, tiers map[string]tier) bool {
	cfg := tiers[t]
	launchLock.Lock()
	defer launchLock.Unlock()

	if !backendOf(cfg.Backend).Admit(t, cfg, reserved[t]) {
		return false
	}
	reserved[t]++
	return true
}

func releaseReservation(t string) {
	launchLock.Lock()
	defer launchLock.Unlock()
	if reserved[t] > 0 {
		reserved[t]--
	}
}

// buildInjectionScript builds the shell script piped into `incus exec`.
func buildInjectionScript(jit string, secretsForTier map[string]string) string {
	jitB64 := base64.StdEncoding.EncodeToString([]byte(jit))
	lines := []string{"set -e"}
	if len(secretsForTier) > 0 {
		// Before the umask tightens: a 0700 directory would hide
		// credentials.env from the unprivileged runner user.
		lines = append(lines, "mkdir -p /etc/infra-runner")
	}
	// Files are created 0600 and only then chowned, so the credentials are never
	// readable by anyone else, not even for the instant before the chmod.
	lines = append(lines,
		"umask 077",
		"base64 -d > /etc/actions-runner/jitconfig <<'JITEOF'",
		jitB64,
		"JITEOF",
		"chown runner:runner /etc/actions-runner/jitconfig",
		"chmod 600 /etc/actions-runner/jitconfig",
	)
	if len(secretsForTier) > 0 {
		var envContent strings.Builder
		for _, k := range slices.Sorted(maps.Keys(secretsForTier)) {
			envContent.WriteString(k + "=" + shellQuote(secretsForTier[k]) + "\n")
		}
		envB64 := base64.StdEncoding.EncodeToString([]byte(envContent.String()))
		lines = append(lines,
			"base64 -d > /etc/infra-runner/credentials.env <<'ENVEOF'",
			envB64,
			"ENVEOF",
			"chown runner:runner /etc/infra-runner/credentials.env",
			"chmod 600 /etc/infra-runner/credentials.env",
		)
	}
	return strings.Join(lines, "\n") + "\n"
}

// shellQuote quotes a value for safe inclusion in a sourced KEY=value line.
// Safe values pass through unchanged, as in Python's shlex.quote; a single
// quote is closed, backslash-escaped and reopened where Python closes, double
// quotes and reopens it — both are valid sh and TestShellQuoteRoundTripsThroughSh
// pins the result.
func shellQuote(s string) string {
	if s != "" && isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether every rune is in Python's shlex._find_unsafe
// safe set (alphanumerics plus a small punctuation set).
func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}

// repoNameRe is GitHub's owner/repo shape. The label_vocabulary deny cancels
// before any owner check, so the payload's name is validated before it becomes
// part of an API path.
var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// cancelJob cancels the workflow run behind a denied job, so it does not sit
// queued for 24h. Best-effort: a failure is logged, not returned, since the deny
// decision already stands. GitHub cancels the whole RUN, not the one job — a
// legitimate job in the same run is cancelled with it (accepted tradeoff).
func cancelJob(token, repoFullName string, runID int) {
	_, repo, _ := strings.Cut(repoFullName, "/")
	if !repoNameRe.MatchString(repoFullName) || repo == "." || repo == ".." {
		logf("ERROR", "cancel skipped for run %d: %q is not an owner/repo name", runID, repoFullName)
		return
	}
	if _, err := gh("POST", fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repoFullName, runID), token, nil); err != nil {
		logf("ERROR", "cancel failed for run %d (%q): %v", runID, repoFullName, err)
	}
}

// spawn launches a VM for a job.
func spawn(token string, jobID int, t string, tiers map[string]tier, secrets map[string]string, repoFullName string) (string, error) {
	cfg := tiers[t]
	name := fmt.Sprintf("ci-%s-%d-%s", t, jobID, randHex())
	// Before reserving or minting: a tier secret that is absent must abort the
	// spawn, never inject KEY='' and start a runner with blank credentials.
	secretsForTier := make(map[string]string, len(cfg.Secrets))
	for _, k := range cfg.Secrets {
		v, ok := secrets[k]
		if !ok {
			return "", fmt.Errorf("tier %s needs secret %s, which is not loaded", t, k)
		}
		secretsForTier[k] = v
	}
	if !tryReserve(t, tiers) {
		logf("WARNING", "tier %s reservation refused (max=%d) — job %d waits for a free slot; "+
			"the reconciler retries every %s", t, cfg.MaxRunners, jobID, reconcileInterval)
		return "", nil
	}
	defer releaseReservation(t)

	jit, runnerID, err := mintRunner(token, name, t, tiers, repoFullName)
	if err != nil {
		return "", err
	}
	// hcloud has no ephemeral flag: an explicit delete is the lifecycle. If the
	// VM is never created after the mint, remove its registration too. Incus
	// keeps its old behaviour.
	launched := false
	if cfg.Backend == backendHcloud {
		defer func() {
			if !launched {
				deleteRunner(token, repoFullName, runnerID)
			}
		}()
	}
	spec := launchSpec{
		Name: name, Tier: t, JobID: jobID, Config: cfg, JIT: jit, Secrets: secretsForTier,
	}
	if cfg.Backend == backendHcloud {
		spec.Remint = func(newName string) (string, error) {
			deleteRunner(token, repoFullName, runnerID)
			newJIT, newID, err := mintRunner(token, newName, t, tiers, repoFullName)
			if err != nil {
				return "", err
			}
			runnerID = newID
			name = newName
			return newJIT, nil
		}
	}
	if err := backendOf(cfg.Backend).Launch(spec); err != nil {
		if errors.Is(err, errLaunchAbandoned) {
			return "", nil
		}
		return "", err
	}
	launched = true
	logf("INFO", "spawned %s for job %d (tier=%s)", name, jobID, t)
	return name, nil
}

// action is the closed set of decideJobAction outcomes.
type action int

const (
	actionIgnore action = iota
	actionDeny
	actionSpawn
)

func (a action) String() string {
	switch a {
	case actionIgnore:
		return "ignore"
	case actionDeny:
		return "deny"
	case actionSpawn:
		return "spawn"
	default:
		return "unknown"
	}
}

// denyReason is the closed set of reasons decideJobAction denies a job.
type denyReason string

const (
	reasonNone            denyReason = ""
	reasonLabelVocabulary denyReason = "label_vocabulary" // not exactly one known tier (cancelled)
	reasonWrongOwner      denyReason = "wrong_owner"      // another owner's run: NOT cancelled
	reasonNotRegistered   denyReason = "not_registered"   // user tenant, repo not in repos: NOT cancelled
	reasonNotEntitled     denyReason = "not_entitled"     // right owner, allowlist says no (cancelled)
)

// decideResult is the outcome of decideJobAction.
type decideResult struct {
	action action
	tier   string
	reason denyReason
}

// decideJobAction is the pure decision core of the POST handler.
func decideJobAction(labels []string, repoFullName string, tiers map[string]tier, tenantOwnerArg string) decideResult {
	known := make(map[string]struct{}, len(tiers))
	for name := range tiers {
		known[name] = struct{}{}
	}
	v, t := resolveTier(labels, known, "")
	switch v {
	case verdictResolved:
	case verdictIgnore:
		return decideResult{actionIgnore, "", reasonNone}
	case verdictDeny:
		return decideResult{actionDeny, "", reasonLabelVocabulary}
	default: // an unknown verdict must never spawn
		return decideResult{actionDeny, "", reasonLabelVocabulary}
	}
	owner := resolveOwner(tenantOwnerArg)
	if ownerOf(repoFullName) != owner {
		return decideResult{actionDeny, t, reasonWrongOwner}
	}
	if !repoIsRegistered(repoFullName, owner) {
		return decideResult{actionDeny, t, reasonNotRegistered}
	}
	if !isEntitled(repoFullName, t, tiers, owner) {
		return decideResult{actionDeny, t, reasonNotEntitled}
	}
	return decideResult{actionSpawn, t, reasonNone}
}
