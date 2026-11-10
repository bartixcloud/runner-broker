package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

// pinDispatchTimeout bounds the default client for one dispatch, so a hung GitHub
// cannot hold the background goroutine open.
const pinDispatchTimeout = 20 * time.Second

// pinTagPattern is the release tag shape pin.yml accepts; checked again here so
// nothing else can reach the dispatch URL or body.
var pinTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// PinDispatcher starts the pin workflow in a tenant's infra repo. Like StatusPoster
// it is a side effect only: every failure is returned for the caller to log and
// never changes what the webhook reply or a poke did.
type PinDispatcher interface {
	Pin(ctx context.Context, job PinJob) error
}

// GitHubPin dispatches pin.yml with a token minted per call, narrowed to
// actions:write on the one infra repository. The zero value talks to github.com.
type GitHubPin struct {
	AppEnvFile string
	KeyPath    string
	APIBase    string
	Client     *http.Client
}

type pinDispatchBody struct {
	Ref    string `json:"ref"`
	Inputs struct {
		Repo string `json:"repo"`
		Tag  string `json:"tag"`
	} `json:"inputs"`
}

func (g GitHubPin) Pin(ctx context.Context, job PinJob) error {
	return g.pin(ctx, job)
}

func (g GitHubPin) pin(ctx context.Context, job PinJob) error {
	infraRepo := job.Owner + "/" + job.InfraRepo
	if !repoPattern.MatchString(infraRepo) {
		return fmt.Errorf("pin: %q is not an owner/name repository", clip(infraRepo, logFieldLimit))
	}
	if !pinTagPattern.MatchString(job.Tag) {
		return fmt.Errorf("pin for %s: %q is not a vMAJOR.MINOR.PATCH tag", infraRepo, clip(job.Tag, logFieldLimit))
	}
	if job.SourceRepo == "" || strings.Contains(job.SourceRepo, "/") || strings.Contains(job.SourceRepo, "\\") {
		return fmt.Errorf("pin for %s: source repo %q is not a bare name", infraRepo, clip(job.SourceRepo, logFieldLimit))
	}

	appID, err := appauth.AppID(g.AppEnvFile)
	if err != nil {
		return fmt.Errorf("pin for %s: %w", infraRepo, err)
	}
	client := g.client()
	token, err := appauth.MintScoped(client, g.apiBase(), appID, job.Installation, g.KeyPath, appauth.Scope{
		Permissions:  map[string]string{"actions": "write"},
		Repositories: []string{job.InfraRepo},
	})
	if err != nil {
		return fmt.Errorf("pin for %s: %w", infraRepo, err)
	}

	body := pinDispatchBody{Ref: "main"}
	body.Inputs.Repo = job.SourceRepo
	body.Inputs.Tag = job.Tag
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("pin for %s: cannot encode body: %w", infraRepo, err)
	}

	url := g.apiBase() + "/repos/" + infraRepo + "/actions/workflows/pin.yml/dispatches"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("pin for %s: %w", infraRepo, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return fmt.Errorf("pin for %s: %w", infraRepo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("github workflow_dispatch returned %s", resp.Status)
	}
	return nil
}

func (g GitHubPin) apiBase() string {
	if g.APIBase == "" {
		return appauth.GHAPI
	}
	return strings.TrimRight(g.APIBase, "/")
}

func (g GitHubPin) client() *http.Client {
	if g.Client == nil {
		return &http.Client{Timeout: pinDispatchTimeout}
	}
	return g.Client
}
