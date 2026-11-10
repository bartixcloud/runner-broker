// Package hcloud is the small Hetzner Cloud REST client the broker needs.
//
// It speaks the documented v1 HTTP API directly: metal runs static Go binaries
// and has no hcloud CLI or SDK (AGENTS.md). The token is held in memory and sent
// only in the Authorization header; it never appears in argv, logs or errors.
package hcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// BaseURL is the current Hetzner Cloud API root.
	BaseURL = "https://api.hetzner.cloud/v1"
	// CallTimeout bounds one HTTP call. The broker's create loop adds its own
	// 25 s bound across locations.
	CallTimeout = 20 * time.Second
	// maxBody keeps a hostile or broken reply from filling memory.
	maxBody = 8 << 20
	// maxMessage is the part of an API error message we retain.
	maxMessage = 200
)

// apiBase and httpTransport are the package-level seams, in the same spirit as
// the broker's `var gh = ghHTTP`: a test swaps the transport and never opens a
// socket. Production never changes them.
var (
	apiBase                         = BaseURL
	httpTransport http.RoundTripper = http.DefaultTransport
)

// APIError is a non-2xx Hetzner reply. Status is the HTTP status; Code is the
// machine-readable error code such as resource_unavailable when the API
// supplied one. Source: docs.hetzner.cloud/reference/cloud — error codes.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hcloud API: HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(strconv.Quote(e.Message))
	}
	return b.String()
}

// Server is the part of a Hetzner server the broker reads.
type Server struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
}

// Snapshot is a Hetzner image of type snapshot.
type Snapshot struct {
	ID      int64             `json:"id"`
	Type    string            `json:"type"`
	Status  string            `json:"status"`
	Name    string            `json:"name"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
}

// FirewallRef attaches one firewall to a new server.
type FirewallRef struct {
	Firewall int64 `json:"firewall"`
}

// PublicNet is D7: public IPv4 only, no IPv6, no private network.
type PublicNet struct {
	EnableIPv4 bool `json:"enable_ipv4"`
	EnableIPv6 bool `json:"enable_ipv6"`
}

// CreateRequest is the POST /servers body. There is deliberately no SSHKeys
// field: the snapshot has no key and the firewall has no inbound rules.
type CreateRequest struct {
	Name             string            `json:"name"`
	ServerType       string            `json:"server_type"`
	Image            int64             `json:"image"`
	Location         string            `json:"location"`
	UserData         string            `json:"user_data"`
	Labels           map[string]string `json:"labels"`
	Firewalls        []FirewallRef     `json:"firewalls,omitempty"`
	PublicNet        PublicNet         `json:"public_net"`
	StartAfterCreate bool              `json:"start_after_create"`
}

// Firewall is the part of GET/POST /firewalls the broker reads.
type Firewall struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Client is safe for concurrent use. It caches the id of each firewall it has
// resolved, so a burst of spawns after a restart makes one lookup.
type Client struct {
	token string

	mu          sync.Mutex
	firewallIDs map[string]int64
}

// New builds a client for one project token.
func New(token string) *Client {
	return &Client{token: token, firewallIDs: map[string]int64{}}
}

// ListServers returns every server matching selector. The API pages at 50, and
// admission/deletion must see every labelled server, so the client follows the
// documented meta.pagination.next_page.
func (c *Client) ListServers(selector string) ([]Server, error) {
	return c.ListServersContext(context.Background(), selector)
}

// ListServersContext is ListServers with the caller's deadline.
func (c *Client) ListServersContext(ctx context.Context, selector string) ([]Server, error) {
	var all []Server
	for page := 1; ; page++ {
		q := url.Values{"per_page": []string{"50"}}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		var reply struct {
			Servers []Server `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, "/servers", q, nil, &reply); err != nil {
			return nil, err
		}
		all = append(all, reply.Servers...)
		if reply.Meta.Pagination.NextPage == nil {
			return all, nil
		}
		if page >= 100 {
			return nil, errors.New("hcloud API: server list did not terminate within 100 pages")
		}
	}
}

// Prove is the startup proof: one authenticated GET /servers?per_page=1.
func (c *Client) Prove() error {
	return c.ProveContext(context.Background())
}

// ProveContext is Prove with the caller's deadline.
func (c *Client) ProveContext(ctx context.Context) error {
	q := url.Values{"per_page": []string{"1"}}
	return c.do(ctx, http.MethodGet, "/servers", q, nil, nil)
}

// CreateServer creates one server and returns the preliminary server object.
// The caller does not poll the create action: a booted VM registers itself.
func (c *Client) CreateServer(req CreateRequest) (Server, error) {
	return c.CreateServerContext(context.Background(), req)
}

