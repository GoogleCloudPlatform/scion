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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/portview"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Port page view (experiment hub.port_view, ptone/scion#5569).
//
// A user who opens an exposed port's link from the hub
// (/api/v1/agents/{id}/ports/{port}/proxy/...) is sent to a page link
// under /api/v1/portview/<capability>/<path> (see pkg/hub/internal/portview
// for the capability). Every request on that route, including the page's
// own scripts, styles, images, requests and WebSockets, is served on the
// capability alone, as the user it names, for the one port it names:
// handlePortView checks the capability, the agent, the user's account and
// the user's port access on each request. Pages are served in the same
// browser sandbox as on the port link.

// SecretKeyPortViewKey is the secret key name of the key that signs page
// view capabilities. It is a key of its own, separate from every other hub
// key (the artifact view key included), so that a capability can never be
// accepted by another route; the signed message also carries its own
// domain (pkg/hub/internal/portview).
const SecretKeyPortViewKey = "port_view_signing_key"

// portViewScope is the only permission of the identity a page view
// request runs as.
const portViewScope = "agent:port_access"

// portViewInvalidBody is the body of the one answer every capability
// failure gets (writePortViewInvalid).
const portViewInvalidBody = "This page's access link is not valid or has expired. Reload the page from the hub.\n"

// portViewPreflightMaxAge is the Access-Control-Max-Age of a preflight
// answer, in seconds.
const portViewPreflightMaxAge = "600"

// initPortViewKey loads or creates the page view capability key, with the
// same stable-key policy as the artifact view key: required on deployments
// that need stable keys, otherwise an in-memory key, since losing it only
// ends page links within their lifetime.
//
// In keyring mode the keys come from the signing ring instead (see
// capability_keys.go) and nothing is read from the secret backend.
func (s *Server) initPortViewKey(ctx context.Context) error {
	if s.keyringManaged() {
		keys, err := s.managedCapabilityKeys(SecretKeyPortViewKey)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.portViewKeys = keys
		s.mu.Unlock()
		return nil
	}
	key, err := s.ensureSigningKey(ctx, SecretKeyPortViewKey, nil)
	if err == nil && len(key) == 0 {
		err = fmt.Errorf("port view key resolved to an empty value")
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("port view key: %w", err)
		}
		slog.Warn("Port view key could not be loaded or persisted; using an ephemeral in-memory key "+
			"(page links will not work on other replicas or after restart)", "error", err)
		key = make([]byte, 32)
		if _, rerr := rand.Read(key); rerr != nil {
			return fmt.Errorf("generate ephemeral port view key: %w", rerr)
		}
		s.recordSigningKeySource(SecretKeyPortViewKey, signingKeySourceEphemeral)
	}
	s.mu.Lock()
	s.portViewKeys = fixedCapabilityKeys(key)
	s.mu.Unlock()
	return nil
}

// portViewCapabilityKeys returns the page view capability keys
// (unconfigured when they were never initialised, which refuses every
// capability and creates none).
func (s *Server) portViewCapabilityKeys() capabilityKeys {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.portViewKeys
}

// portViewKeyLookup returns the keys that may have signed a page link
// with key ID kid: that generation only for a key ID (none when it is
// unknown or retired), the legacy readers for a link without one.
func (s *Server) portViewKeyLookup() portview.Keys {
	keys := s.portViewCapabilityKeys()
	if !keys.configured() {
		return nil
	}
	return func(kid string) [][]byte {
		var out [][]byte
		for _, k := range keys.candidates(kid) {
			out = append(out, k.Bytes())
		}
		return out
	}
}

// portViewIssuanceClosedLogged makes the issuance-closed message of
// mintPortView appear once per process.
var portViewIssuanceClosedLogged atomic.Bool

// mintPortView returns a page link capability for userID's access to port
// of agentID until exp, signed by the active key generation, in the form
// the signing ring selects (with or without a key ID). It fails when no
// key is configured or none may sign now; the error names no key.
func (s *Server) mintPortView(agentID string, port int, userID string, exp time.Time) (string, error) {
	keys := s.portViewCapabilityKeys()
	if !keys.configured() {
		return "", errors.New("no port view key")
	}
	key, versioned, err := keys.issue()
	if err != nil {
		if errors.Is(err, errCapabilityIssuanceClosed) && portViewIssuanceClosedLogged.CompareAndSwap(false, true) {
			slog.Warn("Port view: page links are not issued: the active signing generation may no longer sign (sign_until reached); port links are served directly")
		}
		return "", err
	}
	if versioned {
		return portview.MintVersioned(key.Bytes(), key.ID, agentID, port, userID, exp), nil
	}
	return portview.Mint(key.Bytes(), agentID, port, userID, exp), nil
}

