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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Admin-kind test identities (ptone/scion#4240, admin tier phase A1).
//
// POST /api/v1/test-identities/admin issues a short-lived test identity
// holding a real, expiring system hub-admin binding (kind test_hub_admin,
// email in store.TestHubAdminEmailDomain) and returns one access token for
// it. The test_super_admin kind is defined but its issuance answers 404
// until the admin tier's policy phase (A2) ships and its own gate is on.
// POST /api/v1/test-identities/admin/revoke-all deletes every admin-kind
// identity, for a human super-admin session.
//
// Both routes answer 404 unless the hub started with the test admin gate
// on (test_admin_gate.go). The issuer must be a human (kindForPrincipal),
// hold the kind's capability (test_identity.issue_admin), and be a live
// super-admin through its own current bindings (canIssueTestAdmin). An
// access-token issuer must present a hub-boundary token carrying exactly
// the capability's scope, at most testAdminIssuerPATMaxAge old, which is
// loaded and checked on every issuance. Issuance writes the user row, its
// binding, the hub grant sync, a test_admin_issue mutation audit and a
// test admin audit sink line in one transaction.

// Admin-kind lifetimes (admin tier design §4.2). A token never outlives
// its identity.
const (
	testHubAdminDefaultTokenTTL   = 15 * time.Minute
	testHubAdminMaxTokenTTL       = 30 * time.Minute
	testHubAdminDefaultLifetime   = 60 * time.Minute
	testHubAdminMaxLifetime       = 60 * time.Minute
	testSuperAdminDefaultTokenTTL = 10 * time.Minute
	testSuperAdminMaxTokenTTL     = 15 * time.Minute
	testSuperAdminDefaultLifetime = 30 * time.Minute
	testSuperAdminMaxLifetime     = 30 * time.Minute
)

// Admin-kind caps and rates (admin tier design §4.3), counted separately
// from the member tier's.
const (
	// testAdminPerIssuerCap is the number of live admin-kind identities
	// (either kind) one issuer may hold at once.
	testAdminPerIssuerCap = 1
	// testAdminHubCap is the number of live admin-kind identities the hub
	// holds at once.
	testAdminHubCap = 2
	// testSuperAdminHubCap is how many of those may be test_super_admin.
	testSuperAdminHubCap = 1
	// Issuance rates per issuer: 6 per hour for hub-admin, 2 per hour for
	// super-admin.
	testHubAdminRatePerHour   = 6
	testSuperAdminRatePerHour = 2
	// testAdminIssuerPATMaxAge is the oldest an issuer access token may be
	// (DEC-3 C2, 8 h).
	testAdminIssuerPATMaxAge = 8 * time.Hour

	testAdminPurposeMinRunes = 8
	testAdminEmailPrefix     = "test-admin"
)

// Capabilities that admit admin-kind issuance. Both are in no built-in role
// other than super-admin (allPermissionIDs).
const (
	permissionTestIdentityIssueAdmin      = "test_identity.issue_admin"
	permissionTestIdentityIssueSuperAdmin = "test_identity.issue_super_admin"
)

// Admin request roles.
const (
	testAdminRoleHubAdmin   = "hub-admin"
	testAdminRoleSuperAdmin = "super-admin"
)

// Mutation audit types and details.reason values of the admin kinds.
const (
	testAdminIssueMutation  = "test_admin_issue"
	testAdminRevokeMutation = "test_admin_revoke"

	testAdminReasonIssuerCap       = "admin_issuer_cap_reached"
	testAdminReasonHubCap          = "admin_hub_cap_reached"
	testAdminReasonSuperAdminCap   = "super_admin_hub_cap_reached"
	testAdminReasonNotLiveAdmin    = "issuer_not_live_super_admin"
	testAdminReasonIssuerPAT       = "issuer_token_not_admitted"
	testAdminReasonCallerNotHuman  = "test_identity_cannot_issue"
	testAdminReasonAdminKindTokens = "test_admin_token_not_reissued"
)

// Test admin audit sink events written by this file.
const (
	testAdminAuditEventIssue  = "test_admin_issue"
	testAdminAuditEventRevoke = "test_admin_revoke"
)

var (
	errTestAdminIssuerCap     = errors.New("live test admin identity limit for this issuer reached")
	errTestAdminHubCap        = errors.New("live test admin identity limit for this hub reached")
	errTestSuperAdminHubCap   = errors.New("live test super-admin identity limit for this hub reached")
	errTestAdminIssuerPAT     = errors.New("issuer access token is not admitted for test admin issuance")
	errTestAdminIssuerNotLive = errors.New("issuer is not a live super-admin")
	errTestAdminRateLimited   = errors.New("test admin issuance rate limit reached for this issuer")
)

// testAdminKindSpec holds one admin kind's issuance parameters.
type testAdminKindSpec struct {
	kind            testAdminKind
	systemRole      string
	permission      string
	defaultTokenTTL time.Duration
	maxTokenTTL     time.Duration
	defaultLifetime time.Duration
	maxLifetime     time.Duration
}

// testAdminKindSpecs maps the request's adminRole to its kind.
var testAdminKindSpecs = map[string]testAdminKindSpec{
	testAdminRoleHubAdmin: {
		kind: testAdminKindHubAdmin, systemRole: store.SystemRoleHubAdmin, permission: permissionTestIdentityIssueAdmin,
		defaultTokenTTL: testHubAdminDefaultTokenTTL, maxTokenTTL: testHubAdminMaxTokenTTL,
		defaultLifetime: testHubAdminDefaultLifetime, maxLifetime: testHubAdminMaxLifetime,
	},
	testAdminRoleSuperAdmin: {
		kind: testAdminKindSuperAdmin, systemRole: store.SystemRoleSuperAdmin, permission: permissionTestIdentityIssueSuperAdmin,
		defaultTokenTTL: testSuperAdminDefaultTokenTTL, maxTokenTTL: testSuperAdminMaxTokenTTL,
		defaultLifetime: testSuperAdminDefaultLifetime, maxLifetime: testSuperAdminMaxLifetime,
	},
}

// UAT scopes of the admin issuance capabilities (permissions.Registry).
const (
	uatScopeTestIdentityIssueAdmin      = "test_identity:issue_admin"
	uatScopeTestIdentityIssueSuperAdmin = "test_identity:issue_super_admin"
)

// uatScopeForPermission returns the UAT scope of an admin issuance
// capability, and "" for any other permission.
func uatScopeForPermission(permissionID string) string {
	switch permissionID {
	case permissionTestIdentityIssueAdmin:
		return uatScopeTestIdentityIssueAdmin
	case permissionTestIdentityIssueSuperAdmin:
		return uatScopeTestIdentityIssueSuperAdmin
	}
	return ""
}

// testAdminIssuanceState is the server-side state of admin-kind issuance.
type testAdminIssuanceState struct {
	hubAdminLimiter   *GCPTokenRateLimiter
	superAdminLimiter *GCPTokenRateLimiter
}

func newTestAdminIssuanceState() testAdminIssuanceState {
	return testAdminIssuanceState{
		hubAdminLimiter:   NewGCPTokenRateLimiter(float64(testHubAdminRatePerHour)/3600, testHubAdminRatePerHour),
		superAdminLimiter: NewGCPTokenRateLimiter(float64(testSuperAdminRatePerHour)/3600, testSuperAdminRatePerHour),
	}
}

// limiter returns the per-issuer rate limiter of kind, nil when unset.
func (t *testAdminIssuanceState) limiter(kind testAdminKind) *GCPTokenRateLimiter {
	if kind == testAdminKindSuperAdmin {
		return t.superAdminLimiter
	}
	return t.hubAdminLimiter
}

// kindForPrincipal reads a user principal's stored kind through the
// server's store, sharing the authorization service's per-user cache. It is
// the server's entry to kindForPrincipalVia; see there for the fail-closed
// contract.
func (s *Server) kindForPrincipal(ctx context.Context, userID string) (principalKind, error) {
	if s.store == nil {
		return principalKind{}, errors.New("kind lookup: no store")
	}
	return kindForPrincipalVia(ctx, s.store, s.authzService.kindCache(), userID)
}

// testAdminRowRejection is the admin-kind half of the JWT row block, run
// after testFixtureRejection on the same freshly read row u. It returns the
// row's kind and a non-empty reason when u must not authenticate: an
// admin-kind row whose kind is off on this hub, whose expiry is missing or
// past, whose email is outside its kind's domain, or whose stored role or
// token role claim is not the kind's role. A kind-read error is returned
// and the caller fails closed. kindFn nil classifies u directly; tierFn nil
// treats every admin kind as off.
func testAdminRowRejection(ctx context.Context, kindFn func(context.Context, string) (principalKind, error), tierFn func(string) bool, u *store.User, claimRole string, now time.Time) (principalKind, string, error) {
	if u == nil {
		return principalKind{}, "", nil
	}
	var k principalKind
	var err error
	if kindFn != nil {
		k, err = kindFn(ctx, u.ID)
	} else {
		k, err = classifyUserKind(u)
	}
	if err != nil {
		return principalKind{}, "", err
	}
	if !k.isTestAdmin() {
		return k, "", nil
	}
	switch {
	case store.TestIdentityEmailKind(u.Email) != k.Kind:
		return k, "test_admin_domain_mismatch", nil
	case tierFn == nil || !tierFn(k.Kind):
		return k, "test_admin_tier_disabled", nil
	case k.ExpiresAt == nil || !now.Before(*k.ExpiresAt):
		return k, "test_admin_identity_expired", nil
	case u.Role != store.TestAdminRole(k.Kind):
		return k, "test_admin_role_mismatch", nil
	case claimRole != store.TestAdminRole(k.Kind):
		return k, "test_admin_role_claim_mismatch", nil
	}
	return k, "", nil
}

// testAdminActor is the request context value that marks a request
// authenticated as an admin-kind test identity. It is set only by the JWT
// row block, from the row it read, and feeds mutation audit labels
// (auditActorFromContext).
type testAdminActor struct {
	Kind               string
	IssuerID           string
	IssuerCredentialID string
	IssuanceID         string
	PurposeHash        string
}

type testAdminActorKey struct{}

// contextWithTestAdminActor records u's immutable issuance facts on ctx.
func contextWithTestAdminActor(ctx context.Context, u *store.User) context.Context {
	a := testAdminActor{Kind: u.Kind}
	if u.IssuedBy != nil {
		a.IssuerID = *u.IssuedBy
	}
	if u.IssuerCredentialID != nil {
		a.IssuerCredentialID = *u.IssuerCredentialID
	}
	if u.IssuanceID != nil {
		a.IssuanceID = *u.IssuanceID
	}
	if u.Purpose != nil {
		a.PurposeHash = testAdminPurposeHash(*u.Purpose)
	}
	return context.WithValue(ctx, testAdminActorKey{}, a)
}

// testAdminActorFromContext returns the admin-kind actor of ctx, if any.
func testAdminActorFromContext(ctx context.Context) (testAdminActor, bool) {
	a, ok := ctx.Value(testAdminActorKey{}).(testAdminActor)
	return a, ok
}

// testAdminPurposeHash is the SHA-256 (hex) of an issuance purpose, so
// audit labels correlate a purpose without repeating its text.
func testAdminPurposeHash(purpose string) string {
	sum := sha256.Sum256([]byte(purpose))
	return hex.EncodeToString(sum[:])
}

// labels renders the admin-kind audit labels as a bounded JSON object.
func (a testAdminActor) labels() string {
	return boundedLabelsJSON(map[string]string{
		"test_tier":            "admin",
		"kind":                 a.Kind,
		"issuer_id":            a.IssuerID,
		"issuer_credential_id": a.IssuerCredentialID,
		"issuance_id":          a.IssuanceID,
		"purpose_hash":         a.PurposeHash,
	})
}

// CreateTestAdminIdentityRequest is the request body of POST
// /api/v1/test-identities/admin. Unknown fields are rejected, so no user
// field (kind, email, expiry, issuer, role) can be bound from a request.
type CreateTestAdminIdentityRequest struct {
	// AdminRole is "hub-admin" (or "super-admin", refused with 404 until
	// the admin tier's policy phase).
	AdminRole string `json:"adminRole"`
	// Purpose is required, 8 to 200 characters.
	Purpose string `json:"purpose"`
	// LifetimeSeconds is the identity lifetime. 0 means the kind's
	// default; at most the kind's cap (60 min for hub-admin).
	LifetimeSeconds int64 `json:"lifetimeSeconds,omitempty"`
	// TokenTTLSeconds is the access token lifetime. 0 means the kind's
	// default (15 min for hub-admin); at most the kind's cap (30 min).
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// TestAdminIdentityView is the response view of an admin-kind test
// identity. It never carries a token.
type TestAdminIdentityView struct {
	TestIdentityView
	Kind       string `json:"kind"`
	AdminRole  string `json:"adminRole"`
	IssuanceID string `json:"issuanceId"`
}

// TestAdminIdentityTokenResponse is the response of admin-kind issuance. It
// holds exactly one access token: no refresh token, no cookie.
type TestAdminIdentityTokenResponse struct {
	Identity       TestAdminIdentityView `json:"identity"`
	AccessToken    string                `json:"accessToken"`
	TokenType      string                `json:"tokenType"`
	ExpiresIn      int64                 `json:"expiresIn"`
	TokenExpiresAt time.Time             `json:"tokenExpiresAt"`
}

// RevokedTestAdminIdentity is one entry of the revoke-all response.
type RevokedTestAdminIdentity struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Outcome is "deleted", or "suspended" when the identity owns agents or
	// is a project's last owner (its row stays, suspended and with its role
	// bindings removed, so its tokens stop working at once).
	Outcome string `json:"outcome"`
}

// RevokeAllTestAdminIdentitiesResponse is the response of revoke-all.
type RevokeAllTestAdminIdentitiesResponse struct {
	Revoked []RevokedTestAdminIdentity `json:"revoked"`
	Count   int                        `json:"count"`
}

func testAdminIdentityViewOf(u *store.User, adminRole string, now time.Time) TestAdminIdentityView {
	v := TestAdminIdentityView{TestIdentityView: testIdentityViewOf(u, now), Kind: u.Kind, AdminRole: adminRole}
	if u.IssuanceID != nil {
		v.IssuanceID = *u.IssuanceID
	}
	return v
}

// testAdminGate answers 404 while no admin kind is enabled. It wraps the
// route guard, so the answer does not depend on the caller's permissions.
func (s *Server) testAdminGate404(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.testAdminTierOn() {
			NotFound(w, "endpoint")
			return
		}
		next(w, r)
	}
}

