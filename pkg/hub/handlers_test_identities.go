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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Hub-issued test identities (ptone/scion#4240).
//
// POST /api/v1/test-identities creates a fresh, short-lived synthetic member
// or viewer user (kind=test_fixture, email in store.TestFixtureEmailDomain)
// and returns one access token for it. POST /api/v1/test-identities/{id}/token
// re-issues a token for a live identity, and GET /api/v1/test-identities lists
// identities. The feature is off unless the hub starts with
// --enable-test-identities; while it is off the routes return 404 and the
// auth middleware refuses every test-fixture row (testFixtureRejection).
//
// The caller needs test_identity.issue at hub scope (an admin session, a
// role binding, or a hub-boundary user access token carrying the
// test_identity:issue scope). The role is member or viewer only. No refresh
// token and no cookie is ever issued. Every issuance and re-issue writes a
// durable mutation audit in the same transaction: an audit failure rolls the
// action back.

// Test identity defaults (design §6).
const (
	// testIdentityDefaultTokenTTL is the access token lifetime when the
	// request names none.
	testIdentityDefaultTokenTTL = 30 * time.Minute
	// testIdentityMaxTokenTTL caps a requested token lifetime. A token also
	// never outlives its identity.
	testIdentityMaxTokenTTL = 8 * time.Hour
	// testIdentityDefaultLifetime is the identity lifetime when the request
	// names none.
	testIdentityDefaultLifetime = time.Hour
	// testIdentityMaxLifetime caps an identity's lifetime.
	testIdentityMaxLifetime = 8 * time.Hour
	// testIdentityPerIssuerCap is the number of live identities one issuer
	// may hold at once.
	testIdentityPerIssuerCap = 5
	// testIdentityHubCap is the number of live identities the hub holds at
	// once.
	testIdentityHubCap = 20
	// testIdentityRatePerSecond and testIdentityRateBurst bound issuance
	// and re-issue requests per issuer.
	testIdentityRatePerSecond = 0.2
	testIdentityRateBurst     = 10

	testIdentityPurposeMaxRunes = 200
	testIdentityMaxBodyBytes    = 16 << 10
	testIdentityEmailPrefix     = "test-identity"
)

// permissionTestIdentityIssue is the permission every test identity route
// requires at hub scope.
const permissionTestIdentityIssue = "test_identity.issue"

// Mutation audit types for test identities.
const (
	testIdentityIssueMutation      = "test_identity_issue"
	testIdentityTokenIssueMutation = "test_identity_token_issue"
	testIdentityAuditTargetType    = "user"
	testIdentityGrantsCreatedBy    = "test-identity-issuance"
)

// details.reason values.
const (
	testIdentityReasonIssuerCap   = "issuer_cap_reached"
	testIdentityReasonHubCap      = "hub_cap_reached"
	testIdentityReasonRateLimited = "rate_limited"
	testIdentityReasonExpired     = "test_identity_expired"
	testIdentityReasonFixtureCall = "test_identity_cannot_issue"
)

var (
	errTestIdentityIssuerCap = errors.New("live test identity limit for this issuer reached")
	errTestIdentityHubCap    = errors.New("live test identity limit for this hub reached")
)

// testIdentityRoles is the complete request role enum. There is no admin
// value and no code path that sets one.
var testIdentityRoles = map[string]bool{
	store.UserRoleMember: true,
	store.UserRoleViewer: true,
}

// testIdentityHooks are seams for tests: the hub grant sync and the audit
// write that run inside the issuance transaction.
type testIdentityHooks struct {
	syncGrants func(ctx context.Context, st store.Store, userID, role, createdBy string) error
	writeAudit func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error
}

// testIdentityState is the server-side state of the feature.
type testIdentityState struct {
	enabled bool
	limiter *GCPTokenRateLimiter
	hooks   testIdentityHooks
	now     func() time.Time
}

// clock returns the current time from the state's clock, or time.Now for a
// zero state (a Server not built by New).
func (t *testIdentityState) clock() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

func newTestIdentityState(enabled bool) testIdentityState {
	return testIdentityState{
		enabled: enabled,
		limiter: NewGCPTokenRateLimiter(testIdentityRatePerSecond, testIdentityRateBurst),
		hooks: testIdentityHooks{
			syncGrants: syncHubRoleGrants,
			writeAudit: func(ctx context.Context, tx store.Store, record *store.MutationAuditRecord) error {
				return tx.CreateMutationAudit(ctx, record)
			},
		},
		now: time.Now,
	}
}

// isReservedTestIdentityEmail reports whether email is in the reserved
// test-fixture domain. Every path that resolves or creates a user by email
// refuses such an email, except the issuance endpoint (which creates it)
// and the hub-issued JWT path (where the fixture's own token is checked by
// testFixtureRejection instead). It is deliberately separate from
// isReservedPlatformIdentity, which also guards the JWT path.
func isReservedTestIdentityEmail(email string) bool {
	return store.IsTestFixtureEmail(email)
}

// testFixtureRejection returns a non-empty reason when u must not
// authenticate: a test-fixture row while the feature is off, with no
// expiry, or past its expiry; or a non-fixture row in the reserved domain.
// It runs inside the per-request user-row block of UnifiedAuthMiddleware's
// JWT arm, and in handleAuthValidate, which mirrors that block.
func testFixtureRejection(u *store.User, enabled bool, now time.Time) string {
	if u == nil {
		return ""
	}
	if !u.IsTestFixture() {
		if isReservedTestIdentityEmail(u.Email) {
			return "reserved_test_identity_domain"
		}
		return ""
	}
	if !enabled {
		return "test_identities_disabled"
	}
	if u.ExpiresAt == nil || !now.Before(*u.ExpiresAt) {
		return "test_identity_expired"
	}
	return ""
}

// TestIdentityView is the response view of a test identity. It never
// carries a token.
type TestIdentityView struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	Purpose     string    `json:"purpose,omitempty"`
	IssuedBy    string    `json:"issuedBy"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Created     time.Time `json:"created"`
	Live        bool      `json:"live"`
}

func testIdentityViewOf(u *store.User, now time.Time) TestIdentityView {
	v := TestIdentityView{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Status:      u.Status,
		Created:     u.Created,
	}
	if u.Purpose != nil {
		v.Purpose = *u.Purpose
	}
	if u.IssuedBy != nil {
		v.IssuedBy = *u.IssuedBy
	}
	if u.ExpiresAt != nil {
		v.ExpiresAt = *u.ExpiresAt
		v.Live = now.Before(*u.ExpiresAt)
	}
	return v
}

// CreateTestIdentityRequest is the request body of POST
// /api/v1/test-identities. Unknown fields are rejected, so no other user
// field (kind, email, expiry, issuer) can be bound from a request.
type CreateTestIdentityRequest struct {
	// Role is "member" or "viewer". Empty means member.
	Role string `json:"role,omitempty"`
	// Purpose is a short free-text label recorded on the identity and in
	// the audit.
	Purpose string `json:"purpose,omitempty"`
	// LifetimeSeconds is the identity lifetime. 0 means the default (1 h);
	// at most 8 h.
	LifetimeSeconds int64 `json:"lifetimeSeconds,omitempty"`
	// TokenTTLSeconds is the access token lifetime. 0 means the default
	// (30 min). The token never outlives the identity.
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// IssueTestIdentityTokenRequest is the request body of POST
// /api/v1/test-identities/{id}/token. The body is optional.
type IssueTestIdentityTokenRequest struct {
	TokenTTLSeconds int64 `json:"tokenTtlSeconds,omitempty"`
}

// TestIdentityTokenResponse is the response of issuance and re-issue. It
// holds exactly one token: there is no refresh token.
type TestIdentityTokenResponse struct {
	Identity       TestIdentityView `json:"identity"`
	AccessToken    string           `json:"accessToken"`
	TokenType      string           `json:"tokenType"`
	ExpiresIn      int64            `json:"expiresIn"`
	TokenExpiresAt time.Time        `json:"tokenExpiresAt"`
}

// ListTestIdentitiesResponse is the response of GET /api/v1/test-identities.
type ListTestIdentitiesResponse struct {
	Items []TestIdentityView `json:"items"`
}

// decodeTestIdentityBody strictly decodes an optional JSON body into v.
func decodeTestIdentityBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, testIdentityMaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("failed to read request body")
	}
	if len(body) > testIdentityMaxBodyBytes {
		return fmt.Errorf("request body too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %v", err)
	}
	if dec.More() {
		return fmt.Errorf("invalid request body: trailing data")
	}
	return nil
}

// testIdentityTokenTTL resolves a requested token lifetime against the
// default, the cap, and the identity's remaining lifetime. It returns the
// lifetime, whole seconds, and false when the request is invalid.
func testIdentityTokenTTL(requestedSeconds int64, expiresAt, now time.Time) (time.Duration, bool) {
	if requestedSeconds < 0 {
		return 0, false
	}
	ttl := testIdentityDefaultTokenTTL
	if requestedSeconds > 0 {
		if requestedSeconds > int64(testIdentityMaxTokenTTL/time.Second) {
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

// testIdentityCaller runs the checks every test identity route shares after
// the route guard: the feature flag, the credential kind, the handler-level
// permission check, and the rule that a test fixture cannot itself issue.
// It writes the response and returns false on refusal.
func (s *Server) testIdentityCaller(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
	ctx := r.Context()
	if !s.testIdentities.enabled {
		NotFound(w, "endpoint")
		return nil, false
	}
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok || isNilIdentity(user) {
		writeForbiddenStructured(w, "requires test_identity.issue permission", permissions.ResourceTestIdentity, ActionIssue)
		return nil, false
	}
	switch GetCredentialContextFromContext(ctx).Kind {
	case CredentialKindInteractive, CredentialKindDev, CredentialKindUAT:
	default:
		// Fail closed: broker on-behalf-of, agent, federation and any
		// unknown credential cannot issue test identities.
		logAuthzDenial(r, user, Resource{Type: permissions.ResourceTestIdentity}, ActionIssue, "credential kind not admitted for test identity issuance")
		writeForbiddenStructured(w, "requires test_identity.issue permission", permissions.ResourceTestIdentity, ActionIssue)
		return nil, false
	}
	// Defence in depth: the route guard already ran this decision.
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(user),
		Credential:     credentialContextForIdentity(user),
		Resource:       Resource{Type: permissions.ResourceTestIdentity},
		Action:         ActionIssue,
		Permission:     permissionTestIdentityIssue,
		TargetEvidence: hubCollectionEvidence(permissionTestIdentityIssue),
	})
	if !decision.Allowed {
		logAuthzDenial(r, user, Resource{Type: permissions.ResourceTestIdentity}, ActionIssue, decision.Reason)
		writeForbiddenStructured(w, "requires test_identity.issue permission", permissions.ResourceTestIdentity, ActionIssue)
		return nil, false
	}
	// A test fixture never issues test identities, whatever grant it may
	// have been given.
	if caller, err := s.store.GetUser(ctx, user.ID()); err == nil && caller.IsTestFixture() {
		logAuthzDenial(r, user, Resource{Type: permissions.ResourceTestIdentity}, ActionIssue, "test fixture cannot issue test identities")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "a test identity cannot issue test identities",
			map[string]interface{}{"reason": testIdentityReasonFixtureCall})
		return nil, false
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		InternalError(w)
		return nil, false
	}
	return user, true
}

// testIdentitiesGate answers 404 while the feature is off. It wraps the
// route guard, so the answer does not depend on the caller's permissions.
// The handlers check the flag again (testIdentityCaller).
func (s *Server) testIdentitiesGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.testIdentities.enabled {
			NotFound(w, "endpoint")
			return
		}
		next(w, r)
	}
}

// testIdentityAllow applies the per-issuer rate limit.
func (s *Server) testIdentityAllow(w http.ResponseWriter, issuerID string) bool {
	if s.testIdentities.limiter.Allow(issuerID) {
		return true
	}
	writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many test identity requests; retry later",
		map[string]interface{}{"reason": testIdentityReasonRateLimited})
	return false
}

// testIdentityAuditSummary is the AfterSummary of a test identity audit
// record. It never includes a token.
func testIdentityAuditSummary(u *store.User, tokenTTL time.Duration) string {
	purpose := ""
	if u.Purpose != nil {
		purpose = *u.Purpose
	}
	issuedBy := ""
	if u.IssuedBy != nil {
		issuedBy = *u.IssuedBy
	}
	var expiresAt string
	if u.ExpiresAt != nil {
		expiresAt = u.ExpiresAt.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(struct {
		UserID          string `json:"user_id"`
		Email           string `json:"email"`
		Role            string `json:"role"`
		IssuedBy        string `json:"issued_by"`
		Purpose         string `json:"purpose"`
		ExpiresAt       string `json:"expires_at"`
		TokenTTLSeconds int64  `json:"token_ttl_seconds"`
	}{u.ID, u.Email, u.Role, issuedBy, purpose, expiresAt, int64(tokenTTL / time.Second)})
	return string(b)
}

// newTestIdentityEmail returns a fresh email in the reserved domain with a
// 128-bit random local part.
func newTestIdentityEmail() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return testIdentityEmailPrefix + "-" + hex.EncodeToString(b) + "@" + store.TestFixtureEmailDomain, nil
}

// validTestIdentityPurpose reports whether purpose is acceptable.
func validTestIdentityPurpose(purpose string) bool {
	if utf8.RuneCountInString(purpose) > testIdentityPurposeMaxRunes {
		return false
	}
	return !hasControlChar(purpose, false)
}

// handleCreateTestIdentity handles POST /api/v1/test-identities.
func (s *Server) handleCreateTestIdentity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	issuer, ok := s.testIdentityCaller(w, r)
	if !ok {
		return
	}

	var req CreateTestIdentityRequest
	if err := decodeTestIdentityBody(r, &req); err != nil {
		BadRequest(w, err.Error())
		return
	}
	role := req.Role
	if role == "" {
		role = store.UserRoleMember
	}
	if !testIdentityRoles[role] {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			`invalid role: must be "member" or "viewer"`, map[string]interface{}{"field": "role"})
		return
	}
	purpose := strings.TrimSpace(req.Purpose)
	if !validTestIdentityPurpose(purpose) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			fmt.Sprintf("invalid purpose: at most %d characters, no control characters", testIdentityPurposeMaxRunes),
			map[string]interface{}{"field": "purpose"})
		return
	}
	lifetime := testIdentityDefaultLifetime
	if req.LifetimeSeconds != 0 {
		if req.LifetimeSeconds < 60 || req.LifetimeSeconds > int64(testIdentityMaxLifetime/time.Second) {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("invalid lifetimeSeconds: must be between 60 and %d", int64(testIdentityMaxLifetime/time.Second)),
				map[string]interface{}{"field": "lifetimeSeconds"})
			return
		}
		lifetime = time.Duration(req.LifetimeSeconds) * time.Second
	}
	now := s.testIdentities.clock()
	expiresAt := now.Add(lifetime)
	tokenTTL, ok := testIdentityTokenTTL(req.TokenTTLSeconds, expiresAt, now)
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			fmt.Sprintf("invalid tokenTtlSeconds: must be between 1 and %d", int64(testIdentityMaxTokenTTL/time.Second)),
			map[string]interface{}{"field": "tokenTtlSeconds"})
		return
	}
	if s.userTokenService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "user token service is not configured", nil)
		return
	}
	if !s.testIdentityAllow(w, issuer.ID()) {
		return
	}

	email, err := newTestIdentityEmail()
	if err != nil {
		InternalError(w)
		return
	}
	issuerID := issuer.ID()
	u := &store.User{
		ID:          generateID(),
		Email:       email,
		DisplayName: "Test identity " + strings.TrimPrefix(strings.Split(email, "@")[0], testIdentityEmailPrefix+"-")[:8],
		Role:        role,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestFixture,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuerID,
		Created:     now,
	}
	if purpose != "" {
		u.Purpose = &purpose
	}
	auditActor := s.buildAuditActorFromContext(ctx)

	var token string
	var expiresIn int64
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.LockTestFixtureIssuance(ctx); err != nil {
			return err
		}
		n, err := tx.CountLiveTestFixtureUsers(ctx, issuerID, now)
		if err != nil {
			return err
		}
		if n >= testIdentityPerIssuerCap {
			return errTestIdentityIssuerCap
		}
		n, err = tx.CountLiveTestFixtureUsers(ctx, "", now)
		if err != nil {
			return err
		}
		if n >= testIdentityHubCap {
			return errTestIdentityHubCap
		}
		if err := tx.CreateTestFixtureUser(ctx, u); err != nil {
			return fmt.Errorf("create test identity: %w", err)
		}
		// Unlike sign-in, a grant sync failure fails the issuance.
		if err := s.testIdentities.hooks.syncGrants(ctx, tx, u.ID, u.Role, testIdentityGrantsCreatedBy); err != nil {
			return fmt.Errorf("sync hub role grants: %w", err)
		}
		token, expiresIn, err = s.userTokenService.GenerateAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeAPI, tokenTTL)
		if err != nil {
			return fmt.Errorf("issue token: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType: testIdentityIssueMutation,
			TargetType:   testIdentityAuditTargetType,
			TargetID:     u.ID,
			AfterSummary: testIdentityAuditSummary(u, tokenTTL),
			Timestamp:    now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit test identity issue: %w", err)
		}
		return nil
	})
	if err != nil {
		token = ""
		switch {
		case errors.Is(err, errTestIdentityIssuerCap):
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(),
				map[string]interface{}{"reason": testIdentityReasonIssuerCap, "limit": testIdentityPerIssuerCap})
		case errors.Is(err, errTestIdentityHubCap):
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(),
				map[string]interface{}{"reason": testIdentityReasonHubCap, "limit": testIdentityHubCap})
		default:
			slog.ErrorContext(ctx, "test identity issuance failed", "issuer_id", issuerID, "error", err)
			InternalError(w)
		}
		return
	}

	slog.InfoContext(ctx, "test identity issued", "test_identity_id", u.ID, "issuer_id", issuerID, "role", u.Role, "expires_at", expiresAt)
	writeJSON(w, http.StatusCreated, TestIdentityTokenResponse{
		Identity:       testIdentityViewOf(u, now),
		AccessToken:    token,
		TokenType:      "Bearer",
		ExpiresIn:      expiresIn,
		TokenExpiresAt: now.Add(tokenTTL),
	})
}

// canManageTestIdentity reports whether caller may see or re-issue for u:
// its issuer, or an unscoped platform admin session.
func canManageTestIdentity(caller UserIdentity, u *store.User) bool {
	if IsUnscopedLocalPlatformAdmin(caller) {
		return true
	}
	return u.IssuedBy != nil && *u.IssuedBy == caller.ID()
}

// handleIssueTestIdentityToken handles POST /api/v1/test-identities/{id}/token.
func (s *Server) handleIssueTestIdentityToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityCaller(w, r)
	if !ok {
		return
	}
	var req IssueTestIdentityTokenRequest
	if err := decodeTestIdentityBody(r, &req); err != nil {
		BadRequest(w, err.Error())
		return
	}

	id := r.PathValue("id")
	u, err := s.store.GetUser(ctx, id)
	// One answer for a missing user, a non-fixture user, and a fixture the
	// caller did not issue, so the route is no user-existence oracle.
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) || (err == nil && (!u.IsTestFixture() || !canManageTestIdentity(caller, u))) {
		NotFound(w, "test identity")
		return
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	now := s.testIdentities.clock()
	if u.ExpiresAt == nil || !now.Before(*u.ExpiresAt) {
		writeError(w, http.StatusConflict, ErrCodeConflict, "test identity has expired",
			map[string]interface{}{"reason": testIdentityReasonExpired})
		return
	}
	if u.Status != store.UserStatusActive {
		writeError(w, http.StatusConflict, ErrCodeConflict, "test identity is not active", nil)
		return
	}
	tokenTTL, ok := testIdentityTokenTTL(req.TokenTTLSeconds, *u.ExpiresAt, now)
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			fmt.Sprintf("invalid tokenTtlSeconds: must be between 1 and %d", int64(testIdentityMaxTokenTTL/time.Second)),
			map[string]interface{}{"field": "tokenTtlSeconds"})
		return
	}
	if s.userTokenService == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "user token service is not configured", nil)
		return
	}
	if !s.testIdentityAllow(w, caller.ID()) {
		return
	}

	auditActor := s.buildAuditActorFromContext(ctx)
	var token string
	var expiresIn int64
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		var err error
		token, expiresIn, err = s.userTokenService.GenerateAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeAPI, tokenTTL)
		if err != nil {
			return fmt.Errorf("issue token: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType: testIdentityTokenIssueMutation,
			TargetType:   testIdentityAuditTargetType,
			TargetID:     u.ID,
			AfterSummary: testIdentityAuditSummary(u, tokenTTL),
			Timestamp:    now,
		}
		auditActor.ApplyActor(record)
		return s.testIdentities.hooks.writeAudit(ctx, tx, record)
	})
	if err != nil {
		token = ""
		slog.ErrorContext(ctx, "test identity token issue failed", "test_identity_id", u.ID, "error", err)
		InternalError(w)
		return
	}
	slog.InfoContext(ctx, "test identity token issued", "test_identity_id", u.ID, "issuer_id", caller.ID())
	writeJSON(w, http.StatusOK, TestIdentityTokenResponse{
		Identity:       testIdentityViewOf(u, now),
		AccessToken:    token,
		TokenType:      "Bearer",
		ExpiresIn:      expiresIn,
		TokenExpiresAt: now.Add(tokenTTL),
	})
}

// handleListTestIdentities handles GET /api/v1/test-identities: the
// caller's own identities, or every identity for an unscoped platform
// admin session.
func (s *Server) handleListTestIdentities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityCaller(w, r)
	if !ok {
		return
	}
	issuedBy := caller.ID()
	if IsUnscopedLocalPlatformAdmin(caller) {
		issuedBy = ""
	}
	users, err := s.store.ListTestFixtureUsers(ctx, issuedBy)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	now := s.testIdentities.clock()
	items := make([]TestIdentityView, 0, len(users))
	for i := range users {
		items = append(items, testIdentityViewOf(&users[i], now))
	}
	writeJSON(w, http.StatusOK, ListTestIdentitiesResponse{Items: items})
}
