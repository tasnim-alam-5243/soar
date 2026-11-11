package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(p, []byte("# comment\n\nSOAR_BOOTSTRAP_API_KEY=soar_abc\nexport SOAR_SOURCE_TOKEN_WAZUH_PROD=\"tok en\"\nX='y' \nNATS_CONF=nats-relaxed.conf # fsync every 200 ms\n"), 0o600)
	env := loadDotEnv(p)
	for k, want := range map[string]string{"SOAR_BOOTSTRAP_API_KEY": "soar_abc", "SOAR_SOURCE_TOKEN_WAZUH_PROD": "tok en", "X": "y", "NATS_CONF": "nats-relaxed.conf"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if len(loadDotEnv(filepath.Join(t.TempDir(), "missing"))) != 0 {
		t.Error("a missing file is an empty map")
	}
	t.Setenv("SOAR_API_KEY", "")
	t.Setenv("SOAR_BOOTSTRAP_API_KEY", "")
	if got := setting(env, "def", "SOAR_API_KEY", "SOAR_BOOTSTRAP_API_KEY"); got != "soar_abc" {
		t.Errorf("setting from .env: %q", got)
	}
	t.Setenv("SOAR_API_KEY", "from-env")
	if got := setting(env, "def", "SOAR_API_KEY", "SOAR_BOOTSTRAP_API_KEY"); got != "from-env" {
		t.Errorf("the environment wins over .env: %q", got)
	}
}

func TestMaxOverlap(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := func(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }
	// two waves of two one-second items; the second wave starts as the first ends
	starts := []time.Time{s(0), s(0.1), s(1.0), s(1.1)}
	ends := []time.Time{s(1.0), s(1.1), s(2.0), s(2.1)}
	if got := maxOverlap(starts, ends); got != 2 {
		t.Errorf("two waves: %d, want 2", got)
	}
	if got := maxOverlap([]time.Time{s(0), s(0), s(0)}, []time.Time{s(1), s(1), s(1)}); got != 3 {
		t.Errorf("three at once: %d", got)
	}
}

func TestAt(t *testing.T) {
	v := map[string]any{
		"context": map[string]any{"nodes": map[string]any{"a": map[string]any{"status": "ok", "attempt": 2.0}}},
		"items":   []any{map[string]any{"id": "x"}},
	}
	if str(v, "context", "nodes", "a", "status") != "ok" || num(v, "context", "nodes", "a", "attempt") != 2 ||
		str(v, "items", "0", "id") != "x" || at(v, "items", "5") != nil || at(v, "nope", "x") != nil || str(v, "context", "nodes", "a", "attempt") != "2" {
		t.Error("path lookups")
	}
}

func TestPullPackConfigReadsTheRepoPack(t *testing.T) {
	cfg, err := pullPackConfig("../../" + pullPack)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["key_path"] != "sort" || cfg["overlap_pages"] != 40 {
		t.Errorf("pack config: key_path %v, overlap_pages %v", cfg["key_path"], cfg["overlap_pages"])
	}
	if _, err := json.Marshal(cfg); err != nil {
		t.Errorf("the config must be JSON-encodable for the PATCH: %v", err)
	}
}