// testAdminTokenTTL resolves a requested token lifetime against the kind's
// default and cap and the identity's remaining lifetime.
func testAdminTokenTTL(spec testAdminKindSpec, requestedSeconds int64, expiresAt, now time.Time) (time.Duration, bool) {
	if requestedSeconds < 0 {
		return 0, false
	}
	ttl := spec.defaultTokenTTL
	if requestedSeconds > 0 {
		if requestedSeconds > int64(spec.maxTokenTTL/time.Second) {
			return 0, false
		}
		ttl = time.Duration(requestedSeconds) * time.Second
	}
	if remaining := expiresAt.Sub(now).Truncate(time.Second); remaining < ttl {
		ttl = remaining
	}
	if ttl < time.Second {
		return 0, false
	}
	return ttl, true
}

// validTestAdminPurpose reports whether purpose is acceptable: 8 to 200
// characters, no control characters.
func validTestAdminPurpose(purpose string) bool {
	n := utf8.RuneCountInString(purpose)
	return n >= testAdminPurposeMinRunes && validTestIdentityPurpose(purpose)
}

// newTestAdminEmail returns a fresh email in kind's reserved domain with a
// 128-bit random local part.
func newTestAdminEmail(kind testAdminKind) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return testAdminEmailPrefix + "-" + hex.EncodeToString(b) + "@" + store.TestAdminEmailDomain(string(kind)), nil
}

