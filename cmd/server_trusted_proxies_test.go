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
	"context"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

// server.trusted_proxies populates both TrustedProxies (client-IP extraction)
// and TransportTrustedProxies, with independent copies. When unset,
// TrustedProxies stays nil and TransportTrustedProxiesSet is false; when
// explicitly empty, both are empty and TransportTrustedProxiesSet is true.
func TestBuildHubServerConfig_TrustedProxies(t *testing.T) {
	t.Run("set list", func(t *testing.T) {
		cfg := config.GlobalConfig{
			TrustedProxies:    []string{"10.20.0.0/16"},
			TrustedProxiesSet: true,
		}
		sc := buildHubServerConfig(&cfg, "", "", nil, false, "", nil)
		assert.Equal(t, []string{"10.20.0.0/16"}, sc.TransportTrustedProxies)
		assert.Equal(t, []string{"10.20.0.0/16"}, sc.TrustedProxies)
		assert.True(t, sc.TransportTrustedProxiesSet)

		cfg.TrustedProxies[0] = "192.0.2.0/24"
		assert.Equal(t, []string{"10.20.0.0/16"}, sc.TransportTrustedProxies, "the hub config holds its own copy")
		assert.Equal(t, []string{"10.20.0.0/16"}, sc.TrustedProxies, "client-IP list holds its own copy")
	})

	t.Run("unset", func(t *testing.T) {
		cfg := config.GlobalConfig{}
		sc := buildHubServerConfig(&cfg, "", "", nil, false, "", nil)
		assert.Nil(t, sc.TrustedProxies)
		assert.Nil(t, sc.TransportTrustedProxies)
		assert.False(t, sc.TransportTrustedProxiesSet)
	})

	t.Run("explicit empty list", func(t *testing.T) {
		cfg := config.GlobalConfig{
			TrustedProxies:    []string{},
			TrustedProxiesSet: true,
		}
		sc := buildHubServerConfig(&cfg, "", "", nil, false, "", nil)
		assert.Empty(t, sc.TrustedProxies)
		assert.Empty(t, sc.TransportTrustedProxies)
		assert.True(t, sc.TransportTrustedProxiesSet)
	})
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

// The binding of the key to its fields, through the real load path: with
// server.trusted_proxies set (from settings.yaml, from the canonical env
// name, or from its alias), the server's loaded config passes the startup
// check and the hub config built from it carries the list in both
// TransportTrustedProxies and TrustedProxies.
func TestTrustedProxies_LoadPathBindsHubLists(t *testing.T) {
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
			assert.Equal(t, []string{"10.20.0.0/16", "192.0.2.7"}, sc.TrustedProxies, "hub client-IP trusted proxy list")
		})
	}
}

// auth.proxy.provider "header" is rejected at startup by both the Hub and web
// server constructors.
func TestInitProxyAuth_HeaderProviderRejected(t *testing.T) {
	const wantErr = `auth.proxy.provider "header" is not supported; use "iap" or "jwt"`

	cfg := &config.GlobalConfig{}
	cfg.Hub.Host = "127.0.0.1"
	cfg.Auth.Mode = "proxy"
	cfg.Auth.Proxy = &config.ProxyAuthConfig{Provider: "header"}

	t.Run("hub", func(t *testing.T) {
		var stubStore struct{ store.Store }
		_, err := initHubServer(context.Background(), cfg, &stubStore, nil, hub.HubEndpoints{}, "", nil, false, "", nil, nil, nil, "", nil, nil)
		require.Error(t, err)
		assert.EqualError(t, err, wantErr)
	})

	t.Run("web", func(t *testing.T) {
		_, err := initWebServer(context.Background(), cfg, nil, "", resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, false, "", nil, nil)
		require.Error(t, err)
		assert.EqualError(t, err, wantErr)
	})
}

func TestLogProxyTrustedProxiesHints(t *testing.T) {
	const (
		wantProxyHint      = "auth.mode is proxy and server.trusted_proxies is not set"
		wantDeprecatedWarn = "server.auth.proxy.require_trusted_proxy_ip is deprecated and has no effect"
	)

	t.Run("proxy mode with unset trusted_proxies logs info hint", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{}
		cfg.Auth.Mode = "proxy"
		logProxyTrustedProxiesHints(cfg)
		out := buf.String()
		assert.Contains(t, out, "level=INFO")
		assert.Contains(t, out, wantProxyHint)
		assert.NotContains(t, out, wantDeprecatedWarn)
	})

	t.Run("oauth mode with unset trusted_proxies logs nothing", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{}
		cfg.Auth.Mode = "oauth"
		logProxyTrustedProxiesHints(cfg)
		assert.Empty(t, buf.String())
	})

	t.Run("proxy mode with set trusted_proxies logs nothing", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{
			TrustedProxies:    []string{"10.0.0.0/8"},
			TrustedProxiesSet: true,
		}
		cfg.Auth.Mode = "proxy"
		logProxyTrustedProxiesHints(cfg)
		assert.Empty(t, buf.String())
	})

	t.Run("proxy mode with explicit empty trusted_proxies logs nothing", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{
			TrustedProxies:    []string{},
			TrustedProxiesSet: true,
		}
		cfg.Auth.Mode = "proxy"
		logProxyTrustedProxiesHints(cfg)
		assert.Empty(t, buf.String())
	})

	t.Run("require_trusted_proxy_ip true logs deprecation WARN", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{
			TrustedProxies:    []string{"10.0.0.0/8"},
			TrustedProxiesSet: true,
		}
		cfg.Auth.Mode = "proxy"
		cfg.Auth.Proxy = &config.ProxyAuthConfig{
			Provider:              "iap",
			RequireTrustedProxyIP: true,
		}
		logProxyTrustedProxiesHints(cfg)
		out := buf.String()
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, wantDeprecatedWarn)
		assert.NotContains(t, out, wantProxyHint)
	})

	t.Run("require_trusted_proxy_ip false logs nothing", func(t *testing.T) {
		buf, restore := captureSlog(t)
		defer restore()
		cfg := &config.GlobalConfig{
			TrustedProxies:    []string{"10.0.0.0/8"},
			TrustedProxiesSet: true,
		}
		cfg.Auth.Mode = "proxy"
		cfg.Auth.Proxy = &config.ProxyAuthConfig{
			Provider:              "iap",
			RequireTrustedProxyIP: false,
		}
		logProxyTrustedProxiesHints(cfg)
		assert.Empty(t, buf.String())
	})
}
