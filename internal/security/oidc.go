package security

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDCVerifier validates JWT access tokens issued by an OIDC provider (Keycloak):
// signature against the provider's JWKS (RS256/384/512, PS256, ES256/384), issuer,
// audience, expiry and not-before. Keys are fetched lazily and refreshed when an
// unknown key id appears (at most once a minute).
type OIDCVerifier struct {
	Issuer   string
	Audience string
	JWKSURL  string // discovered from the issuer when empty
	HTTP     *http.Client
	Leeway   time.Duration
	Now      func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
}

func (v *OIDCVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *OIDCVerifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Verify checks a token and returns its claims.
func (v *OIDCVerifier) Verify(ctx context.Context, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: malformed")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSeg(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("jwt header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("jwt: bad signature encoding")
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifySig(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := decodeSeg(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("jwt claims: %w", err)
	}
	if err := v.checkClaims(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func (v *OIDCVerifier) checkClaims(c map[string]any) error {
	leeway := v.Leeway
	if leeway == 0 {
		leeway = 30 * time.Second
	}
	now := v.now()
	if iss, _ := c["iss"].(string); strings.TrimRight(iss, "/") != strings.TrimRight(v.Issuer, "/") {
		return fmt.Errorf("jwt: issuer %q is not trusted", iss)
	}
	exp, ok := c["exp"].(float64)
	if !ok || now.After(time.Unix(int64(exp), 0).Add(leeway)) {
		return errors.New("jwt: expired")
	}
	if nbf, ok := c["nbf"].(float64); ok && now.Add(leeway).Before(time.Unix(int64(nbf), 0)) {
		return errors.New("jwt: not valid yet")
	}
	if v.Audience != "" && !audienceMatches(c, v.Audience) {
		return errors.New("jwt: wrong audience")
	}
	return nil
}

func audienceMatches(c map[string]any, want string) bool {
	switch a := c["aud"].(type) {
	case string:
		if a == want {
			return true
		}
	case []any:
		for _, x := range a {
			if x == want {
				return true
			}
		}
	}
	azp, _ := c["azp"].(string) // Keycloak puts the client id in azp for its own tokens
	return azp == want
}

func decodeSeg(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h hash.Hash
	var ch crypto.Hash
	switch alg {
	case "RS256", "PS256", "ES256":
		h, ch = sha256.New(), crypto.SHA256
	case "RS384", "ES384":
		h, ch = sha512.New384(), crypto.SHA384
	case "RS512":
		h, ch = sha512.New(), crypto.SHA512
	default:
		return fmt.Errorf("jwt: algorithm %q is not allowed", alg)
	}
	h.Write(signed)
	digest := h.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if strings.HasPrefix(alg, "PS") {
			return rsa.VerifyPSS(k, ch, digest, sig, nil)
		}
		if !strings.HasPrefix(alg, "RS") {
			return errors.New("jwt: algorithm does not match the key type")
		}
		return rsa.VerifyPKCS1v15(k, ch, digest, sig)
	case *ecdsa.PublicKey:
		if !strings.HasPrefix(alg, "ES") {
			return errors.New("jwt: algorithm does not match the key type")
		}
		n := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return errors.New("jwt: bad ecdsa signature length")
		}
		r, s := new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("jwt: bad signature")
		}
		return nil
	}
	return errors.New("jwt: unsupported key type")
}

func (v *OIDCVerifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if time.Since(v.fetchedAt) < time.Minute && v.keys != nil {
		return nil, fmt.Errorf("jwt: unknown key id %q", kid)
	}
	keys, err := v.fetch(ctx)
	v.fetchedAt = time.Now()
	if err != nil {
		return nil, err
	}
	v.keys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("jwt: unknown key id %q", kid)
}

func (v *OIDCVerifier) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (v *OIDCVerifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	url := v.JWKSURL
	if url == "" {
		var disc struct {
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, strings.TrimRight(v.Issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
			return nil, fmt.Errorf("oidc discovery: %w", err)
		}
		url = disc.JWKSURI
	}
	var set struct {
		Keys []JWK `json:"keys"`
	}
	if err := v.getJSON(ctx, url, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if pk, err := k.PublicKey(); err == nil {
			out[k.Kid] = pk
		}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable signing keys")
	}
	return out, nil
}

// JWK is one JSON Web Key.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// PublicKey decodes the key.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	dec := base64.RawURLEncoding.DecodeString
	switch k.Kty {
	case "RSA":
		n, err1 := dec(k.N)
		e, err2 := dec(k.E)
		if err1 != nil || err2 != nil {
			return nil, errors.New("jwk: bad RSA key")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return nil, fmt.Errorf("jwk: unsupported curve %q", k.Crv)
		}
		x, err1 := dec(k.X)
		y, err2 := dec(k.Y)
		size := (curve.Params().BitSize + 7) / 8
		if err1 != nil || err2 != nil || len(x) > size || len(y) > size {
			return nil, errors.New("jwk: bad EC key")
		}
		// 0x04 || X || Y, each coordinate left-padded to the curve size; the parser
		// rejects points that are not on the curve (invalid-curve keys)
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):1+size], x)
		copy(point[1+2*size-len(y):], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return nil, fmt.Errorf("jwk: bad EC key: %w", err)
		}
		return pub, nil
	}
	return nil, fmt.Errorf("jwk: unsupported key type %q", k.Kty)
}
