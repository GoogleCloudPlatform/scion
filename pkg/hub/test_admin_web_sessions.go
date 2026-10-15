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

// Admin-kind test identity web sessions (ptone/scion#4240, phase WS4).
//
// The admin kinds (test_hub_admin, test_super_admin) may hold a browser web
// session, through the same single-use web-login code and redeem as the
// member tier (handlers_test_identity_web.go), only when all of these hold:
//
//   - server.hub.test_identities.admin_web_sessions is set, and its value
//     is this hub's resolved (persistent) hub id. The setting is read once
//     at startup, lives in the settings file or environment only, and is
//     never writable through the API (neverAPIWritableSettingsKeys covers
//     server.hub.test_identities and every key under it);
//   - --test-mode is admin or super-admin. test_hub_admin needs admin or
//     above; test_super_admin needs super-admin. The setting with a lower
//     level refuses startup: the level is never lowered and the setting is
//     never ignored;
//   - the test admin audit sink is in place (the A2 sink), so every
//     session-originated admin request is written to it;
//   - member web sessions are available (testIdentityWebSessionGate: the
//     hub-local web session key and, behind proxy auth, driver principals).
//
// The Helm chart renders only --test-mode=member (G9), so a Helm-deployed
// hub with the setting refuses to start: admin web sessions run on systemd
// test hubs only.
//
// An admin web session is capped at the kind's maximum identity lifetime
// (testAdminWebSessionExpiry), and the cap is re-checked on every session
// check. The token minted per request is the identity's own claims with
// client type web: the same interactive credential as the identity's own
// bearer token, so it carries no extra authority.

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestAdminWebSessionsConfig is ServerConfig.TestAdminWebSessions: the
// admin web session setting cmd validated at startup.
type TestAdminWebSessionsConfig struct {
	// HubID is the value of server.hub.test_identities.admin_web_sessions:
	// the hub id the setting is bound to. Empty means off.
	HubID string
}

func (c TestAdminWebSessionsConfig) requested() bool { return c.HubID != "" }

// Named refusals of the admin web session setting. Callers wrap them with
// %w and add detail; match with errors.Is.
var (
	ErrTestAdminWebSessionsBelowAdmin = testAdminGateErr("admin_web_sessions", "below_admin_level",
		config.TestIdentityAdminWebSessionsKey+" requires --test-mode=admin or --test-mode=super-admin; remove the setting or start the hub at an admin level")
	ErrTestAdminWebSessionsHubIDMismatch = testAdminGateErr("admin_web_sessions", "hub_id_mismatch",
		"the value of "+config.TestIdentityAdminWebSessionsKey+" must equal this hub's resolved hub id")
	ErrTestAdminWebSessionsRequiresAuditSink = testAdminGateErr("admin_web_sessions", "requires_audit_sink",
		config.TestIdentityAdminWebSessionsKey+" requires the test admin audit sink")
)

// validateTestAdminWebSessionsConfig re-checks, at construction, the
// setting cmd validated: an admin level whose gate is on, and a hub id
// binding equal to cfg.HubID.
func validateTestAdminWebSessionsConfig(cfg ServerConfig) error {
	c := cfg.TestAdminWebSessions
	if !c.requested() {
		return nil
	}
	if !cfg.TestMode.level().IsAdminLevel() || !cfg.TestAdminGate.HubAdmin {
		return fmt.Errorf("%w: --test-mode is %q", ErrTestAdminWebSessionsBelowAdmin, cfg.TestMode.level())
	}
	if cfg.HubID == "" || c.HubID != cfg.HubID {
		return fmt.Errorf("%w: setting %q, resolved hub id %q", ErrTestAdminWebSessionsHubIDMismatch, c.HubID, cfg.HubID)
	}
	return nil
}

// testAdminWebState is the immutable admin web session switch captured by
// New. It has no setter.
type testAdminWebState struct {
	on bool
}

// Causes for unavailable admin web sessions.
const (
	testAdminWebCauseOff     = "admin_web_sessions_off"
	testAdminWebCauseKindOff = "admin_kind_off"
)

// testAdminWebSessionGate reports whether an identity of the admin kind may
// hold a web session on this hub, and the cause when it may not: the
// setting is on, the kind is on (test_hub_admin at admin level or above,
// test_super_admin at super-admin level), and member web sessions are
// available.
func (s *Server) testAdminWebSessionGate(kind testAdminKind) (bool, string) {
	if !s.testAdminWeb.on {
		return false, testAdminWebCauseOff
	}
	if !s.testAdminTierEnabled(kind) {
		return false, testAdminWebCauseKindOff
	}
	return s.testIdentityWebSessionGate()
}

// testAdminWebSessionsAvailable reports whether admin web sessions are
// available for at least the test hub-admin kind, for hub status.
func (s *Server) testAdminWebSessionsAvailable() bool {
	ok, _ := s.testAdminWebSessionGate(testAdminKindHubAdmin)
	return ok
}

// testIdentityWebKindAdmitted reports whether u's kind may hold a web
// session on this hub: kind test_fixture (member and viewer) whenever
// member web sessions are offered, and an admin kind only while
// testAdminWebSessionGate admits it. Every other kind, an unrecognised one
// included, is refused.
func (s *Server) testIdentityWebKindAdmitted(u *store.User) bool {
	switch {
	case u == nil:
		return false
	case u.IsTestFixture():
		return true
	case u.IsTestAdmin():
		ok, _ := s.testAdminWebSessionGate(testAdminKind(u.Kind))
		return ok
	}
	return false
}