// testAdminIssuer runs the issuer checks of admin tier design §4.1 steps
// 2-5 for kind: an admitted credential kind, a human caller
// (kindForPrincipal), the kind's capability, a live super-admin through
// its own current bindings (canIssueTestAdmin, DEC-3 C1), and for an
// access token the server-side token checks (verifyTestAdminIssuerPAT,
// DEC-3 C2 and C3). It returns the issuer and the access token's ID (""
// for a session). It writes the response and returns false on refusal.
func (s *Server) testAdminIssuer(w http.ResponseWriter, r *http.Request, spec testAdminKindSpec) (UserIdentity, string, bool) {
	ctx := r.Context()
	resource := Resource{Type: permissions.ResourceTestIdentity}
	action := Action(permissionAction(spec.permission))
	deny := func(user UserIdentity, reason, detailReason string) (UserIdentity, string, bool) {
		if user != nil {
			logAuthzDenial(r, user, resource, action, reason)
		}
		details := map[string]interface{}(nil)
		if detailReason != "" {
			details = map[string]interface{}{"reason": detailReason}
		}
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "requires "+spec.permission+" permission", details)
		return nil, "", false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, "", false
	}
	user, ok := identity.(UserIdentity)
	if !ok || isNilIdentity(user) {
		return deny(nil, "", "")
	}
	cred := GetCredentialContextFromContext(ctx)
	switch cred.Kind {
	case CredentialKindInteractive, CredentialKindDev, CredentialKindUAT:
	default:
		return deny(user, "credential kind not admitted for test admin issuance", "")
	}
	// A test identity of any kind never issues test identities. The kind
	// is read from the store; a read error or unknown kind denies.
	k, err := s.kindForPrincipal(ctx, user.ID())
	if err != nil || !k.isHuman() {
		return deny(user, "caller is not a human user", testAdminReasonCallerNotHuman)
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(user),
		Credential:     credentialContextForIdentity(user),
		Resource:       resource,
		Action:         action,
		Permission:     spec.permission,
		TargetEvidence: hubCollectionEvidence(spec.permission),
	})
	if !decision.Allowed {
		return deny(user, decision.Reason, "")
	}
	if err := s.canIssueTestAdmin(ctx, user.ID(), spec.permission); err != nil {
		return deny(user, err.Error(), testAdminReasonNotLiveAdmin)
	}
	credentialID := ""
	if cred.Kind == CredentialKindUAT {
		credentialID = cred.ID
		if scoped, ok := identity.(*ScopedUserIdentity); ok && scoped != nil && scoped.CredentialID() != "" {
			credentialID = scoped.CredentialID()
		}
		if err := s.verifyTestAdminIssuerPAT(ctx, user.ID(), credentialID, uatScopeForPermission(spec.permission)); err != nil {
			return deny(user, err.Error(), testAdminReasonIssuerPAT)
		}
	}
	return user, credentialID, true
}

