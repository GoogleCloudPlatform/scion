/*
Copyright 2026 The Scion Authors.
*/

// Package otlptls builds the client TLS configuration and request headers
// for OTLP export. The agent-side exporter (pkg/sciontool/telemetry) and the
// server log bridge (pkg/util/logging) both use it, so every OTLP exporter
// applies the same CA, client certificate and header settings.
//
// The package depends on the standard library only.
package otlptls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

var (
	// ErrCACertsNotFound reports a CA file that holds no PEM certificates.
	ErrCACertsNotFound = errors.New("parsing OTLP CA file: no certificates found")
	// ErrIncompleteClientKeyPair reports that only one of the OTLP client
	// certificate and key paths is set; mTLS needs both.
	ErrIncompleteClientKeyPair = errors.New("OTLP client certificate and key must be set together")
)

// LoadClientConfig builds the client TLS config for OTLP export.
// caFile adds a PEM CA bundle for verifying the endpoint. certFile and
// keyFile add a client certificate for mTLS; they must be set together.
// Errors name the file paths only, never file contents. With all three
// empty the result carries no roots and no certificates (system defaults).
//
// The client certificate and key are loaded once here, so a missing or
// invalid pair fails at startup. After that, GetClientCertificate reads the
// pair again on every TLS handshake, so a rotated certificate is used by the
// next new connection without a restart. A connection that is already
// established keeps the certificate it was opened with until it reconnects.
// If the pair cannot be read during a handshake (for example a rotation that
// has replaced the certificate but not yet the key), the last pair that
// loaded is used.
func LoadClientConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading OTLP CA file: %w", err)
		}

		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("%w in %q", ErrCACertsNotFound, caFile)
		}
		tlsConfig.RootCAs = roots
	}

	if certFile == "" && keyFile == "" {
		return tlsConfig, nil
	}
	if certFile == "" {
		return nil, fmt.Errorf("%w: key file %q is set but the certificate file is not", ErrIncompleteClientKeyPair, keyFile)
	}
	if keyFile == "" {
		return nil, fmt.Errorf("%w: certificate file %q is set but the key file is not", ErrIncompleteClientKeyPair, certFile)
	}

	pair := &clientKeyPair{certFile: certFile, keyFile: keyFile}
	clientCert, err := pair.load()
	if err != nil {
		return nil, err
	}
	pair.last = &clientCert
	// Certificates records the pair loaded at startup. On the client side
	// crypto/tls ignores it when GetClientCertificate is set.
	tlsConfig.Certificates = []tls.Certificate{clientCert}
	tlsConfig.GetClientCertificate = pair.get
	return tlsConfig, nil
}

// clientKeyPair reloads a client certificate and key from disk.
type clientKeyPair struct {
	certFile, keyFile string

	mu      sync.Mutex
	last    *tls.Certificate
	failing bool
}

func (p *clientKeyPair) load() (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(p.certFile, p.keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading OTLP client certificate %q and key %q: %w", p.certFile, p.keyFile, err)
	}
	return cert, nil
}

// get is the tls.Config.GetClientCertificate callback. It returns the pair
// on disk, or the last pair that loaded when the files cannot be read.
func (p *clientKeyPair) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	cert, err := p.load()
	p.mu.Lock()
	if err == nil {
		p.failing = false
		p.last = &cert
		p.mu.Unlock()
		return &cert, nil
	}
	// Warn once per failure streak: the server log bridge exports slog
	// records, so a warning per handshake would feed itself. The decision
	// is made under the lock; the warning is logged after unlocking, so the
	// lock never depends on the installed slog handler.
	warn := !p.failing
	p.failing = true
	last := p.last
	p.mu.Unlock()
	if warn {
		slog.Warn("OTLP client certificate reload failed; using the last loaded certificate", "error", err)
	}
	return last, nil
}

// ParseHeaders decodes the OTLP headers setting, a JSON object of header
// names to values (the SCION_OTEL_HEADERS format). An empty value yields no
// headers. The error never echoes the value, which may hold credentials.
func ParseHeaders(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return nil, errors.New("OTLP headers: value is not a JSON object of string values")
	}
	return headers, nil
}
