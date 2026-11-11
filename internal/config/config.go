// Package config loads and validates service configuration from the environment.
// Every problem is collected and reported together so a bad deploy is fixed in one pass.
package config

import (
	"crypto/tls"
	"encoding/base64"
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

	// Step 5: queue and processing.
	EventsMaxBytes   int64 // EVENTS stream size cap; a full stream makes ingest answer 503
	NATSReplicas     int   // stream replicas: 1 in dev, 3 in production
	ProcessorWorkers int   // concurrent fetch loops per processor

	// Step 7: actions, runner, object storage.
	RunnerAddr        string      // Python runner gRPC address (host:port); empty disables python actions
	RunnerTLS         *tls.Config // TLS (mTLS with a client certificate) to the runner; nil: plaintext
	RunnerInsecure    bool        // plaintext to a runner that is not on this host (private network only)
	BlobEndpoint      string      // S3-compatible endpoint; empty uses BlobDir (or no object storage)
	BlobAccessKey     string      // secret
	BlobSecretKey     string      // secret
	BlobRegion        string      // default us-east-1
	BlobSSE           bool        // request server-side encryption
	BlobDir           string      // filesystem object store for single-node development
	WorkerPriorities  []string    // priorities this worker serves (default all)
	WorkerClasses     []string    // action classes this worker serves (default all)
	WorkerConcurrency int         // steps in flight for the normal priority (critical gets half, bulk a quarter)
	HTTPAllowLoopback bool        // let http.request reach loopback addresses (tests, local integrations)
	IntegrationsDir   string      // load integration packages from disk instead of the built-in ones
	PolicyFile        string      // action policy rules (spec 20.1); missing file = defaults

	// Step 9: security.
	MasterKeyID   string // id of the secret-encryption master key (rotation)
	MasterKey     []byte // 32 bytes, from base64 SOAR_MASTER_KEY
	OldMasterKeys map[string][]byte
	OIDCIssuer    string
	OIDCAudience  string
	OIDCJWKSURL   string // optional; discovered from the issuer when empty
	AuthDisabled  bool   // development only: every request acts as an admin
	BootstrapKey  string // optional first admin API key (hashed on first start)
	CORSOrigins   []string
	OSTLS         *tls.Config // OpenSearch client TLS (nil: system roots)
	NATSTLS       *tls.Config // NATS client TLS (nil: plain, or system roots for tls:// URLs)
	ServerTLS     *tls.Config // HTTPS for the role's own port (nil: plain HTTP)
	ClientCertReq bool        // ServerTLS requires client certificates (mTLS)
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

	c := Config{
		Partitions: 8, LogLevel: slog.LevelInfo, IndexPrefix: "soar", RetentionFile: "/etc/soar/retention.yaml",
		EventsMaxBytes: 20 << 30, NATSReplicas: 1, ProcessorWorkers: 2,
	}

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
	if v := get("SOAR_EVENTS_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1<<20 {
			bad("SOAR_EVENTS_MAX_BYTES must be a number of bytes of at least 1048576, got %q", v)
		} else {
			c.EventsMaxBytes = n
		}
	}
	for _, r := range []struct {
		key      string
		dst      *int
		min, max int
	}{{"SOAR_NATS_REPLICAS", &c.NATSReplicas, 1, 5}, {"SOAR_PROCESSOR_WORKERS", &c.ProcessorWorkers, 1, 64}} {
		if v := get(r.key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < r.min || n > r.max {
				bad("%s must be an integer from %d to %d, got %q", r.key, r.min, r.max, v)
			} else {
				*r.dst = n
			}
		}
	}

	// Step 7
	c.RunnerAddr = get("SOAR_RUNNER_ADDR")
	c.RunnerInsecure = boolEnv(get("SOAR_RUNNER_INSECURE"), false, "SOAR_RUNNER_INSECURE", bad)
	if t, err := clientTLS(get("SOAR_RUNNER_CA_FILE"), get("SOAR_RUNNER_CERT_FILE"), get("SOAR_RUNNER_KEY_FILE"), false, "SOAR_RUNNER"); err != nil {
		bad("%v", err)
	} else {
		c.RunnerTLS = t
	}
	if c.RunnerAddr != "" {
		host, _, err := net.SplitHostPort(c.RunnerAddr)
		switch {
		case err != nil:
			bad("SOAR_RUNNER_ADDR must look like host:port, got %q", c.RunnerAddr)
		case c.RunnerTLS == nil && !c.RunnerInsecure && !isLoopback(host):
			// the worker sends decrypted connection secrets to the runner
			bad("SOAR_RUNNER_ADDR %s is not on this host: set SOAR_RUNNER_CA_FILE (plus SOAR_RUNNER_CERT_FILE/SOAR_RUNNER_KEY_FILE for mTLS), "+
				"or SOAR_RUNNER_INSECURE=true if the runner network is private (the secrets cross it in plain text)", c.RunnerAddr)
		}
	}
	c.BlobEndpoint = get("SOAR_BLOB_ENDPOINT")
	c.BlobAccessKey = get("SOAR_BLOB_ACCESS_KEY")
	c.BlobSecretKey = get("SOAR_BLOB_SECRET_KEY")
	c.BlobRegion = get("SOAR_BLOB_REGION")
	c.BlobDir = get("SOAR_BLOB_DIR")
	c.BlobSSE = boolEnv(get("SOAR_BLOB_SSE"), false, "SOAR_BLOB_SSE", bad)
	if c.BlobEndpoint != "" {
		if err := checkURL(c.BlobEndpoint, "http", "https"); err != nil {
			bad("SOAR_BLOB_ENDPOINT: %v", err)
		}
		if c.BlobAccessKey == "" || c.BlobSecretKey == "" {
			bad("SOAR_BLOB_ACCESS_KEY and SOAR_BLOB_SECRET_KEY are required with SOAR_BLOB_ENDPOINT")
		}
	}
	c.WorkerPriorities = splitList(get("SOAR_WORKER_PRIORITIES"))
	for _, p := range c.WorkerPriorities {
		if p != "critical" && p != "normal" && p != "bulk" {
			bad("SOAR_WORKER_PRIORITIES: %q is not critical, normal or bulk", p)
		}
	}
	c.WorkerClasses = splitList(get("SOAR_WORKER_CLASSES"))
	for _, p := range c.WorkerClasses {
		if p != "builtin" && p != "python" {
			bad("SOAR_WORKER_CLASSES: %q is not builtin or python", p)
		}
	}
	c.WorkerConcurrency = 16
	if v := get("SOAR_WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1024 {
			bad("SOAR_WORKER_CONCURRENCY must be an integer from 1 to 1024, got %q", v)
		} else {
			c.WorkerConcurrency = n
		}
	}
	c.HTTPAllowLoopback = boolEnv(get("SOAR_HTTP_ALLOW_LOOPBACK"), false, "SOAR_HTTP_ALLOW_LOOPBACK", bad)
	c.IntegrationsDir = get("SOAR_INTEGRATIONS_DIR")
	c.PolicyFile = "/etc/soar/policy.yaml"
	if v := get("SOAR_POLICY_FILE"); v != "" {
		c.PolicyFile = v
	}

	// Step 9
	c.MasterKeyID = get("SOAR_MASTER_KEY_ID")
	if v := get("SOAR_MASTER_KEY"); v != "" {
		k, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(k) != 32 {
			bad("SOAR_MASTER_KEY must be 32 random bytes in base64 (openssl rand -base64 32)")
		} else {
			c.MasterKey = k
		}
		if c.MasterKeyID == "" {
			c.MasterKeyID = "k1"
		}
	}
	c.OldMasterKeys = map[string][]byte{}
	for _, kv := range splitList(get("SOAR_OLD_MASTER_KEYS")) { // id:base64,id:base64 (rotation)
		id, val, ok := strings.Cut(kv, ":")
		k, err := base64.StdEncoding.DecodeString(val)
		if !ok || id == "" || err != nil || len(k) != 32 {
			bad("SOAR_OLD_MASTER_KEYS entries must look like id:<32 bytes base64>")
			continue
		}
		c.OldMasterKeys[id] = k
	}
	c.OIDCIssuer = get("SOAR_OIDC_ISSUER")
	c.OIDCAudience = get("SOAR_OIDC_AUDIENCE")
	c.OIDCJWKSURL = get("SOAR_OIDC_JWKS_URL")
	if c.OIDCIssuer != "" {
		if err := checkURL(c.OIDCIssuer, "http", "https"); err != nil {
			bad("SOAR_OIDC_ISSUER: %v", err)
		}
		if c.OIDCAudience == "" {
			bad("SOAR_OIDC_AUDIENCE is required with SOAR_OIDC_ISSUER")
		}
	}
	c.AuthDisabled = boolEnv(get("SOAR_AUTH_DISABLED"), false, "SOAR_AUTH_DISABLED", bad)
	c.BootstrapKey = get("SOAR_BOOTSTRAP_API_KEY")
	if c.BootstrapKey != "" && (len(c.BootstrapKey) < 37 || !strings.HasPrefix(c.BootstrapKey, "soar_")) {
		bad("SOAR_BOOTSTRAP_API_KEY must start with soar_ and have at least 32 random characters after it")
	}
	c.CORSOrigins = splitList(get("SOAR_CORS_ORIGINS"))

	var err error
	if c.OSTLS, err = clientTLS(get("SOAR_OS_CA_FILE"), get("SOAR_OS_CERT_FILE"), get("SOAR_OS_KEY_FILE"),
		boolEnv(get("SOAR_OS_INSECURE"), false, "SOAR_OS_INSECURE", bad), "SOAR_OS"); err != nil {
		bad("%v", err)
	}
	if c.NATSTLS, err = clientTLS(get("SOAR_NATS_CA_FILE"), get("SOAR_NATS_CERT_FILE"), get("SOAR_NATS_KEY_FILE"), false, "SOAR_NATS"); err != nil {
		bad("%v", err)
	}
	if c.ServerTLS, err = serverTLS(get("SOAR_TLS_CERT_FILE"), get("SOAR_TLS_KEY_FILE"), get("SOAR_TLS_CLIENT_CA_FILE")); err != nil {
		bad("%v", err)
	}
	c.ClientCertReq = c.ServerTLS != nil && c.ServerTLS.ClientCAs != nil

	if len(problems) > 0 {
		return c, &ValidationError{Problems: problems}
	}
	return c, nil
}

