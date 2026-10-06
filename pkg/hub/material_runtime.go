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

// Package hub — runtime material selection: the single check sequence that
// agent runtime secret reads (fetch, get, and the agent secret list) go
// through before any value is read.
package hub

import (
	"context"
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// actionProjectSecretRead is the Action of the project.secret_read registry
// row (permissions/registry.go:264). It is named after the registry row, not
// after any effect — see TestMaterialRuntime_ProjectSecretReadActionMatchesRegistry.
const actionProjectSecretRead Action = Action("secret_read")

// materialRuntimePrecheck runs the whole-request checks (1-5) of the runtime
// material selection check sequence against the presented agent identity. On
// success it returns the resolved TargetFacts and a zero httpStatus. On
// denial it returns a reason code (audit only, never sent to the caller) and
// the HTTP status the caller should use: 403 for a policy/store-fact denial,
// 500 for an infrastructure error. Both fail closed.
//
// The nil-identity case (check 1's status-per-endpoint exception) is handled
// by each endpoint before calling this function; ident is never nil here in
// production. It is accepted as a possibly-nil parameter only so this
// function can be unit-tested directly.
func (s *Server) materialRuntimePrecheck(ctx context.Context, ident AgentIdentity) (*TargetFacts, string, int) {
	if ident == nil {
		return nil, ReasonTargetUnresolved, http.StatusForbidden
	}

	// Check 1: presented identity must be local (hub-attested), not a
	// federated claim about local principal IDs.
	if !AncestryIsHubAttested(ident) {
		return nil, ReasonIdentityNotLocal, http.StatusForbidden
	}

	// Check 2: store record.
	rec, err := s.store.GetAgent(ctx, ident.ID())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ReasonTargetUnresolved, http.StatusForbidden
		}
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	if rec == nil {
		return nil, ReasonTargetUnresolved, http.StatusForbidden
	}
	// GetAgent returns soft-deleted rows, so this check is required.
	if !rec.DeletedAt.IsZero() {
		return nil, ReasonTargetUnresolved, http.StatusForbidden
	}

	// Check 3: store facts only.
	project, err := s.store.GetProject(ctx, rec.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ReasonTargetUnresolved, http.StatusForbidden
		}
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	if project == nil {
		// A real store never returns (nil, nil); this is cheap insurance on
		// an authorization path rather than a reachable production case.
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	if ident.ProjectID() != rec.ProjectID {
		return nil, ReasonTokenProjectMismatch, http.StatusForbidden
	}
	root, ok := runtimeProvenanceRoot(ctx, rec)
	if !ok {
		// Empty ancestry: scheduler children and legacy rows. No fallback to
		// CreatedBy, OwnerID, token OriginUserID(), or scheduledCreatorIdentity.
		return nil, ReasonTargetUnresolved, http.StatusForbidden
	}

	// Check 4: credential capability.
	if !ident.HasScope(ScopeProjectSecretRead) {
		return nil, ReasonCapabilityRequired, http.StatusForbidden
	}

	// Check 5: root human live authority. Admission is project membership
	// (CheckEffectiveMembership: any active project-scoped binding, built-in
	// or custom, direct or group-derived) OR target-applicable system
	// authority for the exact secret.use permission (SystemAuthorityProof).
	// Membership is admission only: the check-7 project.secret_read
	// permission still gates every read, so a custom-only member without
	// that permission is refused there.
	u, err := s.store.GetUser(ctx, root.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Includes Ancestry[0] being an agent ID.
			return nil, ReasonTargetUnresolved, http.StatusForbidden
		}
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	if u == nil {
		// A real store never returns (nil, nil); this is cheap insurance on
		// an authorization path rather than a reachable production case.
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	if u.Status != store.UserStatusActive {
		return nil, ReasonSourceInactive, http.StatusForbidden
	}
	m := s.CheckEffectiveMembership(ctx, root.ID, rec.ProjectID)
	if m.Err != nil {
		return nil, ReasonBackendError, http.StatusInternalServerError
	}
	admitted := m.IsMember
	if !admitted {
		if s.authzService == nil {
			return nil, ReasonBackendError, http.StatusInternalServerError
		}
		// The class is fixed at the project scope kind because this
		// precheck runs before any per-item scope is known (a later
		// user-scope item does not change which class this call reviews).
		ok, err := s.authzService.SystemAuthorityProof(ctx,
			PrincipalContext{Kind: PrincipalKindUser, ID: root.ID},
			rec.ProjectID,
			"secret.use",
			ProjectTargetClass{ResourceType: permissions.ResourceSecret, ScopeKind: store.ScopeProject},
		)
		if err != nil {
			return nil, ReasonBackendError, http.StatusInternalServerError
		}
		admitted = ok
	}
	if !admitted {
		return nil, ReasonMembershipRequired, http.StatusForbidden
	}

	return &TargetFacts{
		Agent:     rec,
		ProjectID: rec.ProjectID,
		Ancestry:  rec.Ancestry,
		Root:      root,
		Project:   project,
	}, ReasonAllowed, 0
}

// projectDecisionCache memoizes the check-7 project.secret_read decision for
// a single request. The decision input does not depend on the key, so it is
// computed once, lazily, at the first project item, and reused for the rest
// of that request. A new cache must be created per request; it is never
// reused across requests.
type projectDecisionCache struct {
	computed bool
	decision Decision
	err      error
}

// projectReadDecision returns the memoized check-7 decision, computing it on
// first use (TestAgentSecretFetch_ProjectDecisionEvaluatedOncePerRequest).
func (s *Server) projectReadDecision(ctx context.Context, ident AgentIdentity, facts *TargetFacts, cache *projectDecisionCache) (Decision, error) {
	if !cache.computed {
		if s.authzService == nil {
			cache.err = errors.New("authz service unavailable")
			cache.computed = true
			return Decision{}, cache.err
		}
		cache.decision = s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(ident),
			Credential: credentialContextForIdentity(ident),
			Resource:   Resource{Type: "project", ID: facts.ProjectID, OwnerID: facts.Project.OwnerID},
			Action:     actionProjectSecretRead,
			Permission: "project.secret_read",
		})
		cache.computed = true
	}
	return cache.decision, cache.err
}

