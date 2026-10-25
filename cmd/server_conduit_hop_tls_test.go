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

package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

const hopTestServerName = "scion-relay.internal"

// hopTestConduitConfig is a conduit config with the internal hop TLS
// settings pointing at files.
func hopTestConduitConfig(files relaytest.HopTLSFiles) config.HubConduitConfig {
	return config.HubConduitConfig{
		InternalTLSCertFile:   files.CertFile,
		InternalTLSKeyFile:    files.KeyFile,
		InternalTLSCAFile:     files.CAFile,
		InternalTLSServerName: hopTestServerName,
	}
}

// TestConduitHopTLS: the listener TLS exists only with a certificate and
// key; the client TLS trusts the configured CA and verifies the server
// name; a file that does not load refuses startup.
func TestConduitHopTLS(t *testing.T) {
	files := relaytest.NewHopTLSFiles(t, hopTestServerName)

	srv, cli, err := conduitHopTLS(hopTestConduitConfig(files))
	require.NoError(t, err)
	require.NotNil(t, srv, "a certificate and key give the listener TLS")
	require.NotNil(t, cli)
	assert.NotNil(t, cli.RootCAs, "the CA file replaces the system roots")
	assert.Equal(t, hopTestServerName, cli.ServerName)
	assert.False(t, cli.InsecureSkipVerify)

	srv, cli, err = conduitHopTLS(config.HubConduitConfig{})
	require.NoError(t, err)
	assert.Nil(t, srv, "no certificate: the listener has no TLS (and must be loopback)")
	require.NotNil(t, cli)
	assert.Nil(t, cli.RootCAs, "no CA file: the system roots")

	missing := filepath.Join(t.TempDir(), "missing.pem")
	for name, c := range map[string]config.HubConduitConfig{
		"missing certificate": {InternalTLSCertFile: missing, InternalTLSKeyFile: files.KeyFile},
		"mismatched key":      {InternalTLSCertFile: files.CertFile, InternalTLSKeyFile: relaytest.NewHopTLSFiles(t).KeyFile},
		"missing CA":          {InternalTLSCAFile: missing},
		"CA without PEM":      {InternalTLSCAFile: writeHopTestFile(t, "not a certificate")},
	} {
		_, _, err := conduitHopTLS(c)
		assert.Error(t, err, name)
	}
}

func writeHopTestFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.pem")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// TestServeConduitInternalListener_HopTransport: without TLS the internal
// listener refuses a non-loopback address before opening any port and
// serves plaintext on loopback; with TLS it serves only TLS, which a hop
// client with the operator CA and server name verifies.
func TestServeConduitInternalListener_HopTransport(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "internal") })
	serve := func(t *testing.T, listen string, srvTLS *tls.Config) (string, error) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		t.Cleanup(func() { cancel(); wg.Wait() })
		addr, _, err := serveConduitInternalListener(ctx, listen, srvTLS, ok, &wg, make(chan error, 1))
		if err != nil {
			return "", err
		}
		return addr.String(), nil
	}

	for _, listen := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.10:0"} {
		_, err := serve(t, listen, nil)
		require.Error(t, err, listen)
		assert.True(t, errors.Is(err, config.ErrPlaintextNonLoopback), "%s: %v", listen, err)
		assert.Contains(t, err.Error(), "server.hub.conduit.internal_listen")
		assert.Contains(t, err.Error(), "internal_tls_ca_file", "the refusal says a private CA needs the CA file")
	}

	addr, err := serve(t, "127.0.0.1:0", nil)
	require.NoError(t, err, "loopback without TLS is allowed")
	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	files := relaytest.NewHopTLSFiles(t, hopTestServerName)
	srv, cli, err := conduitHopTLS(hopTestConduitConfig(files))
	require.NoError(t, err)
	addr, err = serve(t, "127.0.0.1:0", srv)
	require.NoError(t, err)
	hc := relay.NewHopHTTPClient(cli)
	hc.Timeout = 10 * time.Second
	resp, err = hc.Get("https://" + addr + "/")
	require.NoError(t, err, "a hop client with the operator CA and server name verifies the listener")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "internal", string(body))
	plain := &http.Client{Timeout: 10 * time.Second}
	if resp, err := plain.Get("http://" + addr + "/"); err == nil {
		_ = resp.Body.Close()
		assert.NotEqual(t, http.StatusOK, resp.StatusCode, "the TLS listener must not serve plaintext")
	}
}

// TestRelayRoleConfig_HopTransport: --role relay refuses a plaintext
// non-loopback hub internal URL or internal advertise, and with the TLS
// settings serves its internal listener with TLS and verifies peers.
func TestRelayRoleConfig_HopTransport(t *testing.T) {
	for name, mod := range map[string]func(*config.GlobalConfig){
		"plaintext hub internal URL": func(c *config.GlobalConfig) { c.Relay.HubInternalURL = "http://hub-internal.example:9811" },
		"plaintext advertise":        func(c *config.GlobalConfig) { c.Hub.Conduit.InternalAdvertise = "http://relay-7.internal:9811" },
		"wildcard listener, no TLS":  func(c *config.GlobalConfig) { c.Hub.Conduit.InternalListen = "0.0.0.0:9811" },
	} {
		cfg := relayRoleTestConfig()
		mod(cfg)
		err := validateRelayRoleConfig(cfg)
		assert.True(t, errors.Is(err, config.ErrPlaintextNonLoopback), "%s: %v", name, err)
	}

	files := relaytest.NewHopTLSFiles(t, hopTestServerName)
	cfg := relayRoleTestConfig()
	tlsCfg := hopTestConduitConfig(files)
	cfg.Hub.Conduit.InternalTLSCertFile, cfg.Hub.Conduit.InternalTLSKeyFile = tlsCfg.InternalTLSCertFile, tlsCfg.InternalTLSKeyFile
	cfg.Hub.Conduit.InternalTLSCAFile, cfg.Hub.Conduit.InternalTLSServerName = tlsCfg.InternalTLSCAFile, tlsCfg.InternalTLSServerName
	cfg.Hub.Conduit.InternalListen = "0.0.0.0:0"
	cfg.Hub.Conduit.InternalAdvertise = ""
	t.Setenv("POD_IP", "10.2.2.2")
	rcfg, err := relayServerConfig(context.Background(), cfg, &relaydb.DB{Settings: &relaydb.SettingsReader{}})
	require.NoError(t, err)
	assert.NotNil(t, rcfg.InternalTLS, "the internal listener serves TLS")
	require.NotNil(t, rcfg.HopTLS)
	assert.Equal(t, hopTestServerName, rcfg.HopTLS.ServerName)
	bound, err := net.ResolveTCPAddr("tcp", "0.0.0.0:9811")
	require.NoError(t, err)
	assert.Equal(t, "https://10.2.2.2:9811", rcfg.Advertise(bound))
}
