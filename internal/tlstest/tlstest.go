// Package tlstest makes a throwaway certificate authority with a server certificate
// (127.0.0.1, ::1, localhost) and a client certificate, written as PEM files, for
// tests of the TLS and mTLS settings.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Files are PEM file paths in a test's temporary directory.
type Files struct {
	CA                    string // CA certificate
	ServerCert, ServerKey string
	ClientCert, ClientKey string
}

// New writes a CA, a server and a client certificate into t.TempDir().
func New(t testing.TB) Files {
	t.Helper()
	dir := t.TempDir()
	caKey := key(t)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "soar test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	f := Files{CA: write(t, dir, "ca.pem", "CERTIFICATE", caDER)}
	leaf := func(serial int64, name string, usage x509.ExtKeyUsage) (string, string) {
		k := key(t)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
			DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kb, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return write(t, dir, name+".pem", "CERTIFICATE", der), write(t, dir, name+"-key.pem", "EC PRIVATE KEY", kb)
	}
	f.ServerCert, f.ServerKey = leaf(2, "server", x509.ExtKeyUsageServerAuth)
	f.ClientCert, f.ClientKey = leaf(3, "client", x509.ExtKeyUsageClientAuth)
	return f
}

func key(t testing.TB) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func write(t testing.TB, dir, name, typ string, der []byte) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
