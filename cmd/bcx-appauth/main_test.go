package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bartixcloud/runner-broker/appauth"
)

type fakeSource struct {
	token string
	err   error
}

func (f fakeSource) Get() (string, error) { return f.token, f.err }

// sourceCall records what the CLI handed to the token-source factory.
type sourceCall struct {
	appID          string
	installationID int
	keyPath        string
}

// fakeDeps returns deps whose factory records its arguments and yields src. The
// app id comes from the real appauth.AppID, driven through BCX_APP_ID as in the
// Python test_cli_prints_the_token.
func fakeDeps(src tokenSource, calls *[]sourceCall) deps {
	d := defaultDeps()
	d.newSource = func(appID string, installationID int, keyPath string) tokenSource {
		*calls = append(*calls, sourceCall{appID, installationID, keyPath})
		return src
	}
	return d
}

func invoke(t *testing.T, d deps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut, d)
	return code, out.String(), errOut.String()
}

func TestCLIPrintsTheToken(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	var calls []sourceCall
	code, stdout, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_cli"}, &calls), "token", "--installation-id", "5")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "ghs_cli\n" {
		t.Errorf("stdout = %q, want the token and one newline", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := sourceCall{appID: "1", installationID: 5, keyPath: appauth.APPKey}
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("source built with %+v, want exactly %+v", calls, want)
	}
}

func TestCLIFlagForms(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	cases := []struct {
		name string
		args []string
		want sourceCall
	}{
		{"custom key", []string{"token", "--installation-id", "5", "--key", "/tmp/k.pem"},
			sourceCall{"1", 5, "/tmp/k.pem"}},
		{"equals form and flag order", []string{"token", "--key=/tmp/k.pem", "--installation-id=7"},
			sourceCall{"1", 7, "/tmp/k.pem"}},
		{"single dash", []string{"token", "-installation-id", "8"},
			sourceCall{"1", 8, appauth.APPKey}},
		// Python int("010") is 10; flag.Int would read it as octal 8.
		{"leading zeros are decimal", []string{"token", "--installation-id", "010"},
			sourceCall{"1", 10, appauth.APPKey}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []sourceCall
			code, _, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls), tc.args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			if len(calls) != 1 || calls[0] != tc.want {
				t.Errorf("source built with %+v, want %+v", calls, tc.want)
			}
		})
	}
}

func TestCLIRuntimeFailuresExit1WithAppauthPrefix(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	cases := []struct {
		name    string
		src     fakeSource
		wantErr string
	}{
		{"mint error", fakeSource{err: errors.New("boom")}, "appauth: boom\n"},
		// The Python CLI would have printed an empty line and exited 0 here, and
		// fetch-nuk.sh would have carried on with no credential.
		{"empty token", fakeSource{token: ""}, "appauth: empty token\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []sourceCall
			code, stdout, stderr := invoke(t, fakeDeps(tc.src, &calls), "token", "--installation-id", "5")
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing on failure", stdout)
			}
			if stderr != tc.wantErr {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantErr)
			}
		})
	}
}

func TestCLIBadAppIDExit1BeforeBuildingTheSource(t *testing.T) {
	t.Setenv("BCX_APP_ID", "not-numeric")
	var calls []sourceCall
	code, stdout, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls), "token", "--installation-id", "5")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.HasPrefix(stderr, "appauth: BCX_APP_ID missing or not numeric") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(calls) != 0 {
		t.Errorf("source was built despite the app id error: %+v", calls)
	}
}

