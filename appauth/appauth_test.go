package appauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	sharedKeyOnce sync.Once
	sharedKey     *rsa.PrivateKey
	sharedKeyErr  error
)

// rsaKey generates the 2048-bit key once per test binary; generation dominates
// the suite's runtime under -race.
func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	sharedKeyOnce.Do(func() {
		sharedKey, sharedKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if sharedKeyErr != nil {
		t.Fatal(sharedKeyErr)
	}
	return sharedKey
}

func writePEM(t *testing.T, name, blockType string, der []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// genKey writes the shared key as a PKCS#1 app.pem (the format GitHub hands out
// and the one in production) plus its public half for independent verification.
func genKey(t *testing.T) (keyPath, pubPath string, pub *rsa.PublicKey) {
	t.Helper()
	key := rsaKey(t)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return writePEM(t, "app.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)),
		writePEM(t, "pub.pem", "PUBLIC KEY", pubDER),
		&key.PublicKey
}

// splitJWT decodes a compact JWS, failing the test unless it has exactly three
// segments, and verifies the RS256 signature. It returns the raw header and
// claims JSON so tests can compare exact bytes (key order and key set).
func splitJWT(t *testing.T, jwt string, pub *rsa.PublicKey) (header, claims string) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3: %q", len(parts), jwt)
	}
	dec := make([][]byte, len(parts))
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("segment %d is not unpadded base64url: %v", i, err)
		}
		dec[i] = b
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], dec[2]); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	return string(dec[0]), string(dec[1])
}