// portViewTTL is the configured lifetime of a page link
// (server.hub.port_proxy.view_ttl; 0 = the 2h default).
func (s *Server) portViewTTL() time.Duration {
	if d := s.config.PortViewTTL; d > 0 {
		return d
	}
	return config.PortProxyDefaultViewTTL
}

// portViewNow is the clock of the page view route (a test hook).
func (s *Server) portViewNow() time.Time {
	if s.portViewClock != nil {
		return s.portViewClock()
	}
	return time.Now()
}

// isPortViewRequest reports whether r is a request under the page view
// route, of any method, whose path segment after portview.Route holds a
// capability. Both the decoded and the escaped path must be clean (one
// trailing slash allowed) and under the route, the escaped path may not
// encode a slash, dot, backslash or NUL (in any letter case), and the
// capability segment may not be escaped at all, so the request this check
// admits is the one the mux routes to the route. Only the request shape is
// checked here; the route checks the capability.
func isPortViewRequest(r *http.Request) bool {
	p, esc := r.URL.Path, r.URL.EscapedPath()
	if !strings.HasPrefix(p, portview.Route) || !strings.HasPrefix(esc, portview.Route) {
		return false
	}
	if !cleanRoutePath(p) || !cleanRoutePath(esc) {
		return false
	}
	lower := strings.ToLower(esc)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%00") || strings.IndexByte(p, 0) >= 0 {
		return false
	}
	capability, _, _ := strings.Cut(esc[len(portview.Route):], "/")
	return capability != "" && !strings.Contains(capability, "%")
}

// cleanRoutePath reports whether p is already clean (path.Clean), apart
// from one trailing slash.
func cleanRoutePath(p string) bool {
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c == p
}

// writePortViewInvalid writes the single answer the page view route uses
// for a link it cannot serve: a plain-text 404 asking to reload the page
// from the hub.
func writePortViewInvalid(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, portViewInvalidBody)
}

// setPortViewResponseHeaders sets the hub's headers of every answer on the
// page view route: Content-Security-Policy, X-Content-Type-Options,
// Referrer-Policy and Access-Control-Allow-Origin, and removes
// Access-Control-Allow-Credentials. A proxied app's values for these
// headers are dropped (portViewModifyResponse), so each is sent once.
func setPortViewResponseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", untrustedContentSandboxCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Del("Access-Control-Allow-Credentials")
}

