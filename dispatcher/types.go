package dispatcher

import (
	"encoding/json"
	"fmt"
	"strings"
)

// clip cuts s to at most n bytes (never mid-rune) so attacker- or tool-controlled
// text cannot inflate a log line.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// Action is the closed set of things the dispatcher can do with a verified
// delivery. The zero value is deliberately not an action: a Decision nobody filled
// in must fail closed in the handler, never fall into a permissive default.
type Action int

const (
	actionUnset Action = iota
	// ActionPong answers GitHub's signed ping.
	ActionPong
	// ActionReject is a 403: unknown installation, or a repository its owner does not own.
	ActionReject
	// ActionIgnore is a 202 with nothing done.
	ActionIgnore
	// ActionPoke starts nuk-converge on the decision's targets (contract G4).
	ActionPoke
	// ActionForward relays the delivery to the decision's tenant broker.
	ActionForward
	// ActionPin triggers the owner's infra pin workflow for a tenant app release tag.
	ActionPin
	// ActionOrg runs the decision's org plan (backup, detection, mirror) in the
	// background. Only an instance whose table has an org section produces it.
	ActionOrg
	// actionCount is the number of defined values; tests loop up to it.
	actionCount
)

// String names the action for logs; out-of-range values say so instead of panicking.
func (a Action) String() string {
	switch a {
	case ActionPong:
		return "pong"
	case ActionReject:
		return "reject"
	case ActionIgnore:
		return "ignore"
	case ActionPoke:
		return "poke"
	case ActionForward:
		return "forward"
	case ActionPin:
		return "pin"
	case ActionOrg:
		return "org"
	default: // actionUnset, actionCount and anything out of range

		return fmt.Sprintf("invalid action %d", int(a))
	}
}

// Decision is Route's verdict. Which fields matter depends on Action: Reason for
// reject and ignore, Targets and Report for poke, Tenant for forward, Org for org.
type Decision struct {
	Action  Action
	Reason  string
	Targets []PokeTarget
	// Report is the converge job the poke reports; nil when the payload lacks what
	// a status post needs, and the poke still runs.
	Report *Report
	Tenant string
	// PinJob is carried by ActionPin: the tenant's infra pin workflow inputs.
	PinJob PinJob
	// Org is carried by ActionOrg: the backup, detection and mirror plan.
	Org OrgPlan
}

// InstallationID is the decimal text of installation.id. GitHub sends a JSON
// number; the table keys are strings, so both forms are accepted and the number's
// literal text is kept verbatim (no float rounding, and "102.0" never equals "102").
type InstallationID string

// UnmarshalJSON accepts a number or a string; null leaves the ID empty.
func (id *InstallationID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("installation id: empty value")
	}
	switch c := b[0]; {
	case c == 'n': // null
		*id = ""
		return nil
	case c == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("installation id: %w", err)
		}
		*id = InstallationID(s)
		return nil
	case c == '-' || (c >= '0' && c <= '9'):
		*id = InstallationID(b)
		return nil
	default:
		return fmt.Errorf("installation id: want a number or a string, got %s", b)
	}
}

// InstallationRef is the part of a webhook's "installation" object that routes it.
type InstallationRef struct {
	ID InstallationID `json:"id"`
}

// RepositoryRef is the part of a webhook's "repository" object that routes it.
type RepositoryRef struct {
	FullName string `json:"full_name"`
}

// HeadCommitRef is the part of a webhook's "head_commit" object a status post
// needs: the commit the pushed tag points at.
type HeadCommitRef struct {
	ID string `json:"id"`
}

// Payload holds only the webhook fields Route reads; everything else in GitHub's
// payload is ignored on purpose (the broker re-parses the verbatim body itself).
type Payload struct {
	Installation InstallationRef `json:"installation"`
	Repository   RepositoryRef   `json:"repository"`
	HeadCommit   HeadCommitRef   `json:"head_commit"`
	Ref          string          `json:"ref"`
	Deleted      bool            `json:"deleted"`
}

// Installation is one dispatcher.json "installations" entry. A nil Tenant is the
// platform installation: it has no broker, but its infra repo still pokes.
type Installation struct {
	Owner  string  `json:"owner"`
	Tenant *string `json:"tenant"`
}

// PokeTarget is one Incus VM that converges a given infra repo.
type PokeTarget struct {
	Project string
	VM      string
}

// PinJob is the input to a tenant's infra pin.yml workflow: a tag pushed on an
// app repository that the infra repo must converge to.
type PinJob struct {
	Owner        string
	InfraRepo    string
	SourceRepo   string
	Tag          string
	Installation int
}

// Table is the parsed dispatcher.json: installation.id -> tenant, lower-cased
// "<owner>/infra" -> the VMs to poke, and the optional org section.
type Table struct {
	Installations map[string]Installation `json:"installations"`
	Pokes         map[string][]PokeTarget `json:"pokes"`
	// Org switches the instance into org mode (spec K1/K8); nil, the default and the
	// only value the bartixcloud generator writes, leaves behaviour untouched.
	Org *OrgConfig `json:"org"`
}