// permissionAction returns the action segment of a permission ID.
func permissionAction(permissionID string) string {
	if i := strings.LastIndex(permissionID, "."); i >= 0 {
		return permissionID[i+1:]
	}
	return permissionID
}

// canIssueTestAdmin is DEC-3 C1: the issuing user is, right now and
// through its own stored state, a live super-admin holding the capability.
// It reads the user row (active, stored role admin, kind human) and the
// user's current bindings (IsSystemAdmin and the binding-derived permission
// set), never token claims. For an access-token issuer the route and
// handler decisions already intersect this with the token's frozen
// ceiling, so revoking the user's super-admin binding stops the next
// issuance even while the token is valid.
func (s *Server) canIssueTestAdmin(ctx context.Context, userID, permission string) error {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("%w: %v", errTestAdminIssuerNotLive, err)
	}
	k, err := classifyUserKind(u)
	if err != nil || !k.isHuman() {
		return fmt.Errorf("%w: not a human user", errTestAdminIssuerNotLive)
	}
	if u.Status != store.UserStatusActive || u.Role != store.UserRoleAdmin {
		return fmt.Errorf("%w: user is not an active admin", errTestAdminIssuerNotLive)
	}
	if !s.authzService.IsSystemAdmin(ctx, userID) {
		return fmt.Errorf("%w: no active super-admin binding", errTestAdminIssuerNotLive)
	}
	perms, err := s.authzService.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, "", "")
	if err != nil {
		return fmt.Errorf("%w: %v", errTestAdminIssuerNotLive, err)
	}
	for _, p := range perms {
		if p == permission {
			return nil
		}
	}
	return fmt.Errorf("%w: capability not held through current bindings", errTestAdminIssuerNotLive)
}

