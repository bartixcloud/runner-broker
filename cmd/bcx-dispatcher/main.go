// Command bcx-dispatcher is the single webhook endpoint of the bartixcloud-runners
// GitHub App: it verifies the one HMAC, maps installation.id to a tenant broker,
// and pokes nuk-converge on a v* tag push to an infra repo. Configuration is the
// environment only (see dispatcher.LoadConfig); there are no flags. A map with an
// "org" section turns the process into the org instance (backup, detection, mirror).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bartixcloud/runner-broker/dispatcher"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2

	usage = "usage: bcx-dispatcher (no arguments; configured by BCX_WEBHOOK_SECRET and the optional\n" +
		"BCX_DISPATCHER_HOST, BCX_DISPATCHER_PORT, BCX_DISPATCHER_MAP, BCX_BROKER_SOCKET_DIR,\n" +
		"BCX_DISPATCHER_POKE_TIMEOUT; org mode, switched on by an \"org\" section in the map, also reads\n" +
		"AKD_SPIEGEL_APP_ENV, AKD_SPIEGEL_APP_KEY)"
)

// shutdownLimits bounds the two phases of a graceful stop: finishing in-flight HTTP
// requests, then letting in-flight pokes complete before cancelling them. Together
// they stay below systemd's default 90 s stop timeout.
type shutdownLimits struct {
	http  time.Duration
	drain time.Duration
}

var defaultLimits = shutdownLimits{http: 10 * time.Second, drain: 25 * time.Second}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

// run is main with its inputs injected so tests can drive it. It never logs the
// config or the secret: only the listen address and table sizes.
func run(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) != 0 {
		complain(stderr, usage)
		return exitUsage
	}
	cfg, err := dispatcher.LoadConfig(getenv)
	if err != nil {
		complain(stderr, "bcx-dispatcher: %v", err)
		return exitFailed
	}
	table, err := dispatcher.LoadTable(cfg.MapFile)
	if err != nil {
		complain(stderr, "bcx-dispatcher: cannot load the installation table: %v", err)
		return exitFailed
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	srv, err := newServer(cfg, table, getenv, log)
	if err != nil {
		complain(stderr, "bcx-dispatcher: %v", err)
		return exitFailed
	}

	// Registered before listening so a signal during startup is not lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		complain(stderr, "bcx-dispatcher: listen: %v", err)
		return exitFailed
	}
	log.Info("dispatcher listening", "addr", ln.Addr().String(),
		"installations", len(table.Installations), "poke_sources", len(table.Pokes))
	return serve(ctx, ln, srv, log, defaultLimits)
}

// newServer builds the bartixcloud server, or, when the table has an org section,
// the org server whose credentials come from the files named by AKD_SPIEGEL_APP_ENV
// and AKD_SPIEGEL_APP_KEY. Those variables are read only in org mode, so an existing
// deployment is unaffected by them.
func newServer(cfg dispatcher.Config, table dispatcher.Table, getenv func(string) string, log *slog.Logger) (*dispatcher.Server, error) {
	if table.Org == nil {
		return dispatcher.NewServer(cfg, table, log)
	}
	auth, err := dispatcher.LoadOrgAuth(getenv)
	if err != nil {
		return nil, err
	}
	srv, err := dispatcher.NewOrgServer(cfg, table, auth, log)
	if err != nil {
		return nil, err
	}
	log.Info("org mode", "owner", table.Org.Owner, "spiegel", table.Org.SpiegelRepo)
	return srv, nil
}

// serve runs the HTTP server on ln until ctx ends, then stops it gracefully: no new
// requests, in-flight ones finish within limits.http, in-flight pokes within
// limits.drain, after which they are cancelled. It returns the process exit code.
func serve(ctx context.Context, ln net.Listener, srv *dispatcher.Server, log *slog.Logger, limits shutdownLimits) int {
	httpSrv := dispatcher.NewHTTPServer(srv, log)
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()

	select {
	case err := <-served:
		// Nothing asked Serve to stop, so any return is a failure.
		log.Error("http server stopped unexpectedly", "err", err)
		srv.Close()
		return exitFailed
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), limits.http)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown incomplete, cutting connections", "err", err)
		if cerr := httpSrv.Close(); cerr != nil && !errors.Is(cerr, http.ErrServerClosed) {
			log.Error("closing http server failed", "err", cerr)
		}
		<-served
		// Handlers may still be running, which Wait must not race with: Close is safe.
		srv.Close()
		return exitFailed
	}
	<-served // Serve returns http.ErrServerClosed once Shutdown has run
	drainPokes(srv, log, limits.drain)
	return exitOK
}

// drainPokes waits for in-flight pokes and org plans up to timeout, then cancels them. Only valid
// once the HTTP server has fully shut down, so no handler can start another poke.
func drainPokes(srv *dispatcher.Server, log *slog.Logger, timeout time.Duration) {
	finished := make(chan struct{})
	go func() {
		srv.Wait()
		close(finished)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		log.Warn("poke drain window elapsed, cancelling in-flight pokes", "window", timeout.String())
		srv.Close()
		<-finished
	}
}

// complain writes one line to stderr. A failed write to stderr has nowhere to be
// reported, so it is dropped on purpose.
func complain(stderr io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(stderr, format+"\n", args...)
}
