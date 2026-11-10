// hcloud support for the reaper: a narrow, typed reader of the parts of
// tiers.yml the reaper needs, the tenant token file checks, and the Hetzner
// listing scope.
//
// The broker's tier and token code lives in package main of cmd/bcx-broker and
// cannot be imported, so every rule below mirrors a named function there. The
// broker stays the validator: unknown tier keys are ignored here, and only the
// keys that decide a delete are read. Each mirrored rule names its source.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bartixcloud/runner-broker/hcloud"
	"gopkg.in/yaml.v3"
)

const (
	// hcloudStatusOff is the Hetzner server status of a VM whose runner has
	// exited and powered it off. Powered-off servers are still billed.
	hcloudStatusOff = "off"

	// hcloudMaxAgeDefault mirrors cmd/bcx-broker/tiers.go hcloudMaxAgeDefault:
	// the reaper's hard ceiling when an hcloud tier omits max_age.
	hcloudMaxAgeDefault = 2 * time.Hour
)

// backendKind is the closed set of VM hosts a tier can run on. Mirrors
// cmd/bcx-broker/tiers.go backendKind: the zero value is Incus, so a tier that
// predates the backend key keeps the host it always ran on.
type backendKind int

const (
	backendIncus backendKind = iota
	backendHcloud
)

func (b backendKind) String() string {
	switch b {
	case backendIncus:
		return "incus"
	case backendHcloud:
		return "hcloud"
	default:
		panic(fmt.Sprintf("unknown tier backend %d", b))
	}
}

// tier is the part of one tiers.yml entry the reaper needs. Unknown keys are
// ignored on purpose: cmd/bcx-broker/tiers.go decodeTier is the validator and
// refuses a bad file at the broker's own startup.
type tier struct {
	Backend   backendKind
	Allowlist []string
	MaxAge    time.Duration
}

// tiersFile mirrors cmd/bcx-broker/config.go loadConfig: BROKER_TIERS_FILE, with
// the same default when it is unset or empty.
func tiersFile(env map[string]string) string {
	if p := env["BROKER_TIERS_FILE"]; p != "" {
		return p
	}
	return "/etc/bcx-broker/tiers.yml"
}