// verifyTestAdminIssuerPAT is DEC-3 C2 and C3: it loads the issuer's
// access token by its ID on the server, on every issuance, and admits it
// only when it exists, belongs to the issuer, is not revoked or expired,
// has a hub boundary, carries exactly the one scope, and is at most
// testAdminIssuerPATMaxAge old. Any load or check failure refuses.
func (s *Server) verifyTestAdminIssuerPAT(ctx context.Context, userID, credentialID, scope string) error {
	if credentialID == "" || scope == "" {
		return fmt.Errorf("%w: no credential", errTestAdminIssuerPAT)
	}
	tok, err := s.store.GetUserAccessToken(ctx, credentialID)
	if err != nil || tok == nil {
		return fmt.Errorf("%w: token could not be loaded", errTestAdminIssuerPAT)
	}
	now := s.testIdentities.clock()
	switch {
	case tok.UserID != userID:
		return fmt.Errorf("%w: token owner mismatch", errTestAdminIssuerPAT)
	case tok.Revoked:
		return fmt.Errorf("%w: token revoked", errTestAdminIssuerPAT)
	case tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt):
		return fmt.Errorf("%w: token expired", errTestAdminIssuerPAT)
	case tok.BoundaryKind != string(BoundaryKindHub) || tok.ProjectID != "":
		return fmt.Errorf("%w: token boundary is not hub", errTestAdminIssuerPAT)
	case len(tok.Scopes) != 1 || tok.Scopes[0] != scope:
		return fmt.Errorf("%w: token must carry exactly %s", errTestAdminIssuerPAT, scope)
	case tok.Created.IsZero() || now.Sub(tok.Created) > testAdminIssuerPATMaxAge || tok.Created.After(now.Add(time.Minute)):
		return fmt.Errorf("%w: token older than %s", errTestAdminIssuerPAT, testAdminIssuerPATMaxAge)
	}
	return nil
}

