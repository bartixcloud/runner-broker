package dispatcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultOrgSpiegelWorkflow    = "fakten.yml"
	defaultOrgSpiegelOrgWorkflow = "org.yml"
	defaultOrgSpiegelRef         = "main"
	defaultOrgSicherungDir       = "/usr/local/lib/sicherung"
	defaultOrgSicherungWurzel    = "/srv/sicherung"
	defaultOrgBefundDatei        = "/var/lib/sicherung/befunde.jsonl"
	defaultSicherungTimeout      = 10 * time.Minute
	defaultResticTimeout         = 6 * time.Hour

	// maxRepoNameLen is GitHub's own cap on a repository name.
	maxRepoNameLen = 100
)

var (
	// repoNamePattern is the closed alphabet for repository names that reach argv, file
	// paths and workflow inputs. A leading '-' would act as an option.
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)
	// workflowFilePattern is a bare workflow file name: no path, always .yml or .yaml.
	workflowFilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.ya?ml$`)
	// gitRefPattern is a conservative branch name for the workflow_dispatch ref.
	gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// validRepoName is the strict check every payload-derived repository name passes
// before it may reach argv, a path or a workflow input.
func validRepoName(s string) bool {
	return s != "" && len(s) <= maxRepoNameLen && repoNamePattern.MatchString(s)
}

// OrgConfig is the optional "org" section of dispatcher.json. Its presence switches
// the instance into org mode (spec K1/K8): a second instance of this binary with its
// own port, HMAC secret and table. The credentials of the akd-spiegel App are not
// here; they come from files named in the environment (see LoadOrgAuth).
type OrgConfig struct {
	// Owner is the GitHub organisation whose events this instance serves.
	Owner string
	// SpiegelRepo is "<owner>/<name>", the mirror repo the facts generator commits to.
	SpiegelRepo string
	// SpiegelWorkflow is dispatched with the input repo=<name>.
	SpiegelWorkflow string
	// SpiegelOrgWorkflow is the org-wide workflow, dispatched without inputs.
	SpiegelOrgWorkflow string
	// SpiegelRef is the branch the workflows are dispatched on.
	SpiegelRef string
	// SpiegelInstallation is the installation id of the akd-spiegel App on SpiegelRepo.
	SpiegelInstallation int
	// SicherungDir holds sichern.sh and restic-sichern.sh.
	SicherungDir string
	// SicherungWurzel holds the backup clones <wurzel>/<repo>.git.
	SicherungWurzel string
	// BefundDatei is the append-only JSON-lines file of findings.
	BefundDatei string
	// Schwellen is when a deletion counts as a mass change.
	Schwellen Schwellen
	// SicherungTimeout bounds one sichern.sh run, ResticTimeout one restic-sichern.sh run.
	SicherungTimeout time.Duration
	ResticTimeout    time.Duration
}

// spiegelFullName is the lower-cased "<owner>/<name>" Route compares payloads against.
func (o OrgConfig) spiegelFullName() string { return strings.ToLower(o.SpiegelRepo) }

// orgWire is the JSON shape. Pointers tell "absent" (-> default) from "present but
// zero" (-> checked like any value), so a typo never becomes a permissive zero.
type orgWire struct {
	Owner               *string          `json:"owner"`
	SpiegelRepo         *string          `json:"spiegel_repo"`
	SpiegelWorkflow     *string          `json:"spiegel_workflow"`
	SpiegelOrgWorkflow  *string          `json:"spiegel_org_workflow"`
	SpiegelRef          *string          `json:"spiegel_ref"`
	SpiegelInstallation *int             `json:"spiegel_installation"`
	SicherungDir        *string          `json:"sicherung_dir"`
	SicherungWurzel     *string          `json:"sicherung_wurzel"`
	BefundDatei         *string          `json:"befund_datei"`
	Schwellen           *schwellenWire   `json:"schwellen"`
	SicherungTimeout    *json.RawMessage `json:"sicherung_timeout"`
	ResticTimeout       *json.RawMessage `json:"restic_timeout"`
}

type schwellenWire struct {
	GeloeschteDateien *int `json:"geloeschte_dateien"`
	GeloeschteZeilen  *int `json:"geloeschte_zeilen"`
}

// UnmarshalJSON decodes strictly (unknown keys fail) and validates every field.
func (o *OrgConfig) UnmarshalJSON(b []byte) error {
	var w orgWire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("org section: %w", err)
	}
	cfg, err := w.resolve()
	if err != nil {
		return fmt.Errorf("org section: %w", err)
	}
	*o = cfg
	return nil
}

func (w orgWire) resolve() (OrgConfig, error) {
	var cfg OrgConfig
	if w.Owner == nil || !validName(*w.Owner) {
		return cfg, errors.New(`"owner" is missing or not a valid name`)
	}
	cfg.Owner = *w.Owner
	if w.SpiegelRepo == nil {
		return cfg, errors.New(`"spiegel_repo" is missing`)
	}
	owner, name, found := strings.Cut(*w.SpiegelRepo, "/")
	if !found || !strings.EqualFold(owner, cfg.Owner) || !validRepoName(name) || name == "." || name == ".." {
		return cfg, fmt.Errorf(`"spiegel_repo" must be <owner>/<repo> with owner %q`, cfg.Owner)
	}
	cfg.SpiegelRepo = *w.SpiegelRepo
	if w.SpiegelInstallation == nil || *w.SpiegelInstallation <= 0 {
		return cfg, errors.New(`"spiegel_installation" is missing or not a positive integer`)
	}
	cfg.SpiegelInstallation = *w.SpiegelInstallation

	var err error
	if cfg.SpiegelWorkflow, err = patternField("spiegel_workflow", w.SpiegelWorkflow, defaultOrgSpiegelWorkflow, workflowFilePattern); err != nil {
		return cfg, err
	}
	if cfg.SpiegelOrgWorkflow, err = patternField("spiegel_org_workflow", w.SpiegelOrgWorkflow, defaultOrgSpiegelOrgWorkflow, workflowFilePattern); err != nil {
		return cfg, err
	}
	if cfg.SpiegelRef, err = patternField("spiegel_ref", w.SpiegelRef, defaultOrgSpiegelRef, gitRefPattern); err != nil {
		return cfg, err
	}
	if strings.Contains(cfg.SpiegelRef, "..") {
		return cfg, errors.New(`"spiegel_ref" is not a valid branch name`)
	}
	if cfg.SicherungDir, err = pathField("sicherung_dir", w.SicherungDir, defaultOrgSicherungDir); err != nil {
		return cfg, err
	}
	if cfg.SicherungWurzel, err = pathField("sicherung_wurzel", w.SicherungWurzel, defaultOrgSicherungWurzel); err != nil {
		return cfg, err
	}
	if cfg.BefundDatei, err = pathField("befund_datei", w.BefundDatei, defaultOrgBefundDatei); err != nil {
		return cfg, err
	}
	if cfg.Schwellen, err = w.Schwellen.resolve(); err != nil {
		return cfg, err
	}
	if cfg.SicherungTimeout, err = durationField("sicherung_timeout", w.SicherungTimeout, defaultSicherungTimeout); err != nil {
		return cfg, err
	}
	cfg.ResticTimeout, err = durationField("restic_timeout", w.ResticTimeout, defaultResticTimeout)
	return cfg, err
}

func (s *schwellenWire) resolve() (Schwellen, error) {
	if s == nil {
		return DefaultSchwellen(), nil
	}
	if s.GeloeschteDateien == nil || s.GeloeschteZeilen == nil {
		return Schwellen{}, errors.New(`"schwellen" needs both "geloeschte_dateien" and "geloeschte_zeilen" (a missing geloeschte_zeilen or geloeschte_dateien would read as 0)`)
	}
	if *s.GeloeschteDateien < 0 || *s.GeloeschteZeilen < 0 {
		return Schwellen{}, errors.New(`"schwellen" values must not be negative`)
	}
	return Schwellen{GeloeschteDateien: *s.GeloeschteDateien, GeloeschteZeilen: *s.GeloeschteZeilen}, nil
}

func patternField(key string, v *string, fallback string, re *regexp.Regexp) (string, error) {
	if v == nil {
		return fallback, nil
	}
	if !re.MatchString(*v) {
		return "", fmt.Errorf("%q is not valid", key)
	}
	return *v, nil
}

func pathField(key string, v *string, fallback string) (string, error) {
	if v == nil {
		return fallback, nil
	}
	if !filepath.IsAbs(*v) || strings.ContainsRune(*v, 0) {
		return "", fmt.Errorf("%q must be an absolute path", key)
	}
	return *v, nil
}

func durationField(key string, raw *json.RawMessage, fallback time.Duration) (time.Duration, error) {
	if raw == nil {
		return fallback, nil
	}
	var text string
	if err := json.Unmarshal(*raw, &text); err != nil {
		return 0, fmt.Errorf("%q must be a duration string such as \"10m\"", key)
	}
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q must be a positive duration such as \"10m\"", key)
	}
	return d, nil
}

// validateOrg checks the org section against the rest of the table.
func validateOrg(o *OrgConfig, installations map[string]Installation) error {
	if o == nil {
		return nil
	}
	for _, entry := range installations {
		if strings.EqualFold(entry.Owner, o.Owner) {
			return nil
		}
	}
	return fmt.Errorf("org: owner %q has no installation in \"installations\"", o.Owner)
}