func TestAppJWTIsRS256AndVerifies(t *testing.T) {
	keyPath, _, pub := genKey(t)
	jwt, err := AppJWT("12345", keyPath, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	header, claims := splitJWT(t, jwt, pub)
	// Exact bytes: no extra header/claim keys (a stray "kid" or claim would
	// change what GitHub sees), and Python's {iat,exp,iss} order. iat is
	// backdated 60 s against clock drift; exp is 9 min out (GitHub caps at 10).
	if want := `{"alg":"RS256","typ":"JWT"}`; header != want {
		t.Errorf("header = %s, want %s", header, want)
	}
	if want := `{"iat":999940,"exp":1000540,"iss":"12345"}`; claims != want {
		t.Errorf("claims = %s, want %s", claims, want)
	}
}

func TestAppJWTVerifiesWithOpenSSL(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed")
	}
	keyPath, pubPath, _ := genKey(t)
	jwt, err := AppJWT("12345", keyPath, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	inPath, sigPath := filepath.Join(dir, "in"), filepath.Join(dir, "sig")
	if err := os.WriteFile(inPath, []byte(parts[0]+"."+parts[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(openssl, "dgst", "-sha256", "-verify", pubPath, "-signature", sigPath, inPath).CombinedOutput()
	if err != nil {
		t.Errorf("openssl rejected the signature: %v\n%s", err, out)
	}
}

func TestAppJWTAcceptsPKCS8RSAKey(t *testing.T) {
	key := rsaKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := AppJWT("7", writePEM(t, "pkcs8.pem", "PRIVATE KEY", der), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	splitJWT(t, jwt, &key.PublicKey)
}

func TestAppJWTRejectsUnusableKeys(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	// An EC key would otherwise sign a non-RS256 signature under an RS256 header.
	empty := filepath.Join(t.TempDir(), "empty.pem")
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(garbage, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"ec":      writePEM(t, "ec.pem", "PRIVATE KEY", ecDER),
		"empty":   empty,
		"garbage": garbage,
		"bad-der": writePEM(t, "bad.pem", "RSA PRIVATE KEY", []byte("not der")),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			jwt, err := AppJWT("1", path, 1_000_000)
			if err == nil {
				t.Fatalf("expected error, got JWT %q", jwt)
			}
			if jwt != "" {
				t.Errorf("JWT = %q alongside error", jwt)
			}
		})
	}
}

func TestAppJWTRaisesOnUnreadableKey(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	_, err := AppJWT("1", missing, 1)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
	// openssl is no longer involved; naming it sends an operator after a binary
	// that never ran.
	if strings.Contains(err.Error(), "openssl") {
		t.Errorf("error mentions openssl: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot sign with "+missing) {
		t.Errorf("error = %v, want it to name the key path", err)
	}
}

func TestInstallationTokenReusedUntilShortlyBeforeExpiry(t *testing.T) {
	keyPath, _, _ := genKey(t)
	now := int64(1_700_000_000)
	calls := 0

	tok := NewInstallationToken("12345", 777, keyPath)
	tok.clock = func() int64 { return now }
	tok.mintFn = func(appID string, installationID int, keyPath string, now2 int64) (string, float64, error) {
		calls++
		return "ghs_" + strconv.Itoa(calls), float64(now + 3600), nil
	}

	got, err := tok.Get()
	if err != nil || got != "ghs_1" {
		t.Fatalf("Get() = %q, %v", got, err)
	}
	// 300 is pinned literally, not via refreshMarginSeconds: a test computed
	// from the constant cannot notice the constant changing.
	now += 3600 - 300 - 1
	if got, err := tok.Get(); err != nil || got != "ghs_1" {
		t.Errorf("one second before the margin: Get() = %q, %v, want reuse of ghs_1", got, err)
	}
	now++ // exactly expiry-300: the boundary refreshes
	if got, err := tok.Get(); err != nil || got != "ghs_2" {
		t.Errorf("at the margin: Get() = %q, %v, want refresh to ghs_2", got, err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestInstallationTokenSerialisesConcurrentMints(t *testing.T) {
	keyPath, _, _ := genKey(t)
	calls := 0 // plain int on purpose: -race flags it if Get stops holding the mutex

	tok := NewInstallationToken("12345", 777, keyPath)
	tok.clock = func() int64 { return 1_700_000_000 }
	tok.mintFn = func(string, int, string, int64) (string, float64, error) {
		calls++
		return "ghs_once", float64(1_700_000_000 + 3600), nil
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := tok.Get(); err != nil || got != "ghs_once" {
				t.Errorf("Get() = %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Errorf("mint ran %d times for 16 concurrent callers, want 1", calls)
	}
}

func TestAppIDReadsEnvThenFile(t *testing.T) {
	t.Setenv("BCX_APP_ID", "99")
	if got, _ := AppID(filepath.Join(t.TempDir(), "none")); got != "99" {
		t.Errorf("AppID from env = %q", got)
	}
	t.Setenv("BCX_APP_ID", "")
	f := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(f, []byte("# App id\nBCX_APP_ID=4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := AppID(f); got != "4242" {
		t.Errorf("AppID from file = %q", got)
	}
	_, err := AppID(filepath.Join(t.TempDir(), "none"))
	if err == nil {
		t.Fatal("expected error for missing app id")
	}
	if !strings.Contains(err.Error(), "BCX_APP_ID") {
		t.Errorf("error = %v, want it to name BCX_APP_ID", err)
	}
}

func TestAppIDMatrix(t *testing.T) {
	cases := []struct {
		name string
		env  string
		file string // "" means no file
		want string // "" means an error
	}{
		{"env beats file", "1", "BCX_APP_ID=2\n", "1"},
		{"file when env empty", "", "BCX_APP_ID=2\n", "2"},
		{"last assignment wins", "", "BCX_APP_ID=1\nBCX_APP_ID=2\n", "2"},
		{"CRLF", "", "BCX_APP_ID=4242\r\n", "4242"},
		{"no trailing newline", "", "BCX_APP_ID=4242", "4242"},
		{"twenty digits stay a string", "", "BCX_APP_ID=12345678901234567890\n", "12345678901234567890"},
		{"comment is not an assignment", "", "#BCX_APP_ID=5\n", ""},
		{"other keys ignored", "", "OTHER=5\n", ""},
		{"letters in env", "abc", "BCX_APP_ID=2\n", ""},
		{"trailing letter in env", "12a", "BCX_APP_ID=2\n", ""},
		{"leading space in env", " 12", "BCX_APP_ID=2\n", ""},
		{"blank env does not fall back to file", "  ", "BCX_APP_ID=2\n", ""},
		{"letters in file", "", "BCX_APP_ID=abc\n", ""},
		{"empty value in file", "", "BCX_APP_ID=\n", ""},
		{"no file", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BCX_APP_ID", tc.env)
			path := filepath.Join(t.TempDir(), "app.env")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := AppID(path)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("AppID = %q, want error", got)
				}
				if got != "" {
					t.Errorf("AppID = %q alongside error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("AppID = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// AppIDFromFile is the explicit-path form: a BCX_APP_ID in the process
// environment belongs to another app and must never leak into it.
func TestAppIDFromFileIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("BCX_APP_ID", "99")
	path := filepath.Join(t.TempDir(), "leitstand.env")
	if err := os.WriteFile(path, []byte("# other app\nBCX_APP_ID=4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := AppIDFromFile(path); err != nil || got != "4242" {
		t.Errorf("AppIDFromFile = %q, %v, want 4242", got, err)
	}
	if got, err := AppIDFromFile(filepath.Join(t.TempDir(), "none")); err == nil || got != "" {
		t.Errorf("a missing file must be an error, got %q, %v", got, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(bad, []byte("BCX_APP_ID=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := AppIDFromFile(bad)
	if err == nil || !strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "abc") {
		t.Errorf("error = %v, want the path but never the rejected value", err)
	}
}
