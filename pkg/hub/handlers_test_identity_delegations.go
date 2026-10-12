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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Workload-bound issuer delegations (ptone/scion#4240, phase X2).
//
// POST /api/v1/test-identity-delegations lets a human issuer delegate
// member-tier test identity issuance to one pinned workload identity:
// a subject in the startup allowlist server.hub.test_identities.workload_issuers.
// GET lists delegations and DELETE /{id} revokes one. All three need
// test_identity.issue at hub scope from an interactive session of a live
// human user: a PAT, an agent, a workload delegation and every test
// identity kind are refused. The response holds only ids and settings; a
// delegation is not a secret, and presenting its id without a workload
// token proves nothing (see test_identity_workload_issuer.go).

// Delegation defaults and bounds.
const (
	testIdentityDelegationDefaultTTL = 8 * time.Hour
	testIdentityDelegationMinTTL     = time.Minute
	// testIdentityDelegationLabelMaxRunes bounds a delegation label.
	testIdentityDelegationLabelMaxRunes = 64

	testIdentityDelegationListDefaultLimit = 100
	testIdentityDelegationListMaxLimit     = 500
)

// Mutation audit types and target type of delegations.
const (
	testIdentityDelegationCreateMutation = "test_identity_delegation_create"
	testIdentityDelegationRevokeMutation = "test_identity_delegation_revoke"
	testIdentityDelegationAuditTarget    = "test_identity_delegation"
)

// details.reason values of delegation refusals.
const (
	testIdentityDelegationReasonAdminTier   = "admin_tier_not_available"
	testIdentityDelegationReasonScope       = "invalid_scope"
	testIdentityDelegationReasonSub         = "workload_sub_not_allowed"
	testIdentityDelegationReasonTTL         = "invalid_ttl"
	testIdentityDelegationReasonCap         = "invalid_member_cap"
	testIdentityDelegationReasonLabel       = "invalid_label"
	testIdentityDelegationReasonNotHuman    = "grantor_not_human"
	testIdentityDelegationReasonCredential  = "session_credential_required"
	testIdentityDelegationReasonUnavailable = "workload_delegation_unavailable"
)

// CreateTestIdentityDelegationRequest is the body of POST
// /api/v1/test-identity-delegations. Unknown fields are rejected.
type CreateTestIdentityDelegationRequest struct {
	// WorkloadSub is the Google subject (service account unique id) the
	// delegation is pinned to. It must be in the startup allowlist.
	WorkloadSub string `json:"workloadSub"`
	// Scopes holds exactly one scope: "test_identity:issue".
	Scopes []string `json:"scopes"`
	// TTLSeconds is the delegation lifetime. 0 means the default (8 h); at
	// least 60 and at most 7 days.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	// MemberCap is the number of live test identities the delegation may
	// hold at once: 1 to 5. 0 means 5.
	MemberCap int `json:"memberCap,omitempty"`
	// Label is a short free-text label for the test window.
	Label string `json:"label,omitempty"`
}

// TestIdentityDelegationView is the response view of a delegation. It
// holds no secret.
type TestIdentityDelegationView struct {
	ID          string     `json:"id"`
	GrantorID   string     `json:"grantorId"`
	WorkloadSub string     `json:"workloadSub"`
	Scope       string     `json:"scope"`
	MemberCap   int        `json:"memberCap"`
	Label       string     `json:"label,omitempty"`
	Created     time.Time  `json:"created"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	RevokedBy   string     `json:"revokedBy,omitempty"`
	Live        bool       `json:"live"`
}

// testIdentityDelegationViewOf renders d. Live uses the auth arm's own
// definition (checkDelegationLive: not revoked, before its expiry and its
// hard maximum age, the accepted scope, a subject the current startup
// allowlist covers), so a delegation that mints nothing, for example one
// whose subject was removed at a restart, is not shown as live. wi may be
// nil (no workload issuer configured): nothing is live then.
func testIdentityDelegationViewOf(d *store.TestIdentityDelegation, now time.Time, wi *workloadIssuerAuthenticator) TestIdentityDelegationView {
	v := TestIdentityDelegationView{
		ID:          d.ID,
		GrantorID:   d.GrantorID,
		WorkloadSub: d.WorkloadSub,
		Scope:       d.Scope,
		MemberCap:   d.MemberCap,
		Label:       d.Label,
		Created:     d.Created,
		ExpiresAt:   d.ExpiresAt,
		RevokedAt:   d.RevokedAt,
		Live:        wi != nil && wi.checkDelegationLive(d, now) == nil,
	}
	if d.RevokedBy != nil {
		v.RevokedBy = *d.RevokedBy
	}
	return v
}

// ListTestIdentityDelegationsResponse is the body of GET
// /api/v1/test-identity-delegations.
type ListTestIdentityDelegationsResponse struct {
	Items     []TestIdentityDelegationView `json:"items"`
	Truncated bool                         `json:"truncated"`
}

// testIdentityDelegationCaller runs the checks every delegation route
// shares after the route guard (which already refused every non-session
// credential): the feature flag, an interactive or dev session credential,
// the test_identity.issue decision, and a live human user row read from the
// store (a missing, unreadable, non-human or inactive row is refused). It
// writes the response and returns false on refusal.
func (s *Server) testIdentityDelegationCaller(w http.ResponseWriter, r *http.Request) (UserIdentity, bool) {
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
	case CredentialKindInteractive, CredentialKindDev:
	default:
		// PAT, agent, workload delegation, federation, broker and any
		// unknown credential: a delegation is created by a human session.
		logAuthzDenial(r, user, Resource{Type: permissions.ResourceTestIdentity}, ActionIssue, "credential kind not admitted for issuer delegations")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "issuer delegations require an interactive session",
			map[string]interface{}{"reason": testIdentityDelegationReasonCredential})
		return nil, false
	}
	if IsScopedUserIdentity(user) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "issuer delegations require an interactive session",
			map[string]interface{}{"reason": testIdentityDelegationReasonCredential})
		return nil, false
	}
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
	row, err := s.store.GetUser(ctx, user.ID())
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		InternalError(w)
		return nil, false
	case err != nil || row.Kind != store.UserKindHuman || row.Status != store.UserStatusActive || isReservedTestIdentityEmail(row.Email):
		// Fail closed: a dev principal with no row, a test identity of any
		// kind, and a suspended user cannot hold a standing issuer path.
		logAuthzDenial(r, user, Resource{Type: permissions.ResourceTestIdentity}, ActionIssue, "issuer delegations require a live human user")
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "issuer delegations require a live human user",
			map[string]interface{}{"reason": testIdentityDelegationReasonNotHuman})
		return nil, false
	}
	return user, true
}

func writeDelegationValidation(w http.ResponseWriter, field, reason, msg string) {
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, msg,
		map[string]interface{}{"field": field, "reason": reason})
}

// testIdentityDelegationSummary is the audit summary of a delegation. It
// holds no secret.
func testIdentityDelegationSummary(d *store.TestIdentityDelegation) string {
	b, _ := json.Marshal(struct {
		DelegationID string `json:"delegation_id"`
		Grantor      string `json:"grantor"`
		WorkloadSub  string `json:"workload_sub"`
		Scope        string `json:"scope"`
		MemberCap    int    `json:"member_cap"`
		Label        string `json:"label"`
		ExpiresAt    string `json:"expires_at"`
	}{d.ID, d.GrantorID, d.WorkloadSub, d.Scope, d.MemberCap, d.Label, d.ExpiresAt.UTC().Format(time.RFC3339)})
	return string(b)
}

// handleCreateTestIdentityDelegation handles POST
// /api/v1/test-identity-delegations.
func (s *Server) handleCreateTestIdentityDelegation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	grantor, ok := s.testIdentityDelegationCaller(w, r)
	if !ok {
		return
	}
	var req CreateTestIdentityDelegationRequest
	if err := decodeTestIdentityBody(r, &req); err != nil {
		BadRequest(w, err.Error())
		return
	}

	if len(req.Scopes) != 1 {
		writeDelegationValidation(w, "scopes", testIdentityDelegationReasonScope, "scopes must hold exactly one scope")
		return
	}
	scope := req.Scopes[0]
	if testIdentityDelegationAdminScopes[scope] {
		writeDelegationValidation(w, "scopes", testIdentityDelegationReasonAdminTier,
			"admin-tier issuer delegations are not available in this release; only test_identity:issue is accepted")
		return
	}
	if scope != testIdentityDelegationScope {
		writeDelegationValidation(w, "scopes", testIdentityDelegationReasonScope,
			fmt.Sprintf("invalid scope: must be %q", testIdentityDelegationScope))
		return
	}
	if s.workloadIssuer == nil {
		writeDelegationValidation(w, "workloadSub", testIdentityDelegationReasonUnavailable,
			"this hub has no workload issuer configured (server.hub.test_identities.workload_issuers)")
		return
	}
	if !s.workloadIssuer.subCovers(req.WorkloadSub, scope) {
		writeDelegationValidation(w, "workloadSub", testIdentityDelegationReasonSub,
			"workloadSub is not in this hub's workload issuer allowlist for this scope")
		return
	}
	ttl := testIdentityDelegationDefaultTTL
	if req.TTLSeconds != 0 {
		if req.TTLSeconds < int64(testIdentityDelegationMinTTL/time.Second) || req.TTLSeconds > int64(testIdentityDelegationMaxAge/time.Second) {
			writeDelegationValidation(w, "ttlSeconds", testIdentityDelegationReasonTTL,
				fmt.Sprintf("invalid ttlSeconds: must be between %d and %d", int64(testIdentityDelegationMinTTL/time.Second), int64(testIdentityDelegationMaxAge/time.Second)))
			return
		}
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	memberCap := testIdentityPerIssuerCap
	if req.MemberCap != 0 {
		if req.MemberCap < 1 || req.MemberCap > testIdentityPerIssuerCap {
			writeDelegationValidation(w, "memberCap", testIdentityDelegationReasonCap,
				fmt.Sprintf("invalid memberCap: must be between 1 and %d", testIdentityPerIssuerCap))
			return
		}
		memberCap = req.MemberCap
	}
	label := strings.TrimSpace(req.Label)
	if utf8.RuneCountInString(label) > testIdentityDelegationLabelMaxRunes || hasControlChar(label, false) {
		writeDelegationValidation(w, "label", testIdentityDelegationReasonLabel,
			fmt.Sprintf("invalid label: at most %d characters, no control characters", testIdentityDelegationLabelMaxRunes))
		return
	}

	now := s.testIdentities.clock()
	d := &store.TestIdentityDelegation{
		ID:          generateID(),
		GrantorID:   grantor.ID(),
		WorkloadSub: req.WorkloadSub,
		Scope:       scope,
		MemberCap:   memberCap,
		Label:       label,
		Created:     now,
		ExpiresAt:   now.Add(ttl),
	}
	auditActor := s.buildAuditActorFromContext(ctx)
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateTestIdentityDelegation(ctx, d); err != nil {
			return fmt.Errorf("create issuer delegation: %w", err)
		}
		record := &store.MutationAuditRecord{
			MutationType: testIdentityDelegationCreateMutation,
			TargetType:   testIdentityDelegationAuditTarget,
			TargetID:     d.ID,
			AfterSummary: testIdentityDelegationSummary(d),
			Timestamp:    now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit issuer delegation create: %w", err)
		}
		return nil
	})
	if err != nil {
		if isTransientIssuanceConflict(err) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "issuer delegation create is busy; retry shortly",
				map[string]interface{}{"reason": testIdentityReasonBusy})
			return
		}
		slog.ErrorContext(ctx, "issuer delegation create failed", "grantor_id", grantor.ID(), "error", err)
		InternalError(w)
		return
	}
	slog.InfoContext(ctx, "test identity issuer delegation created", "delegation_id", d.ID, "grantor_id", d.GrantorID,
		"workload_sub", d.WorkloadSub, "scope", d.Scope, "member_cap", d.MemberCap, "expires_at", d.ExpiresAt)
	writeJSON(w, http.StatusCreated, testIdentityDelegationViewOf(d, now, s.workloadIssuer))
}

// handleListTestIdentityDelegations handles GET
// /api/v1/test-identity-delegations: the caller's own delegations, or
// every delegation for an unscoped platform admin session.
func (s *Server) handleListTestIdentityDelegations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityDelegationCaller(w, r)
	if !ok {
		return
	}
	grantorID := caller.ID()
	if testIdentityCallerSeesAll(caller) {
		grantorID = ""
	}
	limit := testIdentityDelegationListDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > testIdentityDelegationListMaxLimit {
			BadRequest(w, fmt.Sprintf("invalid limit: must be between 1 and %d", testIdentityDelegationListMaxLimit))
			return
		}
		limit = n
	}
	rows, err := s.store.ListTestIdentityDelegations(ctx, grantorID, limit+1)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	now := s.testIdentities.clock()
	items := make([]TestIdentityDelegationView, 0, len(rows))
	for i := range rows {
		items = append(items, testIdentityDelegationViewOf(&rows[i], now, s.workloadIssuer))
	}
	writeJSON(w, http.StatusOK, ListTestIdentityDelegationsResponse{Items: items, Truncated: truncated})
}

// handleRevokeTestIdentityDelegation handles DELETE
// /api/v1/test-identity-delegations/{id}. The grantor or an unscoped
// platform admin session revokes; any other caller, and an unknown id, get
// the same 404. Revoking a revoked delegation answers 204 and keeps the
// first revocation. A test_identity_delegation_revoke audit record is
// written in the same transaction, only by the call whose store update
// changed the row, so concurrent revocations write one record.
func (s *Server) handleRevokeTestIdentityDelegation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := s.testIdentityDelegationCaller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	d, err := s.store.GetTestIdentityDelegation(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.GrantorID != caller.ID() && !testIdentityCallerSeesAll(caller)) {
		NotFound(w, "issuer delegation")
		return
	}
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if d.RevokedAt != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	now := s.testIdentities.clock()
	auditActor := s.buildAuditActorFromContext(ctx)
	err = s.store.WithTx(ctx, func(tx store.Store) error {
		changed, err := tx.RevokeTestIdentityDelegation(ctx, d.ID, caller.ID(), now)
		if err != nil {
			return err
		}
		if !changed {
			// Revoked concurrently by another request: that request wrote
			// the audit record.
			return nil
		}
		record := &store.MutationAuditRecord{
			MutationType:  testIdentityDelegationRevokeMutation,
			TargetType:    testIdentityDelegationAuditTarget,
			TargetID:      d.ID,
			BeforeSummary: testIdentityDelegationSummary(d),
			Timestamp:     now,
		}
		auditActor.ApplyActor(record)
		if err := s.testIdentities.hooks.writeAudit(ctx, tx, record); err != nil {
			return fmt.Errorf("audit issuer delegation revoke: %w", err)
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "issuer delegation")
		case isTransientIssuanceConflict(err):
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "issuer delegation revoke is busy; retry shortly",
				map[string]interface{}{"reason": testIdentityReasonBusy})
		default:
			slog.ErrorContext(ctx, "issuer delegation revoke failed", "delegation_id", d.ID, "error", err)
			InternalError(w)
		}
		return
	}
	slog.InfoContext(ctx, "test identity issuer delegation revoked", "delegation_id", d.ID, "revoked_by", caller.ID())
	w.WriteHeader(http.StatusNoContent)
}
