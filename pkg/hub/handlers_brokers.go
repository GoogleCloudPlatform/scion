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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleBrokersEndpoint handles POST /api/v1/brokers.
// Creates a new broker registration with join token.
// Requires an authenticated user holding broker.create, presenting an
// interactive session or a dev credential (see authorizeBrokerCreate);
// broker on-behalf-of credentials are not admitted. Turning on auto-provide
// additionally requires broker.auto_provide. This route is
// classified RouteBrokerHMAC in route_metadata.go, so the permission check
// happens in-handler rather than at the route guard.
func (s *Server) handleBrokersEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	s.createBrokerRegistration(w, r)
}

// createBrokerRegistration creates a new broker with join token.
func (s *Server) createBrokerRegistration(w http.ResponseWriter, r *http.Request) {
	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Require an authenticated user identity. This is deliberately
	// GetUserIdentityFromContext rather than GetIdentityFromContext: a
	// broker's own HMAC identity does not satisfy it, so broker-only
	// self-registration is not admitted here (self-rotation is a distinct,
	// separately authorized path — see handleBrokerRotateSecret).
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Unauthorized(w)
		return
	}

	// SECURITY-GATE: broker.create is required for every user-credential path
	// through this handler — first-time registration, re-registration, and
	// re-mint of an existing broker's join token alike. This runs before
	// FindExistingBroker so a caller lacking the permission is denied before
	// any lookup or mutation, and before the additional target
	// owner/super-admin check below, which applies only to the re-register
	// case and is evaluated after this credential restriction.
	if !s.authorizeBrokerCreate(w, r) {
		return
	}

	// Parse request
	var req CreateBrokerRegistrationRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		ValidationError(w, "name is required", map[string]interface{}{
			"field": "name",
		})
		return
	}

	// Validate GCP host SA email format if provided.
	if req.GCPHostServiceAccountEmail != "" {
		if !isValidServiceAccountEmail(req.GCPHostServiceAccountEmail) {
			ValidationError(w, "gcpHostServiceAccountEmail must be a valid GCP service account email "+
				"(name@project.iam.gserviceaccount.com)", map[string]interface{}{
				"field": "gcpHostServiceAccountEmail",
			})
			return
		}
	}

	// If this request matches an existing broker record (by name or by a
	// caller-supplied ID), treat it as re-registration of that broker rather
	// than a brand-new one. Re-registration mutates the existing record and
	// issues a fresh join token, so on top of the broker.create gate above it
	// is additionally gated the same way secret rotation is: the caller must
	// be a super-admin, be the broker itself, or be the user that originally
	// created it. A first-time registration (no existing match) requires only
	// the broker.create gate above; the caller becomes the new broker's
	// owner.
	existingBroker, err := s.brokerAuthService.FindExistingBroker(r.Context(), req.Name, req.BrokerID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if existingBroker != nil {
		brokerIdent := GetBrokerIdentityFromContext(r.Context())
		allowed, err := s.authorizedForBrokerRotate(r.Context(), user, brokerIdent, existingBroker.ID,
			func() (*store.RuntimeBroker, error) { return existingBroker, nil })
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		if !allowed {
			logAuthzDenial(r, user, Resource{Type: "broker", ID: existingBroker.ID}, Action("reregister"),
				"caller is not the broker's creator, the broker itself, or a super-admin")
			Forbidden(w)
			return
		}
	}

	// Turning auto-provide on offers the broker to every project on the
	// hub, so it needs broker.auto_provide in addition to registration.
	// Keeping an existing auto-provide setting, or turning it off, needs
	// nothing extra.
	// ptone/scion#2107: a preserveSettings request writes no settings and
	// joins this condition as "&& !req.PreserveSettings".
	if req.AutoProvide && (existingBroker == nil || !existingBroker.AutoProvide) {
		if !s.authorizeBrokerAutoProvide(w, r) {
			return
		}
	}

	// Create the broker registration. Pin the mutation to what was just
	// authorized above, so a lookup race between the authorization check
	// and this call cannot redirect it onto a broker the caller was not
	// authorized against — whether that means reusing a specific existing
	// broker, or, when none matched, creating a genuinely new one.
	var resp *CreateBrokerRegistrationResponse
	if existingBroker != nil {
		resp, err = s.brokerAuthService.CreateBrokerRegistrationForAuthorizedMatch(r.Context(), req, user.ID(), existingBroker.ID)
	} else {
		resp, err = s.brokerAuthService.CreateBrokerRegistrationForAuthorizedNew(r.Context(), req, user.ID())
	}
	if err != nil {
		writeBrokerRegistrationError(w, err)
		return
	}

	// Log audit event
	LogRegistrationEvent(r.Context(), s.auditLogger, resp.BrokerID, req.Name, user.ID(), getClientIP(r))

	writeJSON(w, http.StatusCreated, resp)
}

