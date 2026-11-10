package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bartixcloud/runner-broker/dispatcher"
)

const testSecret = "s3cret-s3cret-s3cret-s3cret"

// tableJSON: installation 1 is the platform (no broker, pokes o/infra), 2 is tenant t2.
const tableJSON = `{"installations":{"1":{"owner":"o","tenant":null},"2":{"owner":"o2","tenant":"t2"}},` +
	`"pokes":{"o/infra":[["p","vm"]]}}`

// lockedBuffer is an io.Writer shared between run's goroutine and the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envOf(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

// shortTmp: AF_UNIX paths are capped near 104 bytes on macOS, so never t.TempDir().
func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bcxm")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func writeTable(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatcher.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write table: %v", err)
	}
	return path
}

// goodEnv is a complete, valid environment listening on an ephemeral loopback port.
func goodEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"BCX_WEBHOOK_SECRET":    testSecret,
		"BCX_DISPATCHER_HOST":   "127.0.0.1",
		"BCX_DISPATCHER_PORT":   "0",
		"BCX_DISPATCHER_MAP":    writeTable(t, tableJSON),
		"BCX_BROKER_SOCKET_DIR": shortTmp(t),
	}
}

// signature is an independent oracle for X-Hub-Signature-256.
func signature(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	if _, err := mac.Write(body); err != nil {
		panic(err) // hash.Hash.Write is documented never to fail
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postSigned(t *testing.T, addr, event, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/github", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", signature([]byte(body)))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /github: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response: %v", err)
	}
	return resp.StatusCode
}

func get(t *testing.T, addr, path string) int {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

var addrLine = regexp.MustCompile(`dispatcher listening.* addr=(\S+)`)

// startRun runs run() in a goroutine and returns the address it logged plus the
// channel that delivers its exit code.
func startRun(t *testing.T, env map[string]string) (addr string, stderr *lockedBuffer, done <-chan int) {
	t.Helper()
	stderr = &lockedBuffer{}
	codes := make(chan int, 1)
	go func() { codes <- run(nil, envOf(env), stderr) }()
	deadline := time.After(5 * time.Second)
	for {
		if m := addrLine.FindStringSubmatch(stderr.String()); m != nil {
			return m[1], stderr, codes
		}
		select {
		case code := <-codes:
			t.Fatalf("run exited with %d before listening:\n%s", code, stderr.String())
		case <-deadline:
			t.Fatalf("run never logged its address:\n%s", stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func awaitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit")
		return -1
	}
}

// ------------------------------------------------------------------ run ----

func TestRunRefusesAShortSecret(t *testing.T) {
	env := goodEnv(t)
	env["BCX_WEBHOOK_SECRET"] = "zq-7731"
	var stderr lockedBuffer
	if code := run(nil, envOf(env), &stderr); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "BCX_WEBHOOK_SECRET") {
		t.Errorf("stderr must name BCX_WEBHOOK_SECRET:\n%s", out)
	}
	if strings.Contains(out, "zq-7731") {
		t.Errorf("stderr must not echo the secret:\n%s", out)
	}
}

func TestRunRefusesAMissingSecret(t *testing.T) {
	env := goodEnv(t)
	delete(env, "BCX_WEBHOOK_SECRET")
	var stderr lockedBuffer
	if code := run(nil, envOf(env), &stderr); code != 1 || !strings.Contains(stderr.String(), "BCX_WEBHOOK_SECRET") {
		t.Errorf("exit = %d, stderr:\n%s", code, stderr.String())
	}
}

func TestRunRefusesArguments(t *testing.T) {
	var stderr lockedBuffer
	if code := run([]string{"--listen", ":80"}, envOf(goodEnv(t)), &stderr); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Errorf("stderr must explain usage:\n%s", stderr.String())
	}
}

func TestRunRefusesABadPort(t *testing.T) {
	env := goodEnv(t)
	env["BCX_DISPATCHER_PORT"] = "http"
	var stderr lockedBuffer
	if code := run(nil, envOf(env), &stderr); code != 1 || !strings.Contains(stderr.String(), "BCX_DISPATCHER_PORT") {
		t.Errorf("exit = %d, stderr:\n%s", code, stderr.String())
	}
}

func TestRunFailsClosedOnAMissingOrBadTable(t *testing.T) {
	cases := map[string]string{
		"missing file": filepath.Join(t.TempDir(), "absent.json"),
		"bad table":    writeTable(t, `{"installations":{}}`),
		"not json":     writeTable(t, `nonsense`),
	}
	for name, path := range cases {
		env := goodEnv(t)
		env["BCX_DISPATCHER_MAP"] = path
		var stderr lockedBuffer
		if code := run(nil, envOf(env), &stderr); code != 1 {
			t.Errorf("%s: exit = %d, want 1", name, code)
		}
		if !strings.Contains(stderr.String(), path) {
			t.Errorf("%s: stderr must name %s:\n%s", name, path, stderr.String())
		}
		if strings.Contains(stderr.String(), testSecret) {
			t.Errorf("%s: secret leaked:\n%s", name, stderr.String())
		}
	}
}

func TestRunFailsWhenThePortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()
	_, port, err := net.SplitHostPort(taken.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	env := goodEnv(t)
	env["BCX_DISPATCHER_PORT"] = port
	var stderr lockedBuffer
	if code := run(nil, envOf(env), &stderr); code != 1 || !strings.Contains(stderr.String(), "listen") {
		t.Errorf("exit = %d, stderr:\n%s", code, stderr.String())
	}
}

// The two signal tests send a real signal to this process: run's signal.NotifyContext
// is what keeps it alive, so they must not be parallel with each other.
func TestRunServesThenExitsZeroOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			addr, stderr, done := startRun(t, goodEnv(t))

			if code := get(t, addr, "/healthz"); code != 200 {
				t.Errorf("healthz = %d, want 200", code)
			}
			if code := postSigned(t, addr, "ping", `{"zen":"x"}`); code != 200 {
				t.Errorf("signed ping = %d, want 200: the configured secret must reach the handler", code)
			}
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatalf("kill: %v", err)
			}
			if code := awaitExit(t, done); code != 0 {
				t.Errorf("exit = %d, want 0 after a graceful shutdown\n%s", code, stderr.String())
			}
			out := stderr.String()
			if !strings.Contains(out, "shutting down") {
				t.Errorf("shutdown must be logged:\n%s", out)
			}
			if strings.Contains(out, testSecret) {
				t.Errorf("the secret leaked into the log:\n%s", out)
			}
		})
	}
}

