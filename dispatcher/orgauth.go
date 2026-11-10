package dispatcher

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/bartixcloud/runner-broker/appauth"
)

const (
	envSpiegelAppEnv = "AKD_SPIEGEL_APP_ENV"
	envSpiegelAppKey = "AKD_SPIEGEL_APP_KEY"

	defaultSpiegelAppEnv = "/etc/akd-spiegel/app.env"
	defaultSpiegelAppKey = "/etc/akd-spiegel/app.pem"
)

// OrgAuth names the two files that hold the akd-spiegel App's credentials: an env
// file with BCX_APP_ID=<numeric id> and the RSA private key. The environment carries
// the paths, never a value, so neither the key nor the id can show up in a process
// listing or a log line of this daemon.
type OrgAuth struct {
	AppEnvFile string
	KeyFile    string
}

// LoadOrgAuth reads the two paths through getenv; unset or empty means the default.
func LoadOrgAuth(getenv func(string) string) (OrgAuth, error) {
	auth := OrgAuth{
		AppEnvFile: orDefault(getenv(envSpiegelAppEnv), defaultSpiegelAppEnv),
		KeyFile:    orDefault(getenv(envSpiegelAppKey), defaultSpiegelAppKey),
	}
	if !filepath.IsAbs(auth.AppEnvFile) {
		return OrgAuth{}, fmt.Errorf("%s must be an absolute path", envSpiegelAppEnv)
	}
	if !filepath.IsAbs(auth.KeyFile) {
		return OrgAuth{}, fmt.Errorf("%s must be an absolute path", envSpiegelAppKey)
	}
	return auth, nil
}

// tokenSource builds the installation-token source for the mirror repo and proves at
// start that both files work: the id parses, and the key signs a JWT (no network
// involved). The App id is read from the file only; the process environment's
// BCX_APP_ID belongs to the bartixcloud App. Errors name paths, never contents.
func (a OrgAuth) tokenSource(installation int) (TokenSource, error) {
	appID, err := appauth.AppIDFromFile(a.AppEnvFile)
	if err != nil {
		return nil, err
	}
	if _, err := appauth.AppJWT(appID, a.KeyFile, time.Now().Unix()); err != nil {
		return nil, err
	}
	return appauth.NewInstallationToken(appID, installation, a.KeyFile), nil
}

// NewOrgServer is NewServer for an instance whose table has an org section: it wires
// the production runner (real processes, GitHub API with the akd-spiegel App's
// installation token) and fails the start on any credential problem.
func NewOrgServer(cfg Config, table Table, auth OrgAuth, log *slog.Logger) (*Server, error) {
	if table.Org == nil {
		return nil, errors.New("NewOrgServer needs a table with an org section")
	}
	tokens, err := auth.tokenSource(table.Org.SpiegelInstallation)
	if err != nil {
		return nil, fmt.Errorf("org credentials: %w", err)
	}
	srv, err := newServer(cfg, table, log)
	if err != nil {
		return nil, err
	}
	srv.Org = &OrgRunner{
		Cfg: *table.Org,
		Cmd: ExecOutputRunner{},
		Workflows: &GitHubWorkflowDispatcher{
			APIBase: appauth.GHAPI, SpiegelRepo: table.Org.SpiegelRepo, Ref: table.Org.SpiegelRef, Tokens: tokens,
		},
		Log: srv.Log,
	}
	return srv, nil
}
