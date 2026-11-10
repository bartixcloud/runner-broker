package deploytest

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot is the module root; tests run with the package directory as cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// shortTmp returns a short-pathed temp dir: macOS caps AF_UNIX paths near 104 bytes
// and t.TempDir() is longer than that.
func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bcx")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeExec writes an executable file (0755), creating parent directories.
func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// result is the outcome of one command run.
type result struct {
	Code   int
	Stdout string
	Stderr string
}

// runCmd runs name in dir. env entries are appended to the process environment;
// the last value of a duplicated key wins, so a caller can override PATH.
func runCmd(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.Code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", name, err)
	}
	return res
}

// binary is one line of binaries.txt.
type binary struct {
	Role    string
	Name    string
	Package string
}

// loadManifest parses binaries.txt; comment and blank lines are skipped.
func loadManifest(t *testing.T) []binary {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "binaries.txt"))
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []binary
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("manifest line %q: want 3 fields, got %d", line, len(fields))
		}
		out = append(out, binary{Role: fields[0], Name: fields[1], Package: fields[2]})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return out
}

// directive is one key=value line of a systemd unit, with its section.
type directive struct {
	Section string
	Key     string
	Value   string
}

// unitFile is a parsed systemd unit or timer.
type unitFile struct {
	Path       string
	Directives []directive
}

// parseUnit reads a systemd unit file. Repeated keys (EnvironmentFile) are kept in order.
func parseUnit(t *testing.T, path string) unitFile {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open unit: %v", err)
	}
	defer func() { _ = f.Close() }()
	u := unitFile{Path: path}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: line %q is neither a section nor key=value", path, line)
		}
		u.Directives = append(u.Directives, directive{Section: section, Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read unit: %v", err)
	}
	return u
}

// values returns every value of key in section, in file order.
func (u unitFile) values(section, key string) []string {
	var out []string
	for _, d := range u.Directives {
		if d.Section == section && d.Key == key {
			out = append(out, d.Value)
		}
	}
	return out
}

// decodeStrict decodes a YAML file into T and fails on unknown fields, so a
// schema drift in a shipped file fails the test instead of being ignored.
func decodeStrict[T any](t *testing.T, path string) T {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return v
}
