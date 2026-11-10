package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/bartixcloud/runner-broker/mem"
	"gopkg.in/yaml.v3"
)

// backendKind is the closed set of VM hosts a tier can run on. The zero value
// is Incus on purpose: a tier literal that predates the backend key keeps the
// host it always ran on.
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
		return "unknown"
	}
}

// tier is one tier's config from tiers.yml. Per-backend keys are exact: an Incus
// tier may carry network/cpu/mem, an hcloud tier may carry server_type,
// locations and max_age, and neither may carry the other's keys.
type tier struct {
	Backend     backendKind
	Image       string
	ServerType  string
	Locations   []string
	MaxAge      time.Duration
	Network     string
	RunnerGroup string
	CPU         string
	Mem         string
	MaxRunners  int
	Allowlist   []string
	Secrets     []string
}

var incusTierKeys = []string{"image", "network", "runner_group", "cpu", "mem", "max_runners", "allowlist", "secrets"}
var hcloudTierKeys = []string{"image", "server_type", "locations", "runner_group", "max_runners", "allowlist", "secrets"}

// hcloudMaxAgeDefault is the reaper's hard ceiling when a tier omits max_age.
const hcloudMaxAgeDefault = 2 * time.Hour

var hcloudLocations = map[string]struct{}{
	"fsn1": {}, "nbg1": {}, "hel1": {}, "ash": {}, "hil": {}, "sin": {},
}

// scalarText is a YAML string or number rendered the way Python's f-string
// rendered it: `cpu: 2` means limits.cpu=2 and `mem: 4096` is 4096 bytes. Any
// other node (bool, null, list, mapping) is an error, never an empty value.
type scalarText string

func (s *scalarText) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("must be a string or a number")
	}
	switch n.ShortTag() {
	case "!!str":
		*s = scalarText(n.Value)
	case "!!int":
		var v int64
		if err := n.Decode(&v); err != nil {
			return fmt.Errorf("integer out of range: %w", err)
		}
		*s = scalarText(strconv.FormatInt(v, 10))
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return err
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return errors.New("must be a finite number")
		}
		*s = scalarText(pyFloat(f))
	default:
		return fmt.Errorf("must be a string or a number, got %s", n.ShortTag())
	}
	return nil
}

// pyFloat formats like Python's repr(float): positional notation with at
// least one fractional digit between 1e-4 and 1e16, exponent form outside.
func pyFloat(f float64) string {
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	for _, c := range s {
		if c == '.' {
			return s
		}
	}
	return s + ".0"
}

// stringList is a YAML sequence whose every element is a string.
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

// loadTiers parses tiers.yml into map[tierName]tier. Fails loudly and
// completely on any malformed tier — a broker running with a half-loaded tier
// set might silently under-validate one. Loaded once at startup; restart the
// service to pick up edits.
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
	slices.Sort(names) // the first error reported must not depend on map order
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