// handlePortView serves ANY /api/v1/portview/<capability>/<path>.
//
// Order of the checks (each one's position matters):
//  1. the experiment, then the hub-owned headers;
//  2. the request shape; a preflight is answered here on the capability
//     alone and never reaches the app;
//  3. the capability; a page navigation with one that fails is sent to
//     the port's link instead (as in 5);
//  4. a page navigation by a signed-in user other than the capability's
//     is sent to the port's link (which link a browser is sent to only;
//     see portview.IsNavigation);
//  5. the agent, the user's account and the user's port access, as an
//     identity limited to port access in the agent's project;
//  6. the exposed port, the trailing-slash redirect, then the proxy, as
//     that identity.
//
// Any other request that fails 2, 3 or 5 gets writePortViewInvalid. The
// identity in the request context, if any, is read only by step 4.
func (s *Server) handlePortView(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if !s.experimentEnabled(experiments.PortView) {
		NotFound(w, "route")
		return
	}
	setPortViewResponseHeaders(w)
	if !isPortViewRequest(r) {
		writePortViewInvalid(w)
		return
	}
	rest := r.URL.Path[len(portview.Route):]
	capability, sub, hasSlash := strings.Cut(rest, "/")
	_, escSub, _ := strings.Cut(r.URL.EscapedPath()[len(portview.Route):], "/")
	keys, now, ttl := s.portViewKeyLookup(), s.portViewNow(), s.portViewTTL()

	if r.Method == http.MethodOptions {
		if _, err := portview.Parse(keys, capability, now, ttl); err != nil {
			writePortViewInvalid(w)
			return
		}
		writePortViewPreflight(w, r)
		return
	}

	navigation := (r.Method == http.MethodGet || r.Method == http.MethodHead) && portview.IsNavigation(r)
	grant, err := portview.Parse(keys, capability, now, ttl)
	if err != nil {
		if agentID, port, ok := portview.Target(capability); ok && navigation {
			redirectToPortLink(w, r, agentID, port, escSub)
			return
		}
		writePortViewInvalid(w)
		return
	}
	if navigation {
		if user := GetUserIdentityFromContext(r.Context()); user != nil && user.ID() != grant.UserID {
			redirectToPortLink(w, r, grant.AgentID, grant.Port, escSub)
			return
		}
	}

	// unusable answers a link that can no longer be served like one that
	// failed to parse: a page navigation goes to the port's link, anything
	// else gets writePortViewInvalid.
	unusable := func() {
		if navigation {
			redirectToPortLink(w, r, grant.AgentID, grant.Port, escSub)
			return
		}
		writePortViewInvalid(w)
	}
	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, grant.AgentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Warn("Port view: agent lookup failed", "error", err)
	}
	if err != nil || agent == nil || agent.ID != grant.AgentID || !agent.DeletedAt.IsZero() {
		unusable()
		return
	}
	identity, ok := s.portViewIdentity(ctx, grant.UserID, agent)
	if !ok {
		unusable()
		return
	}
	exposed := findExposedPort(agent.ExposedPorts, grant.Port)
	if exposed == nil {
		writePortNotExposed(w, r)
		return
	}
	// The link without a trailing slash: GET and HEAD are answered with a
	// redirect to "<capability>/", so the page's relative URLs resolve
	// inside the link, as the port link does for .../proxy; every other
	// request gets writePortViewInvalid.
	if !hasSlash {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || isWebSocketUpgrade(r) {
			writePortViewInvalid(w)
			return
		}
		loc := capability + "/"
		if r.URL.RawQuery != "" {
			loc += "?" + encodeLocationQuery(r.URL.RawQuery)
		}
		w.Header().Set("Location", loc)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}
	s.proxyToAgentPort(w, r, identity, agent, exposed, "/"+sub, portProxyPolicy{
		surface:     portProxySurfaceView,
		caller:      "user:" + grant.UserID,
		conduitOnly: true,
		portView:    true,
	}, start)
}

// writePortViewPreflight answers a preflight on the page view route: 204
// with Access-Control-Allow-Methods (portview.PreflightMethod),
// Access-Control-Allow-Headers (portview.PreflightHeaders) and a
// 10-minute Access-Control-Max-Age.
func writePortViewPreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if m := portview.PreflightMethod(r.Header.Get("Access-Control-Request-Method")); m != "" {
		h.Set("Access-Control-Allow-Methods", m)
	}
	if hs := portview.PreflightHeaders(strings.Join(r.Header.Values("Access-Control-Request-Headers"), ",")); hs != "" {
		h.Set("Access-Control-Allow-Headers", hs)
	}
	h.Set("Access-Control-Max-Age", portViewPreflightMaxAge)
	w.WriteHeader(http.StatusNoContent)
}

// portViewIdentity returns the identity a page view request for userID
// on agent runs as, and whether userID may open the agent's ports now:
// the user's account must be active, and the user, limited to port access
// within the agent's project, must pass the port access check. The port
// link calls it before it creates a page link, so a link is only created
// for a user it would serve.
func (s *Server) portViewIdentity(ctx context.Context, userID string, agent *store.Agent) (Identity, bool) {
	user, err := checkUserAccount(ctx, s.store, PrincipalContext{Kind: PrincipalKindUser, ID: userID})
	if refusal := classifyUserAccount(user, err); refusal != accountAllowed || user == nil {
		if refusal == accountLookupFault {
			slog.Warn("Port view: user lookup failed", "agent_id", agent.ID, "error", err)
		}
		return nil, false
	}
	base := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	identity := NewScopedUserIdentity(base, agent.ProjectID, []string{portViewScope})
	if !s.userPortAccessAllowed(ctx, identity, agent, ActionPortAccess) {
		return nil, false
	}
	return identity, true
}

