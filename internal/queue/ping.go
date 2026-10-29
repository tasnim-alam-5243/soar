// Package queue owns everything NATS: subjects, streams, consumers (Steps 5+).
// Step 1 only needs a reachability check.
package queue

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Ping succeeds if any server answers with a valid NATS INFO banner.
// With requireJetStream it also demands that JetStream is enabled.
// A NATS server sends INFO as soon as a client connects, so no client library is needed yet.
func Ping(ctx context.Context, urls []string, requireJetStream bool) error {
	var errs []error
	for _, raw := range urls {
		err := pingOne(ctx, raw, requireJetStream)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func pingOne(ctx context.Context, raw string, requireJS bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("nats: invalid URL")
	}
	label := u.Redacted()
	port := u.Port()
	if port == "" {
		port = "4222"
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return fmt.Errorf("nats %s: %w", label, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("nats %s: reading INFO: %w", label, err)
	}
	if !strings.HasPrefix(line, "INFO ") {
		return fmt.Errorf("nats %s: unexpected banner (not a NATS server?)", label)
	}
	var info struct {
		JetStream bool `json:"jetstream"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line[len("INFO "):])), &info); err != nil {
		return fmt.Errorf("nats %s: bad INFO: %w", label, err)
	}
	if requireJS && !info.JetStream {
		return fmt.Errorf("nats %s: JetStream is not enabled", label)
	}
	return nil
}
