package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

const (
	// statusDescriptionLimit is GitHub's cap on a commit status description.
	statusDescriptionLimit = 140
	// statusContextPrefix makes one context per host: several VMs converge one repo.
	statusContextPrefix = "nuk/converge/"
	// convergeStatusPath is where nuk records the last cycle's outcome on a host.
	convergeStatusPath = "/var/lib/nuk/converge-status.json"
	// statusPostTimeout bounds the default client for one status post, so a hung
	// GitHub cannot hold Poke open.
	statusPostTimeout = 20 * time.Second
	// unreadableStatus describes a host whose status file could not be read at all.
	unreadableStatus = "host unreachable or status unreadable"
)

// Commit status states (GitHub's closed set, minus "pending", which this broker
// never posts).
const (
	stateSuccess = "success"
	stateFailure = "failure"
	stateError   = "error"
)

// nuk's converge result values (nuk/nuk/records.py SourceBlock.result).
const (
	resultOK      = "ok"
	resultRefused = "refused"
)

var (
	// commitPattern is the only commit shape that may enter a GitHub URL.
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// repoPattern is "owner/name" and nothing that could reshape a URL path: a
	// segment may not start with "." (so neither half can be "." or "..").
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*/[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
)

// convergeStatus is the part of nuk's converge-status.json a report reads; the rest
// of the document (apply lists, manifests) stays undecoded.
type convergeStatus struct {
	Sources map[string]convergeBlock `json:"sources"`
}

// convergeBlock mirrors one SourceBlock on the wire.
type convergeBlock struct {
	Result string `json:"result"`
	Error  string `json:"error"`
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
}

// ConvergeState turns a host's converge-status.json into the state and description
// of a commit status. A host has succeeded only when the source applied exactly the
// pushed commit; a different commit means it applied something else (an older tag,
// a newer one, or nothing after a refusal) and must read red.
func ConvergeState(source, commit, statusJSON string) (state, description string) {
	var status convergeStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return stateError, clip("unreadable converge status: "+err.Error(), statusDescriptionLimit)
	}
	block, ok := status.Sources[source]
	if !ok {
		return stateError, clip("source "+source+" not in converge status", statusDescriptionLimit)
	}
	if block.Commit != commit {
		return stateFailure, describeMismatch(block)
	}
	switch block.Result {
	case resultOK:
		return stateSuccess, clip(appliedDescription(block.Tag), statusDescriptionLimit)
	case resultRefused:
		return stateFailure, clip(refusedDescription(block.Error), statusDescriptionLimit)
	default:
		return stateError, clip("converge status has result "+strconv.Quote(block.Result), statusDescriptionLimit)
	}
}

// describeMismatch says why the host's last apply is not this tag, naming the tag
// and nuk's error when the host recorded them.
func describeMismatch(block convergeBlock) string {
	parts := []string{"host did not apply this tag"}
	if block.Tag != "" {
		parts = append(parts, "tag "+block.Tag)
	}
	if block.Error != "" {
		parts = append(parts, block.Error)
	}
	return clip(strings.Join(parts, ": "), statusDescriptionLimit)
}

func appliedDescription(tag string) string {
	if tag == "" {
		return "applied"
	}
	return "applied " + tag
}

func refusedDescription(hostError string) string {
	if hostError == "" {
		return "refused"
	}
	return "refused: " + hostError
}

// GitHubPoster posts commit statuses with a token minted per call, narrowed to
// statuses:write on the one repository. The zero value posts to github.com.
type GitHubPoster struct {
	AppEnvFile string
	KeyPath    string
	APIBase    string
	Client     *http.Client
}

// statusBody is GitHub's create-a-commit-status request.
type statusBody struct {
	State       string `json:"state"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

// Post implements StatusPoster. Every failure is returned for the caller to log:
// reporting is the only side effect and must never change what a poke did.
func (g GitHubPoster) Post(ctx context.Context, job Report, statusContext, state, description string) error {
	if !repoPattern.MatchString(job.Repo) {
		return fmt.Errorf("status: %q is not an owner/name repository", clip(job.Repo, logFieldLimit))
	}
	if !commitPattern.MatchString(job.Commit) {
		return fmt.Errorf("status for %s: %q is not a 40-character commit", job.Repo, clip(job.Commit, logFieldLimit))
	}
	_, repo, _ := strings.Cut(job.Repo, "/") // repoPattern guarantees both halves

	appID, err := appauth.AppID(g.AppEnvFile)
	if err != nil {
		return fmt.Errorf("status for %s: %w", job.Repo, err)
	}
	client := g.client()
	token, err := appauth.MintScoped(client, g.apiBase(), appID, job.Installation, g.KeyPath, appauth.Scope{
		Permissions:  map[string]string{"statuses": "write"},
		Repositories: []string{repo},
	})
	if err != nil {
		return fmt.Errorf("status for %s: %w", job.Repo, err)
	}

	payload, err := json.Marshal(statusBody{State: state, Description: description, Context: statusContext})
	if err != nil {
		return fmt.Errorf("status for %s: cannot encode body: %w", job.Repo, err)
	}
	url := g.apiBase() + "/repos/" + job.Repo + "/statuses/" + job.Commit
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("status for %s: %w", job.Repo, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	// Redirects are never followed: a 307 would replay the POST with its bearer
	// token to whatever host the reply named.
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return fmt.Errorf("status for %s: %w", job.Repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain what GitHub sent so the connection can be reused; the body is never
	// surfaced (it can echo the request).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	// GitHub answers 201 for a created status; any other status means it is not
	// on the commit and the tag stays unmarked.
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("github statuses returned %s", resp.Status)
	}
	return nil
}

func (g GitHubPoster) apiBase() string {
	if g.APIBase == "" {
		return appauth.GHAPI
	}
	return strings.TrimRight(g.APIBase, "/")
}

func (g GitHubPoster) client() *http.Client {
	if g.Client == nil {
		return &http.Client{Timeout: statusPostTimeout}
	}
	return g.Client
}