// authorizeRuntimeProjectItem is check 7's per-item metadata step. It must
// only be called with the request's single projectReadDecision. If the
// decision denies, no GetMeta call is made
// (TestAgentSecretFetch_DeniedRequestReadsNoMetadata) and the item is
// not_found with audit reason denied_by_policy; the decision's Reason is
// returned separately as the audit-only Detail string.
func (s *Server) authorizeRuntimeProjectItem(ctx context.Context, key string, facts *TargetFacts, decision Decision) (ItemResult, string) {
	cand := Candidate{
		Kind:    MaterialKindSecret,
		Key:     key,
		Scope:   store.ScopeProject,
		ScopeID: facts.ProjectID,
		Grant:   GrantProjectSecretRead,
	}
	if !decision.Allowed {
		return ItemResult{Candidate: cand, Reason: ReasonDeniedByPolicy}, decision.Reason
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeProject, facts.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ItemResult{Candidate: cand, Reason: ReasonNotFound}, ""
		}
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}, ""
	}
	if meta == nil {
		// A real backend never returns (nil, nil); this is cheap insurance
		// on an authorization path rather than a reachable production case.
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}, ""
	}
	if meta.SecretType == store.SecretTypeInternal {
		return ItemResult{Candidate: cand, Reason: ReasonNotFound}, ""
	}
	cand.Meta = *meta
	return ItemResult{Candidate: cand, Allowed: true, Reason: ReasonAllowed}, ""
}

// authorizeRuntimeUserItem is check 8, the per-item user-scope check.
// Progeny only; every user item carries Grant = GrantProgeny. No
// Decide(project.secret_read) of any shape is made here, and there is no
// owner shortcut.
func (s *Server) authorizeRuntimeUserItem(ctx context.Context, facts *TargetFacts, key string) ItemResult {
	cand := Candidate{
		Kind:    MaterialKindSecret,
		Key:     key,
		Scope:   store.ScopeUser,
		ScopeID: facts.Root.ID,
		Grant:   GrantProgeny,
	}

	meta, err := s.secretBackend.GetMeta(ctx, key, store.ScopeUser, facts.Root.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ItemResult{Candidate: cand, Reason: ReasonNotFound}
		}
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}
	}
	if meta == nil {
		// A real backend never returns (nil, nil); this is cheap insurance
		// on an authorization path rather than a reachable production case.
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}
	}
	if meta.SecretType == store.SecretTypeInternal {
		return ItemResult{Candidate: cand, Reason: ReasonNotFound}
	}
	cand.Meta = *meta
	// SharingSource.ID is known as soon as metadata resolves; Kind is filled
	// in once source liveness is resolved below, for the allowed and
	// source-inactive outcomes.
	cand.SharingSource = &SourceRef{ID: meta.CreatedBy}

	// Checks 8b (AllowProgeny) and 8c (CheckProgenyAccess) run before source
	// liveness is resolved: an unshared row or a row outside the agent's
	// lineage denies without the extra store lookups that resolving the
	// source would cost.
	if !meta.AllowProgeny {
		return ItemResult{Candidate: cand, Reason: ReasonSharingDisabled}
	}

	if s.authzService == nil {
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}
	}
	grant := s.authzService.relationshipResolver.CheckProgenyAccess(
		ctx,
		&storedAgentIdentity{agent: facts.Agent},
		Resource{Type: "secret", ID: meta.ID, OwnerID: meta.ScopeID, ParentType: "user", ParentID: meta.ScopeID},
		ActionRead,
	)
	if !grant.Allowed {
		return ItemResult{Candidate: cand, Reason: ReasonDeniedByPolicy}
	}

	live, kind, liveReason, liveErr := s.progenySourceLive(ctx, *meta)
	if kind != "" {
		cand.SharingSource = &SourceRef{Kind: kind, ID: meta.CreatedBy}
	}
	if liveErr != nil {
		return ItemResult{Candidate: cand, Reason: ReasonBackendError}
	}
	if !live {
		return ItemResult{Candidate: cand, Reason: liveReason}
	}

	return ItemResult{Candidate: cand, Allowed: true, Reason: ReasonAllowed}
}

// fetchAuthorizedValue is check 9, the record-race rule. It must only be
// called after check 7 or 8 has allowed the item.
func (s *Server) fetchAuthorizedValue(ctx context.Context, item ItemResult) (*secret.SecretWithValue, string) {
	sv, err := s.secretBackend.Get(ctx, item.Key, item.Scope, item.ScopeID)
	if err != nil {
		// An error is unavailable, never an empty value.
		return nil, ReasonBackendError
	}
	if sv == nil {
		// A real backend never returns (nil, nil); this is cheap insurance
		// on an authorization path rather than a reachable production case.
		return nil, ReasonBackendError
	}
	if sv.ID != item.Meta.ID ||
		sv.Version != item.Meta.Version ||
		sv.SecretType == store.SecretTypeInternal ||
		sv.AllowProgeny != item.Meta.AllowProgeny ||
		sv.CreatedBy != item.Meta.CreatedBy ||
		sv.ScopeID != item.Meta.ScopeID {
		return nil, ReasonRecordChanged
	}
	return sv, ReasonAllowed
}
