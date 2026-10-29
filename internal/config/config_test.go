package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		"SOAR_OS_URLS":  "http://opensearch:9200",
		"SOAR_NATS_URL": "nats://nats:4222",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Partitions != 8 || c.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if got := c.AddrFor("api"); got != ":8080" {
		t.Fatalf("api addr = %s", got)
	}
	if got := c.AddrFor("ingest"); got != ":8081" {
		t.Fatalf("ingest addr = %s", got)
	}
}

func TestLoadListsAndOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"SOAR_OS_URLS":    " https://a:9200 , https://b:9200 ",
		"SOAR_NATS_URL":   "nats://a:4222,nats://b:4222,nats://c:4222",
		"SOAR_PARTITIONS": "16",
		"SOAR_LOG_LEVEL":  "DEBUG",
		"SOAR_HTTP_ADDR":  "127.0.0.1:9999",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.OSURLs) != 2 || len(c.NATSURLs) != 3 || c.Partitions != 16 || c.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.AddrFor("worker") != "127.0.0.1:9999" {
		t.Fatal("HTTPAddr override ignored")
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SOAR_OS_URLS":     "ftp://x",
		"SOAR_OS_PASSWORD": "secret",
		"SOAR_PARTITIONS":  "0",
		"SOAR_LOG_LEVEL":   "loud",
		"SOAR_HTTP_ADDR":   "nope",
	}))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	// os url scheme, os password w/o user, nats missing, partitions, log level, http addr
	if len(ve.Problems) != 6 {
		t.Fatalf("want 6 problems, got %d: %v", len(ve.Problems), ve.Problems)
	}
}

func TestSecretsNeverLogged(t *testing.T) {
	c, err := Load(env(map[string]string{
		"SOAR_OS_URLS":     "https://admin:hunter2@os:9200",
		"SOAR_OS_USER":     "svc",
		"SOAR_OS_PASSWORD": "hunter3",
		"SOAR_NATS_URL":    "nats://u:hunter4@nats:4222",
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.LogValue().String()
	for _, secret := range []string{"hunter2", "hunter3", "hunter4"} {
		if strings.Contains(s, secret) {
			t.Fatalf("log value leaks %s: %s", secret, s)
		}
	}
}