// testAdminAuditSummary is the AfterSummary of a test_admin_issue record
// (DEC-3 C4: the issuer and its credential ID). It never includes a token.
func testAdminAuditSummary(u *store.User, issuerCredentialID string, tokenTTL time.Duration) string {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	var expiresAt string
	if u.ExpiresAt != nil {
		expiresAt = u.ExpiresAt.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(struct {
		UserID             string `json:"user_id"`
		Email              string `json:"email"`
		Kind               string `json:"kind"`
		Role               string `json:"role"`
		IssuedBy           string `json:"issued_by"`
		IssuerCredentialID string `json:"issuer_credential_id"`
		IssuanceID         string `json:"issuance_id"`
		Purpose            string `json:"purpose"`
		ExpiresAt          string `json:"expires_at"`
		TokenTTLSeconds    int64  `json:"token_ttl_seconds"`
	}{u.ID, u.Email, u.Kind, u.Role, str(u.IssuedBy), issuerCredentialID, str(u.IssuanceID), str(u.Purpose), expiresAt, int64(tokenTTL / time.Second)})
	return string(b)
}

// handleIssueTestAdminIdentity handles POST /api/v1/test-identities/admin.
func (s *Server) handleIssueTestAdminIdentity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.testAdminTierOn() {
		NotFound(w, "endpoint")
		return
	}
	var req CreateTestAdminIdentityRequest
	if err := decodeTestIdentityBody(r, &req); err != nil {
		BadRequest(w, err.Error())
		return
	}
	spec, ok := testAdminKindSpecs[req.AdminRole]
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			`invalid adminRole: must be "hub-admin" or "super-admin"`, map[string]interface{}{"field": "adminRole"})
		return
	}
	// A kind that is off answers 404, as the whole route does while the
	// tier is off: the test super-admin kind until its own gate exists.
	if !s.testAdminTierEnabled(spec.kind) {
		NotFound(w, "endpoint")
		return
	}
	issuer, issuerCredentialID, ok := s.testAdminIssuer(w, r, spec)
	if !ok {
		return
	}

	purpose := strings.TrimSpace(req.Purpose)
	if !validTestAdminPurpose(purpose) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			fmt.Sprintf("invalid purpose: %d to %d characters, no control characters", testAdminPurposeMinRunes, testIdentityPurposeMaxRunes),
			map[string]interface{}{"field": "purpose"})
		return
	}
	lifetime := spec.defaultLifetime
	if req.LifetimeSeconds != 0 {
		if req.LifetimeSeconds < 60 || req.LifetimeSeconds > int64(spec.maxLifetime/time.Second) {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("invalid lifetimeSeconds: must be between 60 and %d", int64(spec.maxLifetime/time.Second)),
				map[string]interface{}{"field": "lifetimeSeconds"})
			return
		}
		lifetime = time.Duration(req.LifetimeSeconds) * time.Second
	}
	now := s.testIdentities.clock()
	expiresAt := now.Add(lifetime)
	tokenTTL, ok := testAdminTokenTTL(spec, req.TokenTTLSeconds, expiresAt, now)
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			fmt.Sprintf("invalid tokenTtlSeconds: must be between 1 and %d", int64(spec.maxTokenTTL/time.Second)),
			map[string]interface{}{"field": "tokenTtlSeconds"})
		return
	}
	if s.userTokenService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "user token service is not configured", nil)
		return
	}
	limiter := s.testAdminIssuance.limiter(spec.kind)
	if limiter == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "test admin issuance is not configured", nil)
		return
	}

	email, err := newTestAdminEmail(spec.kind)
	if err != nil {
		InternalError(w)
		return
	}
	issuerID := issuer.ID()
	issuanceID := generateID()
	kindStr := string(spec.kind)
	u := &store.User{
		ID:          generateID(),
		Email:       email,
		DisplayName: "Test admin " + strings.TrimPrefix(strings.Split(email, "@")[0], testAdminEmailPrefix+"-")[:8],
		Role:        store.TestAdminRole(kindStr),
		Status:      store.UserStatusActive,
		Kind:        kindStr,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuerID,
		Purpose:     &purpose,
		IssuanceID:  &issuanceID,
		Created:     now,
	}
	if issuerCredentialID != "" {
		u.IssuerCredentialID = &issuerCredentialID
	}
	auditActor := s.buildAuditActorFromContext(ctx)

	var token string
	var expiresIn int64
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		// The member tier's issuance lock serializes both tiers' counts.
		if err := tx.LockTestFixtureIssuance(ctx); err != nil {
			return err
		}
		n, err := tx.CountLiveTestAdminUsers(ctx, "", issuerID, now)
		if err != nil {
			return err
		}
		if n >= testAdminPerIssuerCap {
			return errTestAdminIssuerCap
		}
		n, err = tx.CountLiveTestAdminUsers(ctx, "", "", now)
		if err != nil {
			return err
		}
		if n >= testAdminHubCap {
			return errTestAdminHubCap
		}
		if spec.kind == testAdminKindSuperAdmin {
			n, err = tx.CountLiveTestAdminUsers(ctx, kindStr, "", now)
			if err != nil {
				return err
			}
			if n >= testSuperAdminHubCap {
				return errTestSuperAdminHubCap
			}
		}
		// The per-issuer rate is charged only once the caps pass, so a
		// cap-refused attempt does not use up an hourly issuance.
		if !limiter.Allow(issuerID) {
			return errTestAdminRateLimited
		}
		rd, err := tx.GetRoleDefinitionByName(ctx, spec.systemRole, store.RoleScopeSystem)
		if err != nil {
			return fmt.Errorf("resolve %s role: %w", spec.systemRole, err)
		}
		if rd == nil {
			return fmt.Errorf("resolve %s role: not found", spec.systemRole)
		}
		if _, err := tx.CreateTestAdminUser(ctx, u, rd.ID); err != nil {
			return fmt.Errorf("create test admin identity: %w", err)
		}
		if err := s.testIdentities.hooks.syncGrants(ctx, tx, u.ID, u.Role, store.TestIdentityIssuanceCreatedBy); err != nil {
			return fmt.Errorf("sync hub role grants: %w", err)
		}
		token, expiresIn, err = s.userTokenService.GenerateAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeAPI, tokenTTL)
		if err != nil {
			return fmt.Errorf("issue token: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType: testAdminIssueMutation,
			TargetType:   testIdentityAuditTargetType,
			TargetID:     u.ID,
			AfterSummary: testAdminAuditSummary(u, issuerCredentialID, tokenTTL),
			Timestamp:    now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit test admin issue: %w", err)
		}
		// The sink line is written last, inside the transaction: if it
		// fails, nothing is issued.
		if err := s.testAdminAudit.emit(testAdminAuditEventIssue, map[string]any{
			"test_identity_id":     u.ID,
			"kind":                 kindStr,
			"issuer_id":            issuerID,
			"issuer_credential_id": issuerCredentialID,
			"issuance_id":          issuanceID,
			"purpose_hash":         testAdminPurposeHash(purpose),
			"expires_at":           expiresAt.UTC().Format(time.RFC3339),
			"token_ttl_seconds":    int64(tokenTTL / time.Second),
			"request_id":           logging.RequestIDFromContext(ctx),
		}); err != nil {
			return fmt.Errorf("%w: %v", errTestAdminSinkWrite, err)
		}
		return nil
	})
	if err != nil {
		token = ""
		switch {
		case errors.Is(err, errTestAdminIssuerCap):
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(),
				map[string]interface{}{"reason": testAdminReasonIssuerCap, "limit": testAdminPerIssuerCap})
		case errors.Is(err, errTestAdminHubCap):
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(),
				map[string]interface{}{"reason": testAdminReasonHubCap, "limit": testAdminHubCap})
		case errors.Is(err, errTestSuperAdminHubCap):
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(),
				map[string]interface{}{"reason": testAdminReasonSuperAdminCap, "limit": testSuperAdminHubCap})
		case errors.Is(err, errTestAdminRateLimited):
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many test admin identity requests; retry later",
				map[string]interface{}{"reason": testIdentityReasonRateLimited})
		case errors.Is(err, errTestAdminSinkWrite):
			slog.ErrorContext(ctx, "test admin issuance refused: audit sink write failed", "issuer_id", issuerID)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "test admin audit is unavailable", nil)
		case isTransientIssuanceConflict(err):
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "test identity issuance is busy; retry shortly",
				map[string]interface{}{"reason": testIdentityReasonBusy})
		default:
			slog.ErrorContext(ctx, "test admin issuance failed", "issuer_id", issuerID, "error", err)
			InternalError(w)
		}
		return
	}

	slog.InfoContext(ctx, "test admin identity issued", "test_identity_id", u.ID, "issuer_id", issuerID,
		"kind", kindStr, "issuance_id", issuanceID, "expires_at", expiresAt)
	writeJSON(w, http.StatusCreated, TestAdminIdentityTokenResponse{
		Identity:       testAdminIdentityViewOf(u, req.AdminRole, now),
		AccessToken:    token,
		TokenType:      "Bearer",
		ExpiresIn:      expiresIn,
		TokenExpiresAt: now.Add(tokenTTL),
	})
}

