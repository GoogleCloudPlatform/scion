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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// webBaseURLTestEnv clears the operator inputs the resolver reads.
func webBaseURLTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv(hubEndpointEnvVar, "")
	t.Setenv("SCION_SERVER_BASE_URL", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
}

// The web base URL follows the hub's operator endpoint order (--base-url,
// SCION_SERVER_HUB_ENDPOINT, SCION_SERVER_BASE_URL, server.hub.public_url
// from the global settings file), and otherwise the default; the legacy
// settings hub.endpoint is not followed (ptone/scion#5364).
func TestResolveWebBaseURL_Order(t *testing.T) {
	const file = "/home/u/.scion/settings.yaml"
	mode := serverEndpointMode{webEnabled: true, webPort: 8080}

	t.Run("default", func(t *testing.T) {
		webBaseURLTestEnv(t)
		got := resolveWebBaseURL(&config.GlobalConfig{}, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, got)
	})
	t.Run("global settings file public_url", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://file.example.com/"
		got := resolveWebBaseURL(cfg, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://file.example.com", Source: "server.hub.public_url in " + file}, got)
	})
	t.Run("global settings file label fallback", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://file.example.com"
		got := resolveWebBaseURL(cfg, mode, "")
		assert.Equal(t, "server.hub.public_url in the global settings file", got.Source)
	})
	t.Run("SCION_SERVER_BASE_URL outranks the file", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://file.example.com"
		got := resolveWebBaseURL(cfg, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://env.example.com", Source: "SCION_SERVER_BASE_URL"}, got)
	})
	t.Run("SCION_SERVER_HUB_ENDPOINT outranks SCION_SERVER_BASE_URL", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv(hubEndpointEnvVar, "https://hubenv.example.com")
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://hubenv.example.com" // applied over the file value when the config loads
		got := resolveWebBaseURL(cfg, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://hubenv.example.com", Source: hubEndpointEnvVar}, got)
	})
	t.Run("--base-url outranks everything", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv(hubEndpointEnvVar, "https://hubenv.example.com")
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://hubenv.example.com"
		m := mode
		m.baseURLFlag = "https://flag.example.com/"
		got := resolveWebBaseURL(cfg, m, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://flag.example.com", Source: "--base-url"}, got)
	})
	t.Run("the legacy settings hub.endpoint is not followed", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_HUB_ENDPOINT", "https://legacy.example.com")
		got := resolveWebBaseURL(&config.GlobalConfig{}, mode, file)
		assert.Equal(t, "default", got.Source)
	})
	t.Run("hub disabled in the mode still uses the hub order", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		m := mode
		m.hubEnabled = false
		got := resolveWebBaseURL(&config.GlobalConfig{}, m, file)
		assert.Equal(t, "SCION_SERVER_BASE_URL", got.Source)
	})
}

// The IAP-derived hub endpoint is followed only when the
// webBaseURLFollowsIAPAudience decision is on (ptone/scion#5364).
func TestResolveWebBaseURL_IAPAudienceDecision(t *testing.T) {
	webBaseURLTestEnv(t)
	cfg := &config.GlobalConfig{}
	cfg.Auth.Proxy = &config.ProxyAuthConfig{IAP: &config.IAPAuthConfig{Audience: "/projects/123/locations/us-central1/services/hub"}}
	mode := serverEndpointMode{webEnabled: true, webPort: 8080, hosted: true}
	got := resolveWebBaseURL(cfg, mode, "")
	if webBaseURLFollowsIAPAudience {
		assert.Equal(t, "https://hub-123.us-central1.run.app", got.URL)
		assert.Equal(t, "derived from the IAP audience (server.auth.proxy.iap.audience)", got.Source)
	} else {
		assert.Equal(t, resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, got)
	}
}

// A non-loopback listener with a base URL that is not https is refused,
// and the message names the URL, its source, the listen address, that it
// is not loopback, and both fixes (ptone/scion#5364).
func TestCheckWebBaseURLListener_RefusalMessage(t *testing.T) {
	err := checkWebBaseURLListener(resolvedWebBaseURL{URL: "http://hub.example.com:8080", Source: "SCION_SERVER_BASE_URL"}, "0.0.0.0", 8080)
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{
		"refusing to start the web server",
		"http://hub.example.com:8080",
		"(from SCION_SERVER_BASE_URL)",
		"listens on 0.0.0.0:8080",
		"not a loopback address",
		"Use an https base URL",
		"--base-url, SCION_SERVER_BASE_URL or server.hub.public_url",
		"listen on a loopback host (--host 127.0.0.1)",
	} {
		assert.Contains(t, msg, want)
	}

	err = checkWebBaseURLListener(resolvedWebBaseURL{URL: "http://x", Source: "--base-url"}, "::", 9090)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listens on [::]:9090")
}

func TestCheckWebBaseURLListener_Cases(t *testing.T) {
	https := resolvedWebBaseURL{URL: "https://hub.example.com", Source: "--base-url"}
	http := resolvedWebBaseURL{URL: "http://hub.example.com", Source: "--base-url"}
	for _, tc := range []struct {
		name   string
		base   resolvedWebBaseURL
		host   string
		refuse bool
	}{
		{"https on a public listener", https, "0.0.0.0", false},
		{"http on 127.0.0.1", http, "127.0.0.1", false},
		{"http on 127.1.2.3", http, "127.1.2.3", false},
		{"http on ::1", http, "::1", false},
		{"http on localhost", http, "localhost", false},
		{"http on 0.0.0.0", http, "0.0.0.0", true},
		{"http on ::", http, "::", true},
		{"http on a private address", http, "192.168.1.10", true},
		{"http on localhost with a trailing dot", http, "localhost.", true},
		{"unparsable base URL", resolvedWebBaseURL{URL: "://bad", Source: "--base-url"}, "0.0.0.0", true},
		{"https without a host", resolvedWebBaseURL{URL: "https://", Source: "--base-url"}, "0.0.0.0", true},
		{"another scheme", resolvedWebBaseURL{URL: "ftp://hub.example.com", Source: "--base-url"}, "0.0.0.0", true},
		// Pins the current D2 decision (webBaseURLRefuseDefault = true,
		// ptone/scion#5364) as a literal: flipping the const must change
		// this row, deliberately and visibly.
		{"the default on a public listener", resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, "0.0.0.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWebBaseURLListener(tc.base, tc.host, 8080)
			if tc.refuse {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A hostname resolving to loopback does not qualify: the listener check
// never resolves a name, so a hosts-file name that points at 127.0.0.1
// (ip6-localhost, localhost.localdomain) or a public DNS name that does
// (localtest.me) is refused with an http base URL (ptone/scion#5364).
func TestCheckWebBaseURLListener_AHostnameResolvingToLoopbackDoesNotQualify(t *testing.T) {
	http := resolvedWebBaseURL{URL: "http://hub.example.com", Source: "SCION_SERVER_BASE_URL"}
	for _, host := range []string{"ip6-localhost", "localhost.localdomain", "localtest.me"} {
		assert.Error(t, checkWebBaseURLListener(http, host, 8080), "listen host %q", host)
	}
}
