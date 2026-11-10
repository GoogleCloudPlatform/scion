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
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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

// contentURLLiveState is server.hub.content_url with its same-site
// override, applied and read together (Server.contentURLLive), and what the
// hub derives from them: the classification, the parsed content host and the
// hub's own host name. newContentURLLiveState derives them once per settings
// apply, so no request parses the content URL or the hub URL.
type contentURLLiveState struct {
	URL           string
	AllowSameSite bool

	// public and agentEndpoint are the endpoints the derived fields were
	// computed for (contentURLView re-derives when they differ).
	public, agentEndpoint string
	// class is classifyContentURL of URL and AllowSameSite.
	class contentURLClass
	// host is the parsed content host when class is in effect, else zero.
	host artifacts.ContentHost
	// hubHost is the lower-cased host name of public, for the guard's fast
	// path ("" when public has none).
	hubHost string
}

// newContentURLLiveState returns the state for a content URL and its
// override, with the derived fields computed for public and agentEndpoint.
func newContentURLLiveState(contentURL string, allowSameSite bool, public, agentEndpoint string) *contentURLLiveState {
	st := &contentURLLiveState{URL: contentURL, AllowSameSite: allowSameSite, public: public, agentEndpoint: agentEndpoint}
	st.class = classifyContentURL(*st, public, agentEndpoint)
	if st.class.state == ContentURLStateInEffect {
		st.host, _ = artifacts.ParseContentHost(st.class.normalized)
	}
	st.hubHost = hostNameOf(public)
	return st
}

// contentURLView returns the applied state with its derived fields for
// public and this hub's agent endpoint. It is the one load a reader makes.
// The derived fields come from the last apply; they are recomputed only when
// the endpoints differ from the ones they were computed for (the endpoints
// are fixed while the hub runs, so in practice only before the first apply
// or in tests), and that result replaces the stored state with
// CompareAndSwap, so it too is computed once.
func (s *Server) contentURLView(public string) *contentURLLiveState {
	agentEndpoint := s.config.AgentEndpoint
	cur := s.contentURLLive.Load()
	if cur != nil && cur.public == public && cur.agentEndpoint == agentEndpoint {
		return cur
	}
	contentURL, allowSameSite := s.config.ContentURL, s.config.ContentURLAllowSameSite
	if cur != nil {
		contentURL, allowSameSite = cur.URL, cur.AllowSameSite
	}
	next := newContentURLLiveState(contentURL, allowSameSite, public, agentEndpoint)
	if cur != nil {
		s.contentURLLive.CompareAndSwap(cur, next)
	}
	return next
}