// writeBrokerRegistrationError maps an error from
// CreateBrokerRegistrationForAuthorizedMatch or
// CreateBrokerRegistrationForAuthorizedNew to an HTTP response. A stale-pin
// refusal (ErrBrokerRegistrationAuthorizationStale) means the broker match
// changed between the authorization check and the mutation — a
// client-observable conflict the caller can retry, not a server fault — so
// it maps to 409 rather than the generic 500 used for everything else.
func writeBrokerRegistrationError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBrokerRegistrationAuthorizationStale) {
		Conflict(w, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
		"failed to create broker registration: "+err.Error(), nil)
}

// authorizeBrokerCreate is the single broker.create gate for every
// user-credential path that creates or re-mints a broker's join token:
// POST /api/v1/brokers (createBrokerRegistration) and the embedded-broker
// path reached through POST /api/v1/projects/register
// (handlers_projects_core.go). Both call this exact helper rather than
// reproducing the check, so a future caller (e.g. ptone/scion#2107's
// join-token mint) inherits the same gate instead of adding a parallel one.
//
// Admitted credentials: an interactive session or a dev credential (see
// requireUserCredentialKind). A broker request acting on behalf of a user,
// and every other credential kind, is denied with 403 before the permission
// check. Bearer credentials are not admitted for broker creation: a request
// authenticated by a user access token, of any boundary or ceiling, is
// denied with 403 before the permission check. Otherwise it wraps the
// standard fail-closed authorize() helper (authorize.go): 401 for no
// identity, 403 when the identity lacks broker.create, true when allowed.
func (s *Server) authorizeBrokerCreate(w http.ResponseWriter, r *http.Request) bool {
	resource := Resource{Type: "broker"}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if IsScopedUserIdentity(identity) || GetCredentialContextFromContext(ctx).Kind == CredentialKindUAT {
		logAuthzDenial(r, identity, resource, ActionCreate, "bearer credentials are not admitted for broker creation")
		Forbidden(w)
		return false
	}
	if !s.requireUserCredentialKind(w, r, resource, ActionCreate, false) {
		return false
	}
	return s.authorize(w, r, resource, ActionCreate)
}

// authorizeBrokerAutoProvide reports whether the caller may turn on a
// broker's auto-provide setting, which offers the broker to every project
// on the hub. The caller must present an admitted user credential (see
// requireUserCredentialKind) and hold broker.auto_provide (granted to
// super-admins). broker.auto_provide has no user access token selector, so
// a user access token is denied by its ceiling. Writes 401/403 and returns
// false when the caller is not allowed.
func (s *Server) authorizeBrokerAutoProvide(w http.ResponseWriter, r *http.Request) bool {
	resource := Resource{Type: "broker"}
	if !s.requireUserCredentialKind(w, r, resource, ActionAutoProvide, true) {
		return false
	}
	return s.authorize(w, r, resource, ActionAutoProvide)
}

