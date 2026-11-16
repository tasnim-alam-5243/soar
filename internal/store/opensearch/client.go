// Package opensearch is the only place that talks to OpenSearch (spec section 3.1).
// Step 1 added a reachability check, Step 2 adds Do for index management; the
// optimistic-concurrency document layer arrives in Step 3.
package opensearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/config"
)

// Client is a minimal OpenSearch HTTP client.
type Client struct {
	urls []string
	user string
	pass string
	http *http.Client // short timeout, health checks
	slow *http.Client // index management can wait for shard allocation
}

// New builds a client for one or more node URLs. TLS options are added in Step 3.
func New(urls []string, user, pass string) *Client {
	return NewTLS(urls, user, pass, nil)
}

// NewTLS is New with a TLS configuration for https URLs (a private CA, a client
// certificate for mTLS); nil uses the system roots.
func NewTLS(urls []string, user, pass string, tc *tls.Config) *Client {
	var rt http.RoundTripper = http.DefaultTransport
	if tc != nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = tc.Clone()
		rt = t
	}
	return &Client{
		urls: urls, user: user, pass: pass,
		http: &http.Client{Timeout: 5 * time.Second, Transport: rt},
		slow: &http.Client{Timeout: 60 * time.Second, Transport: rt},
	}
}

// Open connects with the process configuration (URLs, credentials, TLS).
func Open(cfg config.Config) *Client {
	return NewTLS(cfg.OSURLs, cfg.OSUser, cfg.OSPassword, cfg.OSTLS)
}

// APIError is a non-2xx answer from OpenSearch.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

// HTTPStatus lets store.Unavailable classify the error without importing this package.
func (e *APIError) HTTPStatus() int { return e.Status }

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 600 {
		b = b[:600] + "..."
	}
	return fmt.Sprintf("opensearch %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, b)
}

// Do sends one JSON request (body may be nil) and returns the status and raw
// response. A status of 400 or above is returned as *APIError unless it is
// listed in allow. If a node cannot be reached the next URL is tried; an HTTP
// answer, even an error one, is final.
func (c *Client) Do(ctx context.Context, method, path string, body any, allow ...int) (int, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, fmt.Errorf("opensearch %s %s: encode body: %w", method, path, err)
		}
	}
	return c.DoRaw(ctx, method, path, "application/json", payload, allow...)
}

// DoRaw is Do with a ready-made body and content type (used for NDJSON bulk requests).
func (c *Client) DoRaw(ctx context.Context, method, path, contentType string, payload []byte, allow ...int) (int, []byte, error) {
	var errs []error
	for _, base := range c.urls {
		status, out, err := c.doOne(ctx, base, method, path, contentType, payload)
		if err != nil {
			errs = append(errs, err) // connection-level problem: try the next node
			continue
		}
		if status >= 400 && !containsInt(allow, status) {
			return status, out, &APIError{Status: status, Method: method, Path: path, Body: string(out)}
		}
		return status, out, nil
	}
	return 0, nil, errors.Join(errs...)
}

func (c *Client) doOne(ctx context.Context, base, method, path, contentType string, payload []byte) (int, []byte, error) {
	label := base
	if u, err := url.Parse(base); err == nil {
		label = u.Redacted()
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("opensearch %s: %w", label, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.slow.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("opensearch %s: %w", label, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("opensearch %s: read response: %w", label, err)
	}
	return resp.StatusCode, out, nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Ping succeeds if any node answers and the cluster is not red.
// Yellow is fine: a single-node dev cluster is always yellow when replicas are configured.
func (c *Client) Ping(ctx context.Context) error {
	var errs []error
	for _, base := range c.urls {
		err := c.pingOne(ctx, base)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Client) pingOne(ctx context.Context, base string) error {
	label := base
	if u, err := url.Parse(base); err == nil {
		label = u.Redacted()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/_cluster/health", nil)
	if err != nil {
		return fmt.Errorf("opensearch %s: %w", label, err)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch %s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opensearch %s: HTTP %d", label, resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("opensearch %s: bad health response: %w", label, err)
	}
	if body.Status == "red" {
		return fmt.Errorf("opensearch %s: cluster status is red", label)
	}
	return nil
}
