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

//go:build !no_sqlite

// Package hub — tests for the user-scope per-item check (check 8): progeny
// sharing, lineage containment, and source liveness.
package hub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// TestAgentGetSecret_OtherUserScopeNotReachable covers a user-scope key that
// exists only in a different user's scope: not_found, never a value.
func TestAgentGetSecret_OtherUserScopeNotReachable(t *testing.T) {
	f := newMaterialFixture(t, "other-user-scope")
	ctx := context.Background()

	otherUserID := tid("other-user-scope-owner")
	require.NoError(t, f.Store.CreateUser(ctx, &store.User{
		ID: otherUserID, Email: "other-user-scope-owner@test.com", DisplayName: "other", Role: "member", Status: store.UserStatusActive,
	}))
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "OTHER_USER_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "OTHER_USER_KEY",
		Scope: store.ScopeUser, ScopeID: otherUserID, AllowProgeny: true, CreatedBy: otherUserID, UpdatedBy: otherUserID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/OTHER_USER_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_UserSecretRequiresSharingEnabled pins that an unset
// AllowProgeny resolves to false (opt-in): a user's own secret is not
// readable by their agent without explicit sharing.
func TestAgentSecretRead_UserSecretRequiresSharingEnabled(t *testing.T) {
	f := newMaterialFixture(t, "sharing-required")
	ctx := context.Background()

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "NOSHARE_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "NOSHARE_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NOSHARE_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (sharing disabled), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_DirectChildRequiresSharingEnabled pins that direct
// children are included in the AllowProgeny requirement — there is no
// automatic owner shortcut for the immediate agent. Once sharing is
// enabled, the same direct child is allowed.
func TestAgentSecretRead_DirectChildRequiresSharingEnabled(t *testing.T) {
	f := newMaterialFixture(t, "direct-child-sharing")
	ctx := context.Background()

	// f.AgentID is a direct child of f.UserID (Ancestry = [f.UserID]).
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "DIRECT_CHILD_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "DIRECT_CHILD_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/DIRECT_CHILD_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (direct children still require AllowProgeny), got %d: %s", rec.Code, rec.Body.String())
	}

	_, err = f.Server.secretBackend.UpdateMeta(ctx, &secret.UpdateMetaInput{
		Name: "DIRECT_CHILD_KEY", Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: boolPtr(true), UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	rec2 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/DIRECT_CHILD_KEY?scope=user", nil, f.Token)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 once AllowProgeny is enabled, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestAgentSecretRead_UserScopeRowAuthoredOutsideLineageDenied pins that a
// row in the root's scope written by an agent outside the
// requesting agent's ancestry is denied, even with AllowProgeny set.
func TestAgentSecretRead_UserScopeRowAuthoredOutsideLineageDenied(t *testing.T) {
	f := newMaterialFixture(t, "outside-lineage")
	ctx := context.Background()

	outsiderAgentID := tid("outsider-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: outsiderAgentID, Slug: "outsider", Name: "outsider", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID},
		Created: time.Now(), Updated: time.Now(),
	}))

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "OUTSIDE_LINEAGE_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "OUTSIDE_LINEAGE_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: outsiderAgentID, UpdatedBy: outsiderAgentID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/OUTSIDE_LINEAGE_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (creator outside ancestry denies), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_SharingSourceMustBeActive unit-tests progenySourceLive
// directly: a suspended user source is inactive. (A
// suspended CreatedBy that is also the requesting agent's own root would be
// caught earlier by check 5 — TestAgentSecretRead_SuspendedRootUserDenied —
// so the source-liveness sub-check is exercised in isolation here.)
func TestAgentSecretRead_SharingSourceMustBeActive(t *testing.T) {
	f := newMaterialFixture(t, "source-must-be-active")
	ctx := context.Background()

	suspendedUserID := tid("suspended-source-user")
	require.NoError(t, f.Store.CreateUser(ctx, &store.User{
		ID: suspendedUserID, Email: "suspended-source@test.com", DisplayName: "s", Role: "member", Status: store.UserStatusSuspended,
	}))

	live, kind, reason, err := f.Server.progenySourceLive(ctx, secret.SecretMeta{CreatedBy: suspendedUserID})
	require.NoError(t, err)
	if live || kind != "user" || reason != ReasonSourceInactive {
		t.Fatalf("expected an inactive user source, got live=%v kind=%s reason=%s", live, kind, reason)
	}
}

// TestAgentSecretRead_SharingSourceEmptyCreatedByDenies pins that an empty
// meta.CreatedBy denies with source_inactive, no kind and no error. The
// early return in progenySourceLive keeps the outcome the user and agent
// lookups already reached (entadapter's parseGetID resolves an empty ID to
// store.ErrNotFound), so this test pins the outcome, not the skipped
// lookups.
func TestAgentSecretRead_SharingSourceEmptyCreatedByDenies(t *testing.T) {
	f := newMaterialFixture(t, "source-empty-created-by")

	live, kind, reason, err := f.Server.progenySourceLive(context.Background(), secret.SecretMeta{CreatedBy: ""})
	require.NoError(t, err)
	if live || kind != "" || reason != ReasonSourceInactive {
		t.Fatalf("expected an inactive source with no kind, got live=%v kind=%s reason=%s", live, kind, reason)
	}
}

// TestAgentSecretRead_SharingSourceAgentDeletedDenied pins that a
// soft-deleted source agent is not live, even though GetAgent returns
// soft-deleted rows.
func TestAgentSecretRead_SharingSourceAgentDeletedDenied(t *testing.T) {
	f := newMaterialFixture(t, "source-agent-deleted")
	ctx := context.Background()

	creatorAgentID := tid("source-agent-to-delete")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: creatorAgentID, Slug: "source-agent", Name: "source", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID},
		Created: time.Now(), Updated: time.Now(),
	}))
	grandchildID := tid("grandchild-of-source-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: grandchildID, Slug: "grandchild", Name: "grandchild", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, creatorAgentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	grandchildToken, err := f.Server.agentTokenService.GenerateAgentToken(grandchildID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, creatorAgentID})
	require.NoError(t, err)

	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SOURCE_DELETED_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "SOURCE_DELETED_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: creatorAgentID, UpdatedBy: creatorAgentID,
	})
	require.NoError(t, err)

	creator, err := f.Store.GetAgent(ctx, creatorAgentID)
	require.NoError(t, err)
	creator.DeletedAt = time.Now()
	require.NoError(t, f.Store.UpdateAgent(ctx, creator))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+grandchildID+"/secrets/SOURCE_DELETED_KEY?scope=user", nil, grandchildToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (soft-deleted source agent denies), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_SharingSourceAgentRootSuspendedDenied pins that
