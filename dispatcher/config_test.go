package dispatcher

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// envOf returns a getenv over a fixed map, like os.Getenv for a clean process.
func envOf(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": testSecret}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := Config{
		Host: "172.17.0.1", Port: 9100,
		MapFile: "/etc/bcx-broker/dispatcher.json", SocketDir: "/run/bcx-broker",
		PokeTimeout: 5 * time.Minute,
		Secret:      testSecret,
	}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	if got := cfg.Addr(); got != "172.17.0.1:9100" {
		t.Errorf("Addr = %q", got)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{
		"BCX_WEBHOOK_SECRET":    testSecret,
		"BCX_DISPATCHER_HOST":   "127.0.0.1",
		"BCX_DISPATCHER_PORT":   "9200",
		"BCX_DISPATCHER_MAP":    "/tmp/map.json",
		"BCX_BROKER_SOCKET_DIR": "/tmp/socks",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := Config{Host: "127.0.0.1", Port: 9200, MapFile: "/tmp/map.json", SocketDir: "/tmp/socks",
		PokeTimeout: 5 * time.Minute, Secret: testSecret}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
	if got := cfg.Addr(); got != "127.0.0.1:9200" {
		t.Errorf("Addr = %q", got)
	}
}

func TestLoadConfigAddrBracketsIPv6(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": testSecret, "BCX_DISPATCHER_HOST": "::1"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Addr(); got != "[::1]:9100" {
		t.Errorf("Addr = %q, want [::1]:9100", got)
	}
}

func TestLoadConfigTreatsEmptyOverridesAsUnset(t *testing.T) {
	// An empty host would make net.Listen bind every interface: never let it through.
	cfg, err := LoadConfig(envOf(map[string]string{
		"BCX_WEBHOOK_SECRET": testSecret, "BCX_DISPATCHER_HOST": "", "BCX_DISPATCHER_PORT": "",
		"BCX_DISPATCHER_MAP": "", "BCX_BROKER_SOCKET_DIR": "", "BCX_DISPATCHER_POKE_TIMEOUT": "",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Host != "172.17.0.1" || cfg.Port != 9100 || cfg.MapFile == "" || cfg.SocketDir == "" ||
		cfg.PokeTimeout != 5*time.Minute {
		t.Errorf("empty overrides must fall back to defaults, got %+v", cfg)
	}
}

func TestLoadConfigRefusesAShortSecret(t *testing.T) {
	for _, secret := range []string{"", "zq-7731", strings.Repeat("a", 19)} {
		_, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": secret}))
		if err == nil {
			t.Errorf("secret of %d bytes: expected an error", len(secret))
			continue
		}
		if !strings.Contains(err.Error(), "BCX_WEBHOOK_SECRET") {
			t.Errorf("error %q must name BCX_WEBHOOK_SECRET", err)
		}
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Errorf("error %q must not echo the secret", err)
		}
	}
}

func TestLoadConfigAcceptsASecretOfExactlyTheMinimum(t *testing.T) {
	if _, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": strings.Repeat("a", 20)})); err != nil {
		t.Errorf("20-byte secret refused: %v", err)
	}
}

func TestLoadConfigRefusesABadPort(t *testing.T) {
	for _, port := range []string{"abc", "-1", "65536", "99999999999999999999", "92 00", "9100\n", "0x10"} {
		_, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": testSecret, "BCX_DISPATCHER_PORT": port}))
		if err == nil || !strings.Contains(err.Error(), "BCX_DISPATCHER_PORT") {
			t.Errorf("port %q: err = %v, want one naming BCX_DISPATCHER_PORT", port, err)
		}
	}
}

func TestLoadConfigAcceptsPortZeroForTests(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": testSecret, "BCX_DISPATCHER_PORT": "0"}))
	if err != nil || cfg.Port != 0 {
		t.Errorf("port 0: cfg.Port=%d err=%v", cfg.Port, err)
	}
}

func TestLoadConfigReadsPokeTimeout(t *testing.T) {
	cfg, err := LoadConfig(envOf(map[string]string{"BCX_WEBHOOK_SECRET": testSecret}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PokeTimeout != 5*time.Minute {
		t.Errorf("PokeTimeout = %v, want the 5m default", cfg.PokeTimeout)
	}

	cfg, err = LoadConfig(envOf(map[string]string{
		"BCX_WEBHOOK_SECRET":          testSecret,
		"BCX_DISPATCHER_POKE_TIMEOUT": "90s",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PokeTimeout != 90*time.Second {
		t.Errorf("PokeTimeout = %v, want 90s", cfg.PokeTimeout)
	}

	for _, bad := range []string{"soon", "0s", "-1m"} {
		_, err := LoadConfig(envOf(map[string]string{
			"BCX_WEBHOOK_SECRET":          testSecret,
			"BCX_DISPATCHER_POKE_TIMEOUT": bad,
		}))
		if err == nil || !strings.Contains(err.Error(), "BCX_DISPATCHER_POKE_TIMEOUT") {
			t.Errorf("PokeTimeout %q: err = %v, want one naming BCX_DISPATCHER_POKE_TIMEOUT", bad, err)
		}
	}
}

func TestConfigNeverRendersTheSecret(t *testing.T) {
	cfg := Config{Host: "h", Port: 1, MapFile: "m", SocketDir: "s", Secret: testSecret}

	rendered := []string{
		fmt.Sprint(cfg), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg), fmt.Sprint(&cfg), cfg.String(),
	}
	if raw, err := json.Marshal(cfg); err != nil {
		t.Fatalf("marshal: %v", err)
	} else {
		rendered = append(rendered, string(raw))
	}
	log, out := testLogger()
	log.Info("config", "cfg", cfg, slog.Any("again", &cfg))
	rendered = append(rendered, out.String())

	for _, text := range rendered {
		if strings.Contains(text, testSecret) {
			t.Errorf("secret leaked in %q", text)
		}
	}
	if !strings.Contains(cfg.String(), "[redacted]") {
		t.Errorf("String should say the secret is redacted, got %q", cfg.String())
	}
}
