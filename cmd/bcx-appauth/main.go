// Command bcx-appauth mints a bartixcloud-runners GitHub App installation token
// and prints it on stdout. It replaces `python3 app_auth.py token ...` for
// fetch-nuk.sh (installed as /opt/bcx-broker/bcx-appauth).
//
//	bcx-appauth token --installation-id <int> [--key <path>] [--app-env <path>]
//
// Without --app-env the app id comes from BCX_APP_ID (environment, else
// appauth.APPEnv). With it, the id is read from that file only, so a second App
// (e.g. the org's akd-leitstand) never picks up the bartixcloud app's id.
//
// Exit status: 0 token printed, 1 runtime failure (`appauth: <err>` on
// stderr), 2 usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/bartixcloud/runner-broker/appauth"
)

const usage = "usage: bcx-appauth token --installation-id <int> [--key <path>] [--app-env <path>]\n"

// tokenSource is what the CLI needs from appauth.InstallationToken.
type tokenSource interface {
	Get() (string, error)
}

// deps are the CLI's seams: the app-id lookup and the token-source factory, so
// tests can drive every path without network or a key on disk.
type deps struct {
	// appID gets the --app-env path and whether the flag was given explicitly.
	appID     func(appEnv string, explicit bool) (string, error)
	newSource func(appID string, installationID int, keyPath string) tokenSource
}

func defaultDeps() deps {
	return deps{
		appID: func(appEnv string, explicit bool) (string, error) {
			if explicit {
				return appauth.AppIDFromFile(appEnv)
			}
			return appauth.AppID(appEnv)
		},
		newSource: func(appID string, installationID int, keyPath string) tokenSource {
			return appauth.NewInstallationToken(appID, installationID, keyPath)
		},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultDeps()))
}

func run(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		return usageError(stderr, "missing subcommand")
	}
	switch args[0] {
	case "token":
		return runToken(args[1:], stdout, stderr, d)
	case "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return 0
	default:
		return usageError(stderr, fmt.Sprintf("unknown subcommand %q", args[0]))
	}
}

func runToken(args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	// The flag package's own error and usage output is replaced by ours so every
	// usage failure prints the same single usage line.
	fs.SetOutput(io.Discard)
	// A string, not fs.Int: flag.Int parses with base 0, so "010" would be octal
	// 8 and "0x10" would be accepted, where Python's int() reads 10 and rejects.
	idArg := fs.String("installation-id", "", "")
	key := fs.String("key", appauth.APPKey, "")
	appEnv := fs.String("app-env", appauth.APPEnv, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usage)
			return 0
		}
		return usageError(stderr, err.Error())
	}
	if fs.NArg() > 0 {
		return usageError(stderr, fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if *idArg == "" {
		return usageError(stderr, "--installation-id is required")
	}
	installationID, err := strconv.Atoi(*idArg)
	if err != nil || installationID <= 0 {
		return usageError(stderr, fmt.Sprintf("--installation-id must be a positive integer, got %q", *idArg))
	}

	// App id first, as in Python: a missing id fails before any key is touched.
	explicitEnv := false
	fs.Visit(func(f *flag.Flag) { explicitEnv = explicitEnv || f.Name == "app-env" })
	appID, err := d.appID(*appEnv, explicitEnv)
	if err != nil {
		return fail(stderr, err)
	}
	token, err := d.newSource(appID, installationID, *key).Get()
	if err != nil {
		return fail(stderr, err)
	}
	// Defence in depth: an empty line on stdout would exit 0 and hand the
	// caller a blank credential.
	if token == "" {
		return fail(stderr, errors.New("empty token"))
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "appauth: %v\n", err)
	return 1
}

func usageError(stderr io.Writer, msg string) int {
	_, _ = fmt.Fprintf(stderr, "appauth: %s\n%s", msg, usage)
	return 2
}