// resolving a source agent's own root and finding it suspended denies.
func TestAgentSecretRead_SharingSourceAgentRootSuspendedDenied(t *testing.T) {
	f := newMaterialFixture(t, "source-agent-root-suspended")
	ctx := context.Background()

	otherRootID := tid("other-root-for-source-agent")
	require.NoError(t, f.Store.CreateUser(ctx, &store.User{
		ID: otherRootID, Email: "other-root-source@test.com", DisplayName: "o", Role: "member", Status: store.UserStatusSuspended,
	}))
	creatorAgentID := tid("source-agent-other-root")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: creatorAgentID, Slug: "source-agent-other-root", Name: "source", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{otherRootID},
		Created: time.Now(), Updated: time.Now(),
	}))
	grandchildID := tid("grandchild-of-other-root-source")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: grandchildID, Slug: "grandchild-other-root", Name: "grandchild", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, creatorAgentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	grandchildToken, err := f.Server.agentTokenService.GenerateAgentToken(grandchildID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, creatorAgentID})
	require.NoError(t, err)

	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "SOURCE_ROOT_SUSPENDED_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "SOURCE_ROOT_SUSPENDED_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: creatorAgentID, UpdatedBy: creatorAgentID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+grandchildID+"/secrets/SOURCE_ROOT_SUSPENDED_KEY?scope=user", nil, grandchildToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (source agent's own root suspended denies), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_SharingSourceLookupErrorDenies unit-tests that a
// genuine store fault while resolving the sharing source denies with
// backend_error, distinct from a definite not-found.
func TestAgentSecretRead_SharingSourceLookupErrorDenies(t *testing.T) {
	f := newMaterialFixture(t, "source-lookup-error")
	f.Server.store = &materialFailingStore{
		Store:      f.Store,
		getUserErr: errors.New("injected source lookup failure"),
	}

	live, _, reason, err := f.Server.progenySourceLive(context.Background(), secret.SecretMeta{CreatedBy: tid("whoever")})
	if live || err == nil || reason != ReasonBackendError {
		t.Fatalf("expected backend_error with a non-nil error, got live=%v reason=%s err=%v", live, reason, err)
	}
}

// TestAgentSecretRead_UserScopeMakesNoProjectSecretDecision pins that a
// user-scope read never calls Decide(project.secret_read) — the
// delegation ceiling and its edge lookup are never consulted.
func TestAgentSecretRead_UserScopeMakesNoProjectSecretDecision(t *testing.T) {
	f := newMaterialFixture(t, "user-scope-no-project-decision")
	ctx := context.Background()

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "USER_ONLY_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "USER_ONLY_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	counting := &callCountingStore{Store: f.Store}
	f.Server.authzService = NewAuthzService(counting, logging.Subsystem("hub.auth"))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/USER_ONLY_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if counting.getDelegationEdgesForDelegateCalls != 0 {
		t.Fatalf("expected no project decision for a user-scope read, got %d edge lookups", counting.getDelegationEdgesForDelegateCalls)
	}
}

// TestAgentGetSecret_UserMetaErrorIsUnavailable pins that a user-scope
// GetMeta backend error on the by-key get endpoint answers unavailable, not
// not found, with no backend error text in the response, and is audited as
// backend_error. This is the user-scope counterpart of
// TestAgentGetSecret_ProjectMetaErrorIsUnavailable; only
// progenySourceLive's own errors (later in check 8) were previously covered
// on this scope.
func TestAgentGetSecret_UserMetaErrorIsUnavailable(t *testing.T) {
	f := newMaterialFixture(t, "get-user-meta-error")
	ctx := context.Background()

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "GET_USER_META_ERROR_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "GET_USER_META_ERROR_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	wrapped := &erroringMetaBackend{
		SecretBackend: f.Server.secretBackend,
		err:           errors.New("backend detail: disk quota exceeded on volume XYZ123"),
	}
	f.Server.SetSecretBackend(wrapped)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
		"/api/v1/agents/"+f.AgentID+"/secrets/GET_USER_META_ERROR_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret unavailable") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "XYZ123") {
		t.Fatalf("backend error text leaked into response: %s", rec.Body.String())
	}
	assertBackendErrorAudited(t, auditor)
}

