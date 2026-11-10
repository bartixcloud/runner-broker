package dispatcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// tagRefPrefix selects the v* tags that mean "a new release of an infra repo".
const tagRefPrefix = "refs/tags/v"

// tagRefPattern is the strict semver shape a release tag must have to trigger
// the pin workflow: vMAJOR.MINOR.PATCH with no pre-release or build metadata.
var tagRefPattern = regexp.MustCompile(`^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`)

// platformSource names the platform installation in a converge report: it has no
// tenant, but its infra repo still converges and reports.
const platformSource = "bartixcloud"

// ParsePayload decodes the routing fields of a verified delivery body. Only a JSON
// object is a payload: null, arrays and scalars would otherwise decode into an
// empty Payload and be routed as "unknown installation".
func ParsePayload(body []byte) (Payload, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Payload{}, errors.New("payload is not a JSON object")
	}
	var p Payload
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return Payload{}, fmt.Errorf("decode payload: %w", err)
	}
	return p, nil
}

// Route is the pure decision core. Order matters: a signed ping is answered before
// any lookup; an unknown installation or a foreign repository is rejected before
// push handling (so a tag on someone else's infra repo never pokes); a poke wins
// over the platform installation's "no broker" rule. An instance with an org section
// in its table routes through RouteOrg instead (see Server.handleWebhook).
func Route(event string, p Payload, t Table) Decision {
	if event == "ping" {
		return Decision{Action: ActionPong}
	}
	entry, repo, rejected := authorize(p, t)
	if rejected != nil {
		return *rejected
	}
	if event == "push" {
		if p.Deleted || !strings.HasPrefix(p.Ref, tagRefPrefix) {
			return Decision{Action: ActionIgnore, Reason: "push is not a new v* tag"}
		}
		targets := t.Pokes[repo]
		if len(targets) > 0 {
			// Cloned: the decision outlives the request in the poke goroutine.
			return Decision{Action: ActionPoke, Targets: slices.Clone(targets), Report: reportFor(p, entry)}
		}
		// Not a converge source: a tenant's app repo tag may still trigger a
		// pin of that tenant's infra repo.
		if entry.Tenant == nil {
			return Decision{Action: ActionIgnore, Reason: orDash(repo) + " is not a converge source"}
		}
		if !tagRefPattern.MatchString(p.Ref) {
			return Decision{Action: ActionIgnore, Reason: orDash(repo) + " is not a converge source"}
		}
		infraRepo := strings.ToLower(entry.Owner) + "/infra"
		pinTargets := t.Pokes[infraRepo]
		if len(pinTargets) == 0 {
			return Decision{Action: ActionIgnore, Reason: orDash(repo) + " is not a converge source"}
		}
		installation, _ := strconv.Atoi(string(p.Installation.ID))
		sourceName := repo
		if _, name, ok := strings.Cut(repo, "/"); ok {
			sourceName = name
		}
		job := PinJob{
			Owner:        strings.ToLower(entry.Owner),
			InfraRepo:    "infra",
			SourceRepo:   strings.ToLower(sourceName),
			Tag:          strings.TrimPrefix(p.Ref, "refs/tags/"),
			Installation: installation,
		}
		return Decision{Action: ActionPin, PinJob: job}
	}
	if entry.Tenant == nil {
		return Decision{Action: ActionIgnore, Reason: event + " for the platform installation (no broker)"}
	}
	return Decision{Action: ActionForward, Tenant: *entry.Tenant}
}

// reportFor builds the converge report for a tag push, or nil when the payload
// cannot fill one: a missing report delays a status, never the poke.
func reportFor(p Payload, entry Installation) *Report {
	installation, err := strconv.Atoi(string(p.Installation.ID))
	if err != nil {
		return nil
	}
	if !commitPattern.MatchString(p.HeadCommit.ID) {
		return nil
	}
	source := platformSource
	if entry.Tenant != nil {
		source = *entry.Tenant
	}
	return &Report{
		Repo:         strings.ToLower(p.Repository.FullName),
		Commit:       p.HeadCommit.ID,
		Source:       source,
		Installation: installation,
	}
}

// authorize is the gate Route and RouteOrg share: the installation must be known and
// the repository, if the payload names one, must belong to that installation's owner.
// It returns the installation's entry and the lower-cased full repository name, or
// the reject decision.
func authorize(p Payload, t Table) (Installation, string, *Decision) {
	inst := string(p.Installation.ID)
	entry, ok := t.Installations[inst]
	if !ok {
		return Installation{}, "", &Decision{Action: ActionReject, Reason: "unknown installation " + orDash(inst)}
	}
	repo := strings.ToLower(p.Repository.FullName)
	if repo != "" {
		owner, _, _ := strings.Cut(repo, "/")
		if !strings.EqualFold(owner, entry.Owner) {
			return Installation{}, "", &Decision{Action: ActionReject, Reason: fmt.Sprintf(
				"repository %s is not owned by installation %s (%s)", repo, inst, entry.Owner)}
		}
	}
	return entry, repo, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
