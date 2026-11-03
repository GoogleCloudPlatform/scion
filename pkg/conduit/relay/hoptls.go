// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package relay

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// TLS on the relay internal hop (server.hub.conduit.internal_tls_*).
//
// The internal listener serves TLS with an operator certificate, and every
// hop client verifies the peer against the operator CA bundle (or the
// system roots) and, when set, one shared server name. There is no option
// to skip verification: a hop client that cannot verify its peer fails.

// HopCertReloadInterval is how often the internal listener re-reads its
// certificate and key files (at most once per interval, on a handshake).
const HopCertReloadInterval = time.Minute

// NewHopServerTLS returns the TLS configuration of an internal listener
// serving the PEM certificate chain certFile with the private key keyFile.
// The files are read now, and an error refuses the listener. Afterwards
// they are re-read at most once per HopCertReloadInterval and replaced when
// their bytes change, so a rotated certificate (for example a Kubernetes
// Secret volume, whose files are swapped through a symlink) is served
// without a restart. A pair that does not load keeps the previous
// certificate and is logged.
func NewHopServerTLS(certFile, keyFile string, log *slog.Logger) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("conduit relay: the internal listener TLS certificate and key files are both required")
	}
	if log == nil {
		log = slog.Default()
	}
	return newHopServerTLS(certFile, keyFile, log, time.Now)
}

func newHopServerTLS(certFile, keyFile string, log *slog.Logger, now func() time.Time) (*tls.Config, error) {
	cr := &hopCertReloader{certFile: certFile, keyFile: keyFile, log: log, now: now}
	if err := cr.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"http/1.1"},
		GetCertificate: cr.getCertificate,
	}, nil
}

// hopCertReloader serves the current certificate pair and re-reads it.
type hopCertReloader struct {
	certFile, keyFile string
	log               *slog.Logger
	now               func() time.Time

	mu              sync.Mutex
	cert            *tls.Certificate
	certPEM, keyPEM []byte
	checked         time.Time
}

func (cr *hopCertReloader) readPair() (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(cr.certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("conduit relay: reading server.hub.conduit.internal_tls_cert_file: %w", err)
	}
	keyPEM, err = os.ReadFile(cr.keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("conduit relay: reading server.hub.conduit.internal_tls_key_file: %w", err)
	}
	return certPEM, keyPEM, nil
}

// load reads and parses the pair; on success it becomes current.
func (cr *hopCertReloader) load() error {
	certPEM, keyPEM, err := cr.readPair()
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("conduit relay: loading the internal listener TLS certificate and key: %w", err)
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.cert, cr.certPEM, cr.keyPEM, cr.checked = &cert, certPEM, keyPEM, cr.now()
	return nil
}

func (cr *hopCertReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if now := cr.now(); now.Sub(cr.checked) >= HopCertReloadInterval {
		cr.checked = now
		cr.reloadLocked()
	}
	return cr.cert, nil
}

// reloadLocked replaces the current pair when the files changed and the
// new pair loads; otherwise the current pair stays.
func (cr *hopCertReloader) reloadLocked() {
	certPEM, keyPEM, err := cr.readPair()
	if err != nil {
		cr.log.Warn("Conduit internal listener: cannot re-read the TLS certificate; serving the previous one", "error", err)
		return
	}
	if bytes.Equal(certPEM, cr.certPEM) && bytes.Equal(keyPEM, cr.keyPEM) {
		return
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// A rotation caught between the two files lands here too; the
		// next check picks up the complete pair.
		cr.log.Warn("Conduit internal listener: the changed TLS certificate and key do not load; serving the previous one", "error", err)
		return
	}
	cr.cert, cr.certPEM, cr.keyPEM = &cert, certPEM, keyPEM
	cr.log.Info("Conduit internal listener: TLS certificate reloaded")
}

// NewHopClientTLS returns the TLS configuration every relay internal hop
// client uses. With caFile set, peers are verified against that PEM bundle
// only (it replaces the system roots); a private or internal CA requires
// it. Without it the system roots apply. With serverName set, the peer
// certificate is verified for that name instead of the dialed host, so one
// certificate can serve every node. Verification is never skipped.
func NewHopClientTLS(caFile, serverName string) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("conduit relay: reading server.hub.conduit.internal_tls_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("conduit relay: server.hub.conduit.internal_tls_ca_file holds no PEM certificate")
		}
		c.RootCAs = pool
	}
	return c, nil
}

// NewHopHTTPClient returns an HTTP client for relay internal hop requests
// (PeerClient.HTTP, HubClient.HTTP) that uses tlsConfig for https and never
// follows a redirect (NewHopHTTPClientWithTimeout with no timeout).
func NewHopHTTPClient(tlsConfig *tls.Config) *http.Client {
	return NewHopHTTPClientWithTimeout(tlsConfig, 0)
}

// NewHopHTTPClientWithTimeout is the one constructor of relay internal hop
// HTTP clients: tlsConfig for https (nil: the system roots), timeout as
// http.Client.Timeout (0: none), and no redirects. A hop request is
// checked against the internal hop rule (config.CheckRelayTransport) only
// at its first URL; following a redirect would re-send its relay-peer
// headers, and on 307 or 308 its body, to wherever the redirect points,
// plaintext included. No internal API redirects, so a 3xx is returned to
// the caller as the answer, which treats it as a failure.
func NewHopHTTPClientWithTimeout(tlsConfig *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{Transport: NewHopTransport(tlsConfig), Timeout: timeout, CheckRedirect: refuseHopRedirect}
}

// refuseHopRedirect is the CheckRedirect of every hop HTTP client.
func refuseHopRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// hopClientWithoutRedirects returns c as every hop request must use it: a
// shallow copy whose CheckRedirect refuses every redirect, so an injected
// client (PeerClient.HTTP, HubClient.HTTP, Config.HTTPClient, and the hub
// and relay server options that feed them) cannot follow a redirect past
// the internal hop rule either. The caller's client is never modified; its
// transport, timeout and cookie jar are kept.
func hopClientWithoutRedirects(c *http.Client) *http.Client {
	cc := *c
	cc.CheckRedirect = refuseHopRedirect
	return &cc
}

// defaultHopHTTPClient is the hop client PeerClient and HubClient use when
// none is set: the system roots, no timeout, no redirects.
var defaultHopHTTPClient = NewHopHTTPClientWithTimeout(nil, 0)

// NewHopTransport returns a clone of http.DefaultTransport that uses
// tlsConfig for https.
func NewHopTransport(tlsConfig *tls.Config) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		t.TLSClientConfig = tlsConfig.Clone()
	}
	return t
}

// NewHopWSDialer returns a WebSocket dialer for relay internal stream hops
// (PeerClient.WS) that uses tlsConfig for wss.
func NewHopWSDialer(tlsConfig *tls.Config) *websocket.Dialer {
	d := *websocket.DefaultDialer
	d.TLSClientConfig = tlsConfig.Clone()
	return &d
}