func TestRunDrainsAnInFlightPokeBeforeExiting(t *testing.T) {
	bin := shortTmp(t)
	marker := filepath.Join(bin, "poked")
	stub := fmt.Sprintf("#!/bin/sh\nsleep 0.3\ntouch %q\n", marker)
	if err := os.WriteFile(filepath.Join(bin, "incus"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")

	addr, stderr, done := startRun(t, goodEnv(t))
	if code := postSigned(t, addr, "push",
		`{"installation":{"id":1},"repository":{"full_name":"o/infra"},"ref":"refs/tags/v1"}`); code != 202 {
		t.Fatalf("push = %d, want 202", code)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if code := awaitExit(t, done); code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, stderr.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("run exited before the in-flight poke finished: %v\n%s", err, stderr.String())
	}
}

// ---------------------------------------------------------------- serve ----

func testLogger() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func serveTable() dispatcher.Table {
	tenant := "t2"
	return dispatcher.Table{
		Installations: map[string]dispatcher.Installation{
			"1": {Owner: "o", Tenant: nil},
			"2": {Owner: "o2", Tenant: &tenant},
		},
		Pokes: map[string][]dispatcher.PokeTarget{"o/infra": {{Project: "p", VM: "vm"}}},
	}
}

// pokeRunner is a dispatcher.Runner whose command finishes after hold, or when its
// context ends, and records which of the two happened.
type pokeRunner struct {
	hold     time.Duration
	started  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	ctxError error
}

func newPokeRunner(hold time.Duration) *pokeRunner {
	return &pokeRunner{hold: hold, started: make(chan struct{})}
}

func (p *pokeRunner) Run(ctx context.Context, _ []string) (string, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-time.After(p.hold):
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctxError = ctx.Err()
	return "", ctx.Err()
}

// Output behaves like Run: reads of the converge status count as poke work.
func (p *pokeRunner) Output(ctx context.Context, argv []string) (string, string, error) {
	_, err := p.Run(ctx, argv)
	return "", "", err
}

func (p *pokeRunner) cancelled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctxError != nil
}

type serveRig struct {
	addr   string
	cancel context.CancelFunc
	done   <-chan int
	logs   *lockedBuffer
	dir    string
}

func startServe(t *testing.T, runner dispatcher.Runner, limits shutdownLimits) *serveRig {
	t.Helper()
	log, logs := testLogger()
	dir := shortTmp(t)
	srv, err := dispatcher.NewServer(dispatcher.Config{Secret: testSecret, SocketDir: dir}, serveTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.Runner = runner
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() { done <- serve(ctx, ln, srv, log, limits) }()
	return &serveRig{addr: ln.Addr().String(), cancel: cancel, done: done, logs: logs, dir: dir}
}

const pokeBody = `{"installation":{"id":1},"repository":{"full_name":"o/infra"},"ref":"refs/tags/v1"}`

func TestServeLetsAnInFlightPokeFinishWithinTheDrainWindow(t *testing.T) {
	runner := newPokeRunner(150 * time.Millisecond)
	rig := startServe(t, runner, shutdownLimits{http: time.Second, drain: 5 * time.Second})
	if code := postSigned(t, rig.addr, "push", pokeBody); code != 202 {
		t.Fatalf("push = %d, want 202", code)
	}
	<-runner.started
	rig.cancel()
	if code := awaitExit(t, rig.done); code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, rig.logs.String())
	}
	if runner.cancelled() {
		t.Error("a poke that finishes inside the drain window must not be cancelled")
	}
}

