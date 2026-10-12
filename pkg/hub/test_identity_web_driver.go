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
	"net/http"
	"strings"
)

// Test identity web sessions on proxy-auth hubs (ptone/scion#4240, phase
// WS2).
//
// Behind an authenticating proxy such as IAP, a browser driver (for
// example a test-UI service account) reaches the hub as itself: every
// request carries the proxy's verified assertion naming the driver. Such a
// driver is listed in auth.proxy.driverPrincipals and is a transport, never
// a user:
//   - it is refused wherever it would otherwise find or create a user: a
//     verified assertion on the web fresh-identity path
//     (proxyAuthMiddleware) and at API Step 3a (UnifiedAuthMiddleware) gets
//     403 driver_principal_has_no_session; a Google identity is refused by
//     GoogleIdentityResolver.Resolve (ErrAccessDenied, a plain 403), which
//     both Google paths use (the external-bearer path and the GE exchange).
//     TestTIWebIAP_TransportIdentityInvariant keeps it that way;
//   - it may redeem a web-login code (handleTestIdentityRedeem verifies
//     the assertion itself and records its email as the session's driver,
//     tiDriver);
//   - it may carry the test identity web session it redeemed: in proxy
//     mode a test identity web session is honoured only when the request
//     carries a valid assertion for exactly that driver (the binding,
//     testIdentityDriverBinding, part of fixtureSessionValid and so of the
//     one classifier every session consumer uses).
//
// Pages cannot send Proxy-Authorization, so a browser driver puts its
// Google ID token in Authorization. On /api/v1/ that header would win over
// the session cookie. fixtureSessionBearer replaces it with the test
// identity's own per-request token, and only when every condition holds
// (proxy mode; a valid test identity web session, binding included; a
// bearer that is not hub-issued; a Google ID token for the configured
// driver token audience whose email is both the current assertion's and
// the session's driver). The result is never more than the test identity.
//
// On hubs that are not in proxy auth mode none of this applies: there is no
// assertion to bind to, redeem needs none, and a non-empty driverPrincipals
// list refuses startup.

const (
	// driverPrincipalNoSessionReason is the details.reason of the 403 a
	// driver principal gets wherever it would otherwise be a user.
	driverPrincipalNoSessionReason = "driver_principal_has_no_session"
	// fixtureSessionOverExternalBearerReason is the debug log reason of a
	// replaced Authorization bearer (fixtureSessionBearer).
	fixtureSessionOverExternalBearerReason = "fixture_session_over_external_bearer"

	// testIdentityWebCauseNoDriverPrincipals: proxy auth mode with an empty
	// auth.proxy.driverPrincipals list. The member tier still runs.
	testIdentityWebCauseNoDriverPrincipals = "proxy_auth_without_driver_principals"
	// testIdentityWebCauseNoProxyAuthenticator: proxy auth mode with no
	// proxy authenticator to verify assertions with.
	testIdentityWebCauseNoProxyAuthenticator = "proxy_authenticator_unavailable"
	// testIdentityWebCauseAuthModeMismatch: the web server and the hub
	// disagree about proxy auth mode.
	testIdentityWebCauseAuthModeMismatch = "auth_mode_mismatch"

	// testIdentityWebNoDriversLogLine is the startup log line of a proxy
	// hub with test identities on and no driver principals.
	testIdentityWebNoDriversLogLine = "test-identity web sessions unavailable: proxy auth mode with no driverPrincipals"

	// proxyDriverPrincipalUserMessage is the message of an admin request
	// that would create a user for a proxy driver principal. It is generic
	// on purpose: the settings API does not reveal the driver list, so the
	// user-facing answer does not either. The specific reason is logged.
	proxyDriverPrincipalUserMessage = "this email cannot be a hub user"

	// testIdentityRedeemEmailKeyPrefix starts the redeem rate-limit key of
	// a verified proxy assertion email, keeping it apart from source IP
	// keys in the same limiter.
	testIdentityRedeemEmailKeyPrefix = "proxy-email:"
)

// driverPrincipalSet is a set of exact, lower-cased driver principal
// emails. The zero value is empty and safe to use.
type driverPrincipalSet map[string]struct{}

// newDriverPrincipalSet builds the set from a list, trimming and
// lower-casing each entry and skipping empty ones. Validation
// (config.NormalizeProxyDriverPrincipals) happens at startup.
func newDriverPrincipalSet(list []string) driverPrincipalSet {
	if len(list) == 0 {
		return nil
	}
	s := make(driverPrincipalSet, len(list))
	for _, e := range list {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			s[e] = struct{}{}
		}
	}
	return s
}

