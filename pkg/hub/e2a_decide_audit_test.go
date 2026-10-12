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

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Decide emits exactly one decision audit record per call, whichever return
// path decide takes internally — including the UAT project/credential-scope
// gate, an out-of-project resource, and an out-of-scope action.
// ---------------------------------------------------------------------------

// TestDecide_UATProjectGateDenialIsAudited proves a UAT request denied by the
// project constraint (pre-kernel gate) produces exactly one decision audit
// record.
func TestDecide_UATProjectGateDenialIsAudited(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID, ownerID := setupUATProjectAndOwner(t, s, "decide-uat-gate")
	otherProjectID := tid("decide-uat-gate-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: otherProjectID, Name: "other", Slug: "decide-uat-gate-other", CreatedBy: ownerID, OwnerID: ownerID,
	}))

	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	tokenID := "decide-uat-gate-token"
	base := NewAuthenticatedUser(ownerID, "owner@test.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithCredentialID(base, projectID, []string{"project:read"}, tokenID)

	before := len(emitter.records)
	c := contextWithCredentialContext(contextWithIdentity(ctx, scoped), credentialContextForIdentity(scoped))
	decision := srv.authzService.Decide(c, AuthzRequestFromContext(c, Resource{Type: "project", ID: otherProjectID}, ActionRead))
	require.False(t, decision.Allowed)
	require.Contains(t, decision.Reason, "not scoped for this project")

	require.Equal(t, before+1, len(emitter.records), "the UAT project-gate denial must emit exactly one decision audit record")
	rec := emitter.records[len(emitter.records)-1]
	require.Equal(t, "deny", rec.Result)
	require.Equal(t, tokenID, rec.CredentialID)

	// And the out-of-scope gate (same token, in-project resource, missing scope).
	before = len(emitter.records)
	c2 := contextWithCredentialContext(contextWithIdentity(ctx, scoped), credentialContextForIdentity(scoped))
	decision2 := srv.authzService.Decide(c2, AuthzRequestFromContext(c2, Resource{Type: "project", ID: projectID}, ActionDelete))
	require.False(t, decision2.Allowed)
	require.Contains(t, decision2.Reason, "does not have scope")
	require.Equal(t, before+1, len(emitter.records), "the UAT scope-gate denial must also emit exactly one decision audit record")
}

// TestDecide_PermissionIDPopulated proves the decision audit record's
// PermissionID is exactly the caller-supplied AuthzRequest.Permission,
// recorded only when it is a canonical ID in the permissions registry —
// never derived from Resource/Action, and never an unregistered string.
func TestDecide_PermissionIDPopulated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	project := &store.Project{ID: tid("permid-project"), Name: "p", Slug: "permid-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "api")
	principal := contextWithIdentity(ctx, identity)

	// Case 1: an explicit, registered Permission gives exactly that value.
	req := AuthzRequestFromContext(principal, Resource{Type: "project", ID: project.ID}, ActionRead)
	req.Permission = "project.read"
	decision := srv.authzService.Decide(ctx, req)
	require.True(t, decision.Allowed)
	require.Equal(t, "project.read", decision.PermissionID)
	last := emitter.records[len(emitter.records)-1]
	require.Equal(t, "project.read", last.PermissionID)

	// Case 2: no Permission supplied. Even though resource+action resolves to
	// a real registry entry internally (for kernel evaluation), the audit
	// value must stay empty: it is never derived from Resource/Action.
	before := len(emitter.records)
	noPermReq := AuthzRequestFromContext(principal, Resource{Type: "project", ID: project.ID}, ActionRead)
	require.Empty(t, noPermReq.Permission)
	decision2 := srv.authzService.Decide(ctx, noPermReq)
	require.True(t, decision2.Allowed)
	require.Empty(t, decision2.PermissionID)
	require.Equal(t, before+1, len(emitter.records))
	require.Empty(t, emitter.records[len(emitter.records)-1].PermissionID)

	// Case 3: an explicit but unregistered Permission string gives "" — never
	// certified as if it were a real permission.
	before = len(emitter.records)
	bogusReq := AuthzRequestFromContext(principal, Resource{Type: "project", ID: project.ID}, ActionRead)
	bogusReq.Permission = "not.a.real.permission"
	decision3 := srv.authzService.Decide(ctx, bogusReq)
	require.Empty(t, decision3.PermissionID)
	require.Equal(t, before+1, len(emitter.records))
	require.Empty(t, emitter.records[len(emitter.records)-1].PermissionID)

	// A decision that fails before permission resolution (missing principal)
	// leaves PermissionID empty — never invented.
	before = len(emitter.records)
	missing := srv.authzService.Decide(ctx, AuthzRequest{Resource: Resource{Type: "project", ID: project.ID}, Action: ActionRead})
	require.False(t, missing.Allowed)
	require.Empty(t, missing.PermissionID)
	require.Equal(t, before+1, len(emitter.records))
	require.Empty(t, emitter.records[len(emitter.records)-1].PermissionID)
}

