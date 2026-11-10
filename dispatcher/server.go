package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

const (
	webhookPath = "/github"
	healthPath  = "/healthz"

	// GitHub caps webhook payloads at 25 MB.
	defaultMaxBody        = 25 << 20
	defaultPokeTimeout    = 5 * time.Minute
	defaultForwardTimeout = 10 * time.Second

	// logFieldLimit bounds header-derived text in log lines; delivery ids are UUIDs.
	logFieldLimit = 64

	hdrEvent     = "X-GitHub-Event"
	hdrDelivery  = "X-GitHub-Delivery"
	hdrTargetID  = "X-GitHub-Hook-Installation-Target-ID"
	hdrSignature = "X-Hub-Signature-256"
)

// Server is the webhook handler. Build it with NewServer, tweak the exported
// fields before serving (tests do), and never change them afterwards.
type Server struct {
	Table       Table
	Forwarder   Forwarder
	Runner      Runner
	Poster      StatusPoster
	Pinner      PinDispatcher
	Log         *slog.Logger
	MaxBody     int64
	PokeTimeout time.Duration

	// Org executes org plans; it is set only on an instance in org mode (NewOrgServer).
	// OrgParallel bounds how many plans run at once.
	Org         *OrgRunner
	OrgParallel int

	// secret is unexported so no %+v of a Server, and no caller, can reach the HMAC key.
	secret string

	// ctx outlives every request: a poke starts in a handler but must not die with
	// the client's connection. Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	pokes  sync.WaitGroup // pokes and org plans: both are background work Close drains

	mu     sync.Mutex // guards closed against pokes.Add racing Close's Wait, and sched
	closed bool
	sched  *orgScheduler
}

// NewServer wires the production defaults. It re-checks the secret so a Server can
// never exist whose HMAC key is empty or guessable, whoever built the Config. A table
// with an org section is refused: it needs NewOrgServer's credentials, and serving it
// in bartixcloud mode would answer an org's events with the wrong behaviour.
func NewServer(cfg Config, table Table, log *slog.Logger) (*Server, error) {
	if table.Org != nil {
		return nil, errors.New("the table has an org section: build the server with NewOrgServer")
	}
	return newServer(cfg, table, log)
}

func newServer(cfg Config, table Table, log *slog.Logger) (*Server, error) {
	if err := validateSecret(cfg.Secret); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	// LoadConfig always fills this; a Config built by hand (tests) still gets a
	// usable window instead of a zero one that expires before any converge can run.
	pokeTimeout := cfg.PokeTimeout
	if pokeTimeout <= 0 {
		pokeTimeout = defaultPokeTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		secret:      cfg.Secret,
		Table:       table,
		Forwarder:   Forwarder{SocketDir: cfg.SocketDir, Timeout: defaultForwardTimeout},
		Runner:      ExecRunner{},
		Poster:      GitHubPoster{AppEnvFile: appauth.APPEnv, KeyPath: appauth.APPKey, APIBase: appauth.GHAPI},
		Pinner:      GitHubPin{AppEnvFile: appauth.APPEnv, KeyPath: appauth.APPKey, APIBase: appauth.GHAPI},
		Log:         log,
		MaxBody:     defaultMaxBody,
		PokeTimeout: pokeTimeout,
		OrgParallel: defaultOrgParallel,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// Wait blocks until every poke started so far has finished. Call it only after the
// HTTP server has stopped accepting requests (http.Server.Shutdown), so no handler
// can start a new poke concurrently.
func (s *Server) Wait() { s.pokes.Wait() }

// Close refuses further pokes, cancels in-flight ones and waits for them. Unlike
// Wait it is safe while handlers are still running: a poke they try to start after
// Close is answered 503 so GitHub records a redeliverable failure.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.pokes.Wait()
}

// NewHTTPServer wraps h with the timeouts a root daemon on a bridge address needs:
// no client may hold a connection or a goroutine open indefinitely.
func NewHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
}

