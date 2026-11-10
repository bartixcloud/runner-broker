package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bartixcloud/runner-broker/hcloud"
)

type hcloudSeams struct {
	token    string
	proofs   int
	proofErr error
}

func useHcloudSeams(t *testing.T, c *hcloudSeams) {
	t.Helper()
	swap(t, &newHcloudClient, func(token string) *hcloud.Client {
		c.token = token
		return hcloud.New("client-token")
	})
	swap(t, &proveHcloud, func(*hcloud.Client) error {
		c.proofs++
		return c.proofErr
	})
}

func tokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alpha-org.hcloud-token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func hcloudConfig(path string, target string) Config {
	return Config{Tenant: "alpha-org", Target: target, HcloudTokenFile: path}
}

func TestLoadConfigDefaultsTheHcloudTokenFile(t *testing.T) {
	cfg, err := loadConfig(lookupOf(orgEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HcloudTokenFile != "/etc/bcx-broker/alpha-org.hcloud-token" {
		t.Errorf("default = %q", cfg.HcloudTokenFile)
	}
	cfg, err = loadConfig(lookupOf(with(orgEnv(), "BROKER_HCLOUD_TOKEN_FILE", "/tmp/other-token")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HcloudTokenFile != "/tmp/other-token" {
		t.Errorf("override = %q", cfg.HcloudTokenFile)
	}
}

func TestHcloudTokenIsNotRequiredWhenNoUsableHcloudTierExists(t *testing.T) {
	for _, c := range []struct {
		name   string
		target string
		tiers  map[string]tier
	}{
		{"incus only", "alpha-org", map[string]tier{"build": tiersFixture["build"]}},
		{"hcloud tier owned by another tenant", "juno", map[string]tier{"e2e": hcloudFixture()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			seams := &hcloudSeams{}
			useHcloudSeams(t, seams)
			client, err := loadHcloudClient(hcloudConfig("/nonexistent/token", c.target), c.tiers)
			if err != nil || client != nil {
				t.Fatalf("got (%v, %v), want no client and no error", client, err)
			}
			if seams.proofs != 0 || seams.token != "" {
				t.Errorf("unnecessary hcloud read: proofs=%d token=%q", seams.proofs, seams.token)
			}
		})
	}
}

func TestHcloudTokenStartupValidation(t *testing.T) {
	dir := t.TempDir()
	valid := tokenFile(t, "test-token\n", 0o600)
	empty := tokenFile(t, " \n\t", 0o600)
	wide := tokenFile(t, "test-token\n", 0o644)
	subdir := filepath.Join(dir, "token-dir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		path      string
		proofErr  error
		want      string
		wantProof int
	}{
		{"missing", filepath.Join(dir, "absent"), nil, "absent", 0},
		{"not a regular file", subdir, nil, "regular", 0},
		{"group or other bits", wide, nil, "mode", 1},
		{"empty after trimming", empty, nil, "empty", 0},
		{"startup proof fails", valid, errors.New("unauthorized"), "startup proof", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seams := &hcloudSeams{proofErr: c.proofErr}
			useHcloudSeams(t, seams)
			client, err := loadHcloudClient(hcloudConfig(c.path, "alpha-org"), map[string]tier{"e2e": hcloudFixture()})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			if client != nil {
				t.Errorf("a failed token check returned a client")
			}
			if seams.proofs != c.wantProof {
				t.Errorf("proof calls = %d, want %d", seams.proofs, c.wantProof)
			}
			if strings.Contains(err.Error(), "test-token") {
				t.Errorf("token leaked into error: %q", err)
			}
		})
	}
}

func TestHcloudTokenProvesTheServerEndpointOnce(t *testing.T) {
	seams := &hcloudSeams{}
	useHcloudSeams(t, seams)
	client, err := loadHcloudClient(hcloudConfig(tokenFile(t, "test-token\n", 0o600), "alpha-org"),
		map[string]tier{"e2e": hcloudFixture()})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if seams.token != "test-token" || seams.proofs != 1 {
		t.Errorf("token = %q proofs = %d, want the trimmed token and one proof", seams.token, seams.proofs)
	}
}

func TestHcloudTokenIgnoresAFileForAnotherTenant(t *testing.T) {
	seams := &hcloudSeams{}
	useHcloudSeams(t, seams)
	path := filepath.Join(t.TempDir(), "missing")
	// juno cannot use the alpha-org-owned tier, so even a missing file is fine.
	client, err := loadHcloudClient(hcloudConfig(path, "juno"), map[string]tier{"e2e": hcloudFixture()})
	if err != nil || client != nil || seams.proofs != 0 {
		t.Errorf("got (%v, %v), proofs=%d", client, err, seams.proofs)
	}
}

// A usable hcloud tier turns a failed token proof into a startup failure before
// a socket exists, not a broker that serves and fails every e2e job later.
func TestRunFailsStartupWhenTheHcloudProofFails(t *testing.T) {
	seams := &hcloudSeams{proofErr: errors.New("unauthorized")}
	useHcloudSeams(t, seams)
	lookup, sock := runEnv(t, hcloudTierYAML("e2e", nil))
	env := map[string]string{}
	for _, k := range brokerEnvNames {
		if v, ok := lookup(k); ok {
			env[k] = v
		}
	}
	env["BROKER_HCLOUD_TOKEN_FILE"] = tokenFile(t, "test-token\n", 0o600)
	err := run(context.Background(), lookupOf(env))
	if err == nil || !strings.Contains(err.Error(), "startup proof") {
		t.Fatalf("err = %v, want the startup proof failure", err)
	}
	if _, statErr := os.Lstat(sock); !os.IsNotExist(statErr) {
		t.Error("a broker that failed the hcloud proof must not leave a socket")
	}
}
