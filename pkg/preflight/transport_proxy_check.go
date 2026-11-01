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

package preflight

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TransportProxyCheckName is the name of the trusted proxy check.
const TransportProxyCheckName = "settings.transport_proxy"

// transportProxyCheckTimeout bounds the settings.transport_proxy check.
const transportProxyCheckTimeout = 10 * time.Second

// loadTransportProxyConfig loads the server config the restarted hub will
// read; a test seam.
var loadTransportProxyConfig = config.LoadGlobalConfig

// TransportProxyCheck returns the settings.transport_proxy check. The hub's
// API listener is plain HTTP, so a hub whose public URL is https sits behind
// a TLS-terminating proxy. With server.trusted_proxies not set, the hub
// trusts only a proxy on its own host; a proxy on another host must be
// listed, or remote runtime brokers cannot connect. The check warns in that
// configuration, and when the config does not load, and never blocks (only
// the registry's timeout, which a local file read does not reach, could).
func TransportProxyCheck() Check {
	return Check{Name: TransportProxyCheckName, Timeout: transportProxyCheckTimeout, Run: runTransportProxyCheck}
}

// runTransportProxyCheck never returns an error, so the registry never
// turns it into a blocking could_not_run finding: a server config that does
// not load is a warning here (the hub's own startup reports the cause).
// The error text is logged only, since it may quote settings values.
func runTransportProxyCheck(_ context.Context, env Env) ([]Finding, error) {
	cfg, err := loadTransportProxyConfig(env.ServerConfigPath)
	if err != nil {
		slog.Warn("preflight: the server config does not load", "error", err)
		return []Finding{{
			Code:        TransportProxyCheckName + ".config_not_loaded",
			Severity:    SeverityWarn,
			Message:     "the server config does not load, so " + config.TrustedProxiesKey + " could not be checked",
			Remediation: "fix the server config; the hub reports the cause when it starts",
		}}, nil
	}
	return transportProxyFindings(cfg), nil
}

// transportProxyFindings returns the warning for a hub whose public URL is
// https while server.trusted_proxies is not set, or nothing.
func transportProxyFindings(cfg *config.GlobalConfig) []Finding {
	if cfg == nil || cfg.TrustedProxiesSet || !publicURLIsHTTPS(cfg.Hub.Endpoint) {
		return nil
	}
	return []Finding{{
		Code:     TransportProxyCheckName + ".trusted_proxies_not_set",
		Severity: SeverityWarn,
		Message: "the hub's public URL is https and " + config.TrustedProxiesKey + " is not set: " +
			"a TLS-terminating proxy on this host is trusted by default, but a proxy on another host must be listed in " +
			config.TrustedProxiesKey + ", or remote runtime brokers cannot connect",
		Remediation: "if TLS ends at a proxy on another host (a load balancer or ingress), list its addresses in " +
			config.TrustedProxiesKey + "; to confirm the default, set it to the proxy's loopback addresses (127.0.0.1, ::1), " +
			"or to [] to trust no proxy",
	}}
}

func publicURLIsHTTPS(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(u.Scheme, "https")
}
