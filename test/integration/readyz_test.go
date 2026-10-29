//go:build integration

// Package integration holds tests that need the Compose stack running (make up).
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func apiURL() string {
	if v := os.Getenv("SOAR_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8080"
}

func TestReadyz(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL()+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz status = %d", resp.StatusCode)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Checks["opensearch"] != "ok" || body.Checks["nats"] != "ok" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}