// redirectToPortView answers a page navigation to an exposed port's link
// with a redirect to a new page link, and reports whether it did. It
// applies with the experiment on, to a GET or HEAD page navigation by a
// signed-in user (not an agent or a token), for a loopback port of an
// agent this node can reach over conduit, and only when the page view
// route would serve that user (portViewIdentity), and when a page link can
// be signed now (mintPortView). In every other case the port link serves
// the request as before.
func (s *Server) redirectToPortView(w http.ResponseWriter, r *http.Request, agent *store.Agent, exposed *store.ExposedPort) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !s.experimentEnabled(experiments.PortView) || !portview.IsNavigation(r) {
		return false
	}
	var userID string
	switch u := GetIdentityFromContext(r.Context()).(type) {
	case *AuthenticatedUser:
		userID = u.ID()
	case *DevUser:
		userID = u.ID()
	default:
		return false
	}
	if !portview.CanonicalID(userID) || !portview.CanonicalID(agent.ID) {
		return false
	}
	if !s.conduitRoutable() || !isLoopbackHost(exposed.Host) {
		return false
	}
	escSub, ok := portLinkSubpath(r.URL.EscapedPath())
	if !ok {
		return false
	}
	if !s.portViewCapabilityKeys().configured() {
		return false
	}
	if _, ok := s.portViewIdentity(r.Context(), userID, agent); !ok {
		slog.Debug("Port view: not opened for this user; serving the port link", "agent_id", agent.ID, "port", exposed.Port)
		return false
	}
	exp := s.portViewNow().Add(s.portViewTTL()).Truncate(time.Second)
	capability, err := s.mintPortView(agent.ID, exposed.Port, userID, exp)
	if err != nil {
		// No key may sign now: the port link serves the request.
		return false
	}
	// A sub-path the page view route would not accept (isPortViewRequest)
	// keeps the port link.
	if u, err := url.Parse(portview.Route + capability + "/" + escSub); err != nil || !isPortViewRequest(&http.Request{URL: u}) {
		return false
	}
	loc := relativeAPIRoot(r.URL.EscapedPath()) + "portview/" + capability + "/" + escSub
	if r.URL.RawQuery != "" {
		loc += "?" + encodeLocationQuery(r.URL.RawQuery)
	}
	w.Header().Set("Location", loc)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return true
}

// redirectToPortLink sends a page navigation on the page view route to
// the port's link with the same sub-path and query, where the caller is
// checked and a page link is created for them.
func redirectToPortLink(w http.ResponseWriter, r *http.Request, agentID string, port int, escSub string) {
	loc := relativeAPIRoot(r.URL.EscapedPath()) + "agents/" + agentID + "/ports/" + strconv.Itoa(port) + "/proxy/" + escSub
	if r.URL.RawQuery != "" {
		loc += "?" + encodeLocationQuery(r.URL.RawQuery)
	}
	w.Header().Set("Location", loc)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

// relativeAPIRoot returns the relative reference from escPath, a path
// under /api/v1/, to /api/v1/ itself ("../" once per slash after
// /api/v1/), so a Location built on it resolves against the URL the
// client used, whatever prefix a front proxy put in front of the hub.
func relativeAPIRoot(escPath string) string {
	n := strings.Count(strings.TrimPrefix(escPath, "/api/v1/"), "/")
	if n == 0 {
		return "./"
	}
	return strings.Repeat("../", n)
}

// portLinkSubpath returns the escaped sub-path after
// /api/v1/agents/{id}/ports/{port}/proxy/ in escPath.
func portLinkSubpath(escPath string) (string, bool) {
	segs := strings.SplitN(escPath, "/", 9)
	// "", "api", "v1", "agents", id, "ports", port, "proxy", sub
	if len(segs) != 9 || segs[0] != "" || segs[1] != "api" || segs[2] != "v1" || segs[3] != "agents" ||
		segs[5] != "ports" || segs[7] != "proxy" {
		return "", false
	}
	return segs[8], true
}

// portViewStrippedRequestHeader reports whether a request header is
// removed before a page view request reaches the app: the hub's own
// credential headers (sensitiveHeader) and the identity headers a
// federated caller or a forwarding hub may add. The front proxy's identity
// headers are removed on every port proxy surface
// (stripFrontProxyIdentityHeaders).
func portViewStrippedRequestHeader(k string) bool {
	if sensitiveHeader(k) {
		return true
	}
	switch strings.ToLower(k) {
	case strings.ToLower(FederationTokenHeader),
		"x-forwarded-user-id", "x-forwarded-user-email", "x-forwarded-user-name", "x-forwarded-user-role":
		return true
	default:
		return false
	}
}

// portViewModifyResponse applies the page view rules to the headers of a
// proxied app response, after the shared ones (stripFromProxyResponse):
// the app's Referrer-Policy is dropped, because the hub sets its own on
// every page view answer (setPortViewResponseHeaders), and a response
// without Cache-Control gets "private".
func portViewModifyResponse(h http.Header) {
	h.Del("Referrer-Policy")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "private")
	}
}
