package dispatcher

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

const (
	envSecret      = "BCX_WEBHOOK_SECRET"
	envHost        = "BCX_DISPATCHER_HOST"
	envPort        = "BCX_DISPATCHER_PORT"
	envMap         = "BCX_DISPATCHER_MAP"
	envSocketDir   = "BCX_BROKER_SOCKET_DIR"
	envPokeTimeout = "BCX_DISPATCHER_POKE_TIMEOUT"

	defaultHost      = "172.17.0.1" // the docker bridge: reachable by host Traefik's edge network only
	defaultPort      = 9100
	defaultMapFile   = "/etc/bcx-broker/dispatcher.json"
	defaultSocketDir = "/run/bcx-broker"

	// minSecretLen refuses a guessable HMAC key; GitHub's own generator emits more.
	minSecretLen = 20
	maxPort      = 65535
)

// Config is the dispatcher's whole configuration, read from the environment. It
// never prints or marshals its Secret.
type Config struct {
	Host        string
	Port        int
	MapFile     string
	SocketDir   string
	PokeTimeout time.Duration
	Secret      string `json:"-"`
}

// Addr is the listen address; net.JoinHostPort brackets an IPv6 host.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// String is the only rendering of a Config (%v, %+v, %s, slog.Any): the secret is
// redacted so a stray log line cannot leak the HMAC key.
func (c Config) String() string {
	return fmt.Sprintf("dispatcher config{listen=%s map=%s socket_dir=%s secret=[redacted]}",
		c.Addr(), c.MapFile, c.SocketDir)
}

// GoString keeps %#v from bypassing String.
func (c Config) GoString() string { return c.String() }

// LoadConfig reads the configuration through getenv (os.Getenv in production). An
// unset or empty override means the default: an empty host would otherwise make
// net.Listen bind every interface.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Host:        orDefault(getenv(envHost), defaultHost),
		Port:        defaultPort,
		MapFile:     orDefault(getenv(envMap), defaultMapFile),
		SocketDir:   orDefault(getenv(envSocketDir), defaultSocketDir),
		PokeTimeout: defaultPokeTimeout,
		Secret:      getenv(envSecret),
	}
	if err := validateSecret(cfg.Secret); err != nil {
		return Config{}, err
	}
	if raw := getenv(envPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 0 || port > maxPort {
			return Config{}, fmt.Errorf("%s must be a port number 0-%d", envPort, maxPort)
		}
		cfg.Port = port
	}
	// A zero or negative window would expire every poke before it could converge.
	if raw := getenv(envPokeTimeout); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive Go duration like 5m", envPokeTimeout)
		}
		cfg.PokeTimeout = timeout
	}
	return cfg, nil
}

// validateSecret never echoes the value it rejects.
func validateSecret(secret string) error {
	if len(secret) < minSecretLen {
		return fmt.Errorf("%s missing or shorter than %d characters", envSecret, minSecretLen)
	}
	return nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
