// Package config loads and validates service configuration from the environment.
// Every problem is collected and reported together so a bad deploy is fixed in one pass.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var prefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)

// Roles lists the subcommands that run a long-lived service.
var Roles = []string{"api", "ingest", "collector", "processor", "orchestrator", "worker", "scheduler"}

var defaultAddr = map[string]string{
	"api":          ":8080",
	"ingest":       ":8081",
	"processor":    ":8082",
	"collector":    ":8083",
	"orchestrator": ":8084",
	"worker":       ":8085",
	"scheduler":    ":8086",
}

// Config is the validated process configuration.
// Settings for later steps (master key, OIDC, blob store) are added by the step that needs them.
type Config struct {
	OSURLs     []string
	OSUser     string
	OSPassword string
	NATSURLs   []string
	Partitions int
	LogLevel   slog.Level
	HTTPAddr   string // empty means the role default

	// Step 2: index management (used by `soar migrate`; other roles only read IndexPrefix).
	IndexPrefix   string // index and alias prefix, default "soar"
	StateReplicas int    // replicas for state indices: 0 on one dev node, 1 in production
	TSReplicas    int    // replicas for time-series indices
	RetentionFile string // retention.yaml (spec section 5.2)
}

// ValidationError lists every invalid setting.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// FromEnv loads configuration from the process environment.
func FromEnv() (Config, error) { return Load(os.LookupEnv) }

// Load reads configuration through lookup (os.LookupEnv in production).
func Load(lookup func(string) (string, bool)) (Config, error) {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}

	c := Config{Partitions: 8, LogLevel: slog.LevelInfo, IndexPrefix: "soar", RetentionFile: "/etc/soar/retention.yaml"}

	c.OSURLs = splitList(get("SOAR_OS_URLS"))
	if len(c.OSURLs) == 0 {
		bad("SOAR_OS_URLS is required (comma-separated http(s) URLs)")
	}
	for _, u := range c.OSURLs {
		if err := checkURL(u, "http", "https"); err != nil {
			bad("SOAR_OS_URLS: %v", err)
		}
	}
	c.OSUser = get("SOAR_OS_USER")
	c.OSPassword = get("SOAR_OS_PASSWORD")
	if c.OSPassword != "" && c.OSUser == "" {
		bad("SOAR_OS_PASSWORD is set but SOAR_OS_USER is empty")
	}

	c.NATSURLs = splitList(get("SOAR_NATS_URL"))
	if len(c.NATSURLs) == 0 {
		bad("SOAR_NATS_URL is required (for example nats://nats:4222)")
	}
	for _, u := range c.NATSURLs {
		if err := checkURL(u, "nats", "tls"); err != nil {
			bad("SOAR_NATS_URL: %v", err)
		}
	}

	if v := get("SOAR_PARTITIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 256 {
			bad("SOAR_PARTITIONS must be an integer from 1 to 256, got %q", v)
		} else {
			c.Partitions = n
		}
	}

	if v := get("SOAR_LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug":
			c.LogLevel = slog.LevelDebug
		case "info":
			c.LogLevel = slog.LevelInfo
		case "warn", "warning":
			c.LogLevel = slog.LevelWarn
		case "error":
			c.LogLevel = slog.LevelError
		default:
			bad("SOAR_LOG_LEVEL must be debug, info, warn or error, got %q", v)
		}
	}

	if v := get("SOAR_HTTP_ADDR"); v != "" {
		_, port, err := net.SplitHostPort(v)
		p, perr := strconv.Atoi(port)
		if err != nil || perr != nil || p < 1 || p > 65535 {
			bad("SOAR_HTTP_ADDR must look like host:port or :port, got %q", v)
		} else {
			c.HTTPAddr = v
		}
	}

	if v := get("SOAR_INDEX_PREFIX"); v != "" {
		if !prefixRe.MatchString(v) {
			bad("SOAR_INDEX_PREFIX must be lowercase letters, digits, - or _ (max 31 chars), got %q", v)
		} else {
			c.IndexPrefix = v
		}
	}
	for _, r := range []struct {
		key string
		dst *int
	}{{"SOAR_OS_STATE_REPLICAS", &c.StateReplicas}, {"SOAR_OS_TS_REPLICAS", &c.TSReplicas}} {
		if v := get(r.key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 5 {
				bad("%s must be an integer from 0 to 5, got %q", r.key, v)
			} else {
				*r.dst = n
			}
		}
	}
	if v := get("SOAR_RETENTION_FILE"); v != "" {
		c.RetentionFile = v
	}

	if len(problems) > 0 {
		return c, &ValidationError{Problems: problems}
	}
	return c, nil
}

// AddrFor returns the HTTP listen address for a role.
func (c Config) AddrFor(role string) string {
	if c.HTTPAddr != "" {
		return c.HTTPAddr
	}
	if a, ok := defaultAddr[role]; ok {
		return a
	}
	return ":8080"
}

// LogValue implements slog.LogValuer. Secrets never reach the logs.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("os_urls", redactAll(c.OSURLs)),
		slog.Bool("os_auth", c.OSUser != ""),
		slog.Any("nats_urls", redactAll(c.NATSURLs)),
		slog.Int("partitions", c.Partitions),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("index_prefix", c.IndexPrefix),
		slog.Int("state_replicas", c.StateReplicas),
		slog.Int("ts_replicas", c.TSReplicas),
		slog.String("retention_file", c.RetentionFile),
	)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a valid URL", Redact(raw))
	}
	for _, s := range schemes {
		if u.Scheme == s {
			return nil
		}
	}
	return fmt.Errorf("%q must use one of: %s", Redact(raw), strings.Join(schemes, ", "))
}

// Redact hides any password embedded in a URL.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	return u.Redacted()
}

func redactAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = Redact(s)
	}
	return out
}
