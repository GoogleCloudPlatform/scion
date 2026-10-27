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
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// The web base URL is the public URL of the web server. It is the base of
// the OAuth redirect_uri (<base>/auth/callback/<provider>), it decides the
// session cookie's Secure flag, and it is sent to the browser client.
//
// It is read only from operator inputs: the --base-url flag, the
// environment (SCION_SERVER_HUB_ENDPOINT, SCION_SERVER_BASE_URL) and the
// GLOBAL settings FILE (server.hub.public_url, and the IAP audience
// server.auth.proxy.iap.audience that a Cloud Run URL is derived from, or
// its SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE env override; the server loads
// the file from the global settings directory only). It is never
// read from the settings database or any value saved through the admin
// settings API: it is resolved at startup, before the store is opened. A runtime
// write through the admin API (from a compromised or mistaken web session)
// must not be able to redirect OAuth logins to another host or downgrade
// the session cookie; changing the web base URL takes operator access to
// the file or the environment and a restart (ptone/scion#5364).
//
// Order (ptone/scion#5364): --base-url, then SCION_SERVER_BASE_URL, then
// SCION_SERVER_HUB_ENDPOINT, then server.hub.public_url from the global
// settings file, then (hosted mode only) the Cloud Run URL derived from the
// IAP audience (the global settings file or its env override), then the default
// http://localhost:<port>. The two inputs
// the web server read before (--base-url and SCION_SERVER_BASE_URL) keep
// their relative order and win; SCION_SERVER_HUB_ENDPOINT and the file only
// fill in when neither is set. SCION_SERVER_HUB_ENDPOINT is documented as an
// override of the endpoint advertised to agents, so it must not move the
// OAuth redirect base away from SCION_SERVER_BASE_URL. Below those two, the
// hub-side values come from the hub's operator endpoint resolver
// (resolveHubEndpointFrom), so they cannot drift from it. The legacy
// settings hub.endpoint (SCION_HUB_ENDPOINT) is not followed, so for the
// web the IAP-derived URL comes right after server.hub.public_url.
//
// In a process that does not run the hub (the web server with the Runtime
// Broker only), server.hub.public_url and SCION_SERVER_HUB_ENDPOINT name the
// hub the broker reports to, which can be another host; they are not
// followed there: only --base-url, then SCION_SERVER_BASE_URL, then the
// default.
//
// The scheme is lower-cased and a trailing "/" removed, so every consumer
// (the redirect URI, the cookie's Secure check on the "https://" prefix,
// the client config) sees the same value the listener check accepted.

// webBaseURLFollowsIAPAudience decides whether the web base URL follows a
// hub endpoint derived from the IAP audience (a Cloud Run IAP front end;
// see iapAudienceToCloudRunURL). That URL is built from operator
// configuration only (server.auth.proxy.iap.audience in the global settings
// file or its SCION_SERVER_AUTH_PROXY_IAP_AUDIENCE env override, never the
// settings database) and is https by construction. It is
// one of the two availability decisions of ptone/scion#5364, decided true:
// a Cloud Run IAP deployment with no explicit base URL starts with the
// derived https URL instead of the default, which a non-loopback listener
// refuses.
const webBaseURLFollowsIAPAudience = true

// webBaseURLRefuseDefault decides whether the non-loopback refusal
// (checkWebBaseURLListener) also covers the default
// http://localhost:<port> base URL, used when no operator input sets one.
// It is the other availability decision of ptone/scion#5364, decided true.
const webBaseURLRefuseDefault = true

// webBaseURLSourceDefault is the source of the default base URL.
const webBaseURLSourceDefault = "default"

// resolvedWebBaseURL is the resolved web base URL and the input it came from, as
// logged at startup and named by the listener check.
type resolvedWebBaseURL struct {
	URL    string
	Source string
}

// resolveWebBaseURL returns the web base URL for mode m (see above).
// globalSettingsFile names the global settings file in the source label of
// a server.hub.public_url value.
func resolveWebBaseURL(cfg *config.GlobalConfig, m serverEndpointMode, globalSettingsFile string) resolvedWebBaseURL {
	normalize := func(raw string) string { return lowerScheme(strings.TrimRight(raw, "/")) }
	if m.baseURLFlag != "" {
		return resolvedWebBaseURL{URL: normalize(m.baseURLFlag), Source: "--base-url"}
	}
	// Explicit override step: SCION_SERVER_BASE_URL deliberately outranks
	// SCION_SERVER_HUB_ENDPOINT (and the file) for the web, the opposite of
	// the hub endpoint order, which ranks SCION_SERVER_HUB_ENDPOINT first.
	// The reason: SCION_SERVER_HUB_ENDPOINT is the agent-facing hub endpoint
	// override, and it must not be able to move the OAuth redirect base or
	// the session cookie's Secure decision away from SCION_SERVER_BASE_URL.
	// With both set, one process has two different URLs (the hub endpoint
	// for agents, the base URL for the web), by design. Do not harmonise
	// the two orders: that would silently reintroduce the problem.
	if baseURL := os.Getenv("SCION_SERVER_BASE_URL"); baseURL != "" {
		return resolvedWebBaseURL{URL: normalize(baseURL), Source: "SCION_SERVER_BASE_URL"}
	}
	def := resolvedWebBaseURL{URL: fmt.Sprintf("http://localhost:%d", m.webPort), Source: webBaseURLSourceDefault}
	if !m.hubEnabled {
		// No hub in this process: the hub endpoint names a remote hub.
		return def
	}
	endpoint, src := resolveHubEndpointFrom(cfg, m, "", nil)
	switch src {
	case hubEndpointSourceConfig:
		if os.Getenv(hubEndpointEnvVar) != "" {
			return resolvedWebBaseURL{URL: normalize(endpoint), Source: hubEndpointEnvVar}
		}
		file := globalSettingsFile
		if file == "" {
			file = "the global settings file"
		}
		return resolvedWebBaseURL{URL: normalize(endpoint), Source: "server.hub.public_url in " + file}
	case hubEndpointSourceIAPAudience:
		if webBaseURLFollowsIAPAudience {
			return resolvedWebBaseURL{URL: normalize(endpoint), Source: "derived from the IAP audience (server.auth.proxy.iap.audience)"}
		}
	}
	return def
}

