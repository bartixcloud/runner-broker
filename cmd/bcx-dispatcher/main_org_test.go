package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orgRig is a complete org-mode instance: a table with an org section whose
// sicherung_dir holds stub scripts, plus the akd-spiegel credential files.
type orgRig struct {
	env    map[string]string
	marker string
	wurzel string
	befund string
	keyPEM string
}

func newOrgRig(t *testing.T) *orgRig {
	t.Helper()
	dir := t.TempDir()
	r := &orgRig{marker: filepath.Join(dir, "marker"), wurzel: filepath.Join(dir, "wurzel"), befund: filepath.Join(dir, "befunde.jsonl")}

	scripts := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scripts, 0o750); err != nil {
		t.Fatal(err)
	}
	// Stubs: sichern.sh is slow on purpose so the drain on shutdown is observable.
	stub := fmt.Sprintf("#!/bin/sh\nsleep 0.3\necho \"$1 $SICHERUNG_WURZEL\" > %q\n", r.marker)
	for name, body := range map[string]string{"sichern.sh": stub, "restic-sichern.sh": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(scripts, name), []byte(body), 0o700); err != nil { //nolint:gosec // a test stub must be executable
			t.Fatal(err)
		}
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	r.keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	keyFile, appEnv := filepath.Join(dir, "app.pem"), filepath.Join(dir, "app.env")
	if err := os.WriteFile(keyFile, []byte(r.keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appEnv, []byte("BCX_APP_ID=4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	org, err := json.Marshal(map[string]any{
		"owner": "ekd-akd", "spiegel_repo": "ekd-akd/spiegel", "spiegel_installation": 77,
		"sicherung_dir": scripts, "sicherung_wurzel": r.wurzel, "befund_datei": r.befund,
	})
	if err != nil {
		t.Fatal(err)
	}
	table := `{"installations":{"1":{"owner":"ekd-akd","tenant":null}},"pokes":{},"org":` + string(org) + `}`
	r.env = map[string]string{
		"BCX_WEBHOOK_SECRET":    testSecret,
		"BCX_DISPATCHER_HOST":   "127.0.0.1",
		"BCX_DISPATCHER_PORT":   "0",
		"BCX_DISPATCHER_MAP":    writeTable(t, table),
		"BCX_BROKER_SOCKET_DIR": shortTmp(t),
		"AKD_SPIEGEL_APP_ENV":   appEnv,
		"AKD_SPIEGEL_APP_KEY":   keyFile,
	}
	return r
}

const orgPushBody = `{"installation":{"id":1},"repository":{"name":"demo","full_name":"ekd-akd/demo","default_branch":"main"},` +
	`"ref":"refs/heads/feature","before":"0000000000000000000000000000000000000000",` +
	`"after":"2222222222222222222222222222222222222222","forced":false,"deleted":false,"commits":[]}`

// oneShot sends one request on a connection that is closed afterwards. The shared
// keep-alive pool can leave a speculatively dialled, never-used connection behind,
// and http.Server.Shutdown waits up to 5 s for such a connection (the same race
// makes the existing signal test fail about once in eighty runs).
func oneShot(t *testing.T, method, addr, path, event, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", "d-1")
		req.Header.Set("X-Hub-Signature-256", signature([]byte(body)))
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain response: %v", err)
	}
	return resp.StatusCode
}

func postOne(t *testing.T, addr, event, body string) int {
	t.Helper()
	return oneShot(t, http.MethodPost, addr, "/github", event, body)
}

