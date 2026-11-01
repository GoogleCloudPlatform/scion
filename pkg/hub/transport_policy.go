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
	"sync"
	"time"

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

// localProxyRefusalPrefix is put in front of transportRefusalMessage when
// the request came from a proxy on this host that server.trusted_proxies
// does not list (config.IsLocalProxyRefusal).
const localProxyRefusalPrefix = "The hub received this request from a proxy on its own host (X-Forwarded-* headers from a loopback peer), but " +
	config.TrustedProxiesKey + " does not list that proxy. "

// Transport refusal detail keys. A broker reads them to name the cause; an
// older broker ignores them and matches on the error code alone.
const (
	transportRefusalDetailReason  = "reason"
	transportRefusalDetailSetting = "setting"
)

// transportProxyRefusalLogInterval is how often the ERROR for one
// local-proxy refusal reason is logged; refusals in between are counted
// and reported with the next line.
const transportProxyRefusalLogInterval = 5 * time.Minute

// transportProxyRefusalHealthWindow is how long after the last local-proxy
// refusal the health check transportProxyHealthCheck is reported.
const transportProxyRefusalHealthWindow = 15 * time.Minute

// transportProxyHealthCheck is the health check key reported while broker
// requests from a proxy on this host are being refused. It is not in
// criticalHealthChecks: the hub is degraded, not unhealthy.
const transportProxyHealthCheck = "broker_transport_proxy"

// transportProxyRefusals records the refusals of broker requests that came
// from a proxy on this host that server.trusted_proxies does not list: the
// last one (for the health check) and, per reason, when the ERROR was last
// logged and how many refusals were not logged since.
type transportProxyRefusals struct {
	mu         sync.Mutex
	last       time.Time
	lastReason string
	logged     map[string]time.Time
	suppressed map[string]uint64
}

// record notes a refusal with reason at now. It returns whether to log it
// and, when it does, how many refusals with that reason were not logged
// since the previous line.
func (t *transportProxyRefusals) record(reason string, now time.Time) (logIt bool, suppressed uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last, t.lastReason = now, reason
	if t.logged == nil {
		t.logged = map[string]time.Time{}
		t.suppressed = map[string]uint64{}
	}
	if last, ok := t.logged[reason]; ok && now.Sub(last) < transportProxyRefusalLogInterval {
		t.suppressed[reason]++
		return false, 0
	}
	suppressed = t.suppressed[reason]
	t.logged[reason] = now
	t.suppressed[reason] = 0
	return true, suppressed
}

// recent returns the reason of the last refusal when it is within
// transportProxyRefusalHealthWindow of now.
func (t *transportProxyRefusals) recent(now time.Time) (reason string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() || now.Sub(t.last) >= transportProxyRefusalHealthWindow {
		return "", false
	}
	return t.lastReason, true
}

// requireConfidentialArrival writes the transport refusal (403) and returns
// false when r did not arrive confidentially (config.ConfidentialArrival
// with the hub's transport-trusted proxies). The refusal keeps the error
// code ErrCodeTransportNotConfidential and adds details naming the reason
// (config.ArrivalRefusal) and the setting. A refusal of a proxy on this
// host that the setting does not list is logged at ERROR (rate-limited) and
// reported by the health check transportProxyHealthCheck; any other
// refusal is logged at WARN.
func (s *Server) requireConfidentialArrival(w http.ResponseWriter, r *http.Request) bool {
	reason := config.ArrivalRefusal(r, s.transportTrustedNets)
	if reason == "" {
		return true
	}
	message := transportRefusalMessage
	if config.IsLocalProxyRefusal(reason) {
		message = localProxyRefusalPrefix + message
		if logIt, suppressed := s.transportProxyRefusals.record(reason, time.Now()); logIt {
			slog.Error(localProxyRefusalLogMessage(reason),
				"reason", reason, "path", logging.RequestPath(r), "remote_addr", r.RemoteAddr, "suppressed", suppressed)
		}
	} else {
		slog.Warn(otherTransportRefusalLogMessage,
			"reason", reason, "path", logging.RequestPath(r), "remote_addr", r.RemoteAddr,
			"trusted_proxies_configured", len(s.transportTrustedNets) > 0)
	}
	writeError(w, http.StatusForbidden, ErrCodeTransportNotConfidential, message, map[string]interface{}{
		transportRefusalDetailReason:  reason,
		transportRefusalDetailSetting: config.TrustedProxiesKey,
	})
	return false
}

// otherTransportRefusalLogMessage is the WARN line of any other transport
// refusal. Any network peer can cause one, so it stays a WARN with no
// health check; it names the setting, because a proxy on this host that
// does not connect from loopback (a proxy container on a bridge network)
// is refused this way too.
const otherTransportRefusalLogMessage = "Refused a broker credential request that did not arrive over TLS or from this host. " +
	"If TLS ends at a proxy that connects from another address (a proxy on another host, or a proxy container on a bridge network), " +
	"list that address in " + config.TrustedProxiesKey + " and restart the hub."

// localProxyRefusalLogMessage is the ERROR line of a local-proxy refusal.
func localProxyRefusalLogMessage(reason string) string {
	cause := config.TrustedProxiesKey + " is empty"
	if reason == config.ArrivalRefusalLocalProxyNotTrusted {
		cause = config.TrustedProxiesKey + " does not list this proxy"
	}
	return "Refused broker credentials from a local TLS-terminating proxy: " + cause +
		". Add the proxy's address (for example 127.0.0.1 and ::1) to " + config.TrustedProxiesKey +
		" and restart the hub. Remote runtime brokers cannot receive commands until then."
}

// checkTransportProxyHealth reports transportProxyHealthCheck while a
// broker request from a proxy on this host was refused within
// transportProxyRefusalHealthWindow.
func (s *Server) checkTransportProxyHealth(checks map[string]string) {
	if _, ok := s.transportProxyRefusals.recent(time.Now()); !ok {
		return
	}
	checks[transportProxyHealthCheck] = HealthStatusDegraded +
		": broker connections from a local proxy are refused; set " + config.TrustedProxiesKey
}
