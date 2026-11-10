package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

// tokenSource is the interface both the appauth token and test fakes satisfy.
type tokenSource interface {
	Get() (string, error)
}

// Seams over appauth so the startup proof can be tested without GitHub.
var (
	appIDFunc             = func() (string, error) { return appauth.AppID(appauth.APPEnv) }
	newInstallationTokens = func(appID string, instID int) tokenSource {
		return appauth.NewInstallationToken(appID, instID, appauth.APPKey)
	}
)

// startReconciler starts the broker's reconcile loop. A seam so a test that
// drives run() end to end keeps background passes out of its API stubs.
var startReconciler = func(ctx context.Context, s *server, tokens tokenSource) {
	go s.reconcileLoop(ctx, tokens)
}

var (
	// drainTimeout bounds how long SIGTERM waits for in-flight requests and
	// spawns; it stays under systemd's default TimeoutStopSec (90s).
	drainTimeout = 30 * time.Second
	// maxBodyBytes caps a webhook body (GitHub's own cap is 25 MiB).
	maxBodyBytes int64 = 26 << 20
)

// server holds the per-broker state the handler reads.
type server struct {
	tokens  tokenSource
	tiers   map[string]tier
	secrets map[string]string
	wg      sync.WaitGroup // spawn and cancel goroutines in flight
}

