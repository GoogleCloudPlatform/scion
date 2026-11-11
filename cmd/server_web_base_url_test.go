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

// The web base URL order: --base-url, SCION_SERVER_BASE_URL,
// SCION_SERVER_HUB_ENDPOINT, server.hub.public_url from the global settings
// file, then (hosted mode, Cloud Run IAP audience; see
// TestResolveWebBaseURL_IAPAudienceDecision) the IAP-derived URL, then the
// default; the legacy settings hub.endpoint is not followed
// (ptone/scion#5364).
func TestResolveWebBaseURL_Order(t *testing.T) {
	const file = "/home/u/.scion/settings.yaml"
	mode := serverEndpointMode{hubEnabled: true, webEnabled: true, webPort: 8080}

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
	t.Run("SCION_SERVER_BASE_URL outranks SCION_SERVER_HUB_ENDPOINT", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv(hubEndpointEnvVar, "https://agents.internal.example.com")
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://agents.internal.example.com" // applied over the file value when the config loads
		got := resolveWebBaseURL(cfg, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://env.example.com", Source: "SCION_SERVER_BASE_URL"}, got)
	})
	t.Run("only SCION_SERVER_HUB_ENDPOINT set: it is used", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv(hubEndpointEnvVar, "https://hubenv.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://hubenv.example.com"
		got := resolveWebBaseURL(cfg, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://hubenv.example.com", Source: hubEndpointEnvVar}, got)
	})
	t.Run("upper-case scheme is lower-cased", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_SERVER_BASE_URL", "HTTPS://Hub.Example.com/")
		got := resolveWebBaseURL(&config.GlobalConfig{}, mode, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://Hub.Example.com", Source: "SCION_SERVER_BASE_URL"}, got)
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
	t.Run("no hub in the process: SCION_SERVER_BASE_URL is used", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_SERVER_BASE_URL", "https://env.example.com")
		m := mode
		m.hubEnabled = false
		got := resolveWebBaseURL(&config.GlobalConfig{}, m, file)
		assert.Equal(t, "SCION_SERVER_BASE_URL", got.Source)
	})
	t.Run("no hub in the process: the file value (a remote hub) is not followed", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://remote-hub.example.com"
		m := mode
		m.hubEnabled = false
		got := resolveWebBaseURL(cfg, m, file)
		assert.Equal(t, resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, got)
	})
	t.Run("no hub in the process: SCION_SERVER_HUB_ENDPOINT (a remote hub) is not followed", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv(hubEndpointEnvVar, "https://remote-hub.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Endpoint = "https://remote-hub.example.com"
		m := mode
		m.hubEnabled = false
		got := resolveWebBaseURL(cfg, m, file)
		assert.Equal(t, "default", got.Source)
	})
}

// The IAP-derived hub endpoint is followed: this pins the D3 decision
// (webBaseURLFollowsIAPAudience = true, ptone/scion#5364) as a literal, so
// flipping the const back must fail this test, deliberately and visibly.
// The derived URL is https, so a non-loopback listener accepts it.
func TestResolveWebBaseURL_IAPAudienceDecision(t *testing.T) {
	webBaseURLTestEnv(t)
	cfg := &config.GlobalConfig{}
	cfg.Auth.Proxy = &config.ProxyAuthConfig{IAP: &config.IAPAuthConfig{Audience: "/projects/123/locations/us-central1/services/hub"}}
	mode := serverEndpointMode{hubEnabled: true, webEnabled: true, webPort: 8080, hosted: true}
	got := resolveWebBaseURL(cfg, mode, "")
	assert.Equal(t, resolvedWebBaseURL{URL: "https://hub-123.us-central1.run.app", Source: "derived from the IAP audience (server.auth.proxy.iap.audience)"}, got)
	assert.NoError(t, checkWebBaseURLListener(got, "0.0.0.0", 8080))

	// The hub endpoint resolver derives it in hosted mode only.
	notHosted := mode
	notHosted.hosted = false
	assert.Equal(t, resolvedWebBaseURL{URL: "http://localhost:8080", Source: "default"}, resolveWebBaseURL(cfg, notHosted, ""))

	// server.hub.public_url outranks the derived URL.
	withFile := &config.GlobalConfig{}
	withFile.Auth.Proxy = cfg.Auth.Proxy
	withFile.Hub.Endpoint = "https://public.example.com"
	assert.Equal(t, "https://public.example.com", resolveWebBaseURL(withFile, mode, "").URL)

	// An explicit base URL still wins over the derived one.
	t.Setenv("SCION_SERVER_BASE_URL", "https://scion.example.com")
	got = resolveWebBaseURL(cfg, mode, "")
	assert.Equal(t, resolvedWebBaseURL{URL: "https://scion.example.com", Source: "SCION_SERVER_BASE_URL"}, got)
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
		{"upper-case HTTPS on a public listener", resolvedWebBaseURL{URL: "HTTPS://hub.example.com", Source: "--base-url"}, "0.0.0.0", false},
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

// The base URL must parse, use http or https and carry a host on EVERY
// listener, loopback included, so no consumer sees a malformed value;
// http stays allowed on a loopback listener (ptone/scion#5364).
func TestCheckWebBaseURLListener_MalformedRefusedOnEveryListener(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "0.0.0.0"} {
		for _, raw := range []string{"https://localhost:8443/%zz", "https://host:abc", "://bad", "https://", "https://:443", "ftp://hub.example.com"} {
			err := checkWebBaseURLListener(resolvedWebBaseURL{URL: raw, Source: "--base-url"}, host, 8080)
			require.Error(t, err, "listen host %q, base URL %q", host, raw)
			assert.Contains(t, err.Error(), "is not a valid http or https URL with a host", "listen host %q, base URL %q", host, raw)
			assert.Contains(t, err.Error(), raw)
			assert.Contains(t, err.Error(), "(from --base-url)")
		}
	}
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		assert.NoError(t, checkWebBaseURLListener(resolvedWebBaseURL{URL: "http://127.0.0.1:8080", Source: "--base-url"}, host, 8080), "http on loopback %q", host)
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

// webStartupPreflight runs at startup before the store and the hub: the
// dev-auth bind check first (same message as initWebServer), then the web
// base URL check, and returns the resolved base URL (ptone/scion#5364).
func TestWebStartupPreflight(t *testing.T) {
	mode := serverEndpointMode{hubEnabled: true, webEnabled: true, webPort: 8080}
	t.Run("default base URL on 0.0.0.0 is refused", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Host = ""
		_, err := webStartupPreflight(cfg, mode, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to start the web server")
		assert.Contains(t, err.Error(), "listens on 0.0.0.0:8080")
	})
	t.Run("https base URL on 0.0.0.0 passes and is returned", func(t *testing.T) {
		webBaseURLTestEnv(t)
		t.Setenv("SCION_SERVER_BASE_URL", "https://hub.example.com")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Host = "0.0.0.0"
		got, err := webStartupPreflight(cfg, mode, "")
		require.NoError(t, err)
		assert.Equal(t, resolvedWebBaseURL{URL: "https://hub.example.com", Source: "SCION_SERVER_BASE_URL"}, got)
	})
	t.Run("dev auth on a non-loopback host is refused first, with the dev-auth message", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Host = "0.0.0.0"
		cfg.Auth.Enabled = true
		_, err := webStartupPreflight(cfg, mode, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dev auth cannot be enabled")
		assert.NotContains(t, err.Error(), "refusing to start the web server")
	})
	t.Run("http on a loopback host with dev auth passes", func(t *testing.T) {
		webBaseURLTestEnv(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Host = "127.0.0.1"
		cfg.Auth.Enabled = true
		got, err := webStartupPreflight(cfg, mode, "")
		require.NoError(t, err)
		assert.Equal(t, "default", got.Source)
	})
}

// SCION_SERVER_BASE_URL outranks SCION_SERVER_HUB_ENDPOINT for the web, the
// opposite of the hub endpoint order, so the agent-facing hub endpoint
// override cannot move the OAuth redirect base or the cookie's Secure
// decision. With both set, the web and the hub endpoint use two different
// URLs in one process, by design (ptone/scion#5364).
func TestWebBaseURL_AgentFacingHubEndpointCannotMoveTheOAuthBase(t *testing.T) {
	webBaseURLTestEnv(t)
	t.Setenv("SCION_SERVER_BASE_URL", "https://hub.example.com")
	t.Setenv(hubEndpointEnvVar, "https://agents.internal.example.com")
	cfg := &config.GlobalConfig{}
	cfg.Hub.Endpoint = "https://agents.internal.example.com" // applied over the file value when the config loads
	mode := serverEndpointMode{hubEnabled: true, webEnabled: true, webPort: 8080}

	web := resolveWebBaseURL(cfg, mode, "")
	hubEndpoint, hubSrc := resolveHubEndpointFrom(cfg, mode, "", nil)

	assert.Equal(t, resolvedWebBaseURL{URL: "https://hub.example.com", Source: "SCION_SERVER_BASE_URL"}, web)
	assert.Equal(t, "https://agents.internal.example.com", hubEndpoint)
	assert.Equal(t, hubEndpointSourceConfig, hubSrc)
}