// lowerScheme lower-cases the scheme of raw ("HTTPS://h" -> "https://h");
// a value without "://" is returned as it is.
func lowerScheme(raw string) string {
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		return strings.ToLower(scheme) + "://" + rest
	}
	return raw
}

// webListenHost is the web server's listen host: cfg.Hub.Host, with an
// empty value normalised to 0.0.0.0 (what the listener binds) before any
// check.
func webListenHost(cfg *config.GlobalConfig) string {
	if cfg.Hub.Host == "" {
		return "0.0.0.0"
	}
	return cfg.Hub.Host
}

// devAuthBindError is the refusal for dev auth on a non-loopback listen
// host (the message initWebServer has always used).
func devAuthBindError(webHost string) error {
	return fmt.Errorf(
		"dev auth cannot be enabled when the server is bound to a non-loopback address (%s). "+
			"Dev auth signs browsers in as admin and must only be used on localhost. "+
			"Either bind to 127.0.0.1/::1/localhost (--host 127.0.0.1) or disable dev auth", webHost)
}

// webStartupPreflight runs the web server's listener checks at startup,
// before the store, dev auth and the hub are initialised: first the
// dev-auth bind check (dev auth is enabled in the config), then the web
// base URL check. It returns the web base URL, resolved once here and
// passed to initWebServer, and logs it with its source.
func webStartupPreflight(cfg *config.GlobalConfig, m serverEndpointMode, globalSettingsFile string) (resolvedWebBaseURL, error) {
	webHost := webListenHost(cfg)
	// Gated on cfg.Auth.Enabled (dev auth configured), which is deliberately
	// stricter than initWebServer's devAuthToken != "" check: it refuses
	// before a token exists. Do not align the two by loosening this one.
	if cfg.Auth.Enabled && !hub.IsLoopbackHost(webHost) {
		return resolvedWebBaseURL{}, devAuthBindError(webHost)
	}
	base := resolveWebBaseURL(cfg, m, globalSettingsFile)
	if err := checkWebBaseURLListener(base, webHost, m.webPort); err != nil {
		return resolvedWebBaseURL{}, err
	}
	log.Printf("Web base URL: %s (from %s)", base.URL, base.Source)
	return base, nil
}

// globalSettingsFileLabel returns the path of the global settings file for
// source labels, or "" when it cannot be resolved.
func globalSettingsFileLabel() string {
	dir, err := config.GetGlobalDir()
	if err != nil {
		return ""
	}
	return config.GetSettingsPath(dir)
}

// checkWebBaseURLListener validates the web base URL on every listener,
// then refuses a non-loopback listener whose base URL is not https.
//
// First, on every listener (loopback included), the base URL must parse,
// use the http or https scheme and carry a host; otherwise the server does
// not start, so no consumer (the redirect URI, the cookie's Secure
// decision, the client config) ever sees a malformed value. Then a web
// server that listens on a non-loopback address with a base URL that is
// not https is refused: OAuth callbacks and the session cookie would cross
// the network unencrypted. http stays allowed on a loopback listener.
// Loopback is decided from the literal listen host only (127.0.0.0/8, ::1
// and the name "localhost"; config.IsLoopbackHost): a name is never
// resolved, so a hosts-file name that points at 127.0.0.1 counts as not
// loopback. Each error names the base URL and its source; the listener
// refusal also names the listen address and both fixes.
func checkWebBaseURLListener(base resolvedWebBaseURL, listenHost string, listenPort int) error {
	u, err := url.Parse(base.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("refusing to start the web server: its base URL %s (from %s) is not a valid http or https URL "+
			"with a host. Set --base-url, SCION_SERVER_BASE_URL or server.hub.public_url in the global settings file "+
			"to a URL such as https://hub.example.com", base.URL, base.Source)
	}
	// The loopback rule, stated so it is not "fixed" either way later:
	// "localhost" is accepted as a literal name by convention, not because
	// anything resolved it; every other name is refused without being
	// resolved. These two rules are not in tension. Resolving "localhost"
	// (or any name) would let a hosts-file or DNS entry turn a public
	// listener into a "loopback" one and reopen the bypass; dropping
	// "localhost" would break local development, which listens on it.
	if config.IsLoopbackHost(listenHost) {
		return nil
	}
	if base.Source == webBaseURLSourceDefault && !webBaseURLRefuseDefault {
		return nil
	}
	if u.Scheme == "https" {
		return nil
	}
	listen := net.JoinHostPort(listenHost, strconv.Itoa(listenPort))
	return fmt.Errorf("refusing to start the web server: its base URL %s (from %s) is not https, "+
		"and it listens on %s, which is not a loopback address, so OAuth callbacks and session cookies "+
		"would cross the network unencrypted. Use an https base URL (--base-url, SCION_SERVER_BASE_URL "+
		"or server.hub.public_url in the global settings file), or listen on a loopback host "+
		"(--host 127.0.0.1)", base.URL, base.Source, listen)
}