// track runs f on a goroutine that shutdown waits for, so a SIGTERM between
// `incus launch` and injection does not abandon a half-built VM.
func (s *server) track(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// handler serves the webhook on a unix socket. There is no HMAC check here:
// bcx-dispatcher verified it, and the 0600 socket in a 0700 dir is the trust
// boundary.
type handler struct {
	srv *server
}

// reply is the JSON body of every response.
type reply struct {
	Status string `json:"status"`
}

func writeReply(w http.ResponseWriter, code int, msg string) {
	b, err := json.Marshal(reply{Status: msg})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// workflowJobEvent is the part of a workflow_job webhook the broker reads.
type workflowJobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int      `json:"id"`
		RunID      int      `json:"run_id"`
		Labels     []string `json:"labels"`
		RunnerName string   `json:"runner_name"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// completedRunnerRe is the name spawn gives a runner: ci-<tier>-<jobID>-<hex6>.
// Only a name of this shape may steer a delete.
var completedRunnerRe = regexp.MustCompile(`^ci-([a-z0-9-]+)-\d+-[0-9a-f]{6}$`)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// RequestURI, not URL.Path: the dispatcher always sends exactly /github, so
	// a query string or an escaped spelling is not our path (Python matched the
	// raw self.path too).
	switch {
	case r.Method == http.MethodGet && r.RequestURI == "/healthz":
		writeReply(w, http.StatusOK, "ok")
		return
	case r.Method != http.MethodPost || r.RequestURI != webhookPath:
		writeReply(w, http.StatusNotFound, "not found")
		return
	}
	if r.Header.Get("X-GitHub-Event") != "workflow_job" {
		writeReply(w, http.StatusAccepted, "ignored (not workflow_job)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeReply(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if err != nil {
		writeReply(w, http.StatusBadRequest, "bad json")
		return
	}
	var ev workflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeReply(w, http.StatusBadRequest, "bad json")
		return
	}
	if ev.Action == "completed" {
		h.completed(w, ev)
		return
	}
	if ev.Action != "queued" {
		writeReply(w, http.StatusAccepted, "ignored (not queued)")
		return
	}

	jobID, runID, labels, repoFullName := ev.WorkflowJob.ID, ev.WorkflowJob.RunID, ev.WorkflowJob.Labels, ev.Repository.FullName
	dec := decideJobAction(labels, repoFullName, h.srv.tiers, "")
	switch dec.action {
	case actionIgnore:
		// Quiet on purpose (Python: DEBUG): one line per GitHub-hosted job in the
		// org would fill the journal.
		writeReply(w, http.StatusAccepted, "ignored (no tier label)")
	case actionSpawn:
		h.srv.track(func() { h.srv.safeSpawn(jobID, dec.tier, repoFullName) })
		writeReply(w, http.StatusAccepted, "spawning")
	case actionDeny:
		h.deny(w, dec, jobID, runID, labels, repoFullName)
	default:
		logf("ERROR", "job %d: unknown decision %d", jobID, dec.action)
		writeReply(w, http.StatusInternalServerError, "internal error")
	}
}

// completed handles the one lifecycle signal that can delete a VM. Only an
// hcloud runner of this tenant may be deleted; every other completed job keeps
// the old ignored reply. The delete runs on a tracked goroutine so SIGTERM
// drains it, just like spawn.
func (h *handler) completed(w http.ResponseWriter, ev workflowJobEvent) {
	name := ev.WorkflowJob.RunnerName
	m := completedRunnerRe.FindStringSubmatch(name)
	if m == nil {
		writeReply(w, http.StatusAccepted, "ignored (not queued)")
		return
	}
	t := m[1]
	cfg, known := h.srv.tiers[t]
	if !known || cfg.Backend != backendHcloud || !tierIsUsable(t, h.srv.tiers, "") {
		writeReply(w, http.StatusAccepted, "ignored (not queued)")
		return
	}
	// The tenant selector already prevents a cross-tenant delete; this check
	// keeps a misrouted delivery from even asking.
	if ownerOf(ev.Repository.FullName) != resolveOwner("") {
		writeReply(w, http.StatusAccepted, "ignored (not queued)")
		return
	}
	h.srv.track(func() { h.srv.safeDeleteRunner(t, name) })
	writeReply(w, http.StatusAccepted, "deleting")
}

// safeDeleteRunner is the background half of a completed job. Delete is
// idempotent: an unknown name is nil, so a duplicate delivery or an already
// reaped VM is harmless.
func (s *server) safeDeleteRunner(t, name string) {
	defer func() {
		if r := recover(); r != nil {
			logf("ERROR", "completed delete failed for %s: %q", name, fmt.Sprint(r))
		}
	}()
	cfg, known := s.tiers[t]
	if !known || cfg.Backend != backendHcloud {
		return
	}
	if err := backendOf(cfg.Backend).Delete(name); err != nil {
		logf("ERROR", "completed delete of %s failed: %v", name, err)
	}
}

// deny logs and answers a denied job. Labels and repository names are
// attacker-influenced (any collaborator picks runs-on), so they are quoted: an
// embedded newline must not forge a log line.
func (h *handler) deny(w http.ResponseWriter, dec decideResult, jobID, runID int, labels []string, repoFullName string) {
	switch dec.reason {
	case reasonLabelVocabulary:
		logf("WARNING", "DENY job %d repo=%q labels=%q: not exactly one known tier", jobID, repoFullName, labels)
		h.srv.track(func() { h.srv.safeCancel(runID, repoFullName) })
		writeReply(w, http.StatusAccepted, "denied (label vocabulary)")
	case reasonWrongOwner:
		// No cancel: this run belongs to another owner. A wrong-owner delivery
		// means a misrouted webhook, not a bad job.
		logf("WARNING", "DENY job %d repo=%q tier=%s: owner is not tenant %s", jobID, repoFullName, dec.tier, tenant)
		writeReply(w, http.StatusAccepted, "denied (wrong owner)")
	case reasonNotRegistered:
		// The user installation covers only its registered repos; its token
		// cannot reach any other, so there is nothing to cancel.
		logf("WARNING", "DENY job %d repo=%q tier=%s: not a registered repo of %s", jobID, repoFullName, dec.tier, target)
		writeReply(w, http.StatusAccepted, "denied (not registered)")
	case reasonNotEntitled:
		logf("WARNING", "DENY job %d repo=%q tier=%s: not entitled", jobID, repoFullName, dec.tier)
		h.srv.track(func() { h.srv.safeCancel(runID, repoFullName) })
		writeReply(w, http.StatusAccepted, "denied (not entitled)")
	case reasonNone:
		fallthrough
	default: // a deny without a reason is a bug; never act on it
		logf("ERROR", "DENY job %d repo=%q: unknown reason %q", jobID, repoFullName, string(dec.reason))
		writeReply(w, http.StatusInternalServerError, "internal error")
	}
}

// safeSpawn is the background half of an accepted job, shared by the webhook
// handler and the reconciler. It owns the job's claim: whoever gets here first
// launches, the other one finds the job already claimed and stops, so one job
// never gets two VMs. A broker goroutine must never die silently or take the
// process with it.
func (s *server) safeSpawn(jobID int, t, repoFullName string) {
	defer func() {
		if r := recover(); r != nil {
			logf("ERROR", "spawn failed for job %d: %q", jobID, fmt.Sprint(r))
		}
	}()
	if !claimJob(jobID) {
		logf("INFO", "job %d is already being launched here — not launching a second VM", jobID)
		return
	}
	defer unclaimJob(jobID)
	token, err := s.tokens.Get()
	if err != nil {
		logf("ERROR", "github api error spawning for job %d: %v", jobID, err)
		return
	}
	if _, err := spawn(token, jobID, t, s.tiers, s.secrets, repoFullName); err != nil {
		var he *httpError
		if errors.As(err, &he) {
			logf("ERROR", "github api error spawning for job %d: %v", jobID, err)
			return
		}
		logf("ERROR", "spawn failed for job %d: %v", jobID, err)
	}
}

func (s *server) safeCancel(runID int, repoFullName string) {
	defer func() {
		if r := recover(); r != nil {
			logf("ERROR", "cancel failed for run %d: %q", runID, fmt.Sprint(r))
		}
	}()
	token, err := s.tokens.Get()
	if err != nil {
		logf("ERROR", "cancel failed for run %d: %v", runID, err)
		return
	}
	cancelJob(token, repoFullName, runID)
}

// bindSocket binds path as 0600, creating its directory 0700 if it is missing
// (an existing directory is never re-moded: it is shared by every tenant's
// broker). Only this broker's own stale socket may be replaced.
func bindSocket(path string, h http.Handler) (*http.Server, net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, nil, err
	}
	// The mode is set by the umask at bind(): there is no window with a wider
	// mode and no chmod. umask is process-wide, so restore it straight away.
	old := umask(0o177)
	listener, err := net.Listen("unix", path)
	umask(old)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler: h,
		// The only peer is the dispatcher; a stalled connection must not hold a
		// goroutine forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return srv, listener, nil
}

// clearStaleSocket removes what a crashed run left at path, and nothing else:
// a stray regular file (as Python did) or a socket nobody listens on. A live
// socket is another broker's, and a directory (os.Remove would delete an empty
// one, possibly the shared /run/bcx-broker) is never ours to replace.
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch m := fi.Mode(); {
	case m.IsRegular():
	case m&os.ModeSocket != 0:
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = c.Close()
			return fmt.Errorf("%s is already served by a live process", path)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("cannot tell whether %s is stale: %w", path, err)
		}
	default:
		return fmt.Errorf("%s exists and is neither a socket nor a regular file (%s)", path, m.Type())
	}
	return os.Remove(path)
}

// makeTokens is this tenant's installation-token source, proven once at startup
// so a wrong App id, key or installation stops the unit loudly instead of
// failing every spawn later.
func makeTokens() (tokenSource, error) {
	fail := func(err error) (tokenSource, error) {
		return nil, fmt.Errorf("cannot mint an installation token for installation %d: %w", installationID, err)
	}
	appID, err := appIDFunc()
	if err != nil {
		return fail(err)
	}
	tokens := newInstallationTokens(appID, installationID)
	if _, err := tokens.Get(); err != nil {
		return fail(err)
	}
	return tokens, nil
}

// run starts the broker and serves until ctx is cancelled (SIGTERM), then
// drains: it stops accepting, lets in-flight requests and spawn/cancel work
// finish within drainTimeout, and returns nil on a clean shutdown. Closing the
// listener unlinks this broker's own socket; the shared directory is untouched.
func run(ctx context.Context, lookup func(string) (string, bool)) error {
	cfg, err := loadConfig(lookup)
	if err != nil {
		return err
	}
	cfg.apply()
	tiers, err := loadTiers(tiersFile)
	if err != nil {
		return err
	}
	secrets, err := loadSecrets()
	if err != nil {
		return err
	}
	if err := checkTierSecrets(tiers, secrets); err != nil {
		return err
	}
	hcloudClient, err := loadHcloudClient(cfg, tiers)
	if err != nil {
		return err
	}
	if hcloudClient != nil {
		hcloudAPI = hcloudClient
	} else {
		hcloudAPI = nil
	}
	tokens, err := makeTokens()
	if err != nil {
		return err
	}
	orphans, err := runningRunnersAll(tiers)
	if err != nil {
		return err
	}
	existing := "none"
	if len(orphans) > 0 {
		existing = fmt.Sprintf("%q", orphans)
	}
	logf("INFO", "reconcile-on-start: %d existing ci-* instance(s): %s", len(orphans), existing)

	srv := &server{tokens: tokens, tiers: tiers, secrets: secrets}
	httpd, listener, err := bindSocket(socketPath, &handler{srv: srv})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(tiers))
	for name := range tiers {
		names = append(names, name)
	}
	slices.Sort(names)
	logf("INFO", "broker up on %s — tenant=%s %s=%s project=%s tiers=%v", socketPath, tenant, targetKind, target, incusProject, names)

	// The webhook is the fast path; this pass is the one that catches a job
	// whose delivery never reached a working broker (GitHub does not redeliver).
	startReconciler(ctx, srv, tokens)

	served := make(chan error, 1)
	go func() { served <- httpd.Serve(listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logf("INFO", "shutting down: draining in-flight work (up to %s)", drainTimeout)
	drain, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	shutdownErr := httpd.Shutdown(drain)
	if shutdownErr != nil {
		_ = httpd.Close() // force the stragglers; also unlinks the socket
	}
	<-served
	// Shutdown has returned, so no handler can add work any more.
	idle := make(chan struct{})
	go func() { srv.wg.Wait(); close(idle) }()
	select {
	case <-idle:
	case <-drain.Done():
		return fmt.Errorf("shutdown: spawn or cancel work still running after %s", drainTimeout)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	return nil
}

// realMain is main's body, returning the exit code: 0 after a clean SIGTERM or
// SIGINT shutdown, 1 on any startup or serving error.
func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.LookupEnv); err != nil {
		logf("ERROR", "%v", err)
		return 1
	}
	return 0
}

func main() {
	log.SetFlags(log.LstdFlags)
	os.Exit(realMain())
}