// hostNameOf returns the lower-cased host name of an absolute URL, a trailing
// dot dropped, or "" when it has none.
func hostNameOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// requestHostName returns the host name of a request Host (host[:port], as
// the hub receives it), lower-cased with a trailing dot dropped.
func requestHostName(reqHost string) string {
	host := reqHost
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// contentURLLiveState returns server.hub.content_url and
// server.hub.content_url_allow_same_site as last applied from the endpoints
// settings section, or the startup values (ServerConfig) before the first
// apply. Callers that need both read them from one result.
func (s *Server) contentURLLiveState() contentURLLiveState {
	if v := s.contentURLLive.Load(); v != nil {
		return *v
	}
	return contentURLLiveState{URL: s.config.ContentURL, AllowSameSite: s.config.ContentURLAllowSameSite}
}

// contentURLAllowSameSiteLiveValue returns
// server.hub.content_url_allow_same_site as last applied (see
// contentURLLiveState).
func (s *Server) contentURLAllowSameSiteLiveValue() bool {
	return s.contentURLLiveState().AllowSameSite
}

// contentURLInEffect returns the content host that may be used, normalized,
// or "" for none. public is the hub's public endpoint. An invalid value is
// ignored (saves refuse it), and so is a value on the hub's own host name,
// at any scheme or port (saves refuse it; a stored one, or a hub whose
// public URL changed after the save, must not confine the hub's own host).
// A value on the host name of server.hub.agent_endpoint is ignored the same
// way, so the guard never confines the host agents reach the hub on.
// A value on the hub's registrable domain is ignored the same way, with an
// error log, unless server.hub.content_url_allow_same_site is set; with the
// override it is used with a warning, since it is not a separate security
// origin for cookies. Each value's problem is logged once.
func (s *Server) contentURLInEffect(public string) string {
	return s.contentURLChecked(s.contentURLView(public), public)
}

// contentURLChecked logs, once per value, why live's content URL is not in
// effect, and returns the normalized content URL when it is, else "".
func (s *Server) contentURLChecked(live *contentURLLiveState, public string) string {
	// One state gives the URL, its override and the classification derived
	// from them at apply time.
	raw := live.URL
	c := live.class
	switch c.state {
	case ContentURLStateUnset:
		return ""
	case ContentURLStateInvalid:
		if s.contentURLWarned.Swap(raw) != raw {
			slog.Warn(config.ContentURLKey+" is ignored because it is invalid; shared artifacts stay on the hub origin",
				"error", c.err)
		}
		return ""
	}
	// The key that dedups the log names the override too, so turning it on
	// or off logs the value's new state once.
	warnKey := raw
	if live.AllowSameSite {
		warnKey = raw + "\x00" + config.ContentURLAllowSameSiteKey
	}
	if (c.state == ContentURLStateSameHost || c.sameSite) && s.contentURLWarned.Swap(warnKey) != warnKey {
		switch {
		case c.state == ContentURLStateSameHost:
			slog.Warn(config.ContentURLKey+" is ignored because it is on the hub's own host name; shared artifacts stay on the hub origin",
				"content_url", c.normalized)
		case !live.AllowSameSite:
			slog.Error(config.ContentURLKey+" is ignored because it shares the hub's registrable domain, so it is not a separate security origin for cookies; shared artifacts stay on the hub origin. Use a content host on a different registrable domain, or set "+config.ContentURLAllowSameSiteKey,
				"content_host", contentURLHostForLog(c.normalized), "hub_host", contentURLHostForLog(public))
		default:
			slog.Warn(contentURLSameSiteWarning, "content_host", contentURLHostForLog(c.normalized), "hub_host", contentURLHostForLog(public))
		}
	}
	if c.state == ContentURLStateAgentEndpointHost && s.contentURLAgentWarned.Swap(raw) != raw {
		slog.Warn(config.ContentURLKey+" is ignored because it is on the host name of server.hub.agent_endpoint; shared artifacts stay on the hub origin",
			"content_url", c.normalized)
	}
	if c.state != ContentURLStateInEffect {
		return ""
	}
	return c.normalized
}

// The states of server.hub.content_url that ContentURLStatus reports.
const (
	// ContentURLStateUnset: no content host is configured.
	ContentURLStateUnset = "unset"
	// ContentURLStateInEffect: the content host is used.
	ContentURLStateInEffect = "in_effect"
	// ContentURLStateInvalid: the value is ignored because it is invalid.
	ContentURLStateInvalid = "ignored_invalid"
	// ContentURLStateSameHost: the value is on the hub's own host name, at
	// some scheme or port. Saves refuse it, and a stored one is ignored
	// whatever the override says.
	ContentURLStateSameHost = "refused_same_host"
	// ContentURLStateSameSite: the value shares the hub's registrable
	// domain and is ignored until server.hub.content_url_allow_same_site is
	// set.
	ContentURLStateSameSite = "ignored_same_site"
	// ContentURLStateAgentEndpointHost: the value is on the host name of
	// server.hub.agent_endpoint and is ignored.
	ContentURLStateAgentEndpointHost = "ignored_agent_endpoint_host"
)

// contentURLClass is the outcome of classifyContentURL.
type contentURLClass struct {
	// state is one of the ContentURLState* values.
	state string
	// normalized is the validated value, "" when unset or invalid.
	normalized string
	// sameSite reports that normalized shares the hub's registrable domain
	// (with or without the override).
	sameSite bool
	// err is the validation error of an invalid value.
	err error
}

// classifyContentURL decides whether the content URL in live may be used,
// for a hub on public with agent endpoint agentEndpoint. It has no side
// effects; contentURLInEffect logs the outcome, ContentURLStatus reports it.
func classifyContentURL(live contentURLLiveState, public, agentEndpoint string) contentURLClass {
	if live.URL == "" {
		return contentURLClass{state: ContentURLStateUnset}
	}
	normalized, err := config.ValidateContentURL(live.URL)
	if err != nil {
		return contentURLClass{state: ContentURLStateInvalid, err: err}
	}
	sameHost, sameSite := config.ContentURLHubConflict(normalized, public)
	c := contentURLClass{normalized: normalized, sameSite: sameSite}
	switch {
	case sameHost:
		c.state = ContentURLStateSameHost
	case sameSite && !live.AllowSameSite:
		c.state = ContentURLStateSameSite
	case config.ContentURLSameHost(normalized, agentEndpoint):
		c.state = ContentURLStateAgentEndpointHost
	default:
		c.state = ContentURLStateInEffect
	}
	return c
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

// guardContentHostKey carries, in a request's context, the content host
// ContentHostGuard decided for that request from its one snapshot load.
type guardContentHostKey struct{}

// guardContentHost is the artifacts guard's view of the content host for
// r: the value ContentHostGuard put in r's context from the snapshot it
// loaded for r. A request that reaches the guard without that value (a
// caller wiring artifacts.ContentHostGuard some other way) gets one
// snapshot load of its own instead, so a mis-wiring fails closed: the
// content host stays confined rather than being served as the hub.
func (s *Server) guardContentHost(r *http.Request) artifacts.ContentHost {
	if o, ok := r.Context().Value(guardContentHostKey{}).(artifacts.ContentHost); ok {
		return o
	}
	return s.contentHostFor(hubEndpointsFromConfig(s.config).Public)
}

// contentHostFor returns the parsed content host in effect for public from
// one snapshot load, or the zero ContentHost when no content URL is in
// effect.
func (s *Server) contentHostFor(public string) artifacts.ContentHost {
	return s.contentHostIn(s.contentURLView(public), public)
}

// contentHostIn returns live's parsed content host when its content URL is
// in effect, else the zero ContentHost.
func (s *Server) contentHostIn(live *contentURLLiveState, public string) artifacts.ContentHost {
	if s.contentURLChecked(live, public) == "" {
		return artifacts.ContentHost{}
	}
	return live.host
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
// A request on the hub's own host name skips the guard: the fast path
// compares the request Host's name with the hub's host name, parsed once
// per settings apply from the public endpoint resolved at startup
// (contentURLView), and a content URL on that host name is never used
// (contentURLInEffect), so such a request is never a content-host request.
// Neither path parses a URL per request, and each guard instance loads the
// snapshot once per request: its fast path and its guard decide from the
// same value, so an apply during the request cannot give them different
// ones. On the combined server a request to /api/v1/ passes two instances
// (the web server's and the hub's); each decides consistently on its own,
// and confinement by either one wins.
func (s *Server) ContentHostGuard(h http.Handler) http.Handler {
	guarded := artifacts.ContentHostGuard(s.guardContentHost, servesOnContentHost, s.stripContentHostCredentials, logContentHostRefused)(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := hubEndpointsFromConfig(s.config).Public
		live := s.contentURLView(public)
		if live.hubHost != "" && requestHostName(r.Host) == live.hubHost {
			h.ServeHTTP(w, r)
			return
		}
		host := s.contentHostIn(live, public)
		guarded.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), guardContentHostKey{}, host)))
	})
}
