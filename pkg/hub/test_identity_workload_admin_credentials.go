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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Admin-kind credentials through an admin-scope workload delegation
// (ptone/scion#4240, phase X4b).
//
// An admin-scope delegation (X4: test_identity:issue_admin or
// test_identity:issue_super_admin) issues admin-kind identities on
// POST /api/v1/test-identities/admin. X4b admits it on two more routes,
// and only for the admin-kind identities of its scope's kind that the same
// delegation issued (testAdminDelegationManages):
//
//   - POST /api/v1/test-identities/{id}/token re-issues one access token
//     (handleIssueTestAdminTokenThroughDelegation), at most the kind's
//     token lifetime and never past the identity's expiry nor its creation
//     plus the kind's maximum identity lifetime;
//   - POST /api/v1/test-identities/{id}/web-login-code mints a web-login
//     code, only while the hub's admin web session setting is on for the
//     kind (WS4, testIdentityWebKindAdmitted), with the same lifetime
//     bound.
//
// Both take the kind's issuer checks (testAdminCredentialIssuer: the
// capability, a live super-admin grantor through current bindings), and
// inside the mint transaction re-check the delegation's liveness
// (checkDelegationLive through recheckWorkloadDelegation: the
// workload-delegation analogue of DEC-3 C2, C3),
// the grantor (C1, recheckTestAdminDelegationGrantor) and the identity
// row. Each writes a mutation audit record with the delegation part and a
// test admin audit sink line carrying the delegation id. Everything else
// stays refused for an admin-scope delegation: list, delete, purge and
// member issuance (testIdentityCaller), and any identity another
// delegation or a human issued (404).

// Test admin audit sink events written by this file and the web-login
// code handler.
const (
	testAdminAuditEventTokenIssue   = "test_admin_token_issue"
	testAdminAuditEventWebCodeIssue = "test_admin_web_code_issue"
)

// errTestAdminIdentityNotManaged refuses a mint whose identity, re-read
// inside the transaction, is gone, suspended, past its deadline or no
// longer stamped with the request's delegation. The handler answers 404,
// or 409 for an identity it can show is past its deadline.
var errTestAdminIdentityNotManaged = errors.New("admin-kind test identity is not managed by this delegation")

// testAdminDelegationManages reports whether the admin-scope workload
// delegation of st manages the admin-kind identity u: u is of the
// delegation's scope's kind, stamped with the delegation's id, and issued
// for the delegation's grantor. Any other identity, a member-tier one
// included, is not managed.
func testAdminDelegationManages(st *workloadDelegationState, u *store.User) bool {
	if st == nil || st.delegation == nil || u == nil {
		return false
	}
	spec, ok := testIdentityDelegationScopes[st.delegation.Scope]
	if !ok || !spec.isAdmin() {
		return false
	}
	return u.IsTestAdmin() && !u.IsTestFixture() && u.Kind == string(spec.adminKind) &&
		u.IssuerDelegationID != nil && *u.IssuerDelegationID == st.delegation.ID &&
		u.IssuedBy != nil && *u.IssuedBy == st.delegation.GrantorID
}

// testIdentityCredentialDeadline returns the latest time a credential (a
// token or web-login code) for u may be valid: the identity's expiry, and
// for an admin kind never later than its creation plus the kind's maximum
// identity lifetime. live is false when u has no expiry, or is an admin
// kind without a creation time or of an unknown kind: such a row gets no
// credential.
func testIdentityCredentialDeadline(u *store.User) (deadline time.Time, live bool) {
	if u == nil || u.ExpiresAt == nil {
		return time.Time{}, false
	}
	deadline = *u.ExpiresAt
	if !u.IsTestAdmin() {
		return deadline, true
	}
	max := testAdminKindMaxLifetime(u.Kind)
	if max <= 0 || u.Created.IsZero() {
		return time.Time{}, false
	}
	if c := u.Created.Add(max); c.Before(deadline) {
		deadline = c
	}
	return deadline, true
}

