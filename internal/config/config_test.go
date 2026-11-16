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

func TestIndexSettingsDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{"SOAR_OS_URLS": "http://os:9200", "SOAR_NATS_URL": "nats://n:4222"}
	c, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.IndexPrefix != "soar" || c.StateReplicas != 0 || c.TSReplicas != 0 || c.RetentionFile != "/etc/soar/retention.yaml" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	base["SOAR_INDEX_PREFIX"] = "soar-prod"
	base["SOAR_OS_STATE_REPLICAS"] = "1"
	base["SOAR_OS_TS_REPLICAS"] = "2"
	base["SOAR_RETENTION_FILE"] = "/tmp/r.yaml"
	c, err = Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.IndexPrefix != "soar-prod" || c.StateReplicas != 1 || c.TSReplicas != 2 || c.RetentionFile != "/tmp/r.yaml" {
		t.Fatalf("overrides ignored: %+v", c)
	}
}

func TestIndexSettingsValidation(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SOAR_OS_URLS": "http://os:9200", "SOAR_NATS_URL": "nats://n:4222",
		"SOAR_INDEX_PREFIX": "Bad Prefix", "SOAR_OS_STATE_REPLICAS": "-1", "SOAR_OS_TS_REPLICAS": "x",
	}))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
}

func TestQueueSettings(t *testing.T) {
	base := map[string]string{"SOAR_OS_URLS": "http://os:9200", "SOAR_NATS_URL": "nats://n:4222"}
	c, err := Load(env(base))
	if err != nil || c.EventsMaxBytes != 20<<30 || c.NATSReplicas != 1 || c.ProcessorWorkers != 2 {
		t.Fatalf("defaults: %v %+v", err, c)
	}
	base["SOAR_EVENTS_MAX_BYTES"], base["SOAR_NATS_REPLICAS"], base["SOAR_PROCESSOR_WORKERS"] = "2147483648", "3", "8"
	c, err = Load(env(base))
	if err != nil || c.EventsMaxBytes != 2<<30 || c.NATSReplicas != 3 || c.ProcessorWorkers != 8 {
		t.Fatalf("overrides: %v %+v", err, c)
	}
	base["SOAR_EVENTS_MAX_BYTES"], base["SOAR_NATS_REPLICAS"], base["SOAR_PROCESSOR_WORKERS"] = "10", "0", "x"
	_, err = Load(env(base))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 3 {
		t.Fatalf("want 3 problems, got %v", err)
	}
}

func TestIngestDirs(t *testing.T) {
	base := map[string]string{"SOAR_OS_URLS": "http://os:9200", "SOAR_NATS_URL": "nats://n:4222"}
	c, err := Load(env(base))
	if err != nil || len(c.IngestDirs) != 0 {
		t.Fatalf("default must allow no directory: %v %v", err, c.IngestDirs)
	}
	base["SOAR_INGEST_DIRS"] = "/var/spool/soar/, /data/drop"
	c, err = Load(env(base))
	if err != nil || strings.Join(c.IngestDirs, ",") != "/var/spool/soar,/data/drop" {
		t.Fatalf("%v %v", err, c.IngestDirs)
	}
	base["SOAR_INGEST_DIRS"] = "relative/dir,/,/ok"
	_, err = Load(env(base))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("want 2 problems, got %v", err)
	}
}
