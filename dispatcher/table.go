package dispatcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

// namePattern is the closed alphabet for tenants, Incus projects and VM names. A
// tenant becomes a socket filename and a project/VM reaches `incus exec` argv, so
// a leading '-' (option injection), '/' (path traversal) and whitespace or shell
// syntax are all refused. Go's `$` is end-of-text, so a trailing newline fails too.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validName(s string) bool { return namePattern.MatchString(s) }

// UnmarshalJSON requires both keys: a missing "tenant" must not silently read as
// the platform installation (null), which would turn a tenant's jobs into no-ops.
func (i *Installation) UnmarshalJSON(b []byte) error {
	var wire struct {
		Owner  string          `json:"owner"`
		Tenant json.RawMessage `json:"tenant"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("installation entry: %w", err)
	}
	if wire.Tenant == nil {
		return errors.New(`installation entry: missing "tenant" (use null for the platform installation)`)
	}
	var tenant *string
	if err := json.Unmarshal(wire.Tenant, &tenant); err != nil {
		return fmt.Errorf("installation entry: tenant: %w", err)
	}
	i.Owner, i.Tenant = wire.Owner, tenant
	return nil
}

// UnmarshalJSON requires exactly [project, vm].
func (p *PokeTarget) UnmarshalJSON(b []byte) error {
	var pair []string
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("poke target must be [project, vm]: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("poke target must be [project, vm] (2 strings), got %d", len(pair))
	}
	p.Project, p.VM = pair[0], pair[1]
	return nil
}

// LoadTable reads and validates dispatcher.json. It fails closed on anything the
// generator would not write: a missing key, an unknown field, a wrongly shaped
// poke target or a name outside namePattern.
func LoadTable(path string) (Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Table{}, err
	}
	t, err := parseTable(data)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func parseTable(data []byte) (Table, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t Table
	if err := dec.Decode(&t); err != nil {
		return Table{}, fmt.Errorf("decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Table{}, errors.New("decode: trailing data after the JSON document")
	}
	if t.Installations == nil {
		return Table{}, errors.New(`missing "installations" mapping`)
	}
	if t.Pokes == nil {
		return Table{}, errors.New(`missing "pokes" mapping`)
	}
	if err := validateInstallations(t.Installations); err != nil {
		return Table{}, err
	}
	if err := validatePokes(t.Pokes); err != nil {
		return Table{}, err
	}
	if err := validateOrg(t.Org, t.Installations); err != nil {
		return Table{}, err
	}
	return t, nil
}

// Both validators walk keys in sorted order so the same bad file always reports
// the same first error.
func validateInstallations(m map[string]Installation) error {
	for _, id := range slices.Sorted(maps.Keys(m)) {
		entry := m[id]
		if entry.Owner == "" {
			return fmt.Errorf("installation %q: owner is empty", id)
		}
		if entry.Tenant != nil && !validName(*entry.Tenant) {
			return fmt.Errorf("installation %q: tenant %q is not a valid name", id, *entry.Tenant)
		}
	}
	return nil
}

func validatePokes(m map[string][]PokeTarget) error {
	for _, repo := range slices.Sorted(maps.Keys(m)) {
		// Route lower-cases the repo before the lookup, so any other key is dead weight.
		owner, name, found := strings.Cut(repo, "/")
		if !found || owner == "" || name == "" || repo != strings.ToLower(repo) {
			return fmt.Errorf("pokes key %q must be a lower-case <owner>/<repo>", repo)
		}
		for _, target := range m[repo] {
			if !validName(target.Project) {
				return fmt.Errorf("pokes %q: project %q is not a valid name", repo, target.Project)
			}
			if !validName(target.VM) {
				return fmt.Errorf("pokes %q: vm %q is not a valid name", repo, target.VM)
			}
		}
	}
	return nil
}