// TestAgentSecretRead_UserScopeCeilingApplied documents the known gap
// (partial coverage): user-scoped reads have no delegation-ceiling step at
// all yet. Check 8 still calls the relationship resolver directly rather
// than routing through a permission decision, so closing this gap needs two
// further changes neither of which has landed: a shared, exact-permission
// progeny decision path that accepts a non-read-only action, and a
// user-material delegation ceiling to evaluate once that path exists.
func TestAgentSecretRead_UserScopeCeilingApplied(t *testing.T) {
	t.Skip("pending the shared exact-permission progeny decision path and the user-material delegation ceiling")
}

// TestAgentSecretRead_ProgenyEligibleSecretIDsNilRecDenies pins that
// progenyEligibleSecretIDs returns an empty, non-nil set with no error for a
// nil rec, rather than panicking when building the storedAgentIdentity.
// facts.Agent is always non-nil at the one call site (materialRuntimePrecheck
// denies before building facts when the agent record does not resolve), so
// this is a defensive guard
// rather than a reachable production case.
func TestAgentSecretRead_ProgenyEligibleSecretIDsNilRecDenies(t *testing.T) {
	f := newMaterialFixture(t, "progeny-eligible-nil-rec")

	ids, err := f.Server.progenyEligibleSecretIDs(context.Background(), nil)
	require.NoError(t, err)
	if len(ids) != 0 {
		t.Fatalf("expected an empty set for a nil rec, got %v", ids)
	}
}
