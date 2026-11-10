package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/bartixcloud/runner-broker/mem"
)

// httpClient is the shared GitHub client (seam for tests).
var httpClient = &http.Client{Timeout: 20 * time.Second}

// availableBytes is a seam over mem.AvailableBytes.
var availableBytes = func() (uint64, error) { return memAvailable() }

func memAvailable() (uint64, error) {
	return mem.AvailableBytes("/proc/meminfo")
}

// execTimeout bounds every subprocess. `incus list` runs under launchLock, so a
// wedged incusd would otherwise block every later reservation forever.
var execTimeout = 5 * time.Minute

// pause is a seam over time.Sleep for the guest-agent wait loop.
var pause = time.Sleep

// execCommand is the seam over exec.Command for subprocesses; incus and
// incusInput are its only callers for incus. A process that could not run (or
// was killed) reports code -1 with the reason in stderr, never an empty reason.
var execCommand = func(argv []string, stdin string) (stdout, stderr string, exitCode int) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.WaitDelay = 5 * time.Second // a killed child's grandchildren may hold the pipes open
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		code = ee.ExitCode()
	default:
		code = -1
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s: %w", execTimeout, err)
		}
		errBuf.WriteString(err.Error())
	}
	return outBuf.String(), errBuf.String(), code
}

// randHex returns 6 random hex chars (mirrors uuid4().hex[:6]).
func randHex() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// umask sets and returns the previous umask.
func umask(mask int) int {
	return syscall.Umask(mask)
}