// brokerUserCredentialKindAdmitted reports whether the request credential
// recorded in ctx is a user credential admitted for broker registration,
// re-registration and secret rotation: an interactive session or a dev
// credential, plus a user access token when allowUAT is set. A broker
// request acting on behalf of a user (CredentialKindBroker), an agent,
// delivery or federation credential, and an empty or unknown kind are not
// admitted.
//
// The kind is read from the credential context the authentication
// middleware recorded for this request, not re-derived from the request
// identity, because an on-behalf-of identity is an ordinary user identity
// while its credential is the broker's. Only when ctx carries no credential
// context at all is the kind derived from the identity
// (credentialContextForIdentity) before classification.
//
// The identity must also agree with the kind: a scoped user identity is
// admitted only with a user access token kind, and a user access token kind
// only with a scoped user identity.
func brokerUserCredentialKindAdmitted(ctx context.Context, identity Identity, allowUAT bool) bool {
	if isNilIdentity(identity) {
		return false
	}
	kind := GetCredentialContextFromContext(ctx).Kind
	if kind == "" {
		kind = credentialContextForIdentity(identity).Kind
	}
	switch kind {
	case CredentialKindInteractive, CredentialKindDev:
	case CredentialKindUAT:
		if !allowUAT {
			return false
		}
	default:
		return false
	}
	return IsScopedUserIdentity(identity) == (kind == CredentialKindUAT)
}

// requireUserCredentialKind writes a 403 and returns false unless the
// request credential is admitted by brokerUserCredentialKindAdmitted for
// the given allowUAT setting. It runs before any permission check, lookup
// or mutation on the broker registration and rotation paths.
func (s *Server) requireUserCredentialKind(w http.ResponseWriter, r *http.Request, resource Resource, action Action, allowUAT bool) bool {
	identity := GetIdentityFromContext(r.Context())
	if brokerUserCredentialKindAdmitted(r.Context(), identity, allowUAT) {
		return true
	}
	logAuthzDenial(r, identity, resource, action, "credential kind not admitted for broker registration")
	Forbidden(w)
	return false
}

// authorizedForBrokerRotate reports whether the caller may rotate the
// secret of, or re-register, the broker identified by brokerID. Access is
// granted to any one of:
//   - the broker itself, authenticated via HMAC (brokerIdent), for its own
//     brokerID only;
//   - a system-scoped super-admin, or
//   - the user recorded as the broker's creator (RuntimeBroker.CreatedBy),
//
// where the last two arms are evaluated only when the request credential
// is an interactive session or a dev credential
// (brokerUserCredentialKindAdmitted with allowUAT=false), so a broker
// request acting on behalf of a user and a user access token never use
// them. The broker self arm depends only on the HMAC-verified broker
// identity, so it admits a broker rotating its own secret whatever
// on-behalf-of user the request names, and never admits another broker's
// ID.
//
// fetchBroker is only invoked when the earlier checks do not already grant
// access, so callers that already have the broker record on hand can
// return it directly instead of re-fetching.
func (s *Server) authorizedForBrokerRotate(ctx context.Context, user UserIdentity, brokerIdent BrokerIdentity, brokerID string, fetchBroker func() (*store.RuntimeBroker, error)) (bool, error) {
	if isNilIdentity(user) || !brokerUserCredentialKindAdmitted(ctx, user, false) {
		user = nil
	}

	if user != nil && s.authzService.IsSystemAdmin(ctx, user.ID()) {
		return true, nil
	}

	// isNilIdentity, not brokerIdent != nil: BrokerIdentity embeds Identity, so
	// a typed-nil concrete broker identity (see isNilIdentity) is a non-nil
	// interface value and would otherwise reach brokerIdent.BrokerID() below.
	if !isNilIdentity(brokerIdent) && brokerIdent.BrokerID() == brokerID {
		return true, nil
	}

	if user != nil && user.ID() != "" {
		broker, err := fetchBroker()
		if err != nil {
			return false, err
		}
		if broker != nil && broker.CreatedBy != "" && broker.CreatedBy == user.ID() {
			return true, nil
		}
	}

	return false, nil
}

// ownerForNewBroker returns the CreatedBy value to record for a newly
// created broker: the caller's ID when non-empty, or "" otherwise. Called by
// the register path's create-new-broker branch (handlers_projects_core.go).
// The two-phase POST /brokers path sets CreatedBy directly in
// createBrokerRegistration above, not through this helper.
func ownerForNewBroker(callerUser UserIdentity) string {
	if callerUser != nil && callerUser.ID() != "" {
		return callerUser.ID()
	}
	return ""
}

