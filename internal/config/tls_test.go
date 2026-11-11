package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yourorg/soar/internal/tlstest"
)

func TestTLSSettings(t *testing.T) {
	f := tlstest.New(t)
	base := map[string]string{"SOAR_OS_URLS": "https://opensearch:9200", "SOAR_NATS_URL": "tls://nats:4222"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	c, err := Load(env(base))
	if err != nil || c.OSTLS != nil || c.NATSTLS != nil || c.ServerTLS != nil {
		t.Fatalf("no TLS settings must mean defaults: %v %+v", err, c)
	}

	c, err = Load(env(with("SOAR_OS_CA_FILE", f.CA, "SOAR_OS_CERT_FILE", f.ClientCert, "SOAR_OS_KEY_FILE", f.ClientKey,
		"SOAR_NATS_CA_FILE", f.CA, "SOAR_TLS_CERT_FILE", f.ServerCert, "SOAR_TLS_KEY_FILE", f.ServerKey, "SOAR_TLS_CLIENT_CA_FILE", f.CA)))
	if err != nil {
		t.Fatal(err)
	}
	if c.OSTLS == nil || c.OSTLS.RootCAs == nil || len(c.OSTLS.Certificates) != 1 || c.OSTLS.InsecureSkipVerify {
		t.Errorf("OpenSearch TLS: %+v", c.OSTLS)
	}
	if c.NATSTLS == nil || c.NATSTLS.RootCAs == nil || len(c.NATSTLS.Certificates) != 0 {
		t.Errorf("NATS TLS: %+v", c.NATSTLS)
	}
	if c.ServerTLS == nil || len(c.ServerTLS.Certificates) != 1 || c.ServerTLS.ClientAuth != tls.VerifyClientCertIfGiven || !c.ClientCertReq {
		t.Errorf("server TLS: %+v", c.ServerTLS)
	}
	if v := fmt.Sprint(c.LogValue()); !strings.Contains(v, "custom CA + client certificate") || !strings.Contains(v, "https + client certificates") ||
		strings.Contains(v, f.ClientKey) {
		t.Errorf("log value: %s", v)
	}

	c, err = Load(env(with("SOAR_OS_INSECURE", "true")))
	if err != nil || c.OSTLS == nil || !c.OSTLS.InsecureSkipVerify {
		t.Errorf("SOAR_OS_INSECURE: %v %+v", err, c.OSTLS)
	}

	for name, m := range map[string]map[string]string{
		"missing CA file":               with("SOAR_OS_CA_FILE", "/nonexistent/ca.pem"),
		"CA file without PEM":           with("SOAR_NATS_CA_FILE", f.ClientKey),
		"cert without key":              with("SOAR_OS_CERT_FILE", f.ClientCert),
		"server key without cert":       with("SOAR_TLS_KEY_FILE", f.ServerKey),
		"client CA without server cert": with("SOAR_TLS_CLIENT_CA_FILE", f.CA),
		"mismatched pair":               with("SOAR_TLS_CERT_FILE", f.ServerCert, "SOAR_TLS_KEY_FILE", f.ClientKey),
	} {
		var ve *ValidationError
		if _, err := Load(env(m)); !errors.As(err, &ve) {
			t.Errorf("%s: want a validation error, got %v", name, err)
		}
	}
}
