package ingest

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is offered only because some senders cannot do better
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Authenticator checks an inbound push request (spec 9.5).
type Authenticator interface {
	Authenticate(r *http.Request, body []byte) error
}

var errUnauthorized = errors.New("unauthorized")

type allowIPs []*net.IPNet

func (a allowIPs) ok(r *http.Request) bool {
	if len(a) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	for _, n := range a {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

type tokenAuth struct {
	token []byte
	ips   allowIPs
}

func (t tokenAuth) Authenticate(r *http.Request, _ []byte) error {
	if !t.ips.ok(r) {
		return errUnauthorized
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" {
		got = r.Header.Get("X-Soar-Token")
	}
	if subtle.ConstantTimeCompare([]byte(got), t.token) != 1 {
		return errUnauthorized
	}
	return nil
}

type hmacAuth struct {
	secret []byte
	header string
	newH   func() hash.Hash
	ips    allowIPs
}

func (h hmacAuth) Authenticate(r *http.Request, body []byte) error {
	if !h.ips.ok(r) {
		return errUnauthorized
	}
	sig := r.Header.Get(h.header)
	if i := strings.IndexByte(sig, '='); i >= 0 { // "sha256=<hex>"
		sig = sig[i+1:]
	}
	got, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return errUnauthorized
	}
	m := hmac.New(h.newH, h.secret)
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return errUnauthorized
	}
	return nil
}

type noAuth struct{ ips allowIPs }

func (n noAuth) Authenticate(r *http.Request, _ []byte) error {
	if !n.ips.ok(r) {
		return errUnauthorized
	}
	return nil
}

// NewAuthenticator builds the authenticator described by a source's auth block.
// Supported: {type: token, token_ref}, {type: hmac, secret_ref, header, algo},
// {type: none}; all accept allow_ips: [CIDR, ...]. Secrets are references
// (env:NAME until Step 9) and are resolved here, never stored in the source.
func NewAuthenticator(cfg map[string]any, resolve func(string) (string, error)) (Authenticator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("auth is required (use {type: none} to accept unauthenticated requests on purpose)")
	}
	var ips allowIPs
	if list, ok := cfg["allow_ips"].([]any); ok {
		for _, x := range list {
			s, _ := x.(string)
			if !strings.Contains(s, "/") {
				if strings.Contains(s, ":") {
					s += "/128"
				} else {
					s += "/32"
				}
			}
			_, n, err := net.ParseCIDR(s)
			if err != nil {
				return nil, fmt.Errorf("auth.allow_ips: %q is not an address or CIDR", s)
			}
			ips = append(ips, n)
		}
	}
	secret := func(key string) ([]byte, error) {
		ref, _ := cfg[key].(string)
		if ref == "" {
			return nil, fmt.Errorf("auth.%s is required", key)
		}
		v, err := resolve(ref)
		if err != nil {
			return nil, err
		}
		return []byte(v), nil
	}
	typ, _ := cfg["type"].(string)
	switch typ {
	case "token":
		tok, err := secret("token_ref")
		if err != nil {
			return nil, err
		}
		return tokenAuth{token: tok, ips: ips}, nil
	case "hmac":
		sec, err := secret("secret_ref")
		if err != nil {
			return nil, err
		}
		hdr, _ := cfg["header"].(string)
		if hdr == "" {
			hdr = "X-Signature"
		}
		algo, _ := cfg["algo"].(string)
		var newH func() hash.Hash
		switch algo {
		case "", "sha256":
			newH = sha256.New
		case "sha512":
			newH = sha512.New
		case "sha1":
			newH = sha1.New //nolint:gosec
		default:
			return nil, fmt.Errorf("auth.algo %q is not supported (sha256, sha512, sha1)", algo)
		}
		return hmacAuth{secret: sec, header: hdr, newH: newH, ips: ips}, nil
	case "none":
		return noAuth{ips: ips}, nil
	}
	return nil, fmt.Errorf("auth.type %q is not supported (token, hmac, none)", typ)
}

// Limiter is a token bucket: rps tokens per second up to burst.
type Limiter struct {
	mu     sync.Mutex
	rps    float64
	burst  float64
	tokens float64
	at     time.Time
	now    func() time.Time
}

// NewLimiter reads {rps, burst}.
func NewLimiter(cfg map[string]any) (*Limiter, error) {
	num := func(k string) float64 {
		switch t := cfg[k].(type) {
		case float64:
			return t
		case int:
			return float64(t)
		}
		return 0
	}
	rps, burst := num("rps"), num("burst")
	if rps <= 0 {
		return nil, fmt.Errorf("rate_limit.rps must be greater than 0")
	}
	if burst < rps {
		burst = rps
	}
	return &Limiter{rps: rps, burst: burst, tokens: burst, now: time.Now, at: time.Now()}, nil
}

// Allow takes n tokens if available. A nil limiter allows everything.
func (l *Limiter) Allow(n int) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.at).Seconds()*l.rps)
	l.at = now
	need := float64(n)
	if need > l.burst {
		need = l.burst // a batch larger than the burst can still pass when the bucket is full
	}
	if l.tokens < need {
		return false
	}
	l.tokens -= need
	return true
}
