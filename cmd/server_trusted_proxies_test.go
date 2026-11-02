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

//go:build !no_sqlite

package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hub refuses to start with an invalid or over-wide server.trusted_proxies
// instead of dropping the entry; a valid or empty list starts.
func TestValidateServerPreflight_TrustedProxies(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for name, tc := range map[string]struct {
		list    []string
		wantErr string
	}{
		"empty":            {},
		"valid":            {list: []string{"10.20.0.0/16", "192.0.2.7"}},
		"not an address":   {list: []string{"proxy.example.com"}, wantErr: `"proxy.example.com"`},
		"catch-all IPv4":   {list: []string{"0.0.0.0/0"}, wantErr: `"0.0.0.0/0"`},
		"catch-all IPv6":   {list: []string{"::/0"}, wantErr: `"::/0"`},
		"wider than a /8":  {list: []string{"10.0.0.0/7"}, wantErr: `"10.0.0.0/7"`},
		"one bad of two":   {list: []string{"10.20.0.0/16", "nope"}, wantErr: `"nope"`},
		"empty list entry": {list: []string{""}, wantErr: config.TrustedProxiesKey},
	} {
		t.Run(name, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			cfg := &config.GlobalConfig{TrustedProxies: tc.list}
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refusing to start the hub")
			assert.Contains(t, err.Error(), config.TrustedProxiesKey)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// The setting reaches the hub only as the transport policy's list, never as
// the legacy TrustedProxies (proxy header login, client IP rate limits).
func TestBuildHubServerConfig_TrustedProxiesFeedTransportPolicyOnly(t *testing.T) {
	cfg := config.GlobalConfig{TrustedProxies: []string{"10.20.0.0/16"}}
	sc := buildHubServerConfig(&cfg, "", "", nil, false, "", nil)
	assert.Equal(t, []string{"10.20.0.0/16"}, sc.TransportTrustedProxies)
	assert.Empty(t, sc.TrustedProxies, "server.trusted_proxies must not enable proxy header login")
	cfg.TrustedProxies[0] = "192.0.2.0/24"
	assert.Equal(t, []string{"10.20.0.0/16"}, sc.TransportTrustedProxies, "the hub config holds its own copy")
}

// A co-located broker endpoint that is plain http on a non-loopback address
// is logged once, naming the endpoint and the setting; a loopback endpoint
// logs nothing.
func TestWarnColocatedBrokerEndpointTransport(t *testing.T) {
	for endpoint, warn := range map[string]bool{
		"http://localhost:9800":  false,
		"http://127.0.0.1:9800":  false,
		"http://10.0.0.5:9800":   true,
		"http://broker.lan:9800": true,
	} {
		t.Run(endpoint, func(t *testing.T) {
			buf, restore := captureSlog(t)
			defer restore()
			warnColocatedBrokerEndpointTransport(endpoint)
			out := buf.String()
			if !warn {
				assert.Empty(t, out)
				return
			}
			assert.Equal(t, 1, strings.Count(out, "level=WARN"), out)
			assert.Contains(t, out, endpoint)
			assert.Contains(t, out, "server.broker.host")
		})
	}
}

// The binding of the key to its field, through the real load path: with ONLY
// server.trusted_proxies set (from settings.yaml, from the canonical env
// name, or from its alias), the server's loaded config passes the startup
// check and the hub config built from it carries the list as
// TransportTrustedProxies while the legacy TrustedProxies (proxy header
// login, client IP rate limits) stays empty. initHubServer passes this
// hub.ServerConfig to hub.New; no code in cmd sets TrustedProxies.
func TestTrustedProxies_LoadPathBindsTransportListOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		env  map[string]string
	}{
		"settings.yaml": {file: "schema_version: \"1\"\nserver:\n  trusted_proxies:\n    - 10.20.0.0/16\n    - 192.0.2.7\n"},
		"env canonical": {file: "schema_version: \"1\"\n", env: map[string]string{"SCION_SERVER_TRUSTEDPROXIES": "10.20.0.0/16, 192.0.2.7"}},
		"env alias":     {file: "schema_version: \"1\"\n", env: map[string]string{"SCION_SERVER_TRUSTED_PROXIES": "10.20.0.0/16,192.0.2.7"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(resetServerFlags)
			resetServerFlags()
			t.Setenv("SCION_SERVER_MODE", "")
			for _, k := range []string{"SCION_SERVER_TRUSTEDPROXIES", "SCION_SERVER_TRUSTED_PROXIES"} {
				t.Setenv(k, "") // restored after the test
				require.NoError(t, os.Unsetenv(k))
			}
			writeServerSettings(t, tc.file)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := loadAndReconcileConfig(serverStartCmd)
			require.NoError(t, err)
			assert.Equal(t, []string{"10.20.0.0/16", "192.0.2.7"}, cfg.TrustedProxies, "GlobalConfig")

			enableHub = true
			require.NoError(t, validateServerPreflight(cfg))

			sc := buildHubServerConfig(cfg, "", "", nil, false, "", nil)
			assert.Equal(t, []string{"10.20.0.0/16", "192.0.2.7"}, sc.TransportTrustedProxies, "hub transport policy list")
			assert.Empty(t, sc.TrustedProxies, "the legacy trusted proxy list must stay empty")
		})
	}
}