func boolEnv(v string, def bool, name string, bad func(string, ...any)) bool {
	switch strings.ToLower(v) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	bad("%s must be true or false, got %q", name, v)
	return def
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
		slog.Int64("events_max_bytes", c.EventsMaxBytes),
		slog.Int("nats_replicas", c.NATSReplicas),
		slog.Int("processor_workers", c.ProcessorWorkers),
		slog.String("runner_addr", c.RunnerAddr),
		slog.String("runner_tls", tlsMode(c.RunnerTLS)),
		slog.String("blob_endpoint", Redact(c.BlobEndpoint)),
		slog.Bool("blob_credentials", c.BlobAccessKey != ""),
		slog.String("blob_dir", c.BlobDir),
		slog.Any("worker_priorities", c.WorkerPriorities),
		slog.Any("worker_classes", c.WorkerClasses),
		slog.Int("worker_concurrency", c.WorkerConcurrency),
		slog.Bool("master_key", len(c.MasterKey) > 0),
		slog.String("master_key_id", c.MasterKeyID),
		slog.String("oidc_issuer", c.OIDCIssuer),
		slog.Bool("auth_disabled", c.AuthDisabled),
		slog.String("os_tls", tlsMode(c.OSTLS)),
		slog.String("nats_tls", tlsMode(c.NATSTLS)),
		slog.String("http_tls", serverMode(c.ServerTLS, c.ClientCertReq)),
	)
}

func tlsMode(t *tls.Config) string {
	switch {
	case t == nil:
		return "default"
	case t.InsecureSkipVerify:
		return "insecure (verification off)"
	case len(t.Certificates) > 0:
		return "custom CA + client certificate"
	default:
		return "custom CA"
	}
}

func serverMode(t *tls.Config, mtls bool) string {
	switch {
	case t == nil:
		return "off"
	case mtls:
		return "https + client certificates"
	default:
		return "https"
	}
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