func TestServeCancelsAPokeThatOutlivesTheDrainWindow(t *testing.T) {
	runner := newPokeRunner(time.Hour)
	rig := startServe(t, runner, shutdownLimits{http: time.Second, drain: 150 * time.Millisecond})
	if code := postSigned(t, rig.addr, "push", pokeBody); code != 202 {
		t.Fatalf("push = %d, want 202", code)
	}
	<-runner.started
	start := time.Now()
	rig.cancel()
	if code := awaitExit(t, rig.done); code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, rig.logs.String())
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("serve returned after %v: it did not wait for the drain window", elapsed)
	}
	if !runner.cancelled() {
		t.Error("a poke that outlives the drain window must be cancelled")
	}
	if !strings.Contains(rig.logs.String(), "drain") {
		t.Errorf("the forced cancel must be logged:\n%s", rig.logs.String())
	}
}

func TestServeExitsOneWhenARequestOutlivesTheHTTPShutdownWindow(t *testing.T) {
	// A broker that never answers pins a handler; Shutdown must give up, not hang.
	log, logs := testLogger()
	dir := shortTmp(t)
	ln, err := net.Listen("unix", filepath.Join(dir, "t2.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	broker := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(reached) })
		<-release
	}))
	broker.Listener = ln
	broker.Start()
	// Cleanups run last-in first-out: release the handler before Close waits for it.
	t.Cleanup(broker.Close)
	t.Cleanup(func() { close(release) })

	srv, err := dispatcher.NewServer(dispatcher.Config{Secret: testSecret, SocketDir: dir}, serveTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- serve(ctx, front, srv, log, shutdownLimits{http: 150 * time.Millisecond, drain: time.Second})
	}()

	go postSignedQuiet(front.Addr().String(), "workflow_job", `{"installation":{"id":2},"repository":{"full_name":"o2/app"}}`)
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery never reached the broker")
	}
	cancel()
	if code := awaitExit(t, done); code != 1 {
		t.Errorf("exit = %d, want 1 when connections had to be cut\n%s", code, logs.String())
	}
	if !strings.Contains(logs.String(), "shutdown") {
		t.Errorf("the forced close must be logged:\n%s", logs.String())
	}
}

// postSignedQuiet is postSigned for goroutines: no t.Fatal, and a cut connection is expected.
func postSignedQuiet(addr, event, body string) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/github", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signature([]byte(body)))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func TestServeExitsOneWhenTheListenerFails(t *testing.T) {
	log, logs := testLogger()
	srv, err := dispatcher.NewServer(dispatcher.Config{Secret: testSecret, SocketDir: shortTmp(t)}, serveTable(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		done <- serve(context.Background(), ln, srv, log, shutdownLimits{http: time.Second, drain: time.Second})
	}()
	if code := awaitExit(t, done); code != 1 {
		t.Errorf("exit = %d, want 1\n%s", code, logs.String())
	}
}
