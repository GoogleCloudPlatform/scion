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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// The hub's content host (server.hub.content_url, ptone/scion#7107 phase
// A): a second public origin, apart from server.hub.public_url, on which
// the hub serves only the route families in contentHostRouteFamilies. The
// guard itself is artifacts.ContentHostGuard; this file feeds it the value
// in effect, the route table, the hub's credential headers and the refused
// request log.

// contentHostRouteFamily is one family of routes the content host serves.
type contentHostRouteFamily struct {
	// name labels the family in logs and tests.
	name string
	// serves reports whether r is a request of the family, by the same
	// checks that pass the family's reads through authentication without a
	// credential.
	serves func(*http.Request) bool
}

// contentHostRouteFamilies is the single table of the route families the
// content host serves (security review condition 2). Every other request on
// the content host answers 404 before authentication. A family is added
// here by its owning lane, behind its own tests and review; the artifact
// families are the artifacts lane's.
var contentHostRouteFamilies = []contentHostRouteFamily{
	{name: "artifact-share", serves: isArtifactSharedRequest},
	{name: "artifact-view", serves: isArtifactViewRequest},
}

// servesOnContentHost reports whether the content host serves r: whether r
// is a request of a family in contentHostRouteFamilies.
func servesOnContentHost(r *http.Request) bool {
	for _, f := range contentHostRouteFamilies {
		if f.serves(r) {
			return true
		}
	}
	return false
}

// contentURLLiveValue returns server.hub.content_url as last applied from
// the endpoints settings section, or the startup value
// (ServerConfig.ContentURL) before the first apply.
func (s *Server) contentURLLiveValue() string {
	if v := s.contentURLLive.Load(); v != nil {
		return *v
	}
	return s.config.ContentURL
}

// contentURLInEffect returns the content host that may be used, normalized,
// or "" for none. public is the hub's public endpoint. An invalid value is
// ignored (saves refuse it), and so is a value on the hub's own host name,
// at any scheme or port (saves refuse it; a stored one, or a hub whose
// public URL changed after the save, must not confine the hub's own host).
// A value on the host name of server.hub.agent_endpoint is ignored the same
// way, so the guard never confines the host agents reach the hub on.
// A value under the hub's registrable domain is used with a warning, since
// it isolates little. Each value's problem is logged once.
func (s *Server) contentURLInEffect(public string) string {
	raw := s.contentURLLiveValue()
	if raw == "" {
		return ""
	}
	normalized, err := config.ValidateContentURL(raw)
	if err != nil {
		if s.contentURLWarned.Swap(raw) != raw {
			slog.Warn(config.ContentURLKey+" is ignored because it is invalid; shared artifacts stay on the hub origin",
				"error", err)
		}
		return ""
	}
	sameHost, sameSite := config.ContentURLHubConflict(normalized, public)
	if (sameHost || sameSite) && s.contentURLWarned.Swap(raw) != raw {
		if sameHost {
			slog.Warn(config.ContentURLKey+" is ignored because it is on the hub's own host name; shared artifacts stay on the hub origin",
				"content_url", normalized)
		} else {
			slog.Warn(config.ContentURLKey+" shares the hub's registrable domain, which gives little isolation; use a separate registrable domain",
				"content_url", normalized, "hub_public_url", public)
		}
	}
	if sameHost {
		return ""
	}
	if config.ContentURLSameHost(normalized, s.config.AgentEndpoint) {
		if s.contentURLAgentWarned.Swap(raw) != raw {
			slog.Warn(config.ContentURLKey+" is ignored because it is on the host name of server.hub.agent_endpoint; shared artifacts stay on the hub origin",
				"content_url", normalized)
		}
		return ""
	}
	return normalized
}

// stripContentHostCredentials removes the hub's credential headers from a
// request on the content host, after artifacts.ContentHostGuard has removed
// Cookie and Authorization: every X-Scion-* header (the agent, federation,
// broker and issuer delegation credentials among them), the
// X-Forwarded-User-* identity headers, and the front proxy's identity
// assertions (stripFrontProxyIdentityHeaders).
func (s *Server) stripContentHostCredentials(h http.Header) {
	for name := range h {
		if strings.HasPrefix(name, "X-Scion-") || strings.HasPrefix(name, "X-Forwarded-User-") {
			h.Del(name)
		}
	}
	s.stripFrontProxyIdentityHeaders(h)
}

// logContentHostRefused logs a request the content host's guard answered
// 404. The guard runs outside the request logger, so this is the only line
// such a request leaves; the path goes through logging.RequestPath, which
// redacts the capability of an artifact or port view path (C1-2).
func logContentHostRefused(r *http.Request) {
	slog.Info("Content host: request refused (route not served on the content host)",
		slog.String("method", r.Method),
		slog.String("path", logging.RequestPath(r)),
		slog.String("remote_addr", r.RemoteAddr),
	)
}

// contentURLForGuard is the guard's view of the content host in effect.
func (s *Server) contentURLForGuard() string {
	return s.HubEndpoints().Content
}

// ContentHostGuard wraps h in the content host's guard
// (artifacts.ContentHostGuard): on a request whose Host is the content host
// in effect (HubEndpoints().Content), only the route families in
// contentHostRouteFamilies are served, every response carries no-referrer
// and no-store, every credential header is removed, and a refused request
// is logged with its capability redacted. applyMiddleware puts it
// outermost, and the web server puts it outermost too when it mounts the
// hub API, so it runs before the web app, the session bridge and
// authentication.
//
// A request on the hub's own host name skips the guard without reading the
// content URL: the fast path compares the request Host with the public
// endpoint resolved at startup only (hubEndpointsFromConfig), and a content
// URL on that host name is never used (contentURLInEffect), so such a
// request is never a content-host request.
func (s *Server) ContentHostGuard(h http.Handler) http.Handler {
	guarded := artifacts.ContentHostGuard(s.contentURLForGuard, servesOnContentHost, s.stripContentHostCredentials, logContentHostRefused)(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if config.RequestOnHubHost(r.Host, hubEndpointsFromConfig(s.config).Public) {
			h.ServeHTTP(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}
