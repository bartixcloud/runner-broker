package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ageFile points secretsAge at a file that exists, so loadSecrets runs age.
func ageFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tenant.age")
	if err := os.WriteFile(p, []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	swap(t, &secretsAge, p)
	swap(t, &ageKey, "/etc/bartixcloud/age.key")
	return p
}

func TestLoadSecretsWithoutAnAgeFileIsEmpty(t *testing.T) {
	swap(t, &secretsAge, "/nonexistent/absent.age")
	rec := incusStub(t, incusOK)
	got, err := loadSecrets()
	if err != nil || len(got) != 0 {
		t.Errorf("got (%v, %v), want empty", got, err)
	}
	if len(rec.all()) != 0 {
		t.Error("age must not run when there is no age file")
	}
}

// GitHub access is the App installation token and the HMAC is the
// dispatcher's: a tenant age file carries tier secrets only.
func TestLoadSecretsNoLongerRequiresAPAT(t *testing.T) {
	path := ageFile(t)
	rec := incusStub(t, func(incusCall) (string, string, int) { return "INFRA_HCLOUD_TOKEN=h\n", "", 0 })
	got, err := loadSecrets()
	if err != nil || !reflect.DeepEqual(got, map[string]string{"INFRA_HCLOUD_TOKEN": "h"}) {
		t.Fatalf("got (%v, %v)", got, err)
	}
	if want := []string{"age", "-d", "-i", "/etc/bartixcloud/age.key", path}; !reflect.DeepEqual(rec.all()[0].argv, want) {
		t.Errorf("argv = %v, want %v", rec.all()[0].argv, want)
	}
}

// ^(\w+)\s*[:=]\s*(.*)$ after stripping, as in broker.py: a line whose key is
// not a bare word is skipped, not stored under a bogus name.
func TestLoadSecretsParsesLikePython(t *testing.T) {
	ageFile(t)
	plaintext := strings.Join([]string{
		"# a comment", "", "   ",
		"A=1", "B : 2", "C=  spaced  ", "D=x=y", "E=", "  F = 3  ", "G:=h", "I=4\r",
		"FOO BAR=baz", "-KEY=val", "=val", "export J=1", "no separator",
	}, "\n")
	incusStub(t, func(incusCall) (string, string, int) { return plaintext, "", 0 })
	got, err := loadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "2", "C": "spaced", "D": "x=y", "E": "", "F": "3", "G": "=h", "I": "4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// CombinedOutput merged age's stderr into the parsed plaintext, so a warning
// line became a bogus secret.
func TestLoadSecretsKeepsStderrOutOfTheSecrets(t *testing.T) {
	ageFile(t)
	incusStub(t, func(incusCall) (string, string, int) {
		return "INFRA_HCLOUD_TOKEN=supersecret\n", "age: warning: something on stderr\n", 0
	})
	got, err := loadSecrets()
	if err != nil || !reflect.DeepEqual(got, map[string]string{"INFRA_HCLOUD_TOKEN": "supersecret"}) {
		t.Errorf("got (%v, %v)", got, err)
	}
}

// Any plaintext age emitted before failing used to land in the journal.
func TestLoadSecretsFailureReportsOnlyStderr(t *testing.T) {
	ageFile(t)
	incusStub(t, func(incusCall) (string, string, int) {
		return "INFRA_HCLOUD_TOKEN=supersecret\n", "age: error: no identity matched any of the recipients\n", 1
	})
	got, err := loadSecrets()
	if err == nil {
		t.Fatalf("got %v, want an error", got)
	}
	if !strings.Contains(err.Error(), "no identity matched") || strings.Contains(err.Error(), "supersecret") {
		t.Errorf("err = %q", err)
	}
}

func TestLoadSecretsReportsWhyAgeCouldNotRun(t *testing.T) {
	ageFile(t)
	incusStub(t, func(incusCall) (string, string, int) {
		return "", "exec: \"age\": executable file not found in $PATH", -1
	})
	if _, err := loadSecrets(); err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("err = %v", err)
	}
}
