package adapters

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
)

func init() {
	RegisterStream("syslog", StreamSpec{New: newSyslog})
}

// syslog listens for syslog over UDP, TCP or TLS (RFC 5424/3164; TCP framing by octet
// counting or newline, RFC 6587). Source config:
//
//	listen: "0.0.0.0:5514"        required
//	protocol: udp | tcp | tls     default udp
//	tls_cert_file, tls_key_file   required for tls; files in SOAR_STREAM_CERTS_DIR
//	tls_client_ca_file            tls: require client certificates signed by this CA
//	max_message_bytes: 65536      longer messages are dropped (and counted)
//	batch_size: 256, flush: 200ms
//
// Back-pressure: when the queue refuses a batch, a TCP/TLS connection stops being read
// until it is accepted, which slows the sender down. UDP cannot push back: a refused
// batch is retried briefly, then dropped and counted (soar_source_events_total{result="dropped"}).
type syslogStream struct {
	source   string
	listen   string
	protocol string
	tls      *tls.Config
	certFile string // tls: names in the certificate directory (SOAR_STREAM_CERTS_DIR)
	keyFile  string
	caFile   string
	maxBytes int
	batch    int
	flush    time.Duration
	log      *slog.Logger

	mu    sync.Mutex
	addr  net.Addr
	ready chan struct{}
}

func newSyslog(src domain.Source, deps StreamDeps) (Stream, error) {
	cfg := src.Config
	s := &syslogStream{
		source: src.Name, protocol: "udp", maxBytes: 64 << 10, batch: 256, flush: 200 * time.Millisecond,
		log: deps.Log, ready: make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.listen, _ = cfg["listen"].(string)
	if _, _, err := net.SplitHostPort(s.listen); err != nil {
		return nil, fmt.Errorf("config.listen must be host:port (for example 0.0.0.0:5514), got %q", s.listen)
	}
	if p, _ := cfg["protocol"].(string); p != "" {
		s.protocol = p
	}
	switch s.protocol {
	case "udp", "tcp":
	case "tls":
		s.certFile, _ = cfg["tls_cert_file"].(string)
		s.keyFile, _ = cfg["tls_key_file"].(string)
		s.caFile, _ = cfg["tls_client_ca_file"].(string)
		if s.certFile == "" || s.keyFile == "" {
			return nil, fmt.Errorf("protocol tls needs config.tls_cert_file and config.tls_key_file (files in SOAR_STREAM_CERTS_DIR)")
		}
		// the files are read only by the collector, from its certificate directory
		if deps.CertDir != "" {
			if err := s.loadTLS(deps.CertDir); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("config.protocol must be udp, tcp or tls, got %q", s.protocol)
	}
	if n := intOf(cfg["max_message_bytes"]); n > 0 {
		s.maxBytes = n
	}
	if n := intOf(cfg["batch_size"]); n > 0 {
		s.batch = n
	}
	if f, _ := cfg["flush"].(string); f != "" {
		d, err := time.ParseDuration(f)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("config.flush must be a duration, got %q", f)
		}
		s.flush = d
	}
	return s, nil
}

// loadTLS builds the listener's TLS settings from files in the certificate directory.
func (s *syslogStream) loadTLS(dir string) error {
	certPEM, err := readCertFile(dir, s.certFile)
	if err != nil {
		return fmt.Errorf("config.tls_cert_file: %w", err)
	}
	keyPEM, err := readCertFile(dir, s.keyFile)
	if err != nil {
		return fmt.Errorf("config.tls_key_file: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.New("config.tls_cert_file and tls_key_file are not a matching PEM certificate and key")
	}
	s.tls = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	if s.caFile != "" {
		pem, err := readCertFile(dir, s.caFile)
		if err != nil {
			return fmt.Errorf("config.tls_client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("config.tls_client_ca_file has no PEM certificate")
		}
		s.tls.ClientCAs, s.tls.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return nil
}

// Addr is the bound address once the listener is up (tests listen on port 0).
func (s *syslogStream) Addr(ctx context.Context) (net.Addr, error) {
	select {
	case <-s.ready:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.addr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *syslogStream) bound(a net.Addr) {
	s.mu.Lock()
	s.addr = a
	s.mu.Unlock()
	close(s.ready)
	s.log.Info("syslog listening", "addr", a.String(), "protocol", s.protocol)
}

func (s *syslogStream) Run(ctx context.Context, emit Emit) error {
	if s.protocol == "tls" && s.tls == nil {
		return errors.New("syslog tls: the certificate files were not loaded (no SOAR_STREAM_CERTS_DIR)")
	}
	if s.protocol == "udp" {
		return s.runUDP(ctx, emit)
	}
	return s.runTCP(ctx, emit)
}

func (s *syslogStream) runUDP(ctx context.Context, emit Emit) error {
	pc, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", s.listen)
	if err != nil {
		return err
	}
	s.bound(pc.LocalAddr())
	go func() { <-ctx.Done(); _ = pc.Close() }()
	in := make(chan Message, s.batch*4)
	done := make(chan struct{})
	go func() { defer close(done); s.pump(ctx, in, emit, true) }()
	buf := make([]byte, s.maxBytes+1)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			close(in)
			<-done
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n > s.maxBytes {
			obs.SourceEvents.WithLabelValues(s.source, "dropped").Inc()
			continue
		}
		msg := Message{Body: append([]byte(nil), buf[:n]...), Meta: s.meta(from)}
		select {
		case in <- msg:
		default: // the batcher is stuck on a refusing queue; UDP cannot wait
			obs.SourceEvents.WithLabelValues(s.source, "dropped").Inc()
		}
	}
}

func (s *syslogStream) runTCP(ctx context.Context, emit Emit) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.listen)
	if err != nil {
		return err
	}
	if s.tls != nil {
		ln = tls.NewListener(ln, s.tls)
	}
	s.bound(ln.Addr())
	go func() { <-ctx.Done(); _ = ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConn(ctx, conn, emit)
		}()
	}
}

func (s *syslogStream) serveConn(ctx context.Context, conn net.Conn, emit Emit) {
	defer conn.Close()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-cctx.Done(); _ = conn.Close() }()
	in := make(chan Message, s.batch)
	done := make(chan struct{})
	go func() { defer close(done); s.pump(cctx, in, emit, false) }()
	meta := s.meta(conn.RemoteAddr())
	err := readFrames(bufio.NewReaderSize(conn, s.maxBytes+16), s.maxBytes, func(b []byte) {
		// a full channel blocks this reader: TCP back-pressure to the sender
		select {
		case in <- Message{Body: b, Meta: meta}:
		case <-cctx.Done():
		}
	}, func() { obs.SourceEvents.WithLabelValues(s.source, "dropped").Inc() })
	close(in)
	<-done
	if !errors.Is(err, io.EOF) && cctx.Err() == nil {
		s.log.Debug("syslog connection ended", "remote", conn.RemoteAddr().String(), "error", err.Error())
	}
}