// failingWriter makes stdout unwritable, as a closed pipe would.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestCLIFailsWhenTheTokenCannotBeWritten(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	var calls []sourceCall
	var errOut bytes.Buffer
	code := run([]string{"token", "--installation-id", "5"}, failingWriter{}, &errOut, fakeDeps(fakeSource{token: "ghs_x"}, &calls))
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.HasPrefix(errOut.String(), "appauth: ") || !strings.Contains(errOut.String(), "broken pipe") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// The real token source with an unreadable key fails in JWT signing, before any
// network call, so this runs the production wiring hermetically.
func TestCLIRealWiringReportsUnreadableKey(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	code, stdout, stderr := invoke(t, defaultDeps(), "token", "--installation-id", "5", "--key", "/nonexistent/app.pem")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.HasPrefix(stderr, "appauth: cannot sign with /nonexistent/app.pem") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIUsageErrorsExit2(t *testing.T) {
	cases := map[string][]string{
		"no subcommand":       {},
		"unknown subcommand":  {"mint", "--installation-id", "5"},
		"unknown flag":        {"token", "--installation-id", "5", "--bogus"},
		"missing id":          {"token"},
		"missing id with key": {"token", "--key", "/tmp/k.pem"},
		"empty id":            {"token", "--installation-id", ""},
		"id without value":    {"token", "--installation-id"},
		"key without value":   {"token", "--installation-id", "5", "--key"},
		"non-integer id":      {"token", "--installation-id", "abc"},
		"fractional id":       {"token", "--installation-id", "5.5"},
		"hex id":              {"token", "--installation-id", "0x10"},
		"zero id":             {"token", "--installation-id", "0"},
		"negative id":         {"token", "--installation-id", "-5"},
		"id overflows int":    {"token", "--installation-id", "99999999999999999999"},
		"stray argument":      {"token", "--installation-id", "5", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BCX_APP_ID", "1")
			var calls []sourceCall
			code, stdout, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls), args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if !strings.Contains(stderr, "usage: bcx-appauth token --installation-id <int> [--key <path>]") {
				t.Errorf("stderr lacks the usage line: %q", stderr)
			}
			if len(calls) != 0 {
				t.Errorf("a usage error must not reach the token source: %+v", calls)
			}
		})
	}
}

func TestCLIHelpExit0(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"token", "-h"}, {"token", "--help"}} {
		var calls []sourceCall
		code, stdout, _ := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls), args...)
		if code != 0 {
			t.Errorf("%v: exit = %d, want 0", args, code)
		}
		if !strings.Contains(stdout, "usage: bcx-appauth token") {
			t.Errorf("%v: stdout = %q, want the usage", args, stdout)
		}
		if len(calls) != 0 {
			t.Errorf("%v: help must not reach the token source", args)
		}
	}
}

// --app-env switches to file-only app-id lookup: another app's BCX_APP_ID in the
// environment must not be used for this app's token.
func TestCLIAppEnvFlagReadsTheIDFromThatFileOnly(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	envFile := filepath.Join(t.TempDir(), "leitstand.env")
	if err := os.WriteFile(envFile, []byte("BCX_APP_ID=777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []sourceCall
	code, stdout, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_l"}, &calls),
		"token", "--installation-id", "5", "--key", "/tmp/l.pem", "--app-env", envFile)
	if code != 0 || stdout != "ghs_l\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if want := (sourceCall{"777", 5, "/tmp/l.pem"}); len(calls) != 1 || calls[0] != want {
		t.Errorf("source built with %+v, want %+v", calls, want)
	}
}

func TestCLIAppEnvFlagFailsWithoutFallingBackToTheEnvironment(t *testing.T) {
	t.Setenv("BCX_APP_ID", "1")
	var calls []sourceCall
	code, stdout, stderr := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls),
		"token", "--installation-id", "5", "--app-env", "/nonexistent/leitstand.env")
	if code != 1 || stdout != "" {
		t.Errorf("exit = %d, stdout = %q, want 1 and nothing", code, stdout)
	}
	if !strings.HasPrefix(stderr, "appauth: BCX_APP_ID missing or not numeric") ||
		!strings.Contains(stderr, "/nonexistent/leitstand.env") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(calls) != 0 {
		t.Errorf("source was built without an app id: %+v", calls)
	}
}

func TestCLIAppEnvFlagWithoutValueIsAUsageError(t *testing.T) {
	var calls []sourceCall
	code, _, _ := invoke(t, fakeDeps(fakeSource{token: "ghs_x"}, &calls), "token", "--installation-id", "5", "--app-env")
	if code != 2 || len(calls) != 0 {
		t.Errorf("exit = %d, calls = %+v, want 2 and none", code, calls)
	}
}