// recheckTestAdminIdentityForDelegation re-reads the identity id inside
// the mint transaction and returns errTestAdminIdentityNotManaged unless
// the delegation of st still manages it, it is active, and now is before
// its credential deadline. A revoke-all or delete that ran after the
// request read the row mints nothing.
func recheckTestAdminIdentityForDelegation(ctx context.Context, tx store.Store, st *workloadDelegationState, id string, now time.Time) error {
	row, err := tx.GetUser(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return errTestAdminIdentityNotManaged
	}
	if err != nil {
		return err
	}
	if !testAdminDelegationManages(st, row) || row.Status != store.UserStatusActive {
		return errTestAdminIdentityNotManaged
	}
	if deadline, live := testIdentityCredentialDeadline(row); !live || !now.Before(deadline) {
		return errTestAdminIdentityNotManaged
	}
	return nil
}

// testAdminRoleForKind returns the request adminRole of an admin kind
// ("hub-admin" or "super-admin"), or "" for any other kind.
func testAdminRoleForKind(kind string) string {
	for role, spec := range testAdminKindSpecs {
		if string(spec.kind) == kind {
			return role
		}
	}
	return ""
}

// testAdminTokenAuditSummary is the AfterSummary of a
// test_identity_token_issue record for an admin-kind identity re-issued
// through a workload delegation: the identity, its issuer and issuance, the
// delegation (issuer credential) and the token lifetime. It never includes
// a token.
func testAdminTokenAuditSummary(u *store.User, tokenTTL time.Duration, wl *testIdentityWorkloadAudit) string {
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
	credentialID := ""
	if wl != nil {
		credentialID = wl.DelegationID
	}
	b, _ := json.Marshal(struct {
		UserID             string                     `json:"user_id"`
		Email              string                     `json:"email"`
		Kind               string                     `json:"kind"`
		Role               string                     `json:"role"`
		IssuedBy           string                     `json:"issued_by"`
		IssuerCredentialID string                     `json:"issuer_credential_id"`
		IssuanceID         string                     `json:"issuance_id"`
		ExpiresAt          string                     `json:"expires_at"`
		TokenTTLSeconds    int64                      `json:"token_ttl_seconds"`
		Workload           *testIdentityWorkloadAudit `json:"workload_delegation,omitempty"`
	}{u.ID, u.Email, u.Kind, u.Role, str(u.IssuedBy), credentialID, str(u.IssuanceID), expiresAt, int64(tokenTTL / time.Second), wl})
	return string(b)
}