func (s *syslogStream) meta(from net.Addr) map[string]any {
	host := from.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return map[string]any{"transport": "syslog/" + s.protocol, "remote_addr": host}
}

// pump batches messages and hands them to emit. A refused batch is retried with a
// back-off (blocking the producer: back-pressure); lossy (UDP) gives up after a few tries.
func (s *syslogStream) pump(ctx context.Context, in <-chan Message, emit Emit, lossy bool) {
	tick := time.NewTicker(s.flush)
	defer tick.Stop()
	var batch []Message
	flush := func() {
		if len(batch) == 0 {
			return
		}
		for attempt := 0; ; attempt++ {
			err := emit(ctx, batch)
			if err == nil || ctx.Err() != nil {
				break
			}
			if lossy && attempt >= 2 {
				s.log.Warn("dropping syslog datagrams: the queue does not accept them", "count", len(batch), "error", err.Error())
				obs.SourceEvents.WithLabelValues(s.source, "dropped").Add(float64(len(batch)))
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(200*time.Millisecond<<min(attempt, 5), 5*time.Second)):
			}
		}
		batch = nil
	}
	for {
		select {
		case m, ok := <-in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, m)
			if len(batch) >= s.batch {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			return
		}
	}
}

// readFrames splits a TCP syslog stream into messages: "LEN SP MSG" (octet counting)
// when a frame starts with a digit, otherwise one message per line. A message longer
// than max is skipped (oversize is called). It returns when the stream ends: io.EOF on
// a clean close, otherwise the read or framing error.
func readFrames(r *bufio.Reader, max int, yield func([]byte), oversize func()) error {
	for {
		b, err := r.Peek(1)
		if err != nil {
			return err
		}
		if b[0] >= '1' && b[0] <= '9' {
			// at most 9 digits then a space; read byte by byte so a sender cannot make
			// this buffer grow without end
			n := 0
			for i := 0; ; i++ {
				c, err := r.ReadByte()
				if err != nil {
					return err
				}
				if c == ' ' {
					break
				}
				if c < '0' || c > '9' || i == 9 {
					return fmt.Errorf("bad octet count (byte %q after %d digits)", c, i)
				}
				n = n*10 + int(c-'0')
			}
			if n > max {
				oversize()
				if _, err := r.Discard(n); err != nil {
					return err
				}
				continue
			}
			msg := make([]byte, n)
			if _, err := io.ReadFull(r, msg); err != nil {
				return err
			}
			yield(trimFrame(msg))
			continue
		}
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			oversize()
			for errors.Is(err, bufio.ErrBufferFull) { // skip the rest of the long line
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return err
			}
			continue
		}
		if len(line) > 0 {
			if m := trimFrame(line); len(m) > 0 {
				yield(append([]byte(nil), m...))
			}
		}
		if err != nil {
			return err
		}
	}
}

func trimFrame(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return b
}

func intOf(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}
