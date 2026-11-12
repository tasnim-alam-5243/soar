package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Secret uses. A connection or source may name an environment variable ("env:NAME")
// instead of a stored secret, but only one meant for that purpose: the process
// environment also holds the platform's own keys (SOAR_MASTER_KEY, the OpenSearch and
// object-store passwords, the bootstrap API key), and a connection's secrets are sent
// to whatever its configuration points at.
const (
	UseConnection = "connection"  // sent to an external system by an action or the collector
	UseIngestAuth = "ingest_auth" // compared with what a sender presents; never sent
	UseWorkflow   = "workflow"    // secrets.<name> in workflow parameters (stored secrets only)
)

var envPrefixes = map[string][]string{
	UseConnection: {"SOAR_SECRET_", "SOAR_WAZUH_INDEXER_"},
	UseIngestAuth: {"SOAR_SECRET_", "SOAR_SOURCE_TOKEN_", "SOAR_SOURCE_SECRET_"},
}

// EnvRefAllowed reports whether the environment variable name may back a secret of
// the given use. An empty use means any use (the runtime check).
func EnvRefAllowed(name, use string) bool {
	for u, prefixes := range envPrefixes {
		if use != "" && u != use {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) && len(name) > len(p) {
				return true
			}
		}
	}
	return false
}

// EnvRefRule describes the allowed names for error messages.
func EnvRefRule(use string) string {
	return "environment references must name a variable starting with " + strings.Join(envPrefixes[use], " or ")
}

// ResolveSecret turns an environment reference ("env:NAME") into its value. It is
// kept for configuration that predates the secret store; SecretStore.Resolve accepts
// both env: references and secret names. Only variables EnvRefAllowed accepts resolve.
func ResolveSecret(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	if !ok {
		return "", fmt.Errorf("secret reference %q: only env:NAME references can be resolved without the secret store", ref)
	}
	if !EnvRefAllowed(name, "") {
		return "", fmt.Errorf("secret reference %q: %s, %s", ref, EnvRefRule(UseConnection), EnvRefRule(UseIngestAuth))
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("secret reference %q: environment variable %s is not set", ref, name)
	}
	return v, nil
}

// ---- envelope encryption (spec section 13) ----
//
// Each secret has its own random 256-bit data key. The value is encrypted with the
// data key (AES-256-GCM, the secret name as associated data); the data key is
// encrypted ("wrapped") with the master key identified by key_id. Rotating the
// master key re-wraps data keys without touching values.

// ErrNoKey means the master key needed for a secret is not configured.
var ErrNoKey = errors.New("security: master key not configured")

// Keyring holds the active master key and older keys kept for decryption.
type Keyring struct {
	ActiveID string
	Keys     map[string][]byte // id -> 32-byte key
}

