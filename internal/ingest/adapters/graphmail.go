package adapters

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // x5t is defined as the SHA-1 thumbprint of the certificate
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

func init() { Register("graph_mail", NewGraphMailPoller) }

// GraphMailPoller reads one mailbox folder through Microsoft Graph (spec 10.1, Outlook
// source): the shared phishing-report mailbox. It pages by receivedDateTime with the
// collector's overlap; the processor's duplicate guard (external_id = message id)
// drops repeats. Phase 2 (change notifications) and delta links can be added later
// without changing sources or mappings.
type GraphMailPoller struct {
	mailbox, folder string
	top             int
	graph           string
	tok             *MSToken
	client          *http.Client
}

// NewGraphMailPoller builds the graph_mail adapter. Source config: mailbox, folder
// (well-known name like inbox, or an id), page_size. Connection (connector outlook):
// config tenant_id, client_id, optional graph_url/authority_url; secret_refs
// certificate_pem + private_key_pem (preferred) or client_secret.
func NewGraphMailPoller(src domain.Source, conn domain.Connection, resolve func(string) (string, error)) (Poller, error) {
	mailbox, _ := src.Config["mailbox"].(string)
	if mailbox == "" {
		return nil, fmt.Errorf("source %s: config.mailbox is required", src.Name)
	}
	folder, _ := src.Config["folder"].(string)
	if folder == "" {
		folder = "inbox"
	}
	tok, err := MSTokenFromConnection(conn, resolve)
	if err != nil {
		return nil, err
	}
	graph, _ := conn.Config["graph_url"].(string)
	if graph == "" {
		graph = "https://graph.microsoft.com"
	}
	return &GraphMailPoller{
		mailbox: mailbox, folder: folder, top: asInt(src.Config["page_size"], 50), graph: strings.TrimRight(graph, "/"),
		tok: tok, client: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

var graphSelect = "id,internetMessageId,subject,from,sender,toRecipients,ccRecipients,replyTo,receivedDateTime,bodyPreview,body,hasAttachments,internetMessageHeaders,webLink,conversationId"

// Page implements Poller. Graph has no keyset paging here, so the collector pages by
// receivedDateTime and the duplicate guard drops the boundary repeats.
func (g *GraphMailPoller) Page(ctx context.Context, cq Query) (Page, error) {
	const layout = "2006-01-02T15:04:05Z"
	filter := "receivedDateTime ge " + cq.From.UTC().Format(layout)
	if !cq.To.IsZero() {
		filter += " and receivedDateTime le " + cq.To.UTC().Add(time.Second).Format(layout) // the filter has whole seconds
	}
	order := "receivedDateTime asc"
	if cq.Newest {
		order = "receivedDateTime desc"
	}
	q := url.Values{}
	q.Set("$filter", filter)
	q.Set("$orderby", order)
	q.Set("$top", fmt.Sprint(g.top))
	q.Set("$select", graphSelect)
	u := fmt.Sprintf("%s/v1.0/users/%s/mailFolders/%s/messages?%s", g.graph, url.PathEscape(g.mailbox), url.PathEscape(g.folder), q.Encode())
	token, err := g.tok.Get(ctx)
	if err != nil {
		return Page{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", `outlook.body-content-type="text"`)
	resp, err := g.client.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return Page{}, fmt.Errorf("graph throttled the request (Retry-After %s)", resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return Page{}, fmt.Errorf("graph: HTTP %d: %s", resp.StatusCode, b)
	}
	var body struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Page{}, fmt.Errorf("graph: decode: %w", err)
	}
	p := Page{Items: body.Value, Full: len(body.Value) >= g.top}
	for _, m := range body.Value {
		p.IDs = append(p.IDs, idOf(m["id"]))
		if s, _ := m["receivedDateTime"].(string); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil && t.After(p.Last) {
				p.Last = t
			}
		}
	}
	return p, nil
}

// ---- Microsoft identity platform: OAuth2 client credentials ----

// MSToken caches an app-only access token for Microsoft Graph.
type MSToken struct {
	TokenURL, ClientID, Scope string
	Secret                    string            // client secret, or
	Cert                      *x509.Certificate // certificate credential (preferred)
	Key                       *rsa.PrivateKey
	HTTP                      *http.Client

	mu     sync.Mutex
	value  string
	expiry time.Time
}

// MSTokenFromConnection reads tenant_id/client_id from the connection config and the
// credential from its secrets.
func MSTokenFromConnection(conn domain.Connection, resolve func(string) (string, error)) (*MSToken, error) {
	tenant, _ := conn.Config["tenant_id"].(string)
	clientID, _ := conn.Config["client_id"].(string)
	if tenant == "" || clientID == "" {
		return nil, fmt.Errorf("connection %s: config.tenant_id and config.client_id are required", conn.Name)
	}
	authority, _ := conn.Config["authority_url"].(string)
	if authority == "" {
		authority = "https://login.microsoftonline.com"
	}
	t := &MSToken{
		TokenURL: strings.TrimRight(authority, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token",
		ClientID: clientID, Scope: "https://graph.microsoft.com/.default", HTTP: &http.Client{Timeout: 30 * time.Second},
	}
	get := func(k string) (string, error) {
		ref, _ := conn.SecretRefs[k].(string)
		if ref == "" {
			return "", nil
		}
		return resolve(ref)
	}
	certPEM, err := get("certificate_pem")
	if err != nil {
		return nil, err
	}
	keyPEM, err := get("private_key_pem")
	if err != nil {
		return nil, err
	}
	if certPEM != "" && keyPEM != "" {
		if t.Cert, t.Key, err = parseCertKey(certPEM, keyPEM); err != nil {
			return nil, fmt.Errorf("connection %s: %w", conn.Name, err)
		}
		return t, nil
	}
	if t.Secret, err = get("client_secret"); err != nil {
		return nil, err
	}
	if t.Secret == "" {
		return nil, fmt.Errorf("connection %s: secret_refs needs certificate_pem + private_key_pem, or client_secret", conn.Name)
	}
	return t, nil
}

func parseCertKey(certPEM, keyPEM string) (*x509.Certificate, *rsa.PrivateKey, error) {
	cb, _ := pem.Decode([]byte(certPEM))
	if cb == nil {
		return nil, nil, errors.New("certificate_pem is not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode([]byte(keyPEM))
	if kb == nil {
		return nil, nil, errors.New("private_key_pem is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(kb.Bytes); err == nil {
		return cert, k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("private key must be RSA")
	}
	return cert, rk, nil
}

// Get returns a valid token, refreshing it five minutes before expiry.
func (t *MSToken) Get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && time.Now().Before(t.expiry.Add(-5*time.Minute)) {
		return t.value, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {t.ClientID}, "scope": {t.Scope}}
	if t.Cert != nil {
		a, err := t.assertion()
		if err != nil {
			return "", err
		}
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", a)
	} else {
		form.Set("client_secret", t.Secret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Desc        string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("token request: HTTP %d %s %s", resp.StatusCode, out.Error, firstLine(out.Desc))
	}
	t.value, t.expiry = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return t.value, nil
}

// assertion builds the signed client assertion of the certificate credential.
func (t *MSToken) assertion() (string, error) {
	thumb := sha1.Sum(t.Cert.Raw) //nolint:gosec // required by the x5t header
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	now := time.Now()
	signed := enc(map[string]any{"alg": "RS256", "typ": "JWT", "x5t": base64.RawURLEncoding.EncodeToString(thumb[:])}) + "." +
		enc(map[string]any{
			"aud": t.TokenURL, "iss": t.ClientID, "sub": t.ClientID, "jti": hex.EncodeToString(jti),
			"nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		})
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, t.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