// ServeHTTP routes by exact method and path. It is hand-written, not a ServeMux:
// anything but GET /healthz and POST /github is a JSON 404, never a 405.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == healthPath:
		s.reply(w, http.StatusOK, "ok")
	case r.Method == http.MethodPost && r.URL.Path == webhookPath:
		s.handleWebhook(w, r)
	default:
		s.reply(w, http.StatusNotFound, "not found")
	}
}

// delivery is one webhook that passed the HMAC and parsed as a JSON object.
type delivery struct {
	id      string
	event   string
	body    []byte
	headers ForwardHeaders
}

// handleWebhook checks in a fixed order, cheapest and least trusting first: size
// (413), HMAC over the raw bytes (401), JSON object (400), then routing. Nothing
// from the body is parsed before the signature verifies.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > s.MaxBody {
		s.reply(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	// ContentLength is -1 for chunked bodies, so the reader enforces the cap too.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.reply(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		s.Log.Warn("reading delivery body failed", "err", err)
		s.reply(w, http.StatusBadRequest, "bad request body")
		return
	}

	// Attacker-controlled until the HMAC verifies: clipped here, escaped by slog.
	id := clip(r.Header.Get(hdrDelivery), logFieldLimit)
	if !VerifySignature(s.secret, body, r.Header.Get(hdrSignature)) {
		s.Log.Warn("rejected delivery: bad signature", "delivery", id)
		s.reply(w, http.StatusUnauthorized, "bad signature")
		return
	}
	payload, err := ParsePayload(body)
	if err != nil {
		s.Log.Warn("rejected delivery: bad json", "delivery", id, "err", err)
		s.reply(w, http.StatusBadRequest, "bad json")
		return
	}

	d := delivery{
		id:    id,
		event: clip(r.Header.Get(hdrEvent), logFieldLimit),
		body:  body,
		headers: ForwardHeaders{
			Event:       r.Header.Get(hdrEvent),
			Delivery:    r.Header.Get(hdrDelivery),
			TargetID:    r.Header.Get(hdrTargetID),
			ContentType: r.Header.Get("Content-Type"),
		},
	}
	event := r.Header.Get(hdrEvent)
	if s.Table.Org != nil {
		s.respond(r.Context(), w, d, RouteOrg(event, body, payload, s.Table))
		return
	}
	s.respond(r.Context(), w, d, Route(event, payload, s.Table))
}

// respond carries out a Decision. The switch is exhaustive over Action; the default
// is a 500, so an Action nobody handled (including the zero value) fails closed.
func (s *Server) respond(ctx context.Context, w http.ResponseWriter, d delivery, dec Decision) {
	switch dec.Action {
	case ActionPong:
		s.reply(w, http.StatusOK, "pong")
	case ActionReject:
		s.Log.Warn("REJECT delivery", "delivery", d.id, "event", d.event, "reason", dec.Reason)
		s.reply(w, http.StatusForbidden, "forbidden")
	case ActionIgnore:
		s.Log.Info("ignored delivery", "delivery", d.id, "event", d.event, "reason", dec.Reason)
		s.reply(w, http.StatusAccepted, "ignored ("+dec.Reason+")")
	case ActionPoke:
		if !s.startPoke(dec.Targets, dec.Report) {
			s.Log.Warn("poke refused: dispatcher is shutting down", "delivery", d.id)
			s.reply(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		s.Log.Info("poking", "delivery", d.id, "targets", targetNames(dec.Targets))
		s.reply(w, http.StatusAccepted, fmt.Sprintf("poking %d vm(s)", len(dec.Targets)))
	case ActionPin:
		job := dec.PinJob
		if !s.startPin(job) {
			s.Log.Warn("pin refused: dispatcher is shutting down", "delivery", d.id)
			s.reply(w, http.StatusServiceUnavailable, "shutting down")
			return
		}
		s.Log.Info("pinning", "delivery", d.id, "repo", job.SourceRepo, "tag", job.Tag)
		s.reply(w, http.StatusAccepted, "pinning")
	case ActionForward:
		s.forward(ctx, w, d, dec.Tenant)
	case ActionOrg:
		s.org(w, d, dec.Org)
	default:
		s.Log.Error("unhandled action", "action", dec.Action.String(), "delivery", d.id, "event", d.event)
		s.reply(w, http.StatusInternalServerError, "internal error")
	}
}

// startPoke runs the pokes in the background; it reports false once Close has run.
func (s *Server) startPoke(targets []PokeTarget, report *Report) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.pokes.Add(1)
	go func() {
		defer s.pokes.Done()
		Poke(s.ctx, s.Runner, s.Log, s.PokeTimeout, targets, report, s.Poster)
	}()
	return true
}

// startPin runs the pin dispatch in the background; it reports false once Close has run.
func (s *Server) startPin(job PinJob) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.pokes.Add(1)
	go func() {
		defer s.pokes.Done()
		if s.Pinner == nil {
			s.Log.Error("pin dispatch unavailable")
			return
		}
		if err := s.Pinner.Pin(s.ctx, job); err != nil {
			s.Log.Error("pin dispatch failed", "err", clip(err.Error(), logFieldLimit))
		} else {
			s.Log.Info("pinned", "source", job.SourceRepo, "tag", job.Tag)
		}
	}()
	return true
}