// NewKeyring validates keys and returns a keyring.
func NewKeyring(activeID string, active []byte, old map[string][]byte) (*Keyring, error) {
	if len(active) != 32 {
		return nil, fmt.Errorf("%w: SOAR_MASTER_KEY must be 32 bytes", ErrNoKey)
	}
	k := &Keyring{ActiveID: activeID, Keys: map[string][]byte{activeID: active}}
	for id, key := range old {
		if len(key) != 32 {
			return nil, fmt.Errorf("security: old master key %s must be 32 bytes", id)
		}
		if id != activeID {
			k.Keys[id] = key
		}
	}
	return k, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func seal(key, plaintext, aad []byte) (nonce, ct []byte, err error) {
	a, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, a.Seal(nil, nonce, plaintext, aad), nil
}

func open(key, nonce, ct, aad []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return a.Open(nil, nonce, ct, aad)
}

// Encrypt fills the crypto fields of s for value.
func (k *Keyring) Encrypt(s *domain.Secret, value string) error {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	nonce, ct, err := seal(dek, []byte(value), []byte("secret:"+s.Name))
	if err != nil {
		return err
	}
	return k.wrapInto(s, dek, nonce, ct)
}

func (k *Keyring) wrapInto(s *domain.Secret, dek, nonce, ct []byte) error {
	key, ok := k.Keys[k.ActiveID]
	if !ok {
		return ErrNoKey
	}
	wn, wrapped, err := seal(key, dek, []byte("dek:"+s.Name+":"+k.ActiveID))
	if err != nil {
		return err
	}
	s.Ciphertext = base64.StdEncoding.EncodeToString(ct)
	s.Nonce = base64.StdEncoding.EncodeToString(nonce)
	s.WrappedKey = base64.StdEncoding.EncodeToString(append(wn, wrapped...))
	s.KeyID = k.ActiveID
	return nil
}

func (k *Keyring) unwrap(s domain.Secret) ([]byte, error) {
	key, ok := k.Keys[s.KeyID]
	if !ok {
		return nil, fmt.Errorf("%w: secret %q is encrypted with key %q, which is not configured", ErrNoKey, s.Name, s.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(s.WrappedKey)
	if err != nil || len(raw) < 12 {
		return nil, fmt.Errorf("security: secret %q has a corrupt wrapped key", s.Name)
	}
	dek, err := open(key, raw[:12], raw[12:], []byte("dek:"+s.Name+":"+s.KeyID))
	if err != nil {
		return nil, fmt.Errorf("security: cannot unwrap secret %q (wrong master key?)", s.Name)
	}
	return dek, nil
}

// Decrypt returns the plaintext of s.
func (k *Keyring) Decrypt(s domain.Secret) (string, error) {
	dek, err := k.unwrap(s)
	if err != nil {
		return "", err
	}
	nonce, err1 := base64.StdEncoding.DecodeString(s.Nonce)
	ct, err2 := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("security: secret %q is corrupt", s.Name)
	}
	pt, err := open(dek, nonce, ct, []byte("secret:"+s.Name))
	if err != nil {
		return "", fmt.Errorf("security: secret %q failed authentication", s.Name)
	}
	return string(pt), nil
}

// Rewrap re-encrypts the data key of s with the active master key.
func (k *Keyring) Rewrap(s *domain.Secret) error {
	if s.KeyID == k.ActiveID {
		return nil
	}
	dek, err := k.unwrap(*s)
	if err != nil {
		return err
	}
	nonce, _ := base64.StdEncoding.DecodeString(s.Nonce)
	ct, _ := base64.StdEncoding.DecodeString(s.Ciphertext)
	return k.wrapInto(s, dek, nonce, ct)
}

// ---- secret store ----

var secretNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)

// ValidSecretName reports whether a name is acceptable.
func ValidSecretName(n string) bool { return secretNameRe.MatchString(n) }

// SecretID is the document id of a named secret.
func SecretID(name string) string { return domain.DeterministicID("sec", name) }

// SecretStore keeps encrypted secrets in soar-secrets. Only the worker (and the
// collector/ingest for source auth) call Resolve; the API only writes and lists.
type SecretStore struct {
	Repo store.Repo[domain.Secret]
	Keys *Keyring // nil: only env: references resolve
	TTL  time.Duration
	Now  func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	scope string
	value string
	until time.Time
}

func (s *SecretStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Put creates or replaces a secret.
func (s *SecretStore) Put(ctx context.Context, name, value, scope string) (domain.Secret, error) {
	if !ValidSecretName(name) {
		return domain.Secret{}, fmt.Errorf("%w: secret names start with a letter and use letters, digits, _ . - (max 128)", store.ErrInvalid)
	}
	if s.Keys == nil {
		return domain.Secret{}, ErrNoKey
	}
	doc := domain.Secret{Meta: domain.NewMeta("sec"), Name: name, Scope: scope}
	doc.ID = SecretID(name)
	if old, _, err := s.Repo.Get(ctx, doc.ID); err == nil {
		doc.CreatedAt = old.CreatedAt
	}
	if err := s.Keys.Encrypt(&doc, value); err != nil {
		return domain.Secret{}, err
	}
	if _, err := s.Repo.Put(ctx, doc); err != nil {
		return domain.Secret{}, err
	}
	s.forget(name)
	return Metadata(doc), nil
}

// Delete removes a secret.
func (s *SecretStore) Delete(ctx context.Context, name string) error {
	s.forget(name)
	return s.Repo.Delete(ctx, SecretID(name))
}

// Get returns a secret's metadata (never its value).
func (s *SecretStore) Get(ctx context.Context, name string) (domain.Secret, error) {
	doc, _, err := s.Repo.Get(ctx, SecretID(name))
	if err != nil {
		return domain.Secret{}, err
	}
	return Metadata(doc), nil
}

// Metadata strips the encrypted material from a secret document.
func Metadata(d domain.Secret) domain.Secret {
	d.Ciphertext, d.Nonce, d.WrappedKey = "", "", ""
	return d
}

func (s *SecretStore) forget(name string) {
	s.mu.Lock()
	delete(s.cache, name)
	s.mu.Unlock()
}

// Resolve returns the plaintext of an unscoped secret name or an allowed env:
// reference. A scoped secret is refused: whoever needs one must say what it is for
// (ResolveFor), so no code path can skip the scope check by accident.
func (s *SecretStore) Resolve(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "env:") {
		return ResolveSecret(ref)
	}
	name := strings.TrimPrefix(ref, "secret:")
	v, scope, err := s.resolve(ctx, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(scope) != "" {
		return "", fmt.Errorf("secret %q is scoped to %q; it can only be resolved for a named use", name, scope)
	}
	return v, nil
}

// ResolveFor resolves ref for one use (UseConnection, UseIngestAuth, UseWorkflow) by a
// consumer described by attributes it cannot choose freely together: for a connection
// its connector id, "connection:<id>" and "host:<host>" when all its URLs share it (see
// ConnectionConsumers); for ingest auth "ingest" and "source:<name>"; for workflow
// parameters "workflows". An env: reference must fit the use, and a stored secret with a
// scope is released only when one of its scope entries names one of the attributes.
func (s *SecretStore) ResolveFor(ctx context.Context, ref, use string, consumers ...string) (string, error) {
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		if use == UseWorkflow || !EnvRefAllowed(name, use) {
			rule := "workflow parameters can only use stored secrets"
			if use != UseWorkflow {
				rule = EnvRefRule(use)
			}
			return "", fmt.Errorf("secret reference %q is not allowed here: %s", ref, rule)
		}
		return ResolveSecret(ref)
	}
	name := strings.TrimPrefix(ref, "secret:")
	v, scope, err := s.resolve(ctx, name)
	if err != nil {
		return "", err
	}
	if !ScopeAllows(scope, consumers...) {
		return "", fmt.Errorf("secret %q is scoped to %q and may not be used by %s", name, scope, strings.Join(consumers, ", "))
	}
	return v, nil
}

// Resolver returns ResolveFor as a function with its own timeout, for ingestion and
// the collector.
func (s *SecretStore) Resolver() func(ref, use string, consumers ...string) (string, error) {
	return func(ref, use string, consumers ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.ResolveFor(ctx, ref, use, consumers...)
	}
}

// ConnectionConsumers are the attributes a connection is known by when it asks for a
// secret: its connector id (any connection of that kind), "connection:<id>" (its
// immutable id; names are not unique) and "host:<host>" when every URL in its
// configuration is on that one host. Scoping a secret to a host pins it to that
// endpoint: a connection pointing anywhere else, or at several hosts (whichever
// key a connector reads, it could send the secret to another host), cannot receive it.
func ConnectionConsumers(conn domain.Connection) []string {
	out := []string{conn.ConnectorID, "connection:" + conn.ID}
	if h := soleHost(conn.Config); h != "" {
		out = append(out, "host:"+h)
	}
	return out
}

// soleHost returns the one host every endpoint in a connection's configuration names,
// or "" (no host attribute). It fails closed: every value that looks like an endpoint
// (it contains "://", starts with "//", or sits under an endpoint-like key such as
// host, url, server or endpoint, at any depth) must parse strictly to a host, and all
// of them must agree. Anything a connector or an HTTP library could read differently
// (user info, backslashes, spaces, an upper-case or non-http scheme, a bare host
// elsewhere) withholds the attribute, so a host-pinned secret is refused.
//
// A bare host[:port] (Kafka brokers, a "host" key) carries no scheme: the connector
// decides how it travels, so it counts only when the configuration says exactly
// "tls": true at its top level (the key and boolean type connectors read; aliases such
// as ssl or TLS do not count, and set to false they withhold the host). Any value
// shaped like host:port counts as an endpoint whatever its key.
func soleHost(v any) string {
	hosts := map[string]bool{}
	ok := true
	bare, tlsOn := false, false
	var walk func(key string, x any, depth int)
	walk = func(key string, x any, depth int) {
		if weakensTransport(key, x) {
			ok = false // a pinned host is only worth something over verified TLS
			return
		}
		switch t := x.(type) {
		case bool:
			// only the exact key connectors read turns TLS on; an alias (ssl, TLS,
			// use_tls, ...) a connector may ignore leaves the bare host unproven
			if depth == 1 && t && key == "tls" {
				tlsOn = true
			}
		case string:
			urlish := strings.Contains(t, "://") || strings.HasPrefix(t, "//") || strings.Contains(t, `:\`)
			if !urlish && !endpointKey(key) && !hostPortRe.MatchString(t) {
				return
			}
			h, good := strictHost(t, urlish)
			if !good {
				ok = false
				return
			}
			bare = bare || !urlish
			hosts[h] = true
		case map[string]any:
			for k, e := range t {
				walk(k, e, depth+1)
			}
		case []any:
			for _, e := range t {
				walk(key, e, depth)
			}
		}
	}
	walk("", v, 0)
	if !ok || len(hosts) != 1 || (bare && !tlsOn) {
		return ""
	}
	for h := range hosts {
		return h
	}
	return ""
}

// endpointKey reports whether a configuration key names where requests go.
func endpointKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{
		"url", "uri", "host", "endpoint", "server", "domain", "addr", "base",
		"broker", "bootstrap", "seed", "node", "peer", "dsn", "target",
	} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// tlsKey reports whether a key switches transport security on or off.
func tlsKey(key string) bool {
	switch strings.ToLower(key) {
	case "tls", "ssl", "use_tls", "use_ssl", "tls_enabled", "ssl_enabled", "enable_tls", "secure":
		return true
	}
	return false
}

// hostPortRe matches a value shaped like host:port, whatever key it sits under.
var hostPortRe = regexp.MustCompile(`^[^\s/:]+:[0-9]{1,5}$`)

var bareHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)

// strictHost parses an endpoint the strict way: an absolute http(s) URL with a plain
// host (no user info), or, under an endpoint key, a bare host[:port].
func strictHost(s string, urlish bool) (string, bool) {
	for _, r := range s {
		if r <= ' ' || r == '\\' || r == 0x7f || r == '@' {
			return "", false
		}
	}
	if !urlish {
		if !bareHostRe.MatchString(s) {
			return "", false
		}
		h, _, _ := strings.Cut(s, ":")
		return h, true
	}
	if !strings.HasPrefix(s, "https://") {
		return "", false // plain http lets anyone on the path read the secret
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" {
		return "", false
	}
	h := strings.ToLower(u.Hostname()) // DNS names are case-insensitive
	if strings.HasSuffix(h, ".") {
		return "", false
	}
	return h, true
}

// ScopeAllows reports whether a secret's scope (comma-separated entries; empty =
// anywhere) lists one of the consumer attributes.
func ScopeAllows(scope string, consumers ...string) bool {
	if strings.TrimSpace(scope) == "" {
		return true
	}
	for _, s := range strings.Split(scope, ",") {
		s = strings.TrimSpace(s)
		for _, c := range consumers {
			if c != "" && strings.EqualFold(s, c) {
				return true
			}
		}
	}
	return false
}

// ValidScope reports whether scope is a comma-separated list of entries: a connector
// id, "workflows", "ingest", "host:<name>", "connection:<connection id>" or "source:<name>".
func ValidScope(scope string) bool {
	if scope == "" {
		return true
	}
	for _, s := range strings.Split(scope, ",") {
		if !scopeRe.MatchString(strings.TrimSpace(s)) {
			return false
		}
	}
	return true
}

var scopeRe = regexp.MustCompile(`^((host|connection|source):[A-Za-z0-9_.-]{1,200}|[a-z0-9][a-z0-9_.-]{0,99})$`)

func (s *SecretStore) resolve(ctx context.Context, name string) (value, scope string, err error) {
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	s.mu.Lock()
	if c, ok := s.cache[name]; ok && s.now().Before(c.until) {
		s.mu.Unlock()
		return c.value, c.scope, nil
	}
	s.mu.Unlock()
	if s.Keys == nil {
		return "", "", fmt.Errorf("%w: cannot decrypt secret %q", ErrNoKey, name)
	}
	doc, _, err := s.Repo.Get(ctx, SecretID(name))
	if errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("secret %q does not exist", name)
	}
	if err != nil {
		return "", "", err
	}
	v, err := s.Keys.Decrypt(doc)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cached{}
	}
	s.cache[name] = cached{value: v, scope: doc.Scope, until: s.now().Add(ttl)}
	s.mu.Unlock()
	return v, doc.Scope, nil
}

// RotateAll re-wraps every secret not yet under the active master key and returns
// how many changed.
func (s *SecretStore) RotateAll(ctx context.Context) (int, error) {
	if s.Keys == nil {
		return 0, ErrNoKey
	}
	n, cursor := 0, ""
	for {
		page, err := s.Repo.List(ctx, store.ListQuery{Limit: 200, After: cursor})
		if err != nil {
			return n, err
		}
		for _, d := range page.Items {
			if d.KeyID == s.Keys.ActiveID {
				continue
			}
			if _, _, err := s.Repo.Update(ctx, d.ID, func(x *domain.Secret) error { return s.Keys.Rewrap(x) }); err != nil {
				return n, err
			}
			n++
		}
		if page.NextCursor == "" {
			return n, nil
		}
		cursor = page.NextCursor
	}
}

// FromConfig builds the secret store from the configuration. Without SOAR_MASTER_KEY
// only env: references resolve and secrets cannot be written.
func FromConfig(masterKeyID string, masterKey []byte, old map[string][]byte, repo store.Repo[domain.Secret], warn func(string)) (*SecretStore, error) {
	ss := &SecretStore{Repo: repo}
	if len(masterKey) == 0 {
		if warn != nil {
			warn("SOAR_MASTER_KEY is not set: secrets cannot be stored or decrypted (env: references still work)")
		}
		return ss, nil
	}
	k, err := NewKeyring(masterKeyID, masterKey, old)
	if err != nil {
		return nil, err
	}
	ss.Keys = k
	return ss, nil
}

// weakensTransport reports whether a configuration entry changes who the connection
// trusts or how it travels: verification switched off, a custom CA, or a proxy. Any of
// them lets a pinned host's traffic be read on the way, so the host attribute is
// withheld (fail closed; client certificates do not weaken anything and are allowed).
func weakensTransport(key string, v any) bool {
	k := strings.ToLower(key)
	truthy := func() bool {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			s := strings.ToLower(strings.TrimSpace(t))
			return s != "" && s != "false" && s != "0" && s != "no"
		case float64:
			return t != 0
		case int:
			return t != 0
		}
		return v != nil
	}
	// explicitTrue is the only value that leaves verification or TLS on for certain: a
	// connector may read "off", null, 0 or [] as false, so all of them weaken (fail closed)
	explicitTrue := func() bool {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return strings.ToLower(strings.TrimSpace(t)) == "true"
		}
		return false
	}
	switch {
	case tlsKey(key):
		return !explicitTrue() // tls: false sends everything in clear text
	case strings.Contains(k, "insecure"), strings.Contains(k, "skip_verify"), strings.Contains(k, "skipverify"):
		return truthy()
	case k == "verify" || strings.HasSuffix(k, "_verify") || strings.HasPrefix(k, "verify_"):
		return !explicitTrue() // verify_tls: false (or anything a connector could read as false)
	case strings.Contains(k, "proxy"), k == "ca", strings.HasPrefix(k, "ca_"), strings.HasSuffix(k, "_ca"),
		strings.Contains(k, "cacert"), strings.Contains(k, "ca_cert"), strings.Contains(k, "ca_file"),
		strings.Contains(k, "cafile"), strings.Contains(k, "ca_bundle"), strings.Contains(k, "root_ca"):
		return truthy()
	}
	return false
}
