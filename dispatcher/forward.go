package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

const (
	// forwardPath is the broker's webhook route; the broker does no HMAC check, the
	// dispatcher already did it.
	forwardPath = "/github"
	// maxBrokerResponse caps what is read back from a broker: its answers are tiny
	// JSON, so anything bigger is a fault, not a payload to relay.
	maxBrokerResponse = 1 << 20
)

// ForwardHeaders is the whole set of request headers a broker ever sees. Being a
// struct, not a copy of the incoming header map, makes the whitelist structural:
// X-Hub-Signature-256 and everything else cannot be forwarded by accident. An
// empty field is omitted.
type ForwardHeaders struct {
	Event       string // X-GitHub-Event
	Delivery    string // X-GitHub-Delivery
	TargetID    string // X-GitHub-Hook-Installation-Target-ID
	ContentType string // Content-Type
}

func (h ForwardHeaders) apply(dst http.Header) {
	for name, value := range map[string]string{
		"X-GitHub-Event":                       h.Event,
		"X-GitHub-Delivery":                    h.Delivery,
		"X-GitHub-Hook-Installation-Target-ID": h.TargetID,
		"Content-Type":                         h.ContentType,
	} {
		if value != "" {
			dst.Set(name, value)
		}
	}
}

// Response is a broker's answer, relayed to GitHub verbatim.
type Response struct {
	Status int
	Body   []byte
}

// Forwarder delivers webhooks to tenant brokers over unix sockets.
type Forwarder struct {
	// SocketDir holds <tenant>.sock for every tenant broker (0700, root-only).
	SocketDir string
	// Timeout bounds one whole delivery: dial, request and response.
	Timeout time.Duration
}

// Forward POSTs body to <SocketDir>/<tenant>.sock. Any transport failure, an
// oversized answer or an invalid tenant name is an error; the caller turns that
// into a 503 so GitHub records a redeliverable failure. A broker's own error status
// is not an error here: it is relayed as-is.
func (f Forwarder) Forward(ctx context.Context, tenant string, body []byte, h ForwardHeaders) (Response, error) {
	// The tenant comes from the trusted table, but it is also a path component:
	// re-check so a bad table entry can never aim this at another socket.
	if !validName(tenant) {
		return Response{}, fmt.Errorf("refusing to forward: tenant %q is not a valid name", tenant)
	}
	sock := filepath.Join(f.SocketDir, tenant+".sock")
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   f.Timeout,
		// A broker answer is relayed, never chased.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+forwardPath, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	h.apply(req.Header)
	req.Header.Set("User-Agent", "") // an explicitly empty value suppresses Go's default

	resp, err := client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("forward to %s: %w", sock, err)
	}
	defer func() { _ = resp.Body.Close() }() // read side only; nothing to recover

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxBrokerResponse+1))
	if err != nil {
		return Response{}, fmt.Errorf("read response from %s: %w", sock, err)
	}
	if len(out) > maxBrokerResponse {
		return Response{}, errors.New("broker response exceeds the cap")
	}
	return Response{Status: resp.StatusCode, Body: out}, nil
}
