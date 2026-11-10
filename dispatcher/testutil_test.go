package dispatcher

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
)

// testSecret is long enough for the minimum-length gate.
const testSecret = "s3cret-s3cret-s3cret-s3cret"

// sign is an independent oracle for X-Hub-Signature-256: it does not call the
// production VerifySignature.
func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(body); err != nil {
		t.Fatalf("hmac write: %v", err)
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// shortTmp returns a short-pathed temp dir: macOS caps AF_UNIX paths near 104
// bytes and t.TempDir() is longer than that.
func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bcxd")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func strPtr(s string) *string { return &s }

// syncBuffer is an io.Writer safe for the handler, poke goroutines and the test
// to share under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger logs everything, including debug, into the returned buffer.
func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// fakeRunner is a Runner that records argv and fails for chosen VMs.
type fakeRunner struct {
	mu          sync.Mutex
	calls       [][]string
	outputCalls [][]string
	failVM      map[string]string // vm name -> stderr to return with an error
	outputErr   map[string]string // vm name -> stderr to return with an Output error
	outputs     map[string]string // vm name -> stdout of Output
	onCall      func(argv []string)
	blockOn     bool // block until the context is done, then fail
}

func (f *fakeRunner) Run(ctx context.Context, argv []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(argv))
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(argv)
	}
	if f.blockOn {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if len(argv) > 4 {
		if stderr, bad := f.failVM[argv[4]]; bad {
			return stderr, errors.New("exit status 1")
		}
	}
	return "", nil
}

// Output serves whatever the test configured for the VM at argv[4].
func (f *fakeRunner) Output(_ context.Context, argv []string) (string, string, error) {
	f.mu.Lock()
	f.outputCalls = append(f.outputCalls, slices.Clone(argv))
	f.mu.Unlock()
	if len(argv) > 4 {
		if stderr, bad := f.outputErr[argv[4]]; bad {
			return "", stderr, errors.New("exit status 1")
		}
		return f.outputs[argv[4]], "", nil
	}
	return "", "", nil
}

func (f *fakeRunner) argvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeRunner) outputArgvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.outputCalls)
}