// CreateServerContext is CreateServer with the caller's deadline.
func (c *Client) CreateServerContext(ctx context.Context, req CreateRequest) (Server, error) {
	var reply struct {
		Server Server `json:"server"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", nil, req, &reply); err != nil {
		return Server{}, err
	}
	return reply.Server, nil
}

// DeleteServer deletes a server. Source: docs.hetzner.cloud/reference/cloud —
// DELETE /servers/{id}. A 404 is success: the goal is absence.
func (c *Client) DeleteServer(id int64) error {
	err := c.do(context.Background(), http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// ListSnapshots returns snapshots matching selector, newest first. Source:
// docs.hetzner.cloud/reference/cloud — GET /images takes type, label_selector
// and sort=created:desc. The client sorts again so a proxy that drops `sort`
// cannot silently pick an old base image.
func (c *Client) ListSnapshots(selector string) ([]Snapshot, error) {
	return c.ListSnapshotsContext(context.Background(), selector)
}

// ListSnapshotsContext is ListSnapshots with the caller's deadline.
func (c *Client) ListSnapshotsContext(ctx context.Context, selector string) ([]Snapshot, error) {
	q := url.Values{"type": []string{"snapshot"}}
	if selector != "" {
		q.Set("label_selector", selector)
	}
	q.Set("sort", "created:desc")
	var reply struct {
		Images []Snapshot `json:"images"`
	}
	if err := c.do(ctx, http.MethodGet, "/images", q, nil, &reply); err != nil {
		return nil, err
	}
	sort.SliceStable(reply.Images, func(i, j int) bool {
		if reply.Images[i].Created.Equal(reply.Images[j].Created) {
			return reply.Images[i].ID > reply.Images[j].ID
		}
		return reply.Images[i].Created.After(reply.Images[j].Created)
	})
	return reply.Images, nil
}

// EnsureFirewall returns the id of the named firewall, creating it with no rules
// if it does not exist. Source: docs.hetzner.cloud/reference/cloud — GET
// /firewalls?name=... and POST /firewalls. A concurrent create by another
// broker instance is tolerated: the loser re-reads by name. The id is cached
// for this process.
func (c *Client) EnsureFirewall(name string) (int64, error) {
	return c.EnsureFirewallContext(context.Background(), name)
}

// EnsureFirewallContext is EnsureFirewall with the caller's deadline.
func (c *Client) EnsureFirewallContext(ctx context.Context, name string) (int64, error) {
	// Hold the lock across the network calls: a burst of spawns should make one
	// lookup, and two local goroutines must not race to create the same firewall.
	c.mu.Lock()
	defer c.mu.Unlock()
	if id := c.firewallIDs[name]; id > 0 {
		return id, nil
	}
	id, found, err := c.findFirewall(ctx, name)
	if err != nil {
		return 0, err
	}
	if found {
		c.cacheFirewall(name, id)
		return id, nil
	}
	body := struct {
		Name string `json:"name"`
	}{Name: name}
	var reply struct {
		Firewall Firewall `json:"firewall"`
	}
	err = c.do(ctx, http.MethodPost, "/firewalls", nil, body, &reply)
	if err == nil {
		if reply.Firewall.ID < 1 {
			return 0, fmt.Errorf("hcloud API: firewall %q was created without a positive id", name)
		}
		c.cacheFirewall(name, reply.Firewall.ID)
		return reply.Firewall.ID, nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Code == "uniqueness_error" || apiErr.Code == "conflict" ||
		apiErr.Status == http.StatusConflict) {
		id, found, findErr := c.findFirewall(ctx, name)
		if findErr != nil {
			return 0, findErr
		}
		if found {
			c.cacheFirewall(name, id)
			return id, nil
		}
	}
	return 0, err
}

func (c *Client) cacheFirewall(name string, id int64) {
	if c.firewallIDs == nil {
		c.firewallIDs = map[string]int64{}
	}
	c.firewallIDs[name] = id
}

func (c *Client) findFirewall(ctx context.Context, name string) (int64, bool, error) {
	q := url.Values{"name": []string{name}}
	var reply struct {
		Firewalls []Firewall `json:"firewalls"`
	}
	if err := c.do(ctx, http.MethodGet, "/firewalls", q, nil, &reply); err != nil {
		return 0, false, err
	}
	for _, fw := range reply.Firewalls {
		if fw.Name == name && fw.ID > 0 {
			return fw.ID, true, nil
		}
	}
	return 0, false, nil
}

// do builds one authenticated request. Every call gets its own 20 s bound in
// addition to the caller's context, so a wedged connection cannot hold a broker
// goroutine forever.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("hcloud API: cannot encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(callCtx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		Timeout:   CallTimeout,
		Transport: httpTransport,
		// Never replay the bearer token at a redirect target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxBody {
		return fmt.Errorf("hcloud API: response exceeds %d bytes", maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.apiError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("hcloud API: undecodable response: %w", err)
	}
	return nil
}

func (c *Client) apiError(status int, data []byte) *APIError {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	msg := envelope.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if c.token != "" {
		msg = strings.ReplaceAll(msg, c.token, "[redacted]")
	}
	if r := []rune(msg); len(r) > maxMessage {
		msg = string(r[:maxMessage])
	}
	return &APIError{Status: status, Code: envelope.Error.Code, Message: msg}
}