// handleBrokerJoin handles POST /api/v1/brokers/join.
// Completes broker registration with join token exchange.
// This is an unauthenticated endpoint - the join token serves as authentication.
func (s *Server) handleBrokerJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Parse request
	var req BrokerJoinRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// Validate required fields
	if req.BrokerID == "" {
		ValidationError(w, "brokerId is required", map[string]interface{}{
			"field": "brokerId",
		})
		return
	}
	if req.JoinToken == "" {
		ValidationError(w, "joinToken is required", map[string]interface{}{
			"field": "joinToken",
		})
		return
	}

	// Determine hub endpoint
	hubEndpoint := s.config.HubEndpoint
	if hubEndpoint == "" {
		// Fall back to constructing from request
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		hubEndpoint = scheme + "://" + r.Host
	}

	// Complete the join
	resp, err := s.brokerAuthService.CompleteBrokerJoin(r.Context(), req, hubEndpoint)
	if err != nil {
		// Log failed join attempt
		LogJoinEvent(r.Context(), s.auditLogger, req.BrokerID, getClientIP(r), false, err.Error())

		// Determine error type and return appropriate response
		errMsg := err.Error()
		switch errMsg {
		case "invalid join token", "join token does not match broker":
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidJoinToken, errMsg, nil)
		case "join token has expired":
			writeError(w, http.StatusUnauthorized, ErrCodeExpiredJoinToken, errMsg, nil)
		default:
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"failed to complete broker join: "+errMsg, nil)
		}
		return
	}

	// Log successful join
	LogJoinEvent(r.Context(), s.auditLogger, req.BrokerID, getClientIP(r), true, "")

	writeJSON(w, http.StatusOK, resp)
}

// handleBrokerByIDRoutes handles routes under /api/v1/brokers/{id}/...
func (s *Server) handleBrokerByIDRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract broker ID and action from path: /api/v1/brokers/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/brokers/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		NotFound(w, "broker")
		return
	}

	brokerID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "rotate-secret":
		s.handleBrokerRotateSecret(w, r, brokerID)
	default:
		NotFound(w, "broker action")
	}
}

// handleBrokerRotateSecret handles POST /api/v1/brokers/{id}/rotate-secret.
// Rotates the HMAC secret for a broker.
// Admitted credentials (see authorizedForBrokerRotate):
//   - the broker's own HMAC credential, for its own ID only (self-rotation);
//   - an interactive session or a dev credential whose user is a
//     super-admin or the broker's recorded creator.
//
// A user access token and a broker request acting on behalf of a user are
// not admitted for the creator or super-admin arms.
func (s *Server) handleBrokerRotateSecret(w http.ResponseWriter, r *http.Request, brokerID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// Check if broker auth service is available
	if s.brokerAuthService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"broker authentication service not configured", nil)
		return
	}

	// Check authorization - admin user, broker owner, or the broker itself
	user := GetUserIdentityFromContext(r.Context())
	brokerIdent := GetBrokerIdentityFromContext(r.Context())

	authorized, err := s.authorizedForBrokerRotate(r.Context(), user, brokerIdent, brokerID,
		func() (*store.RuntimeBroker, error) { return s.store.GetRuntimeBroker(r.Context(), brokerID) })
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if !authorized {
		var denyIdentity Identity
		switch {
		case user != nil:
			denyIdentity = user
		case brokerIdent != nil:
			denyIdentity = brokerIdent
		}
		logAuthzDenial(r, denyIdentity, Resource{Type: "broker", ID: brokerID}, Action("rotate_secret"),
			"caller is not the broker's creator, the broker itself, or a super-admin")
		Forbidden(w)
		return
	}

	// Parse request (optional)
	var req RotateSecretRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "invalid request body: "+err.Error())
			return
		}
	}

	// Default grace period
	gracePeriod := req.GracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 5 * time.Minute
	}

	// Rotate the secret
	resp, err := s.brokerAuthService.RotateBrokerSecret(r.Context(), brokerID, gracePeriod)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to rotate secret: "+err.Error(), nil)
		return
	}

	// Log audit event
	actorID := ""
	actorType := "system"
	if user != nil {
		actorID = user.ID()
		actorType = "user"
	} else if brokerIdent != nil {
		actorID = brokerIdent.BrokerID()
		actorType = "broker"
	}
	LogRotateEvent(r.Context(), s.auditLogger, brokerID, actorID, actorType, getClientIP(r))

	writeJSON(w, http.StatusOK, resp)
}
