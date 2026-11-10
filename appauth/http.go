package appauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// mintTimeout bounds one mint end to end (Python: urlopen timeout=20). Get
	// holds the token mutex across the call, so without it one stalled
	// connection would queue every spawn/cancel goroutine and hang the oneshot
	// reaper.
	mintTimeout = 20 * time.Second
	// maxMintBody caps what is read from GitHub; real replies are ~1 KiB.
	maxMintBody = 1 << 20
	// maxErrMessage bounds the GitHub error message echoed into our errors.
	maxErrMessage = 200
)

// newMintClient is the default client for the access_tokens call.
func newMintClient() *http.Client {
	return &http.Client{Timeout: mintTimeout}
}

// tokenBody is the access_tokens endpoint response.
type tokenBody struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// errorBody is GitHub's error envelope. Only the message is surfaced: it is
// the operator-useful part ("Bad credentials"), and nothing else of the body
// is trusted into logs.
type errorBody struct {
	Message string `json:"message"`
}

// Scope narrows what a minted token may do: GitHub grants only the named
// permissions, and with Repositories set, only on those repositories.
type Scope struct {
	Permissions  map[string]string `json:"permissions"`
	Repositories []string          `json:"repositories"`
}

// mintInstallationToken POSTs to GitHub's access_tokens endpoint with the given
// App JWT and returns the minted token and its expiry (unix seconds). A nil
// scope sends no body, which mints a token with the installation's full
// permissions. It never returns an empty token without an error: an empty
// bearer would pass the broker's fail-closed startup proof and 401 on every
// later call.
func mintInstallationToken(client *http.Client, apiBase string, installationID int, jwt string, scope *Scope) (string, float64, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, installationID)
	var reqBody io.Reader = http.NoBody
	if scope != nil {
		payload, err := json.Marshal(scope)
		if err != nil {
			return "", 0, fmt.Errorf("github access_tokens: cannot encode scope: %w", err)
		}
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(http.MethodPost, url, reqBody)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if scope != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Redirects are never followed: the JWT is for api.github.com alone, and a
	// 307/308 would replay the POST with its Authorization header elsewhere.
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMintBody+1))
	if err != nil {
		return "", 0, err
	}
	if len(body) > maxMintBody {
		return "", 0, fmt.Errorf("github access_tokens: response exceeds %d bytes", maxMintBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, statusError(resp.Status, body, jwt)
	}
	var tb tokenBody
	if err := json.Unmarshal(body, &tb); err != nil {
		return "", 0, fmt.Errorf("github access_tokens: undecodable response: %w", err)
	}
	if tb.Token == "" {
		return "", 0, errors.New("github access_tokens: response has no token")
	}
	if tb.ExpiresAt == "" {
		return "", 0, errors.New("github access_tokens: response has no expires_at")
	}
	exp, err := expiry(tb.ExpiresAt)
	if err != nil {
		return "", 0, fmt.Errorf("github access_tokens: bad expires_at: %w", err)
	}
	return tb.Token, exp, nil
}

// statusError describes a non-2xx reply. The JWT is scrubbed from the echoed
// message in case an intermediary reflects request headers.
func statusError(status string, body []byte, jwt string) error {
	var eb errorBody
	if json.Unmarshal(body, &eb) != nil || eb.Message == "" {
		return fmt.Errorf("github access_tokens returned %s", status)
	}
	msg := strings.ReplaceAll(eb.Message, jwt, "[redacted]")
	if r := []rune(msg); len(r) > maxErrMessage {
		msg = string(r[:maxErrMessage])
	}
	return fmt.Errorf("github access_tokens returned %s: %q", status, msg)
}
