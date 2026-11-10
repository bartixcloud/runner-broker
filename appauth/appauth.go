// Package appauth mints GitHub App installation tokens for the
// bartixcloud-runners App (platform design §3.3).
//
// Every GitHub call of the broker layer uses a short-lived installation token
// minted here; no PAT exists any more (§3.3).
//
// RS256 is signed with Go's crypto/rsa directly — the Python version shelled
// out to openssl to stay stdlib-only; Go needs no such workaround, so the
// openssl dependency disappears entirely.
package appauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	GHAPI                = "https://api.github.com"
	APPKey               = "/etc/bartixcloud/app.pem"
	APPEnv               = "/etc/bartixcloud/app.env"
	refreshMarginSeconds = 300 // tokens live 1 h; renew 5 min before expiry
)

func b64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func segment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return b64url(b), nil
}

// jwtHeader and jwtClaims are structs, not maps, so field order is fixed
// (Python emits alg,typ and iat,exp,iss) and no stray key can slip in.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Iss string `json:"iss"`
}

// AppID returns BCX_APP_ID from the environment (systemd EnvironmentFile), else
// reads it from app.env directly: the CLI also runs outside any unit.
func AppID(envFile string) (string, error) {
	value := os.Getenv("BCX_APP_ID")
	if value == "" {
		value = appIDFromFile(envFile)
	}
	if !isDigits(value) {
		return "", fmt.Errorf("BCX_APP_ID missing or not numeric (env or %s)", envFile)
	}
	return value, nil
}

// AppIDFromFile reads BCX_APP_ID from envFile only. A second app (the org's
// leitstand) has its own env file, and the process environment's BCX_APP_ID
// belongs to the bartixcloud app, so it is deliberately not consulted. The
// error names the path, never the rejected value.
func AppIDFromFile(envFile string) (string, error) {
	value := appIDFromFile(envFile)
	if !isDigits(value) {
		return "", fmt.Errorf("BCX_APP_ID missing or not numeric in %s", envFile)
	}
	return value, nil
}

// appIDFromFile returns the last BCX_APP_ID assignment in envFile, or "".
func appIDFromFile(envFile string) string {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	value := ""
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "BCX_APP_ID" {
			value = strings.TrimSpace(val)
		}
	}
	return value
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AppJWT builds an RS256 App JWT. iat is backdated 60 s against clock drift;
// exp is 9 min out because GitHub rejects anything beyond 10.
func AppJWT(appID, keyPath string, now int64) (string, error) {
	key, err := loadPrivateKey(keyPath)
	if err != nil {
		return "", fmt.Errorf("cannot sign with %s: %w", keyPath, err)
	}
	header, err := segment(jwtHeader{Alg: "RS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("cannot sign with %s: %w", keyPath, err)
	}
	claims, err := segment(jwtClaims{Iat: now - 60, Exp: now + 540, Iss: appID})
	if err != nil {
		return "", fmt.Errorf("cannot sign with %s: %w", keyPath, err)
	}
	signingInput := header + "." + claims
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("cannot sign with %s: %w", keyPath, err)
	}
	return signingInput + "." + b64url(sig), nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", path)
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := key.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("not an RSA key in %s", path)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func expiry(ts string) (float64, error) {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0, err
	}
	return float64(t.Unix()), nil
}

// InstallationToken caches one installation's token: minted on first use,
// reused until refreshMarginSeconds before GitHub's expires_at. Thread-safe —
// the broker's spawn and cancel goroutines share one instance.
type InstallationToken struct {
	appID          string
	installationID int
	keyPath        string
	clock          func() int64

	// apiBase and client are seams: tests point them at an httptest server so the
	// real mint path runs without network.
	apiBase string
	client  *http.Client

	mu        sync.Mutex
	token     string
	expiresAt float64

	mintFn func(appID string, installationID int, keyPath string, now int64) (token string, exp float64, err error)
}

// NewInstallationToken builds a token source. clock defaults to time.Now.
func NewInstallationToken(appID string, installationID int, keyPath string) *InstallationToken {
	return &InstallationToken{
		appID:          appID,
		installationID: installationID,
		keyPath:        keyPath,
		clock:          func() int64 { return time.Now().Unix() },
		apiBase:        GHAPI,
		client:         newMintClient(),
	}
}

// Get returns the current token, minting or refreshing as needed.
func (t *InstallationToken) Get() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := float64(t.clock())
	if t.token == "" || now >= t.expiresAt-refreshMarginSeconds {
		token, exp, err := t.doMint()
		if err != nil {
			return "", err
		}
		t.token, t.expiresAt = token, exp
	}
	return t.token, nil
}

func (t *InstallationToken) doMint() (string, float64, error) {
	if t.mintFn != nil {
		return t.mintFn(t.appID, t.installationID, t.keyPath, t.clock())
	}
	jwt, err := AppJWT(t.appID, t.keyPath, t.clock())
	if err != nil {
		return "", 0, err
	}
	return mintInstallationToken(t.client, t.apiBase, t.installationID, jwt, nil)
}

// MintScoped mints an installation token restricted to scope and returns it
// uncached: the status reporter needs a fresh token per post, and caching a
// token scoped to one repository would hand it to the next repository's post.
// A nil client or an empty apiBase selects the same defaults as
// NewInstallationToken.
func MintScoped(client *http.Client, apiBase, appID string, installationID int, keyPath string, scope Scope) (string, error) {
	if client == nil {
		client = newMintClient()
	}
	if apiBase == "" {
		apiBase = GHAPI
	}
	jwt, err := AppJWT(appID, keyPath, time.Now().Unix())
	if err != nil {
		return "", err
	}
	token, _, err := mintInstallationToken(client, apiBase, installationID, jwt, &scope)
	if err != nil {
		return "", err
	}
	return token, nil
}
