package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// ResolveSecret turns an environment reference ("env:NAME") into its value. It is
// kept for configuration that predates the secret store; SecretStore.Resolve accepts
// both env: references and secret names.
func ResolveSecret(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	if !ok {
		return "", fmt.Errorf("secret reference %q: only env:NAME references can be resolved without the secret store", ref)
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

// Resolve returns the plaintext of a secret name or an env: reference. Values are
// cached briefly in memory to keep decryption off the hot path.
func (s *SecretStore) Resolve(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "env:") {
		return ResolveSecret(ref)
	}
	name := strings.TrimPrefix(ref, "secret:")
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	s.mu.Lock()
	if c, ok := s.cache[name]; ok && s.now().Before(c.until) {
		s.mu.Unlock()
		return c.value, nil
	}
	s.mu.Unlock()
	if s.Keys == nil {
		return "", fmt.Errorf("%w: cannot decrypt secret %q", ErrNoKey, name)
	}
	doc, _, err := s.Repo.Get(ctx, SecretID(name))
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("secret %q does not exist", name)
	}
	if err != nil {
		return "", err
	}
	v, err := s.Keys.Decrypt(doc)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]cached{}
	}
	s.cache[name] = cached{value: v, until: s.now().Add(ttl)}
	s.mu.Unlock()
	return v, nil
}

// ResolveFunc adapts Resolve to the func(ref) signature used by ingestion.
func (s *SecretStore) ResolveFunc() func(string) (string, error) {
	return func(ref string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.Resolve(ctx, ref)
	}
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
