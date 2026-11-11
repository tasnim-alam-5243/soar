package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLS settings (spec section 13, "TLS/mTLS"). All of them are optional: the dev
// stack talks plain HTTP on a private network.
//
//	SOAR_OS_CA_FILE, SOAR_OS_CERT_FILE + SOAR_OS_KEY_FILE   OpenSearch: trusted CA, client certificate (mTLS)
//	SOAR_OS_INSECURE=true                                   OpenSearch: skip verification (development only)
//	SOAR_NATS_CA_FILE, SOAR_NATS_CERT_FILE + SOAR_NATS_KEY_FILE   NATS: trusted CA, client certificate (mTLS)
//	SOAR_TLS_CERT_FILE + SOAR_TLS_KEY_FILE                  serve the role's HTTP port over HTTPS
//	SOAR_TLS_CLIENT_CA_FILE                                 and require client certificates signed by this CA
//	                                                        (except on /healthz and /readyz)
//
// The files are read when the configuration loads, so a missing or broken file stops
// the process at start (exit 2) instead of failing on the first connection.

// clientTLS builds a client configuration, or nil when no setting is given.
func clientTLS(caFile, certFile, keyFile string, insecure bool, name string) (*tls.Config, error) {
	if caFile == "" && certFile == "" && keyFile == "" && !insecure {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // opt-in, development only
	if caFile != "" {
		pool, err := loadPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("%s_CA_FILE: %w", name, err)
		}
		cfg.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("%s_CERT_FILE and %s_KEY_FILE must be set together", name, name)
	}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("%s_CERT_FILE/%s_KEY_FILE: %w", name, name, err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// serverTLS builds the HTTPS configuration of the role's port, or nil for plain HTTP.
func serverTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" && clientCAFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("SOAR_TLS_CERT_FILE and SOAR_TLS_KEY_FILE must be set together (SOAR_TLS_CLIENT_CA_FILE needs both)")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("SOAR_TLS_CERT_FILE/SOAR_TLS_KEY_FILE: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	if clientCAFile != "" {
		pool, err := loadPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("SOAR_TLS_CLIENT_CA_FILE: %w", err)
		}
		// verified when presented; the HTTP layer refuses requests without one (obs.RequireClientCert)
		cfg.ClientCAs, cfg.ClientAuth = pool, tls.VerifyClientCertIfGiven
	}
	return cfg, nil
}

func loadPool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s contains no PEM certificate", file)
	}
	return pool, nil
}
