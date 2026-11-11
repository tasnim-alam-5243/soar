package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Roles (spec section 13).
const (
	RoleAdmin    = "admin"
	RoleEngineer = "engineer"
	RoleAnalyst  = "analyst"
	RoleViewer   = "viewer"
	RoleService  = "service"
)

// AllRoles lists the roles in order of decreasing privilege.
var AllRoles = []string{RoleAdmin, RoleEngineer, RoleAnalyst, RoleViewer, RoleService}

// rolePerms maps a role to permissions "resource:action"; "*" matches anything.
var rolePerms = map[string][]string{
	RoleAdmin: {"*:*"},
	RoleEngineer: {
		"workflows:*", "executions:*", "approvals:*", "alerts:*", "cases:*", "observables:*",
		"sources:*", "mappings:*", "filters:*", "lists:*", "connections:*", "connectors:read",
		"secrets:write", "secrets:read_metadata", "queue:*", "audit:read",
	},
	RoleAnalyst: {
		"workflows:read", "workflows:run", "executions:read", "executions:run", "executions:cancel",
		"approvals:read", "approvals:decide", "alerts:*", "cases:*", "observables:*",
		"sources:read", "mappings:read", "filters:read", "lists:read", "connections:read", "connectors:read", "queue:read",
	},
	RoleViewer: {
		"workflows:read", "executions:read", "approvals:read", "alerts:read", "cases:read", "observables:read",
		"sources:read", "mappings:read", "filters:read", "lists:read", "connections:read", "connectors:read", "queue:read",
	},
	RoleService: {"alerts:write", "alerts:read", "workflows:run", "executions:read", "executions:run", "cases:read", "cases:write"},
}

// RolePermissions returns the permissions of each role (GET /roles).
func RolePermissions() map[string][]string {
	out := make(map[string][]string, len(rolePerms))
	for r, p := range rolePerms {
		out[r] = append([]string(nil), p...)
	}
	return out
}

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { _, ok := rolePerms[r]; return ok }

// Principal is an authenticated caller.
type Principal struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"` // user | api_key | service
	Roles     []string `json:"roles"`
	ActorType string   `json:"actor_type"` // user | system
}

// Can reports whether the principal holds a permission.
func (p Principal) Can(perm string) bool {
	res, act, _ := strings.Cut(perm, ":")
	for _, r := range p.Roles {
		for _, g := range rolePerms[r] {
			gr, ga, _ := strings.Cut(g, ":")
			if (gr == "*" || gr == res) && (ga == "*" || ga == act) {
				return true
			}
		}
	}
	return false
}

// HasAny reports whether the principal holds one of the roles or is named in names.
func (p Principal) HasAny(names []string) bool {
	for _, n := range names {
		if n == p.Name || n == p.ID {
			return true
		}
		for _, r := range p.Roles {
			if r == n || r == RoleAdmin {
				return true
			}
		}
	}
	return false
}

// ---- API keys (hashed, shown once) ----

const apiKeyPrefix = "soar_"

// NewAPIKey returns a fresh key and its storage hash.
func NewAPIKey() (key, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = apiKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, HashAPIKey(key), nil
}

// HashAPIKey is the stored form of a key (SHA-256; keys have 256 bits of entropy).
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// IsAPIKey reports whether a bearer token looks like an API key rather than a JWT.
func IsAPIKey(token string) bool { return strings.HasPrefix(token, apiKeyPrefix) }

// ErrUnauthenticated means the credentials are missing, invalid or disabled.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator resolves bearer tokens to principals: API keys from soar-principals,
// JWTs from the OIDC provider.
type Authenticator struct {
	Principals store.Repo[domain.Principal]
	OIDC       *OIDCVerifier // nil: only API keys
	RoleClaims []string      // claim paths holding roles; default Keycloak realm and client roles
	Audience   string

	mu    sync.Mutex
	cache map[string]cachedPrincipal
}

type cachedPrincipal struct {
	p     Principal
	until time.Time
}

// Authenticate validates a bearer token.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	if IsAPIKey(token) {
		return a.apiKey(ctx, token)
	}
	if a.OIDC == nil {
		return Principal{}, ErrUnauthenticated
	}
	claims, err := a.OIDC.Verify(ctx, token)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	name, _ := claims["preferred_username"].(string)
	if name == "" {
		name, _ = claims["email"].(string)
	}
	sub, _ := claims["sub"].(string)
	if name == "" {
		name = sub
	}
	p := Principal{ID: "oidc:" + sub, Name: name, Type: "user", ActorType: "user", Roles: a.rolesFrom(claims)}
	if len(p.Roles) == 0 {
		p.Roles = []string{RoleViewer} // authenticated but no SOAR role: read-only
	}
	return p, nil
}

func (a *Authenticator) apiKey(ctx context.Context, token string) (Principal, error) {
	h := HashAPIKey(token)
	a.mu.Lock()
	if c, ok := a.cache[h]; ok && time.Now().Before(c.until) {
		a.mu.Unlock()
		return c.p, nil
	}
	a.mu.Unlock()
	page, err := a.Principals.List(ctx, store.ListQuery{Terms: map[string][]string{"key_hash": {h}}, Limit: 2})
	if err != nil {
		return Principal{}, err
	}
	if len(page.Items) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	d := page.Items[0]
	if d.Disabled || subtle.ConstantTimeCompare([]byte(d.KeyHash), []byte(h)) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	actorType := "user"
	if d.Type == "service" {
		actorType = "system"
	}
	p := Principal{ID: d.ID, Name: d.Name, Type: d.Type, Roles: d.Roles, ActorType: actorType}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cachedPrincipal{}
	}
	a.cache[h] = cachedPrincipal{p: p, until: time.Now().Add(30 * time.Second)} // disabling takes effect within 30 s
	a.mu.Unlock()
	return p, nil
}

// Forget drops cached API-key principals (after a change to principals).
func (a *Authenticator) Forget() {
	a.mu.Lock()
	a.cache = nil
	a.mu.Unlock()
}

func (a *Authenticator) rolesFrom(claims map[string]any) []string {
	paths := a.RoleClaims
	if len(paths) == 0 {
		paths = []string{"realm_access.roles", "roles", "groups"}
		if a.Audience != "" {
			paths = append(paths, "resource_access."+a.Audience+".roles")
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		var cur any = claims
		for _, part := range strings.Split(p, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur = m[part]
		}
		list, _ := cur.([]any)
		for _, x := range list {
			s, _ := x.(string)
			s = strings.TrimPrefix(strings.TrimPrefix(s, "/"), "soar-") // accept "soar-analyst" and group paths
			if ValidRole(s) && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}
