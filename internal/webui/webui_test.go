package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var bundle = fstest.MapFS{
	"index.html":         {Data: []byte(`<!doctype html><div id="root"></div>`)},
	"assets/app-abc.js":  {Data: []byte(`console.log(1)`)},
	"assets/app-abc.css": {Data: []byte(`body{}`)},
	"favicon.svg":        {Data: []byte(`<svg/>`)},
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestServesBundleWithSPAFallbackAndCaching(t *testing.T) {
	h := newHandler(Config{APIBase: "/api/v1"}, bundle)
	if !h.Embedded() {
		t.Fatal("bundle with index.html must count as embedded")
	}
	for _, tc := range []struct {
		path, wantBody, wantCache string
		wantStatus                int
	}{
		{"/", `id="root"`, "no-cache", 200},
		{"/alerts/alt_123", `id="root"`, "no-cache", 200}, // client-side route
		{"/cases/cas_1/timeline", `id="root"`, "no-cache", 200},
		{"/assets/app-abc.js", "console.log", "public, max-age=31536000, immutable", 200},
		{"/favicon.svg", "<svg/>", "no-cache", 200},
		{"/assets/gone-123.js", "", "", 404}, // stale tab: never HTML for a script
		{"/robots.txt", "", "", 404},
		{"/api/v2/whatever", "", "", 404}, // API paths belong to the API mux
	} {
		rec := get(t, h, "GET", tc.path)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s: status %d, want %d", tc.path, rec.Code, tc.wantStatus)
			continue
		}
		if tc.wantStatus != 200 {
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: body %q lacks %q", tc.path, rec.Body.String(), tc.wantBody)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
			t.Errorf("%s: Cache-Control %q, want %q", tc.path, got, tc.wantCache)
		}
	}
	if rec := get(t, h, "POST", "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", rec.Code)
	}
	if rec := get(t, h, "HEAD", "/"); rec.Code != 200 {
		t.Errorf("HEAD /: %d", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHandler(Config{OIDCAuthority: "https://sso.example.com:8443/realms/soar", OIDCClientID: "soar-web"}, bundle)
	rec := get(t, h, "GET", "/")
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'",
		"connect-src 'self' https://sso.example.com:8443", "frame-src https://sso.example.com:8443",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe") {
		t.Errorf("scripts must never be allowed inline: %q", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	noOIDC := newHandler(Config{}, bundle)
	if csp := get(t, noOIDC, "GET", "/").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'none'") || !strings.Contains(csp, "connect-src 'self';") {
		t.Errorf("without OIDC nothing but this origin: %q", csp)
	}
}

func TestConfigJSON(t *testing.T) {
	h := newHandler(Config{APIBase: "/api/v1", OIDCAuthority: "http://localhost:8180/realms/soar", OIDCClientID: "soar-web"}, nil)
	rec := get(t, h, "GET", "/config.json")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("config.json: %d %v", rec.Code, rec.Header())
	}
	var c map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	oidc, _ := c["oidc"].(map[string]any)
	if c["apiBase"] != "/api/v1" || oidc["authority"] != "http://localhost:8180/realms/soar" || oidc["clientId"] != "soar-web" {
		t.Errorf("config: %v", c)
	}
	rec = get(t, newHandler(Config{}, nil), "GET", "/config.json")
	if strings.Contains(rec.Body.String(), "oidc") || !strings.Contains(rec.Body.String(), `"apiBase":"/api/v1"`) {
		t.Errorf("without an authority the UI offers API keys only: %s", rec.Body.String())
	}
}

func TestPlaceholderWithoutBundle(t *testing.T) {
	for _, files := range []fstest.MapFS{nil, {"assets/x.js": {Data: []byte("x")}}} { // none, or no index.html
		var h *Handler
		if files == nil {
			h = newHandler(Config{}, nil)
		} else {
			h = newHandler(Config{}, files)
		}
		if h.Embedded() {
			t.Fatal("no index.html: not embedded")
		}
		rec := get(t, h, "GET", "/alerts")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "make build") || rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("placeholder: %d %q", rec.Code, rec.Body.String())
		}
	}
}