// loadTiers mirrors cmd/bcx-broker/tiers.go loadTiers: a missing or malformed
// file fails loudly (a tier the reaper cannot see is a paid VM it cannot reap),
// and tiers are decoded in name order so the first error is deterministic.
func loadTiers(path string) (map[string]tier, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tiers map[string]yaml.Node `yaml:"tiers"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Tiers) == 0 {
		return nil, fmt.Errorf("%s: no tiers defined", path)
	}
	names := make([]string, 0, len(doc.Tiers))
	for name := range doc.Tiers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make(map[string]tier, len(names))
	for _, name := range names {
		node := doc.Tiers[name]
		t, err := decodeTier(&node)
		if err != nil {
			return nil, fmt.Errorf("%s: tier %q %w", path, name, err)
		}
		out[name] = t
	}
	return out, nil
}

// decodeTier reads backend for every tier and, only for an hcloud tier, the
// allowlist and max_age that decide whether and when the reaper deletes. An
// Incus tier's allowlist and max_age are not read: the Incus path does not
// depend on them, and the broker has already validated the file it serves from.
func decodeTier(n *yaml.Node) (tier, error) {
	var fields map[string]yaml.Node
	if n.Kind != yaml.MappingNode || n.Decode(&fields) != nil {
		return tier{}, errors.New("must be a mapping")
	}
	var t tier
	var err error
	if t.Backend, err = decodeBackend(fields); err != nil {
		return tier{}, err
	}
	switch t.Backend {
	case backendIncus:
		return t, nil
	case backendHcloud:
		if t.Allowlist, err = decodeAllowlist(fields); err != nil {
			return tier{}, err
		}
		if t.MaxAge, err = decodeMaxAge(fields); err != nil {
			return tier{}, err
		}
		return t, nil
	default:
		panic(fmt.Sprintf("unknown tier backend %d", t.Backend))
	}
}

// decodeBackend mirrors cmd/bcx-broker/tiers.go decodeBackend: absent, null or
// "incus" means Incus; "hcloud" selects Hetzner; anything else is an error
// naming the key and the value.
func decodeBackend(fields map[string]yaml.Node) (backendKind, error) {
	node, ok := fields["backend"]
	if !ok || node.ShortTag() == "!!null" {
		return backendIncus, nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return 0, fmt.Errorf("backend must be a string")
	}
	switch s {
	case "incus":
		return backendIncus, nil
	case "hcloud":
		return backendHcloud, nil
	default:
		return 0, fmt.Errorf("backend must be %q or %q, got %q", backendIncus.String(), backendHcloud.String(), s)
	}
}

// stringList is a YAML sequence whose every element is a string. Mirrors
// cmd/bcx-broker/tiers.go stringList.
type stringList []string

func (l *stringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return errors.New("must be a list")
	}
	out := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode || item.ShortTag() != "!!str" {
			return fmt.Errorf("line %d: entries must be strings", item.Line)
		}
		out = append(out, item.Value)
	}
	*l = out
	return nil
}

// decodeAllowlist mirrors cmd/bcx-broker/tiers.go decodeTier's allowlist rule:
// a non-empty list of strings. A scalar `allowlist: "org/repo"` must not pass as
// a list, or membership would silently become substring matching.
func decodeAllowlist(fields map[string]yaml.Node) ([]string, error) {
	var allow stringList
	if node := fields["allowlist"]; node.Decode(&allow) != nil || len(allow) == 0 {
		return nil, errors.New(`allowlist must be a non-empty list of strings — use ["*"] to mean 'any repo' explicitly`)
	}
	return allow, nil
}

// decodeMaxAge has the same accept/reject set as cmd/bcx-broker/tiers.go
// decodeMaxAge: absent or null means hcloudMaxAgeDefault; a zero or negative
// duration would reap every VM immediately, so it is an error.
func decodeMaxAge(fields map[string]yaml.Node) (time.Duration, error) {
	node, ok := fields["max_age"]
	if !ok || node.ShortTag() == "!!null" {
		return hcloudMaxAgeDefault, nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return 0, fmt.Errorf("max_age must be a positive Go duration (for example 2h)")
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("max_age must be a positive Go duration (for example 2h), got %q", s)
	}
	return d, nil
}

// tierIsUsable mirrors cmd/bcx-broker/broker.go tierIsUsable: the wildcard or an
// entry whose owner is this tenant's target owner.
func tierIsUsable(t tier, targetOwner string) bool {
	owner := asciiLower(targetOwner)
	for _, a := range t.Allowlist {
		if a == "*" {
			return true
		}
		if ownerOf(a) == owner {
			return true
		}
	}
	return false
}

// ownerOf mirrors cmd/bcx-broker/broker.go ownerOf: "Juno/repo" -> "juno".
func ownerOf(fullName string) string {
	if i := strings.IndexByte(fullName, '/'); i >= 0 {
		return asciiLower(fullName[:i])
	}
	return asciiLower(fullName)
}

// asciiLower mirrors cmd/bcx-broker/broker.go asciiLower: A-Z only. Owners and
// labels are ASCII, and Unicode folding would match names GitHub does not.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// hcloudTokenPath mirrors cmd/bcx-broker/config.go loadConfig: the override
// wins when set and non-empty; otherwise /etc/bcx-broker/<tenant>.hcloud-token
// (spec §4.1).
func hcloudTokenPath(env map[string]string) (string, error) {
	if p := env["BROKER_HCLOUD_TOKEN_FILE"]; p != "" {
		return p, nil
	}
	tenant := env["BROKER_TENANT"]
	if tenant == "" {
		return "", errors.New("BROKER_TENANT is required to locate the hcloud token file")
	}
	return "/etc/bcx-broker/" + tenant + ".hcloud-token", nil
}

// readHcloudToken mirrors the file checks in cmd/bcx-broker/config.go
// loadHcloudClient (spec §4.1): regular file, no group or other access,
// root-owned when the unit runs as root, non-empty after trimming. Unlike the
// broker it does not prove the token with a call: the sweep's own list is the
// proof, and a failure there aborts the pass. The token never enters an error.
func readHcloudToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("hcloud token file %s: %w", path, err)
	}
	var errs []error
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
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return token, nil
}

// hcloudOps is the slice of the hcloud client the reaper uses. Production is
// *hcloud.Client; tests swap a fake and never open a socket.
type hcloudOps interface {
	ListServersContext(context.Context, string) ([]hcloud.Server, error)
	DeleteServer(int64) error
}

// hcloudSelector mirrors cmd/bcx-broker/hcloud_backend.go hcloudSelector: the
// one listing scope, managed by us, this tenant, one tier.
func hcloudSelector(tenant, tierName string) string {
	return "bcx-managed=ci,bcx-tenant=" + tenant + ",bcx-tier=" + tierName
}

// hcloudPass is what one sweep learned from Hetzner: the servers to judge and
// the client that will delete them.
type hcloudPass struct {
	instances []instance
	tiers     []string
	api       hcloudOps
}

// hcloudSweep loads tiers.yml and, only when this tenant can use at least one
// hcloud tier, the tenant token and every usable tier's servers. No usable tier
// means no token read and no Hetzner call: the pass behaves exactly as the
// Incus-only reaper did.
func (d deps) hcloudSweep(env map[string]string) (hcloudPass, error) {
	if d.loadTiers == nil {
		// The seam is unset only in tests that do not exercise hcloud; a
		// deployment's defaultDeps always wires it.
		return hcloudPass{}, nil
	}
	tiers, err := d.loadTiers(tiersFile(env))
	if err != nil {
		return hcloudPass{}, fmt.Errorf("tiers: %w", err)
	}
	var usable []string
	for name, t := range tiers {
		if t.Backend == backendHcloud && tierIsUsable(t, env["BROKER_TARGET"]) {
			usable = append(usable, name)
		}
	}
	if len(usable) == 0 {
		return hcloudPass{}, nil
	}
	slices.Sort(usable)
	if d.readToken == nil || d.newHcloudClient == nil {
		return hcloudPass{}, errors.New("hcloud support is not wired into the reaper")
	}
	// The tenant names both the selector and the default token path, so it is
	// checked before any file is opened.
	tenant := env["BROKER_TENANT"]
	if tenant == "" {
		return hcloudPass{}, errors.New("BROKER_TENANT is required to list hcloud servers")
	}
	tokenPath, err := hcloudTokenPath(env)
	if err != nil {
		return hcloudPass{}, err
	}
	token, err := d.readToken(tokenPath)
	if err != nil {
		return hcloudPass{}, err
	}
	api := d.newHcloudClient(token)
	pass := hcloudPass{tiers: usable, api: api}
	for _, name := range usable {
		servers, err := api.ListServersContext(context.Background(), hcloudSelector(tenant, name))
		if err != nil {
			return hcloudPass{}, fmt.Errorf("hcloud list tier %q: %w", name, err)
		}
		maxAge := tiers[name].MaxAge
		for _, s := range servers {
			// The selector already scopes the reply. Re-checking keeps a broken
			// or hostile reply from steering a destructive step, as the Incus
			// list does with its ^ci- filter.
			if s.ID <= 0 {
				return hcloudPass{}, fmt.Errorf("hcloud list tier %q returned server %q without a positive id", name, s.Name)
			}
			if !strings.HasPrefix(s.Name, "ci-") {
				return hcloudPass{}, fmt.Errorf("hcloud list tier %q returned server %q, want only ci-* names", name, s.Name)
			}
			// The selector is the tenant boundary; re-checking its labels keeps
			// a reply that ignored it (proxy, fake, API bug) from steering a
			// delete across tenants or tiers.
			if s.Labels["bcx-managed"] != "ci" || s.Labels["bcx-tenant"] != tenant || s.Labels["bcx-tier"] != name {
				return hcloudPass{}, fmt.Errorf("hcloud list tier %q returned server %q outside the tenant/tier scope", name, s.Name)
			}
			if s.Created.IsZero() {
				return hcloudPass{}, fmt.Errorf("hcloud list tier %q returned server %q without a creation time", name, s.Name)
			}
			pass.instances = append(pass.instances, instance{
				Backend:   backendHcloud,
				Tier:      name,
				ID:        s.ID,
				Name:      s.Name,
				CreatedAt: unixSeconds(s.Created),
				Status:    s.Status,
				MaxAge:    maxAge,
			})
		}
	}
	return pass, nil
}
