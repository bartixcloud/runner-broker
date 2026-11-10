package dispatcher

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAppFiles writes a fresh RSA key and an env file with BCX_APP_ID into dir.
func writeAppFiles(t *testing.T, dir, appID string) OrgAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	auth := OrgAuth{AppEnvFile: filepath.Join(dir, "app.env"), KeyFile: filepath.Join(dir, "app.pem")}
	if err := os.WriteFile(auth.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.AppEnvFile, []byte("BCX_APP_ID="+appID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestLoadOrgAuthDefaultsAndOverrides(t *testing.T) {
	got, err := LoadOrgAuth(envOf(nil))
	if err != nil || got != (OrgAuth{AppEnvFile: "/etc/akd-spiegel/app.env", KeyFile: "/etc/akd-spiegel/app.pem"}) {
		t.Errorf("defaults = %+v, %v", got, err)
	}
	got, err = LoadOrgAuth(envOf(map[string]string{"AKD_SPIEGEL_APP_ENV": "/x/a.env", "AKD_SPIEGEL_APP_KEY": "/x/k.pem"}))
	if err != nil || got != (OrgAuth{AppEnvFile: "/x/a.env", KeyFile: "/x/k.pem"}) {
		t.Errorf("overrides = %+v, %v", got, err)
	}
}

func TestLoadOrgAuthRefusesRelativePaths(t *testing.T) {
	for _, env := range []map[string]string{
		{"AKD_SPIEGEL_APP_ENV": "app.env"},
		{"AKD_SPIEGEL_APP_KEY": "k.pem"},
	} {
		_, err := LoadOrgAuth(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("%v: err = %v", env, err)
		}
	}
}

func TestOrgAuthTokenSourceChecksIDAndKeyAtStart(t *testing.T) {
	auth := writeAppFiles(t, t.TempDir(), "4242")
	if _, err := auth.tokenSource(77); err != nil {
		t.Fatalf("tokenSource: %v", err)
	}

	missing := OrgAuth{AppEnvFile: filepath.Join(t.TempDir(), "none.env"), KeyFile: auth.KeyFile}
	if _, err := missing.tokenSource(77); err == nil || !strings.Contains(err.Error(), missing.AppEnvFile) {
		t.Errorf("missing env file: err = %v", err)
	}
	badKey := OrgAuth{AppEnvFile: auth.AppEnvFile, KeyFile: filepath.Join(t.TempDir(), "none.pem")}
	if _, err := badKey.tokenSource(77); err == nil || !strings.Contains(err.Error(), badKey.KeyFile) {
		t.Errorf("missing key: err = %v", err)
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("-----BEGIN NOPE-----\nSECRETJUNK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OrgAuth{AppEnvFile: auth.AppEnvFile, KeyFile: junk}.tokenSource(77)
	if err == nil || strings.Contains(err.Error(), "SECRETJUNK") {
		t.Errorf("junk key: err = %v, want an error without key material", err)
	}
}

func TestOrgAuthIgnoresTheBartixcloudAppIDInTheEnvironment(t *testing.T) {
	t.Setenv("BCX_APP_ID", "999")
	auth := writeAppFiles(t, t.TempDir(), "4242")
	if err := os.WriteFile(auth.AppEnvFile, []byte("# no id here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.tokenSource(77); err == nil {
		t.Error("the org instance used BCX_APP_ID from the process environment")
	}
}

func TestNewOrgServerNeedsAnOrgSectionAndWorkingCredentials(t *testing.T) {
	log, _ := testLogger()
	auth := writeAppFiles(t, t.TempDir(), "4242")
	cfg := Config{Secret: testSecret, SocketDir: "/x"}

	srv, err := NewOrgServer(cfg, orgTestTable(), auth, log)
	if err != nil {
		t.Fatalf("NewOrgServer: %v", err)
	}
	defer srv.Close()
	if srv.Org == nil || srv.Org.Workflows == nil || srv.Org.Cmd == nil || srv.Org.Cfg.Owner != "ekd-akd" {
		t.Errorf("org runner = %+v", srv.Org)
	}
	if srv.OrgParallel != 4 {
		t.Errorf("OrgParallel = %d, want 4", srv.OrgParallel)
	}
	if _, ok := srv.Org.Cmd.(ExecOutputRunner); !ok {
		t.Errorf("Cmd = %T, want ExecOutputRunner", srv.Org.Cmd)
	}

	if _, err := NewOrgServer(cfg, testTable(), auth, log); err == nil {
		t.Error("NewOrgServer accepted a table without an org section")
	}
	if _, err := NewOrgServer(cfg, orgTestTable(), OrgAuth{AppEnvFile: "/nonexistent/a.env", KeyFile: auth.KeyFile}, log); err == nil {
		t.Error("NewOrgServer accepted missing credentials")
	}
	if _, err := NewOrgServer(Config{Secret: "short"}, orgTestTable(), auth, log); err == nil {
		t.Error("NewOrgServer accepted a short secret")
	}
}

// A plain NewServer must refuse an org table: silently serving it in bartixcloud
// mode would answer an org's events with the wrong behaviour.
func TestNewServerRefusesAnOrgTable(t *testing.T) {
	log, _ := testLogger()
	_, err := NewServer(Config{Secret: testSecret, SocketDir: "/x"}, orgTestTable(), log)
	if err == nil || !strings.Contains(err.Error(), "org") {
		t.Errorf("err = %v", err)
	}
}
