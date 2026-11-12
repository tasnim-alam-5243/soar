package adapters

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/tlstest"
)

// collect is an Emit that records messages; it refuses the first `refuse` batches.
type collect struct {
	mu     sync.Mutex
	refuse int
	msgs   []Message
	calls  int
}

func (c *collect) emit(_ context.Context, b []Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.refuse > 0 {
		c.refuse--
		return fmt.Errorf("queue full")
	}
	c.msgs = append(c.msgs, b...)
	return nil
}

func (c *collect) bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		out = append(out, string(m.Body))
	}
	return out
}

func startSyslog(t *testing.T, cfg map[string]any, c *collect) net.Addr {
	t.Helper()
	cfg["flush"] = "20ms"
	st, err := newSyslog(domain.Source{Name: "sys", Config: cfg}, StreamDeps{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx, c.emit) }()
	t.Cleanup(func() { cancel(); <-done })
	actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer acancel()
	addr, err := st.(*syslogStream).Addr(actx)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitBodies(t *testing.T, c *collect, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b := c.bodies(); len(b) >= n {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d messages, want %d: %v", len(c.bodies()), n, c.bodies())
	return nil
}

func TestSyslogUDP(t *testing.T) {
	c := &collect{}
	addr := startSyslog(t, map[string]any{"listen": "127.0.0.1:0", "protocol": "udp"}, c)
	conn, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 3; i++ {
		fmt.Fprintf(conn, "<34>Nov 11 18:00:0%d host app: udp %d", i, i)
	}
	got := waitBodies(t, c, 3)
	if !strings.HasSuffix(got[2], "udp 2") {
		t.Errorf("%v", got)
	}
	c.mu.Lock()
	meta := c.msgs[0].Meta
	c.mu.Unlock()
	if meta["transport"] != "syslog/udp" || meta["remote_addr"] != "127.0.0.1" {
		t.Errorf("meta %v", meta)
	}
}

// TCP: octet counting and newline framing on one connection, an oversize message
// skipped, and a refused batch delivered later (back-pressure, nothing lost).
func TestSyslogTCPFramingAndBackPressure(t *testing.T) {
	c := &collect{refuse: 2}
	addr := startSyslog(t, map[string]any{"listen": "127.0.0.1:0", "protocol": "tcp", "max_message_bytes": 100}, c)
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	m1 := "<13>1 - h a - - - counted one"
	big := "<13>" + strings.Repeat("x", 200)
	fmt.Fprintf(conn, "%d %s", len(m1), m1)
	fmt.Fprintf(conn, "<13>Nov 11 18:00:00 h a: line two\r\n")
	fmt.Fprintf(conn, "%s\n", big)
	fmt.Fprintf(conn, "%d %s", len(big), big)
	fmt.Fprintf(conn, "<13>Nov 11 18:00:01 h a: line three\n")
	conn.Close()
	got := waitBodies(t, c, 3)
	want := []string{m1, "<13>Nov 11 18:00:00 h a: line two", "<13>Nov 11 18:00:01 h a: line three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
	c.mu.Lock()
	calls := c.calls
	c.mu.Unlock()
	if calls < 3 {
		t.Errorf("the refused batches must be retried (calls %d)", calls)
	}
}

func TestSyslogTLSRequiresAClientCertificate(t *testing.T) {
	f := tlstest.New(t)
	c := &collect{}
	addr := startSyslog(t, map[string]any{
		"listen": "127.0.0.1:0", "protocol": "tls",
		"tls_cert_file": f.ServerCert, "tls_key_file": f.ServerKey, "tls_client_ca_file": f.CA,
	}, c)
	pem, _ := os.ReadFile(f.CA)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	pair, _ := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)

	conn, err := tls.Dial("tcp", addr.String(), &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "<13>Nov 11 18:00:00 h a: over tls\n")
	conn.Close()
	if got := waitBodies(t, c, 1); got[0] != "<13>Nov 11 18:00:00 h a: over tls" {
		t.Errorf("%q", got)
	}
	// without a client certificate the handshake fails and nothing arrives
	if anon, err := tls.Dial("tcp", addr.String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}); err == nil {
		fmt.Fprintf(anon, "<13>Nov 11 18:00:00 h a: anonymous\n")
		_ = anon.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(c.bodies()); n != 1 {
		t.Errorf("a client without a certificate delivered messages: %v", c.bodies())
	}
}

func TestSyslogConfigValidation(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"no listen":     {},
		"bad protocol":  {"listen": ":5514", "protocol": "sctp"},
		"tls no cert":   {"listen": ":5514", "protocol": "tls"},
		"bad flush":     {"listen": ":5514", "flush": "soon"},
		"missing files": {"listen": ":5514", "protocol": "tls", "tls_cert_file": "/nope.pem", "tls_key_file": "/nope.key"},
	} {
		if _, err := newSyslog(domain.Source{Config: cfg}, StreamDeps{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// An octet count is at most 9 digits: an endless run of digits ends the connection
// instead of growing a buffer.
func TestSyslogOctetCountIsBounded(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("9", 1<<20)), 64)
	err := readFrames(r, 100, func([]byte) { t.Error("no message expected") }, func() {})
	if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "octet count") {
		t.Fatalf("got %v", err)
	}
	if r.Buffered() > 64 {
		t.Errorf("buffered %d bytes", r.Buffered())
	}
}