// contains reports whether email is a driver principal: an exact,
// case-insensitive match.
func (s driverPrincipalSet) contains(email string) bool {
	if len(s) == 0 || email == "" {
		return false
	}
	_, ok := s[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

// empty reports whether the set has no entries.
func (s driverPrincipalSet) empty() bool { return len(s) == 0 }

// Transport identities: the hub's configured platform/transport service
// account (isReservedPlatformIdentity) and its proxy driver principals.
// Neither ever becomes, signs in as, or is served as a hub user. Every
// place that would provision a user, mint or honour a user credential, or
// keep a user session checks the one predicate of its layer
// (Server.isTransportIdentity, WebServer.isTransportIdentity,
// AuthConfig.isTransportIdentity, GoogleIdentityResolver.isTransportIdentity);
// TestTIWebIAP_TransportIdentityInvariant keeps it that way.

// isTransportIdentity reports whether email is a transport identity of s.
func (s *Server) isTransportIdentity(email string) bool {
	return isReservedPlatformIdentity(email, s.platformAuthSA) || s.proxyDrivers.contains(email)
}

// isTransportIdentity reports whether email is a transport identity of ws.
func (ws *WebServer) isTransportIdentity(email string) bool {
	return isReservedPlatformIdentity(email, ws.config.PlatformAuthSA) || ws.proxyDrivers.contains(email)
}

// isTransportIdentity reports whether email is a transport identity of cfg.
func (cfg AuthConfig) isTransportIdentity(email string) bool {
	return isReservedPlatformIdentity(email, cfg.PlatformAuthSA) || cfg.DriverPrincipals.contains(email)
}

// isTransportIdentity reports whether email is a transport identity of r.
func (r *GoogleIdentityResolver) isTransportIdentity(email string) bool {
	return isReservedPlatformIdentity(email, r.platformAuthSA) || r.driverPrincipals.contains(email)
}

// writeDriverPrincipalRefusal answers a driver principal request that has no
// test identity web session to carry: 403 driver_principal_has_no_session.
func writeDriverPrincipalRefusal(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"this proxy principal is a test identity web session driver and has no hub session of its own",
		map[string]interface{}{"reason": driverPrincipalNoSessionReason})
}

// SetDriverTokenValidator replaces the verifier of drivers' Google ID tokens
// (fixtureSessionBearer). Tests use it to verify against a local key set.
func (ws *WebServer) SetDriverTokenValidator(v GoogleCredentialValidator) {
	ws.driverTokenValidator = v
}

// proxyMode reports whether the web server is in proxy auth mode.
func (ws *WebServer) proxyMode() bool { return ws.config.AuthMode == "proxy" }

// proxyWebSessionGate is the proxy-mode part of the web server's test
// identity gate: it needs a proxy authenticator to verify assertions with
// and at least one driver principal. Outside proxy mode it always passes.
func (ws *WebServer) proxyWebSessionGate() (bool, string) {
	if !ws.proxyMode() {
		return true, ""
	}
	if ws.config.ProxyAuthenticator == nil {
		return false, testIdentityWebCauseNoProxyAuthenticator
	}
	if ws.proxyDrivers.empty() {
		return false, testIdentityWebCauseNoDriverPrincipals
	}
	return true, ""
}

// verifiedProxyAssertion verifies the request's proxy assertion with the
// configured proxy authenticator. It returns the verified email, or "" with
// "driver_assertion_missing" or "driver_assertion_invalid".
func (ws *WebServer) verifiedProxyAssertion(r *http.Request) (email, problem string) {
	if ws.config.ProxyAuthenticator == nil {
		return "", testIdentityWebCauseNoProxyAuthenticator
	}
	info, err := ws.config.ProxyAuthenticator.Authenticate(r)
	switch {
	case err != nil:
		return "", "driver_assertion_invalid"
	case info == nil || info.Email == "":
		return "", "driver_assertion_missing"
	}
	return strings.ToLower(info.Email), ""
}

// testIdentityDriverBinding checks the driver binding of a test identity web
// session (C1). It returns the verified assertion email (proxy mode only)
// and "" when the binding holds, or the refusal reason.
//
// In proxy mode the session must name a driver (marker cookie and session
// value agreeing), that driver must still be configured, and this request
// must carry a valid proxy assertion for exactly that driver. An empty or
// missing driver is refused: a session made on a hub without proxy auth, or
// before the binding existed, is never honoured behind the proxy.
//
// Outside proxy mode there is nothing to bind to; a session that names a
// driver (made while the hub ran behind the proxy) is refused there too.
//
// On an open event stream this runs on every heartbeat against the
// request the stream was opened with, so the stream also ends once that
// request's assertion expires; the browser then reconnects with a fresh
// one.
func (ws *WebServer) testIdentityDriverBinding(r *http.Request, marker *tiMarkerValue, sessDriver string) (assertionEmail, reason string) {
	if !ws.proxyMode() {
		if marker.Driver != "" || sessDriver != "" {
			return "", "driver_bound_session_without_proxy_auth"
		}
		return "", ""
	}
	switch {
	case marker.Driver == "":
		return "", "driver_binding_missing"
	case sessDriver != marker.Driver:
		return "", "driver_mismatch_in_session"
	case !ws.proxyDrivers.contains(marker.Driver):
		return "", "driver_not_configured"
	}
	email, problem := ws.verifiedProxyAssertion(r)
	if problem != "" {
		return "", problem
	}
	if email != strings.ToLower(marker.Driver) {
		return "", "driver_assertion_mismatch"
	}
	return email, ""
}

// testIdentityRedeemEmailRateKey is the redeem rate-limit key of a verified
// proxy assertion email.
func testIdentityRedeemEmailRateKey(email string) string {
	return testIdentityRedeemEmailKeyPrefix + strings.ToLower(email)
}

// fixtureSessionBearer decides whether a valid test identity web session
// takes the place of the request's Authorization bearer on /api/v1/, and
// returns the test identity's per-request token when it does, or "" when
// the request must keep today's behaviour (the bearer goes on to the hub's
// own authentication: hub token, external bearer path, or 401).
//
// Every condition must hold:
//   - the web server is in proxy auth mode and a driver token audience is
//     configured (otherwise the rule never applies);
//   - the request carries no other hub credential header (agent, broker,
//     federation, issuer delegation);
//   - the bearer is not hub-issued: it is JWT-shaped (so not a PAT, dev
//     token or delegated agent credential) and fails both user token and
//     agent token validation;
//   - the request carries a valid test identity web session, including
//     the driver binding to this request's verified assertion;
//   - the bearer verifies as a Google-signed ID token (the hub's Google
//     ID token verifier) whose only audience is the driver token audience;
//   - its email equals both the current verified assertion email and the
//     session's driver.
//
// A test identity session that the classifier refuses on such a request
// (behind proxy auth, with a bearer that is not hub-issued) is cleared
// through w, as on every other consumer; the bearer still goes on
// unchanged.
//
// The result is only ever the test identity's own token, so the request
// never gets more than the test identity could do with its issued token.
func (ws *WebServer) fixtureSessionBearer(w http.ResponseWriter, r *http.Request) string {
	if !ws.proxyMode() || ws.config.ProxyAuthenticator == nil || ws.sessionStore == nil || ws.userTokenSvc == nil {
		return ""
	}
	if _, present := ws.readTestIdentityMarker(r); !present {
		return ""
	}
	for _, h := range []string{"X-Scion-Agent-Token", "X-Scion-Broker-ID", FederationTokenHeader, testIssuerDelegationHeader} {
		if r.Header.Get(h) != "" {
			return ""
		}
	}
	token := extractBearerToken(r)
	if token == "" || detectTokenType(token) != tokenTypeUser {
		return ""
	}
	if _, err := ws.userTokenSvc.ValidateUserToken(token); err == nil {
		return ""
	}
	if hub := ws.testIdentityHub; hub != nil && hub.agentTokenService != nil {
		if _, err := hub.agentTokenService.ValidateAgentToken(token); err == nil {
			return ""
		}
	}

	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		return ""
	}
	res := ws.classifyTestIdentitySession(r, session, nil)
	if res.state == tiSessionInvalid {
		ws.logger().Info("Clearing test identity web session", "reason", res.reason)
		ws.clearTestIdentitySession(w, r, session)
		return ""
	}
	if res.state != tiSessionValid || res.marker == nil || res.assertionEmail == "" {
		return ""
	}

	audience := strings.TrimSpace(ws.config.ProxyDriverTokenAudience)
	if audience == "" || ws.driverTokenValidator == nil {
		return ""
	}
	id, err := ws.driverTokenValidator.ValidateIDToken(r.Context(), token, []string{audience})
	if err != nil || id == nil {
		ws.logger().Debug("Driver bearer not replaced: not a valid driver ID token", "error", err)
		return ""
	}
	if _, auds := unverifiedJWTPayload(token); len(auds) != 1 || auds[0] != audience {
		return ""
	}
	email := strings.ToLower(id.Email)
	if email == "" || email != res.assertionEmail || email != strings.ToLower(res.marker.Driver) {
		return ""
	}

	tok, err := ws.testIdentityAccessToken(res)
	if err != nil {
		return ""
	}
	ws.logger().Debug("Replacing the driver bearer with the test identity web session token",
		"reason", fixtureSessionOverExternalBearerReason, "test_identity_id", res.row.ID)
	return tok
}