// handleIssueTestAdminTokenThroughDelegation re-issues one access token
// for an admin-kind identity through the admin-scope workload delegation
// that issued it (phase X4b). handleIssueTestIdentityToken calls it for
// every admin-scope delegation request, after testIdentityCallerAdmitting
// admitted it. An identity the delegation does not manage (another
// delegation's, a human's, a member-tier one, a missing one) answers the
// same 404 as the member path.
func (s *Server) handleIssueTestAdminTokenThroughDelegation(w http.ResponseWriter, r *http.Request, caller UserIdentity, st *workloadDelegationState, allowed map[string]bool) {
	ctx := r.Context()
	var req IssueTestIdentityTokenRequest
	if err := decodeTestIdentityBody(r, &req); err != nil {
		BadRequest(w, err.Error())
		return
	}
	id := r.PathValue("id")
	u, err := s.store.GetUser(ctx, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) || (err == nil && !testAdminDelegationManages(st, u)) {
		NotFound(w, "test identity")
		return
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	spec, ok := testAdminKindSpecFor(u.Kind)
	if !ok || !s.testAdminTierEnabled(spec.kind) {
		NotFound(w, "test identity")
		return
	}
	if !s.testAdminCredentialIssuer(w, r, caller, u, allowed) {
		return
	}
	now := s.testIdentities.clock()
	deadline, live := testIdentityCredentialDeadline(u)
	if !live || !now.Before(deadline) {
		writeError(w, http.StatusConflict, ErrCodeConflict, "test identity has expired",
			map[string]interface{}{"reason": testIdentityReasonExpired})
		return
	}
	if u.Status != store.UserStatusActive {
		writeError(w, http.StatusConflict, ErrCodeConflict, "test identity is not active", nil)
		return
	}
	tokenTTL, ok := testAdminTokenTTL(spec, req.TokenTTLSeconds, deadline, now)
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
	if !s.testIdentityAllow(w, testIdentityRateKey(ctx, caller)) {
		return
	}

	auditActor := s.buildAuditActorFromContext(ctx)
	workloadAudit := testIdentityWorkloadAuditOf(ctx)
	var token string
	var expiresIn int64
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		// Under the issuance lock, as issuance: the delegation's liveness
		// (checkDelegationLive, the workload-delegation analogue of C2, C3),
		// the grantor (C1) and the identity row are re-read in this
		// transaction, so a revocation, demotion, delete or revoke-all
		// since the request authenticated mints nothing.
		if err := tx.LockTestFixtureIssuance(ctx); err != nil {
			return err
		}
		if err := s.recheckWorkloadDelegation(ctx, tx, st, now); err != nil {
			return err
		}
		if err := s.recheckTestAdminDelegationGrantor(ctx, tx, st.delegation.EffectiveGrantorKind(), st.delegation.GrantorID, spec.permission); err != nil {
			return err
		}
		if err := recheckTestAdminIdentityForDelegation(ctx, tx, st, u.ID, now); err != nil {
			return err
		}
		var err error
		token, expiresIn, err = s.userTokenService.MintAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, u.SessionGeneration, ClientTypeAPI, tokenTTL)
		if err != nil {
			return fmt.Errorf("issue token: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType: testIdentityTokenIssueMutation,
			TargetType:   testIdentityAuditTargetType,
			TargetID:     u.ID,
			AfterSummary: testAdminTokenAuditSummary(u, tokenTTL, workloadAudit),
			Timestamp:    now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit test admin token issue: %w", err)
		}
		issuanceID := ""
		if u.IssuanceID != nil {
			issuanceID = *u.IssuanceID
		}
		// Written last, inside the transaction: if it fails, no token.
		if err := s.testAdminAudit.emit(testAdminAuditEventTokenIssue, map[string]any{
			"test_identity_id":     u.ID,
			"kind":                 u.Kind,
			"issuer_id":            st.delegation.GrantorID,
			"issuer_credential_id": st.delegation.ID,
			"issuer_delegation_id": st.delegation.ID,
			"workload_sub":         st.sub,
			"issuance_id":          issuanceID,
			"token_ttl_seconds":    int64(tokenTTL / time.Second),
			"token_expires_at":     now.Add(tokenTTL).UTC().Format(time.RFC3339),
			"request_id":           logging.RequestIDFromContext(ctx),
		}); err != nil {
			return fmt.Errorf("%w: %v", errTestAdminSinkWrite, err)
		}
		return nil
	})
	if err != nil {
		token = ""
		switch {
		case errors.Is(err, errTestIdentityDelegationNotLive):
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "workload delegation does not grant this request", nil)
		case errors.Is(err, errTestAdminIssuerNotLive):
			slog.WarnContext(ctx, "test admin token re-issue refused: grantor no longer a live super-admin", "test_identity_id", u.ID, "delegation_id", st.delegation.ID)
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "requires "+spec.permission+" permission",
				map[string]interface{}{"reason": testAdminReasonNotLiveAdmin})
		case errors.Is(err, errTestAdminIdentityNotManaged):
			NotFound(w, "test identity")
		case errors.Is(err, errTestAdminSinkWrite):
			slog.ErrorContext(ctx, "test admin token re-issue refused: audit sink write failed", "test_identity_id", u.ID)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "test admin audit is unavailable", nil)
		case isTransientIssuanceConflict(err):
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "test identity token issuance is busy; retry shortly",
				map[string]interface{}{"reason": testIdentityReasonBusy})
		default:
			slog.ErrorContext(ctx, "test admin token re-issue failed", "test_identity_id", u.ID, "error", err)
			InternalError(w)
		}
		return
	}
	slog.InfoContext(ctx, "test admin token re-issued", "test_identity_id", u.ID, "issuer_id", st.delegation.GrantorID,
		"kind", u.Kind, "delegation_id", st.delegation.ID)
	writeJSON(w, http.StatusOK, TestAdminIdentityTokenResponse{
		Identity:       testAdminIdentityViewOf(u, testAdminRoleForKind(u.Kind), now),
		AccessToken:    token,
		TokenType:      "Bearer",
		ExpiresIn:      expiresIn,
		TokenExpiresAt: now.Add(tokenTTL),
	})
}