// org queues the plan for background execution and answers at once: GitHub's
// delivery timeout is short, a backup or snapshot is not. Without a runner (an
// instance not built by NewOrgServer) it fails closed.
func (s *Server) org(w http.ResponseWriter, d delivery, plan OrgPlan) {
	if s.Org == nil {
		s.Log.Error("org plan without an org runner", "delivery", d.id, "event", d.event)
		s.reply(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !s.startOrg(plan) {
		s.Log.Warn("org plan refused: dispatcher is shutting down", "delivery", d.id)
		s.reply(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	s.Log.Info("org plan queued", "delivery", d.id, "event", d.event, "repo", plan.Repo, "jobs", jobNames(plan.Jobs))
	s.reply(w, http.StatusAccepted, fmt.Sprintf("org: %d job(s)", len(plan.Jobs)))
}

// startOrg queues the plan behind earlier plans of the same repository and runs it
// when a parallelism slot is free. It reports false once Close has run. The queue
// position is taken here, under the lock, so plans run in delivery order.
func (s *Server) startOrg(plan OrgPlan) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.sched == nil {
		s.sched = newOrgScheduler(s.OrgParallel)
	}
	run := s.sched.enqueue(orgKey(plan))
	s.pokes.Add(1)
	go func() {
		defer s.pokes.Done()
		if !run(s.ctx, func() { s.Org.Run(s.ctx, plan) }) {
			s.Org.Abandon(plan)
		}
	}()
	return true
}

// forward relays the verbatim body to the tenant broker and the broker's answer
// back. Failure to reach it is a 503 so GitHub records a redeliverable failure.
func (s *Server) forward(ctx context.Context, w http.ResponseWriter, d delivery, tenant string) {
	resp, err := s.Forwarder.Forward(ctx, tenant, d.body, d.headers)
	if err != nil {
		s.Log.Error(fmt.Sprintf("forward to %s failed", tenant), "delivery", d.id, "err", err)
		s.reply(w, http.StatusServiceUnavailable, "broker "+tenant+" unavailable")
		return
	}
	s.write(w, resp.Status, resp.Body)
}

// reply writes the compact {"status": msg} JSON every dispatcher answer uses.
func (s *Server) reply(w http.ResponseWriter, code int, msg string) {
	body, err := json.Marshal(struct {
		Status string `json:"status"`
	}{Status: msg})
	if err != nil {
		// Unreachable for a string field; fail closed with a fixed body regardless.
		s.Log.Error("encode reply failed", "err", err)
		code, body = http.StatusInternalServerError, []byte(`{"status":"internal error"}`)
	}
	s.write(w, code, body)
}

func (s *Server) write(w http.ResponseWriter, code int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if _, err := w.Write(body); err != nil {
		s.Log.Debug("write response failed", "err", err)
	}
}

func jobNames(jobs []OrgJob) string {
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.String()
	}
	return strings.Join(names, ",")
}

func targetNames(targets []PokeTarget) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Project + "/" + t.VM
	}
	return strings.Join(names, ", ")
}
