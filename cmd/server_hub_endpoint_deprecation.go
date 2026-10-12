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
	"log"
	"net"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// serverHubEndpointDeprecationWarning is logged once at server startup, and
// reported by 'scion doctor', when the server takes a hub endpoint from the
// CLI's hub.endpoint setting or SCION_HUB_ENDPOINT. That fallback is removed
// in the next release; the server-scoped keys replace it.
const serverHubEndpointDeprecationWarning = "deprecated: the server's hub endpoint came from hub.endpoint/SCION_HUB_ENDPOINT; set server.hub.public_url (SCION_SERVER_HUB_ENDPOINT) or server.broker.hub_endpoint (SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT)."

// serverHubEndpointDeprecated reports whether a server in mode m takes a hub
// endpoint from legacyEndpoint (the settings hub.endpoint, which koanf also
// fills from SCION_HUB_ENDPOINT) in a way that removing that fallback would
// change. It resolves the advertised endpoint, and the broker's reporting
// endpoint when the broker is enabled, with and without the legacy value and
// compares the results. A workstation whose hub.endpoint is its own loopback
// URL therefore does not count: without it the localhost fallback yields the
// same endpoint.
func serverHubEndpointDeprecated(cfg *config.GlobalConfig, m serverEndpointMode, legacyEndpoint string) bool {
	if legacyEndpoint == "" {
		return false
	}
	used, _ := resolveHubEndpointFrom(cfg, m, legacyEndpoint, nil)
	without, _ := resolveHubEndpointFrom(cfg, m, "", nil)
	if !sameServerHubEndpoint(used, without) {
		return true
	}
	if m.brokerEnabled {
		used := resolveBrokerHubEndpointFrom(cfg, m, legacyEndpoint, nil)
		without := resolveBrokerHubEndpointFrom(cfg, m, "", nil)
		if !sameServerHubEndpoint(used, without) {
			return true
		}
	}
	return false
}

// warnDeprecatedServerHubEndpoint logs serverHubEndpointDeprecationWarning
// when serverHubEndpointDeprecated holds. Server startup calls it once,
// after resolving the hub endpoint, so the warning appears once whichever
// paths use the legacy value.
func warnDeprecatedServerHubEndpoint(cfg *config.GlobalConfig, m serverEndpointMode, settings *config.Settings, logf func(string, ...any)) {
	if serverHubEndpointDeprecated(cfg, m, settings.GetHubEndpoint()) {
		logf("%s", serverHubEndpointDeprecationWarning)
	}
}

// sameServerHubEndpoint reports whether a and b name the same endpoint,
// ignoring a trailing slash and treating loopback hosts (localhost,
// 127.0.0.0/8, ::1) as one host.
func sameServerHubEndpoint(a, b string) bool {
	a, b = strings.TrimRight(a, "/"), strings.TrimRight(b, "/")
	if a == b {
		return true
	}
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		ua.Path == ub.Path &&
		urlPort(ua) == urlPort(ub) &&
		isLoopbackHost(ua.Hostname()) && isLoopbackHost(ub.Hostname())
}

// urlPort returns u's port, or the scheme's default port when none is given.
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// logServerHubEndpointDeprecation is the startup hook: it checks the running
// server's mode against its loaded settings.
func logServerHubEndpointDeprecation(cfg *config.GlobalConfig, settings *config.Settings) {
	warnDeprecatedServerHubEndpoint(cfg, currentServerEndpointMode(), settings, log.Printf)
}
