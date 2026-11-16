// Package webui serves the React console (web/) from the soar binary: the api role
// mounts it at / next to the REST API, so one process and one origin serve both.
//
// The bundle is embedded only in builds with the "ui" tag (make build runs the Vite
// build into internal/webui/dist first). Other builds, such as a plain go build or
// go test, serve a short page that says how to build the UI, so Go work never needs Node.
package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Config is what the browser needs at start-up, served as /config.json.
type Config struct {
	APIBase       string // path prefix of the REST API, normally /api/v1
	OIDCAuthority string // empty: no OIDC sign-in (API keys only)
	OIDCClientID  string
}

// Handler serves the UI. The zero value is not usable; use New.
type Handler struct {
	files     fs.FS // nil: the bundle is not embedded
	index     []byte
	config    []byte
	csp       string
	modTime   time.Time
	fileNames map[string]bool
}

// New returns a handler for the embedded bundle (or the placeholder page without one).
func New(cfg Config) *Handler {
	return newHandler(cfg, embedded())
}

func newHandler(cfg Config, files fs.FS) *Handler {
	h := &Handler{files: files, modTime: time.Now(), fileNames: map[string]bool{}}
	if files != nil {
		if b, err := fs.ReadFile(files, "index.html"); err == nil {
			h.index = b
		} else {
			h.files = nil
		}
	}
	if h.files != nil {
		_ = fs.WalkDir(h.files, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				h.fileNames["/"+p] = true
			}
			return nil
		})
	}
	h.config, _ = json.Marshal(clientConfig(cfg))
	h.csp = contentSecurityPolicy(cfg.OIDCAuthority)
	return h
}

// Embedded reports whether this binary carries the UI bundle.
func (h *Handler) Embedded() bool { return h.files != nil }

type clientOIDC struct {
	Authority string `json:"authority"`
	ClientID  string `json:"clientId"`
}

type clientConfigJSON struct {
	APIBase string      `json:"apiBase"`
	OIDC    *clientOIDC `json:"oidc,omitempty"`
}

func clientConfig(cfg Config) clientConfigJSON {
	out := clientConfigJSON{APIBase: cfg.APIBase}
	if out.APIBase == "" {
		out.APIBase = "/api/v1"
	}
	if cfg.OIDCAuthority != "" {
		out.OIDC = &clientOIDC{Authority: cfg.OIDCAuthority, ClientID: cfg.OIDCClientID}
	}
	return out
}

// contentSecurityPolicy allows only this origin, plus the OIDC provider for token calls
// and the silent-renew iframe. Styles allow inline because Monaco and the toast library
// inject <style> elements at run time; scripts never do.
func contentSecurityPolicy(authority string) string {
	oidc := ""
	if u, err := url.Parse(authority); err == nil && u.Scheme != "" && u.Host != "" {
		oidc = u.Scheme + "://" + u.Host
	}
	frame := "'none'"
	connect := "'self'"
	if oidc != "" {
		frame = oidc
		connect += " " + oidc
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"worker-src 'self' blob:",
		"connect-src " + connect,
		"frame-src " + frame,
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; ")
}

func (h *Handler) securityHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", h.csp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
}

// ServeHTTP serves /config.json, the bundle's files, and index.html for every other
// GET path (client-side routes). It never answers /api paths: the API mux owns them.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		http.NotFound(w, r)
		return
	}
	h.securityHeaders(w)
	switch {
	case p == "/config.json":
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(h.config)
	case h.files == nil:
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, placeholder)
	case p != "/index.html" && p != "/" && h.fileNames[p]:
		h.serveFile(w, r, p)
	case strings.HasPrefix(p, "/assets/") || path.Ext(p) != "" && !strings.HasSuffix(p, ".html"):
		// a missing asset is a real 404, not the app: a stale tab must not get HTML as JavaScript
		http.NotFound(w, r)
	default:
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", h.modTime, bytes.NewReader(h.index))
	}
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, p string) {
	f, err := h.files.Open(strings.TrimPrefix(p, "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(p, "/assets/") { // content-hashed names never change
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, path.Base(p), h.modTime, rs)
}

const placeholder = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>SOAR</title>
<style>
:root{color-scheme:dark light}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d10;color:#e6e8eb;font:15px/1.6 system-ui,sans-serif}
main{max-width:34rem;padding:2rem}
h1{font-size:1.25rem;margin:0 0 .5rem}
code{background:#1b1f24;padding:.15rem .4rem;border-radius:4px;font-size:.9em}
p{color:#a3aab3}
@media (prefers-color-scheme:light){body{background:#f7f8fa;color:#111418}code{background:#e9ecf0}p{color:#4b5560}}
</style></head>
<body><main>
<h1>The SOAR web UI is not built into this binary</h1>
<p>The API is running at <code>/api/v1</code>. To get the console, build the binary with the UI bundle:</p>
<p><code>make build</code> &nbsp;then run <code>bin/soar api</code></p>
<p>For UI development with hot reload, run <code>make web-dev</code> against this API.</p>
</main></body></html>
`