// decodeTier decodes one tier mapping field by field so every error names the
// offending key. A null value counts as missing. The accepted key set is chosen
// by the backend, so a tier cannot half-run on the wrong host.
func decodeTier(n *yaml.Node) (tier, error) {
	var fields map[string]yaml.Node
	if n.Kind != yaml.MappingNode || n.Decode(&fields) != nil {
		return tier{}, errors.New("must be a mapping")
	}
	backend, err := decodeBackend(fields)
	if err != nil {
		return tier{}, err
	}
	required, optional := tierKeys(backend)
	allowed := make(map[string]struct{}, len(required)+len(optional)+1)
	allowed["backend"] = struct{}{}
	for _, k := range required {
		allowed[k] = struct{}{}
	}
	for _, k := range optional {
		allowed[k] = struct{}{}
	}
	var unknown []string
	for k := range fields {
		if _, ok := allowed[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return tier{}, fmt.Errorf("key(s) %v are not valid for backend %s", unknown, backend)
	}
	var missing []string
	for _, k := range required {
		if f, ok := fields[k]; !ok || f.ShortTag() == "!!null" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return tier{}, fmt.Errorf("missing required key(s): %v", missing)
	}

	var t tier
	t.Backend = backend
	if t.Image, err = decodeText(fields, "image"); err != nil {
		return tier{}, err
	}
	switch backend {
	case backendIncus:
		if t.Network, err = decodeText(fields, "network"); err != nil {
			return tier{}, err
		}
		if t.CPU, err = decodeText(fields, "cpu"); err != nil {
			return tier{}, err
		}
		if t.Mem, err = decodeText(fields, "mem"); err != nil {
			return tier{}, err
		}
		// Refused here, once, instead of at every reservation: a mem the parser
		// rejects would otherwise queue every job for the tier forever.
		if _, err := mem.ParseMemBytes(t.Mem); err != nil {
			return tier{}, fmt.Errorf("mem: %w", err)
		}
	case backendHcloud:
		if t.ServerType, err = decodeText(fields, "server_type"); err != nil {
			return tier{}, err
		}
		if t.Locations, err = decodeLocations(fields); err != nil {
			return tier{}, err
		}
		if t.MaxAge, err = decodeMaxAge(fields); err != nil {
			return tier{}, err
		}
	default:
		panic(fmt.Sprintf("unknown tier backend %d", backend))
	}
	if t.RunnerGroup, err = decodeText(fields, "runner_group"); err != nil {
		return tier{}, err
	}
	// The tag check matters: yaml.v3 would otherwise truncate 4.5 into an int.
	maxRunners := fields["max_runners"]
	if maxRunners.ShortTag() != "!!int" || maxRunners.Decode(&t.MaxRunners) != nil || t.MaxRunners < 1 {
		return tier{}, errors.New("max_runners must be a positive int")
	}
	var allow, secrets stringList
	// A scalar `allowlist: "org/repo"` must not pass as a list: membership
	// would silently become substring matching and over-entitle.
	if node := fields["allowlist"]; node.Decode(&allow) != nil || len(allow) == 0 {
		return tier{}, errors.New(`allowlist must be a non-empty list of strings — use ["*"] to mean 'any repo' explicitly`)
	}
	if backend == backendHcloud {
		for _, a := range allow {
			if a == "*" {
				return tier{}, errors.New(`allowlist must not contain "*": hcloud VMs cost money and are not open to every repo`)
			}
		}
	}
	if node := fields["secrets"]; node.Decode(&secrets) != nil {
		return tier{}, errors.New("secrets must be a list of strings")
	}
	t.Allowlist, t.Secrets = allow, secrets
	return t, nil
}

// tierKeys returns the exact required and optional key sets for a backend.
func tierKeys(backend backendKind) (required, optional []string) {
	switch backend {
	case backendIncus:
		return incusTierKeys, nil
	case backendHcloud:
		return hcloudTierKeys, []string{"max_age"}
	default:
		panic(fmt.Sprintf("unknown tier backend %d", backend))
	}
}

// decodeLocations reads a non-empty list of Hetzner locations from the closed
// set the project may use.
func decodeLocations(fields map[string]yaml.Node) ([]string, error) {
	var locations stringList
	if node := fields["locations"]; node.Decode(&locations) != nil || len(locations) == 0 {
		return nil, errors.New("locations must be a non-empty list of strings")
	}
	for _, loc := range locations {
		if _, ok := hcloudLocations[loc]; !ok {
			return nil, fmt.Errorf("locations contains %q, which is not one of fsn1 nbg1 hel1 ash hil sin", loc)
		}
	}
	return locations, nil
}

// decodeMaxAge reads the optional reaper ceiling. A zero or negative duration
// would reap every VM immediately, so it fails startup instead.
func decodeMaxAge(fields map[string]yaml.Node) (time.Duration, error) {
	node, ok := fields["max_age"]
	if !ok || node.ShortTag() == "!!null" {
		return hcloudMaxAgeDefault, nil
	}
	var s scalarText
	if err := node.Decode(&s); err != nil {
		return 0, fmt.Errorf("max_age %w", err)
	}
	d, err := time.ParseDuration(string(s))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("max_age must be a positive Go duration (for example 2h), got %q", string(s))
	}
	return d, nil
}

// decodeBackend reads the optional backend key: absent, null or "incus" means
// Incus; "hcloud" selects Hetzner. Any other value is an error naming the key
// and the value, because a tier silently landing on the wrong host is worse
// than a refused startup.
func decodeBackend(fields map[string]yaml.Node) (backendKind, error) {
	node, ok := fields["backend"]
	if !ok || node.ShortTag() == "!!null" {
		return backendIncus, nil
	}
	var s scalarText
	if err := node.Decode(&s); err != nil {
		return 0, fmt.Errorf("backend %w", err)
	}
	switch string(s) {
	case "incus":
		return backendIncus, nil
	case "hcloud":
		return backendHcloud, nil
	default:
		return 0, fmt.Errorf("backend must be %q or %q, got %q", backendIncus.String(), backendHcloud.String(), string(s))
	}
}

// decodeText reads a required, non-empty string-or-number field.
func decodeText(fields map[string]yaml.Node, key string) (string, error) {
	node := fields[key]
	var s scalarText
	if err := node.Decode(&s); err != nil {
		return "", fmt.Errorf("%s %w", key, err)
	}
	if s == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return string(s), nil
}
