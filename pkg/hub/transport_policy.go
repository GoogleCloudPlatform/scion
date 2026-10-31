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

package hub

import (
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Hub-broker transport policy, hub side.
//
// The broker join and secret rotation responses carry the broker's HMAC
// secret, broker registration and join-token re-mint (POST /api/v1/brokers)
// return a join token, and the control channel and the broker conduit
// session carry the secrets of every agent the broker runs. The hub serves
// them only on a request that arrived over an encrypted channel or from this
// host (config.ConfidentialArrival); every other request is refused before anything
// secret is produced. The broker side refuses the same hubs before sending
// anything (config.CheckHubTransport); this is the hub's own check, for a
// broker that does not.

// ErrCodeTransportNotConfidential is the error code of a request refused by
// the hub-broker transport policy.
const ErrCodeTransportNotConfidential = "transport_not_confidential"

// transportRefusalMessage is the body message of that refusal. It names
// both fixes and the trusted proxy setting, and nothing about the request.
const transportRefusalMessage = "This request carries broker credentials and must reach the hub over TLS or from this host. " +
	"Use the hub's https:// URL. If TLS ends at a proxy in front of the hub, list the proxy's addresses in " +
	config.TrustedProxiesKey + " so the hub trusts its X-Forwarded-Proto header."

// requireConfidentialArrival writes the transport refusal (403) and returns
// false when r did not arrive confidentially (config.ConfidentialArrival with the
// hub's server.trusted_proxies).
func (s *Server) requireConfidentialArrival(w http.ResponseWriter, r *http.Request) bool {
	if config.ConfidentialArrival(r, s.transportTrustedNets) {
		return true
	}
	slog.Warn("Refused a broker credential request that did not arrive over TLS or from this host",
		"path", logging.RequestPath(r), "remote_addr", r.RemoteAddr, "trusted_proxies_configured", len(s.transportTrustedNets) > 0)
	writeError(w, http.StatusForbidden, ErrCodeTransportNotConfidential, transportRefusalMessage, nil)
	return false
}
