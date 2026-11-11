package runner

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/tlstest"
)

// startRunner launches the real Python runner (runner/python/.venv) on a free port.
// The test is skipped when the virtualenv is missing (`make runner-venv` creates it).
func startRunner(t *testing.T, extra ...string) string {
	t.Helper()
	root, _ := filepath.Abs("../../runner/python")
	py := filepath.Join(root, ".venv", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("runner virtualenv not found; run `make runner-venv` to enable this test")
	}
	args := append([]string{
		"-m", "soar_runner.server", "--listen", "127.0.0.1:0",
		"--integrations", filepath.Join(root, "tests", "fixtures", "integrations"),
		"--scripts", filepath.Join(root, "tests", "fixtures", "scripts"), "--workers", "4",
	}, extra...)
	cmd := exec.Command(py, args...)
	cmd.Dir = root
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	sc := bufio.NewScanner(out)
	ready := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if p, ok := strings.CutPrefix(sc.Text(), "READY "); ok {
				ready <- p
				return
			}
		}
	}()
	select {
	case port := <-ready:
		return "127.0.0.1:" + port
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not start")
	}
	return ""
}

func TestExecuteThroughRealRunner(t *testing.T) {
	addr := startRunner(t)
	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	var logs []string
	m := actions.Manifest{ID: "demo.echo", Class: actions.ClassPython, Connector: "demo", Entrypoint: "actions/demo.py:echo", Limits: actions.Limits{MemoryMB: 256}}
	out, err := c.Execute(ctx, m, actions.Input{
		Params: map[string]any{"q": "x"}, Attempt: 1, DryRun: true,
		Connection: &actions.Connection{Config: map[string]any{"base_url": "https://iris.example"}, Secrets: map[string]string{"api_key": "k"}},
		Log:        func(s string) { logs = append(logs, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["conn"] != "https://iris.example" || out.(map[string]any)["dry_run"] != true {
		t.Fatalf("output %v", out)
	}
	if len(logs) == 0 {
		t.Error("logs were not streamed")
	}

	_, err = c.Execute(ctx, actions.Manifest{ID: "demo.fail", Class: actions.ClassPython}, actions.Input{})
	ae := actions.AsError(err)
	if ae.Code != "http_429" || !ae.Retryable {
		t.Fatalf("want retryable http_429, got %+v", ae)
	}

	short, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_, err = c.Execute(short, actions.Manifest{ID: "demo.sleep", Class: actions.ClassPython}, actions.Input{})
	if ae := actions.AsError(err); ae.Code != "timeout" || !ae.Retryable {
		t.Fatalf("want timeout, got %+v", ae)
	}

	dec, err := c.Filter(ctx, "filters/odd.py", []map[string]any{{"n": 1}, {"n": 4}}, 5*time.Second)
	if err != nil || dec[0] != "drop" || dec[1] != "keep" {
		t.Fatalf("filter %v %v", dec, err)
	}
}

// The worker sends decrypted connection secrets to the runner, so the channel can be
// mutually authenticated TLS: only a client with a certificate from the CA gets in.
func TestRunnerOverMutualTLS(t *testing.T) {
	f := tlstest.New(t)
	addr := startRunner(t, "--tls-cert", f.ServerCert, "--tls-key", f.ServerKey, "--tls-client-ca", f.CA)
	caPEM, err := os.ReadFile(f.CA)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	pair, err := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	ping := func(tc *tls.Config) error {
		c, err := Dial(addr, tc)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.Ping(ctx)
	}
	if err := ping(&tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("CA + client certificate: %v", err)
	}
	if err := ping(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}); err == nil {
		t.Error("a client without a certificate must be refused")
	}
	if err := ping(nil); err == nil {
		t.Error("a plaintext client must be refused by a TLS runner")
	}
}