// Sends a real signal to this process, like the other signal tests: not parallel.
func TestRunOrgModeServesAndFinishesQueuedJobsBeforeExiting(t *testing.T) {
	r := newOrgRig(t)
	addr, stderr, done := startRun(t, r.env)

	if code := oneShot(t, http.MethodGet, addr, "/healthz", "", ""); code != 200 {
		t.Errorf("healthz = %d", code)
	}
	if code := postOne(t, addr, "ping", `{"zen":"x"}`); code != 200 {
		t.Errorf("ping = %d", code)
	}
	if code := postOne(t, addr, "workflow_job", `{"installation":{"id":1},"repository":{"name":"demo","full_name":"ekd-akd/demo"},"action":"queued"}`); code != 202 {
		t.Errorf("workflow_job = %d, want 202 (ignored: the org instance has no broker)", code)
	}
	start := time.Now()
	if code := postOne(t, addr, "push", orgPushBody); code != 202 {
		t.Fatalf("push = %d, want 202", code)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("the push reply took %v: it must not wait for the 0.3 s backup", time.Since(start))
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d\n%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not exit\n%s", stderr.String())
	}
	got, err := os.ReadFile(r.marker)
	if err != nil {
		t.Fatalf("the queued backup did not finish before exit: %v\n%s", err, stderr.String())
	}
	if want := "demo " + r.wurzel + "\n"; string(got) != want {
		t.Errorf("sichern.sh saw %q, want %q (repo argument and SICHERUNG_WURZEL)", got, want)
	}
	out := stderr.String()
	if !strings.Contains(out, "org mode") {
		t.Errorf("org mode must be logged:\n%s", out)
	}
	for _, secret := range []string{testSecret, r.keyPEM, "4242"} {
		if strings.Contains(out, strings.TrimSpace(secret)) {
			t.Errorf("a secret leaked into the log:\n%s", out)
		}
	}
}

func TestRunOrgModeFailsTheStartOnBadCredentialsOrSection(t *testing.T) {
	cases := map[string]struct {
		tweak   func(r *orgRig, env map[string]string)
		wantErr string
	}{
		"missing app env": {func(_ *orgRig, e map[string]string) { e["AKD_SPIEGEL_APP_ENV"] = "/nonexistent/app.env" }, "/nonexistent/app.env"},
		"missing key":     {func(_ *orgRig, e map[string]string) { e["AKD_SPIEGEL_APP_KEY"] = "/nonexistent/app.pem" }, "/nonexistent/app.pem"},
		"relative path":   {func(_ *orgRig, e map[string]string) { e["AKD_SPIEGEL_APP_KEY"] = "app.pem" }, "AKD_SPIEGEL_APP_KEY"},
		"default paths absent": {func(_ *orgRig, e map[string]string) {
			delete(e, "AKD_SPIEGEL_APP_ENV")
			delete(e, "AKD_SPIEGEL_APP_KEY")
		}, "/etc/akd-spiegel/app.env"},
		"bad org section": {func(_ *orgRig, e map[string]string) {
			e["BCX_DISPATCHER_MAP"] = writeTable(t, `{"installations":{},"pokes":{},"org":{"owner":"ekd-akd"}}`)
		}, "org section"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newOrgRig(t)
			tc.tweak(r, r.env)
			var stderr lockedBuffer
			if code := run(nil, envOf(r.env), &stderr); code != 1 {
				t.Fatalf("exit = %d, want 1\n%s", code, stderr.String())
			}
			out := stderr.String()
			if !strings.Contains(out, tc.wantErr) {
				t.Errorf("stderr must name %q:\n%s", tc.wantErr, out)
			}
			if strings.Contains(out, "listening") {
				t.Errorf("the server started despite the config error:\n%s", out)
			}
			for _, secret := range []string{testSecret, strings.TrimSpace(r.keyPEM)} {
				if strings.Contains(out, secret) {
					t.Errorf("a secret leaked:\n%s", out)
				}
			}
		})
	}
}

// Without an org section the Spiegel variables are never read: the existing
// deployment must start exactly as before even if they are garbage.
func TestRunWithoutOrgSectionIgnoresTheSpiegelVariables(t *testing.T) {
	env := goodEnv(t)
	env["AKD_SPIEGEL_APP_ENV"] = "relative-and-wrong"
	env["AKD_SPIEGEL_APP_KEY"] = "relative-and-wrong"
	_, _, done := startRun(t, env)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if code := awaitExit(t, done); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}
