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
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The web base URL is the public URL of the web server. It is the base of
// the OAuth redirect_uri (<base>/auth/callback/<provider>), it decides the
// session cookie's Secure flag, and it is sent to the browser client.
//
// It is read only from operator inputs: the --base-url flag, the
// environment (SCION_SERVER_HUB_ENDPOINT, SCION_SERVER_BASE_URL) and the
// GLOBAL settings FILE (server.hub.public_url, which the server loads from
// the global settings directory only). It is never read from the settings
// database or any value saved through the admin settings API. A runtime
// write through the admin API (from a compromised or mistaken web session)
// must not be able to redirect OAuth logins to another host or downgrade
// the session cookie; changing the web base URL takes operator access to
// the file or the environment and a restart (ptone/scion#5364).
//
// It follows the hub's operator endpoint resolver (resolveHubEndpointFrom),
// so the hub endpoint and the web base URL cannot disagree on the order:
// --base-url, SCION_SERVER_HUB_ENDPOINT, SCION_SERVER_BASE_URL, then
// server.hub.public_url from the global settings file. The legacy settings
// hub.endpoint (SCION_HUB_ENDPOINT) is not followed.

// webBaseURLFollowsIAPAudience decides whether the web base URL follows a
// hub endpoint derived from the IAP audience (a Cloud Run IAP front end;
// see iapAudienceToCloudRunURL). That URL is built from operator
// configuration only and is https by construction. It is one of the two
// availability decisions of ptone/scion#5364 (pending): false keeps such a
// deployment on the default, which a non-loopback listener refuses.
const webBaseURLFollowsIAPAudience = false

// webBaseURLRefuseDefault decides whether the non-loopback refusal
// (checkWebBaseURLListener) also covers the default
// http://localhost:<port> base URL, used when no operator input sets one.
// It is the other availability decision of ptone/scion#5364 (pending).
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
	// The web server always fronts the hub, so the hub-enabled order
	// applies; the legacy settings endpoint is left out ("").
	m.hubEnabled = true
	endpoint, src := resolveHubEndpointFrom(cfg, m, "", nil)
	endpoint = strings.TrimRight(endpoint, "/")
	switch src {
	case hubEndpointSourceBaseURLFlag:
		return resolvedWebBaseURL{URL: endpoint, Source: "--base-url"}
	case hubEndpointSourceBaseURLEnv:
		return resolvedWebBaseURL{URL: endpoint, Source: "SCION_SERVER_BASE_URL"}
	case hubEndpointSourceConfig:
		if os.Getenv(hubEndpointEnvVar) != "" {
			return resolvedWebBaseURL{URL: endpoint, Source: hubEndpointEnvVar}
		}
		file := globalSettingsFile
		if file == "" {
			file = "the global settings file"
		}
		return resolvedWebBaseURL{URL: endpoint, Source: "server.hub.public_url in " + file}
	case hubEndpointSourceIAPAudience:
		if webBaseURLFollowsIAPAudience {
			return resolvedWebBaseURL{URL: endpoint, Source: "derived from the IAP audience (server.auth.proxy.iap.audience)"}
		}
	}
	return resolvedWebBaseURL{URL: fmt.Sprintf("http://localhost:%d", m.webPort), Source: webBaseURLSourceDefault}
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

// checkWebBaseURLListener refuses a web server that listens on a
// non-loopback address with a base URL that is not https: OAuth callbacks
// and the session cookie would then cross the network unencrypted.
// Loopback is decided from the literal listen host only (127.0.0.0/8, ::1
// and the name "localhost"; config.IsLoopbackHost): a name is never
// resolved, so a hosts-file name that points at 127.0.0.1 counts as not
// loopback. The error names the base URL, its source, the listen address,
// and both fixes.
func checkWebBaseURLListener(base resolvedWebBaseURL, listenHost string, listenPort int) error {
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
	u, err := url.Parse(base.URL)
	if err == nil && u.Scheme == "https" && u.Host != "" {
		return nil
	}
	listen := net.JoinHostPort(listenHost, strconv.Itoa(listenPort))
	return fmt.Errorf("refusing to start the web server: its base URL %s (from %s) is not https, "+
		"and it listens on %s, which is not a loopback address, so OAuth callbacks and session cookies "+
		"would cross the network unencrypted. Use an https base URL (--base-url, SCION_SERVER_BASE_URL "+
		"or server.hub.public_url in the global settings file), or listen on a loopback host "+
		"(--host 127.0.0.1)", base.URL, base.Source, listen)
}