// errTestAdminSinkWrite marks an issuance or revocation refused because the
// test admin audit sink could not be written.
var errTestAdminSinkWrite = errors.New("test admin audit sink write failed")

// handleRevokeAllTestAdminIdentities handles POST
// /api/v1/test-identities/admin/revoke-all: the emergency stop of admin
// tier design §3.3, for a human super-admin on an interactive session (or
// dev credential) only. It deletes every admin-kind identity, live or
// expired, with its role bindings, group memberships and user-scope data,
// and writes a test_admin_revoke audit record and sink line for each. An
// identity the user delete refuses (it owns agents, or is a project's last
// owner) is instead suspended and stripped of its role bindings in one
// transaction, so its tokens stop working at once; the response reports
// which outcome each identity got.
func (s *Server) handleRevokeAllTestAdminIdentities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.testAdminTierOn() {
		NotFound(w, "endpoint")
		return
	}
	actor, ok := s.requireSessionCredentialFor(w, ctx, authzop.ReasonCredentialManagement)
	if !ok {
		return
	}
	k, err := s.kindForPrincipal(ctx, actor.ID())
	if err != nil || !k.isHuman() {
		logAuthzDenial(r, actor, Resource{Type: permissions.ResourceTestIdentity}, Action("revoke"), "caller is not a human user")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "revoke-all requires a human super-admin session",
			map[string]interface{}{"reason": testAdminReasonCallerNotHuman})
		return
	}
	if !s.authzService.IsSystemAdmin(ctx, actor.ID()) {
		logAuthzDenial(r, actor, Resource{Type: permissions.ResourceTestIdentity}, Action("revoke"), "caller is not a super-admin")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "revoke-all requires a human super-admin session", nil)
		return
	}

	users, err := s.store.ListTestAdminUsers(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	auditActor := s.buildAuditActorFromContext(ctx)
	now := s.testIdentities.clock()
	resp := RevokeAllTestAdminIdentitiesResponse{Revoked: []RevokedTestAdminIdentity{}}
	for i := range users {
		u := &users[i]
		outcome, err := s.revokeTestAdminIdentity(ctx, u, auditActor, now)
		if err != nil {
			slog.ErrorContext(ctx, "test admin revoke-all failed", "test_identity_id", u.ID, "error", err)
			if errors.Is(err, errTestAdminSinkWrite) {
				writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "test admin audit is unavailable", nil)
				return
			}
			InternalError(w)
			return
		}
		if outcome == "deleted" {
			s.afterUserDeleted(ctx, u.ID)
		}
		resp.Revoked = append(resp.Revoked, RevokedTestAdminIdentity{ID: u.ID, Kind: u.Kind, Outcome: outcome})
	}
	resp.Count = len(resp.Revoked)
	slog.InfoContext(ctx, "test admin identities revoked", "count", resp.Count, "revoked_by", actor.ID())
	writeJSON(w, http.StatusOK, resp)
}

