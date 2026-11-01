package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUsageAndUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("no args: code %d", code)
	}
	if code := run([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("unknown: code %d", code)
	}
	out.Reset()
	if code := run([]string{"help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "orchestrator") {
		t.Fatalf("help: code %d out %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "soar ") {
		t.Fatalf("version: code %d out %q", code, out.String())
	}
}

func TestServeRefusesBadConfig(t *testing.T) {
	t.Setenv("SOAR_OS_URLS", "")
	t.Setenv("SOAR_NATS_URL", "")
	var out, errb bytes.Buffer
	if code := run([]string{"api"}, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(out.String(), "refusing to start") {
		t.Fatalf("missing log line: %s", out.String())
	}
}

func TestEveryRoleHasACommand(t *testing.T) {
	for _, r := range []string{"api", "ingest", "collector", "processor", "orchestrator", "worker", "scheduler", "migrate", "migrate-check"} {
		if _, ok := commands[r]; !ok {
			t.Errorf("missing command %s", r)
		}
	}
}