// testAdminKindMaxLifetime returns the maximum identity lifetime of an
// admin kind (60 minutes for test_hub_admin, 30 for test_super_admin), and
// 0 for any other kind.
func testAdminKindMaxLifetime(kind string) time.Duration {
	if spec, ok := testAdminKindSpecFor(kind); ok {
		return spec.maxLifetime
	}
	return 0
}

// testAdminWebSessionExpiry returns the expiry of a web session redeemed at
// now for row: the identity's expiry, and for an admin kind never later
// than now plus the kind's maximum lifetime, nor the identity's creation
// plus that maximum.
func testAdminWebSessionExpiry(row *store.User, now time.Time) time.Time {
	exp := *row.ExpiresAt
	if max := testAdminKindMaxLifetime(row.Kind); max > 0 {
		if c := now.Add(max); c.Before(exp) {
			exp = c
		}
		if !row.Created.IsZero() {
			if c := row.Created.Add(max); c.Before(exp) {
				exp = c
			}
		}
	}
	return exp
}

// testIdentityWebSessionExpiryReason returns "" when a session expiry
// (from the marker cookie) is within row's limits, and the refusal reason
// otherwise: never later than the identity's expiry, and for an admin kind
// never later than the identity's creation plus the kind's maximum
// lifetime.
func testIdentityWebSessionExpiryReason(row *store.User, sessionExpiry time.Time) string {
	if row.ExpiresAt == nil || sessionExpiry.After(*row.ExpiresAt) {
		return "session_expiry_beyond_identity"
	}
	if max := testAdminKindMaxLifetime(row.Kind); max > 0 && !row.Created.IsZero() && sessionExpiry.After(row.Created.Add(max)) {
		return "session_expiry_beyond_kind_cap"
	}
	return ""
}

// logTestAdminWebSessionState writes the startup log line of the admin web
// session setting.
func (s *Server) logTestAdminWebSessionState() {
	if !s.testAdminWeb.on {
		return
	}
	kinds := []string{}
	for _, k := range []testAdminKind{testAdminKindHubAdmin, testAdminKindSuperAdmin} {
		if s.testAdminTierEnabled(k) {
			kinds = append(kinds, string(k))
		}
	}
	if ok, cause := s.testIdentityWebSessionGate(); !ok {
		slog.Warn("Admin test identity web sessions are configured but unavailable: test identity web sessions are unavailable on this hub",
			"setting", config.TestIdentityAdminWebSessionsKey, "kinds", kinds, "cause", cause)
		return
	}
	slog.Warn("Admin test identity web sessions are ENABLED: admin-kind test identities can hold browser sessions on this hub. Test hubs only.",
		"setting", config.TestIdentityAdminWebSessionsKey, "kinds", kinds)
}

// testAdminKindSpecFor returns the spec of an admin kind.
func testAdminKindSpecFor(kind string) (testAdminKindSpec, bool) {
	for _, spec := range testAdminKindSpecs {
		if string(spec.kind) == kind {
			return spec, true
		}
	}
	return testAdminKindSpec{}, false
}

// testAdminWebCodeIssuer runs, for a web-login code for the admin-kind
// identity u, the issuer checks of u's kind on top of the shared caller
// checks (testIdentityCallerFor) and canManageTestIdentity: the request
// holds the kind's capability (from the handler-level decision in allowed,
// or decided here when that decision stopped at another permission),
// the caller is a live super-admin through its own current bindings
// holding that capability (canIssueTestAdmin), and an access-token caller
// presents a hub-boundary token with exactly the kind's scope, at most
// testAdminIssuerPATMaxAge old (verifyTestAdminIssuerPAT). A code is a
// credential for an admin-kind identity, so it takes what issuing one
// takes. It writes 403 and returns false on refusal.
func (s *Server) testAdminWebCodeIssuer(w http.ResponseWriter, r *http.Request, caller UserIdentity, u *store.User, allowed map[string]bool) bool {
	ctx := r.Context()
	spec, ok := testAdminKindSpecFor(u.Kind)
	if !ok {
		NotFound(w, "test identity")
		return false
	}
	resource := Resource{Type: permissions.ResourceTestIdentity}
	action := Action(permissionAction(spec.permission))
	deny := func(reason, detailReason string) bool {
		logAuthzDenial(r, caller, resource, action, reason)
		details := map[string]interface{}(nil)
		if detailReason != "" {
			details = map[string]interface{}{"reason": detailReason}
		}
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "requires "+spec.permission+" permission", details)
		return false
	}
	if !allowed[spec.permission] {
		// testIdentityCallerFor stops at the first allowed permission, so
		// the kind's capability is decided here, for an admin-kind code
		// only.
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:      principalContextForIdentity(caller),
			Credential:     credentialContextForIdentity(caller),
			Resource:       resource,
			Action:         action,
			Permission:     spec.permission,
			TargetEvidence: hubCollectionEvidence(spec.permission),
		})
		if !decision.Allowed {
			return deny(decision.Reason, "")
		}
	}
	if err := s.canIssueTestAdmin(ctx, caller.ID(), spec.permission); err != nil {
		return deny(err.Error(), testAdminReasonNotLiveAdmin)
	}
	cred := GetCredentialContextFromContext(ctx)
	switch cred.Kind {
	case CredentialKindInteractive, CredentialKindDev:
	case CredentialKindUAT:
		credentialID := cred.ID
		if scoped, ok := caller.(*ScopedUserIdentity); ok && scoped != nil && scoped.CredentialID() != "" {
			credentialID = scoped.CredentialID()
		}
		if err := s.verifyTestAdminIssuerPAT(ctx, caller.ID(), credentialID, uatScopeForPermission(spec.permission)); err != nil {
			return deny(err.Error(), testAdminReasonIssuerPAT)
		}
	default:
		return deny("credential kind not admitted for an admin-kind web-login code", "")
	}
	return true
}