// revokeTestAdminIdentity deletes one admin-kind identity, or, when the
// user delete refuses it, suspends it and removes its role bindings. It
// returns "deleted" or "suspended". The audit record and sink line are
// written in the same transaction.
func (s *Server) revokeTestAdminIdentity(ctx context.Context, u *store.User, auditActor AuditActor, now time.Time) (string, error) {
	write := func(tx store.Store, outcome string) error {
		record := &store.MutationAuditRecord{
			MutationType:  testAdminRevokeMutation,
			TargetType:    testIdentityAuditTargetType,
			TargetID:      u.ID,
			BeforeSummary: testIdentityDeleteSummary(u),
			AfterSummary:  fmt.Sprintf(`{"outcome":%q}`, outcome),
			Timestamp:     now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit test admin revoke: %w", err)
		}
		issuanceID := ""
		if u.IssuanceID != nil {
			issuanceID = *u.IssuanceID
		}
		if err := s.testAdminAudit.emit(testAdminAuditEventRevoke, map[string]any{
			"test_identity_id": u.ID,
			"kind":             u.Kind,
			"issuance_id":      issuanceID,
			"outcome":          outcome,
			"revoked_by":       auditActor.PrincipalID,
			"request_id":       logging.RequestIDFromContext(ctx),
		}); err != nil {
			return fmt.Errorf("%w: %v", errTestAdminSinkWrite, err)
		}
		return nil
	}
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := deleteUserRowCascadeTx(ctx, tx, u.ID, s.membershipNow()); err != nil {
			return err
		}
		return write(tx, "deleted")
	})
	if err == nil {
		return "deleted", nil
	}
	if errors.Is(err, store.ErrNotFound) {
		// Deleted concurrently: nothing left to revoke.
		return "deleted", nil
	}
	var lastOwnerErr *lastProjectOwnerDeleteError
	var ownsAgentsErr *userOwnsAgentsDeleteError
	if !errors.As(err, &lastOwnerErr) && !errors.As(err, &ownsAgentsErr) && !errors.Is(err, errUserRoleBindingsChanged) {
		return "", err
	}
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		row, err := tx.GetUser(ctx, u.ID)
		if err != nil {
			return err
		}
		if _, err := tx.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID); err != nil {
			return fmt.Errorf("delete role bindings: %w", err)
		}
		row.Status = store.UserStatusSuspended
		if err := tx.UpdateUser(ctx, row); err != nil {
			return fmt.Errorf("suspend: %w", err)
		}
		return write(tx, "suspended")
	})
	if err != nil {
		return "", err
	}
	s.publishConduitAuthzChanged(conduitAuthzMatch{UserID: u.ID})
	return "suspended", nil
}
