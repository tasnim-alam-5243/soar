// Package opensearch is the only place that talks to OpenSearch (spec section 3.1).
// Step 1 only needs a reachability check; the real client arrives in Step 3.
package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal OpenSearch HTTP client.
type Client struct {
	urls []string
	user string
	pass string
	http *http.Client
}

// New builds a client for one or more node URLs. TLS options are added in Step 3.
func New(urls []string, user, pass string) *Client {
	return &Client{urls: urls, user: user, pass: pass, http: &http.Client{Timeout: 5 * time.Second}}
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