// TestDecide_AlwaysAuditOverridesSampling proves the always-audit marker (for
// G's delegated-agent events) forces an audit record for an allow decision
// even when the sample rate would otherwise have dropped it.
func TestDecide_AlwaysAuditOverridesSampling(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	srv.authzService.DecisionAuditSampleRate = 0.0 // never sample allows

	project := &store.Project{ID: tid("alwaysaudit-project"), Name: "p", Slug: "alwaysaudit-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "api")

	before := len(emitter.records)
	req := AuthzRequestFromContext(contextWithIdentity(ctx, identity), Resource{Type: "project", ID: project.ID}, ActionRead)
	decision := srv.authzService.Decide(ctx, req)
	require.True(t, decision.Allowed)
	require.Equal(t, before, len(emitter.records), "sample rate 0 must drop an ordinary allow")

	req.AlwaysAudit = true
	decision = srv.authzService.Decide(ctx, req)
	require.True(t, decision.Allowed)
	require.Equal(t, before+1, len(emitter.records), "AlwaysAudit must override allow-sampling")
}

// TestDecide_DecisionAlwaysAuditOverridesSampling proves Decision.AlwaysAudit
// (settable from inside decide's body, e.g. a delegated-agent branch that
// only learns partway through evaluation that this decision must not be
// sampled away) forces an audit record the same way AuthzRequest.AlwaysAudit
// does, even when the request-level flag is false.
func TestDecide_DecisionAlwaysAuditOverridesSampling(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)
	srv.authzService.DecisionAuditSampleRate = 0.0 // never sample allows

	project := &store.Project{ID: tid("decisionalwaysaudit-project"), Name: "p", Slug: "decisionalwaysaudit-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "api")
	req := AuthzRequestFromContext(contextWithIdentity(ctx, identity), Resource{Type: "project", ID: project.ID}, ActionRead)
	require.False(t, req.AlwaysAudit)

	before := len(emitter.records)
	srv.authzService.emitDecisionAudit(ctx, req, Decision{Allowed: true, AlwaysAudit: true})
	require.Equal(t, before+1, len(emitter.records), "Decision.AlwaysAudit must override allow-sampling on its own")
}

// TestBuildDecisionAuditRecord_MatchesEmittedShape proves the exported
// builder produces the same field mapping Decide's own emit path uses, so a
// non-Decide caller (e.g. G's aggregated list-filter record) gets
// schema-consistent records.
func TestBuildDecisionAuditRecord_MatchesEmittedShape(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	project := &store.Project{ID: tid("builder-project"), Name: "p", Slug: "builder-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "api")

	req := AuthzRequestFromContext(contextWithIdentity(ctx, identity), Resource{Type: "project", ID: project.ID}, ActionRead)
	decision := srv.authzService.Decide(ctx, req)
	require.True(t, decision.Allowed)

	emitted := emitter.records[len(emitter.records)-1]
	built := BuildDecisionAuditRecord(ctx, req, decision)
	require.Equal(t, emitted.PrincipalID, built.PrincipalID)
	require.Equal(t, emitted.PermissionID, built.PermissionID)
	require.Equal(t, emitted.ResourceType, built.ResourceType)
	require.Equal(t, emitted.Result, built.Result)
}

// TestBuildDecisionAuditRecord_UndecoratedFallsBackToSuppliedPrincipalID
// proves the request.Principal.ID fallback fires only for a Decision built
// without decorateDecision (e.g. a non-Decide caller of this function, such
// as G's aggregated list-filter record) — not whenever decision.PrincipalID
// happens to be empty.
func TestBuildDecisionAuditRecord_UndecoratedFallsBackToSuppliedPrincipalID(t *testing.T) {
	req := AuthzRequest{
		Principal: PrincipalContext{ID: "supplied-claim"},
		Resource:  Resource{Type: "project", ID: tid("undecorated-project")},
		Action:    ActionRead,
	}
	decision := Decision{Allowed: true}
	record := BuildDecisionAuditRecord(context.Background(), req, decision)
	require.Equal(t, "supplied-claim", record.PrincipalID)
}
