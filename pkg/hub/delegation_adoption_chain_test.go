// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provenanceAdopter is the boot adoption entry point of the ent store.
type provenanceAdopter interface {
	AdoptLegacyDelegationProvenance(ctx context.Context) error
}

// runBootAdoption runs the boot adoption migration over the store's current
// rows, as on the first boot of a hub whose database holds unrecorded edges.
// Test servers run Migrate on an empty database first, which writes an empty
// snapshot and the marker; both are cleared so the snapshot covers the rows
// the test seeded.
func runBootAdoption(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, section := range []string{delegationadoption.MarkerSection, delegationadoption.CohortSection} {
		if err := s.DeleteHubSetting(ctx, section); err != nil && !errors.Is(err, store.ErrNotFound) {
			require.NoError(t, err)
		}
	}
	a, ok := s.(provenanceAdopter)
	require.True(t, ok, "store %T has no boot adoption", s)
	require.NoError(t, a.AdoptLegacyDelegationProvenance(ctx))
}

// adoptOnly writes the planned adoption of agentID's hop alone, skipping the
// top-down check, to build partially adopted chains.
func adoptOnly(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
		plan, err := delegationadoption.Build(ctx, tx, delegationadoption.Scope{AgentIDs: []string{agentID}})
		if err != nil {
			return err
		}
		h := plan.Hop(agentID)
		require.NotNil(t, h)
		require.Equal(t, delegationadoption.OutcomeAdopt, h.Outcome, "reason %q", h.Reason)
		res, err := delegationadoption.ApplyPlannedAdopt(ctx, tx, h, uuid.NewString(), delegationadoption.Actor{})
		if err != nil {
			return err
		}
		require.Equal(t, store.DelegationAdoptionAdopted, res.Status, "reason %q", res.Reason)
		return nil
	}))
}

// seedLegacyAgent stores an agent under parent (nil: under the fixture
// owner) with an unrecorded edge of role.
func (f *legacyFixture) seedLegacyAgent(t *testing.T, name string, parent *store.Agent, role AgentRole) *store.Agent {
	t.Helper()
	ancestry := []string{f.owner.ID}
	delegatorType, delegatorID := store.DelegationPrincipalUser, f.owner.ID
	if parent != nil {
		ancestry = append(append([]string{}, parent.Ancestry...), parent.ID)
		delegatorType, delegatorID = store.DelegationPrincipalAgent, parent.ID
	}
	a := f.storeAgent(t, name, ancestry, role)
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: a.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, Role: string(role), Active: true,
	}))
	return a
}

// storeAgent stores a live agent in the fixture project with ancestry
// (root user first) and applied role.
func (f *legacyFixture) storeAgent(t *testing.T, name string, ancestry []string, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(name), Slug: tid(name), Name: name, ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: ancestry[0], OwnerID: ancestry[0], Ancestry: ancestry,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

func (f *legacyFixture) defaultAssignSA(t *testing.T) {
	t.Helper()
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentityMode, store.GCPMetadataModeAssign)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentitySAID, f.sa.ID)
}

// withAssignedSA gives a stored agent the fixture service account in assign
// mode, as legacy agents created under a project-default service account
// carry. The GCP actAs layer accepts an assignment of the caller's own
// account.
func (f *legacyFixture) withAssignedSA(t *testing.T, a *store.Agent) {
	t.Helper()
	ctx := context.Background()
	stored, err := f.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	stored.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign,
		ServiceAccountID: f.sa.ID, ServiceAccountEmail: f.sa.Email, ProjectID: f.sa.ProjectID}
	require.NoError(t, f.store.UpdateAgent(ctx, stored))
}

func compatIDs(t *testing.T, role AgentRole, sa bool) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), sa)
	require.True(t, ok)
	return ids
}

func requireCreated(t *testing.T, code int, body string) {
	t.Helper()
	require.Contains(t, []int{http.StatusCreated, http.StatusOK, http.StatusAccepted}, code, body)
}

// The acceptance case: a legacy agent on a hub with a project-default
// assign-mode service account is denied, the boot adoption runs, and the
// same token (minted before the adoption) creates directly.
func TestAdoptedLegacyAgentCreatesWithDefaultSAUsingExistingToken(t *testing.T) {
	f := newLegacyFixture(t, "adopt-dsa")
	f.defaultAssignSA(t)
	f.withAssignedSA(t, f.legacy)
	token := f.agentToken(t, f.legacy.ID)
	assertSAGateUnrecordedDenied(t, f.createAsParent(t, token, CreateAgentRequest{Name: "adopt-dsa-pre"}))

	// The edge backfill marker of an upgraded hub is present.
	markEdgeBackfillComplete(t, f.store)
	runBootAdoption(t, f.store)
	adopted := activeEdgesFor(t, f.store, f.legacy.ID)
	require.Len(t, adopted, 1)
	assert.Equal(t, store.SourceCredentialSystemMigration, adopted[0].SourceCredentialKind)
	assert.Equal(t, compatIDs(t, AgentRoleFull, false), adopted[0].PermissionIDs)

	child, childEdge := f.createdAgent(t, f.createAsParent(t, token, CreateAgentRequest{Name: "adopt-dsa-c"}), "adopt-dsa-c")
	require.NotNil(t, child.AppliedConfig.GCPIdentity)
	assert.Equal(t, f.sa.ID, child.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, store.EffectCeilingBounded, childEdge.Kind)
	assert.Equal(t, store.ProvenanceVersionV1, childEdge.ProvenanceVersion)
	assert.Equal(t, f.legacy.ID, childEdge.DelegatorID)
}

// The child keeps its assigned service account and mints a GCP access token
// for it (mocked token endpoint).
func TestAdoptedChildRetainsAssignedSAAndMintsGCPToken(t *testing.T) {
	f := newLegacyFixture(t, "adopt-mint")
	f.defaultAssignSA(t)
	f.withAssignedSA(t, f.legacy)
	token := f.agentToken(t, f.legacy.ID)
	runBootAdoption(t, f.store)
	child, _ := f.createdAgent(t, f.createAsParent(t, token, CreateAgentRequest{Name: "adopt-mint-c"}), "adopt-mint-c")
	require.Equal(t, f.sa.ID, child.AppliedConfig.GCPIdentity.ServiceAccountID)

	ctx := context.Background()
	f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.example"})
	childToken, err := f.srv.issueAgentTokenForTest(ctx, child)
	require.NoError(t, err)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(childToken)
	require.NoError(t, err)
	require.Contains(t, claims.Scopes, GCPTokenScopeForSA(f.sa.ID))
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agent/gcp-token", nil, childToken)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFullyAdoptedChainAllowsAssign(t *testing.T) {
	f := newLegacyFixture(t, "adopt-full")
	c := f.seedLegacyAgent(t, "adopt-full-c", f.legacy, AgentRoleFull)
	f.withAssignedSA(t, c)
	token := f.agentToken(t, c.ID)
	assertSAGateUnrecordedDenied(t, f.createAsParent(t, token, f.assignBody("adopt-full-pre")))

	runBootAdoption(t, f.store)
	rec := f.createAsParent(t, token, f.assignBody("adopt-full-gc"))
	requireCreated(t, rec.Code, rec.Body.String())
	gc, _ := f.createdAgent(t, rec, "adopt-full-gc")
	assert.Equal(t, f.sa.ID, gc.AppliedConfig.GCPIdentity.ServiceAccountID)
}

func TestPartiallyAdoptedChainDeniesAssign(t *testing.T) {
	// Only the upper hop adopted: the lower hop is unrecorded.
	f := newLegacyFixture(t, "adopt-part-a")
	c := f.seedLegacyAgent(t, "adopt-part-a-c", f.legacy, AgentRoleFull)
	f.withAssignedSA(t, c)
	adoptOnly(t, f.store, f.legacy.ID)
	assertSAGateUnrecordedDenied(t, f.createAsParent(t, f.agentToken(t, c.ID), f.assignBody("adopt-part-a-gc")))
	f.assertGateUnrecorded(t, f.agentToken(t, c.ID), SurfaceAgentCreate)

	// Only the lower hop adopted: the walk denies at the unrecorded upper hop.
	g := newLegacyFixture(t, "adopt-part-b")
	d := g.seedLegacyAgent(t, "adopt-part-b-c", g.legacy, AgentRoleFull)
	g.withAssignedSA(t, d)
	adoptOnly(t, g.store, d.ID)
	assertSAGateUnrecordedDenied(t, g.createAsParent(t, g.agentToken(t, d.ID), g.assignBody("adopt-part-b-gc")))
	g.assertGateUnrecorded(t, g.agentToken(t, d.ID), SurfaceAgentCreate)
}

func TestAdoptedParentChildCeilingIsSubsetOfAncestors(t *testing.T) {
	f := newLegacyFixture(t, "adopt-subset")
	c := f.seedLegacyAgent(t, "adopt-subset-c", f.legacy, AgentRoleBaseline)
	runBootAdoption(t, f.store)
	lEdge := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	cEdge := activeEdgesFor(t, f.store, c.ID)[0]
	assert.Subset(t, lEdge.PermissionIDs, cEdge.PermissionIDs)

	_, childEdge := f.childOf(t, f.legacy, "adopt-subset-x")
	assert.Subset(t, lEdge.PermissionIDs, childEdge.PermissionIDs, "a new child of an adopted parent stays within it")
	assert.Equal(t, hubDeliveryPermissionList, deliverOf(childEdge.PermissionIDs), "the adopted chain passes delivery eligibility")
}

// Parity: the adopted full ceiling is what a session-created full parent
// gives a new child, plus gcp_service_account.use, minus the permissions no
// compatibility policy V1 ceiling carries. Each permission left out must be
// one unrecorded chains never held (legacyChainExcludedPermissions).
func TestAdoptedEdgeMatchesNewChildParity(t *testing.T) {
	f := newLegacyFixture(t, "adopt-parity")
	runBootAdoption(t, f.store)
	adopted := activeEdgesFor(t, f.store, f.legacy.ID)[0]

	parent, _ := f.createdAgent(t, doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.path,
		CreateAgentRequest{Name: "adopt-parity-p"}), "adopt-parity-p")
	_, childEdge := f.childOf(t, parent, "adopt-parity-c")
	var withoutUse []string
	for _, id := range adopted.PermissionIDs {
		if id != "gcp_service_account.use" {
			withoutUse = append(withoutUse, id)
		}
	}
	var childAtV1 []string
	for _, id := range childEdge.PermissionIDs {
		if adoptionCeilingCovers(id) {
			childAtV1 = append(childAtV1, id)
			continue
		}
		assert.True(t, legacyChainExcludedPermissions[id], "%s is left out of the adopted ceiling but held by unrecorded chains", id)
	}
	assert.Equal(t, childAtV1, withoutUse)
	assert.True(t, roleFitsCeiling(adopted.EffectCeiling, AgentRoleFull))
}

func TestAdoptedBaselineAndReadonlyAgentsStayLimited(t *testing.T) {
	f := newLegacyFixture(t, "adopt-low")
	b := f.seedLegacyAgent(t, "adopt-low-b", nil, AgentRoleBaseline)
	r := f.seedLegacyAgent(t, "adopt-low-r", nil, AgentRoleReadOnly)
	runBootAdoption(t, f.store)
	assert.Equal(t, compatIDs(t, AgentRoleBaseline, false), activeEdgesFor(t, f.store, b.ID)[0].PermissionIDs)
	assert.Equal(t, compatIDs(t, AgentRoleReadOnly, false), activeEdgesFor(t, f.store, r.ID)[0].PermissionIDs)
	for _, a := range []*store.Agent{b, r} {
		token := f.agentToken(t, a.ID)
		rec := f.createAsParent(t, token, CreateAgentRequest{Name: a.Name + "-c"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		identity := f.agentIdentityFor(t, token)
		ctx := contextWithIdentity(context.Background(), identity)
		assert.False(t, f.srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(f.sa), ActionAssign).Allowed)
	}
}

// A recorded token-bounded ancestor bounds the adopted descendant.
func TestAdoptedChainKeepsRecordedTokenLimits(t *testing.T) {
	f := newLegacyFixture(t, "adopt-uat")
	ctx := context.Background()
	p := f.storeAgent(t, "adopt-uat-p", []string{f.owner.ID}, AgentRoleFull)
	limit := []string{"agent.create", "agent.notify", "agent.status_update", "agent.token_refresh", "project.read", "template.read"}
	require.NoError(t, f.store.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.owner.ID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: p.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID, Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: "user",
			SourcePrincipalID: f.owner.ID, SourceCredentialKind: store.SourceCredentialUAT, SourceCredentialID: "uat-1"},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1,
			PermissionIDs: limit, BoundaryKind: string(permissions.BoundaryKindProject), BoundaryProjectID: f.proj.ID},
	}))
	c := f.seedLegacyAgent(t, "adopt-uat-c", p, AgentRoleFull)
	runBootAdoption(t, f.store)

	pEdge := activeEdgesFor(t, f.store, p.ID)[0]
	assert.Equal(t, store.SourceCredentialUAT, pEdge.SourceCredentialKind, "a recorded hop is not rewritten")
	cEdge := activeEdgesFor(t, f.store, c.ID)[0]
	assert.Equal(t, store.SourceCredentialSystemMigration, cEdge.SourceCredentialKind)
	assert.Subset(t, limit, cEdge.PermissionIDs)
	assert.NotContains(t, cEdge.PermissionIDs, "gcp_service_account.assign")
	f.withAssignedSA(t, c)
	token := f.agentToken(t, c.ID)
	rec := f.createAsParent(t, token, f.assignBody("adopt-uat-gc"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	identity := f.agentIdentityFor(t, token)
	ictx := contextWithIdentity(ctx, identity)
	assert.False(t, f.srv.authzService.CheckAccess(ictx, identity, gcpServiceAccountResource(f.sa), ActionAssign).Allowed)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	assert.NotContains(t, claims.Scopes, ScopeAgentSAAssign, "the bounded chain mints no assign scope")
}

func TestAdoptedEdgeDeniesOtherProject(t *testing.T) {
	f := newLegacyFixture(t, "adopt-proj")
	ctx := context.Background()
	runBootAdoption(t, f.store)
	e := activeEdgesFor(t, f.store, f.legacy.ID)[0]
	assert.Equal(t, f.proj.ID, e.BoundaryProjectID)

	other := &store.Project{ID: uuid.NewString(), Name: "adopt-proj-2", Slug: "adopt-proj-2", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, f.store.CreateProject(ctx, other))
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/projects/"+other.ID+"/agents",
		CreateAgentRequest{Name: "adopt-proj-x"}, f.agentToken(t, f.legacy.ID))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var cause DenyCause
	allowed, _, err := f.srv.authzService.walkDelegationChainWithCause(ctx, Resource{Type: "agent", ParentType: "project", ParentID: other.ID},
		ActionCreate, "agent.create", f.legacy.ID, false, store.RoleScopeProject, other.ID, nil, &cause)
	require.NoError(t, err)
	assert.False(t, allowed, "the adopted edge supplies nothing in another project")
}

func TestRevokedAdoptedAncestorDeniesDescendant(t *testing.T) {
	f := newLegacyFixture(t, "adopt-revoke")
	c := f.seedLegacyAgent(t, "adopt-revoke-c", f.legacy, AgentRoleFull)
	f.withAssignedSA(t, c)
	token := f.agentToken(t, c.ID)
	runBootAdoption(t, f.store)
	first := f.createAsParent(t, token, f.assignBody("adopt-revoke-1"))
	requireCreated(t, first.Code, first.Body.String())

	revokeDelegateEdges(t, f.store, f.legacy.ID)
	assert.Equal(t, http.StatusForbidden, f.createAsParent(t, token, f.assignBody("adopt-revoke-2")).Code)

	ctx := context.Background()

	// Deleting the ancestor denies the same way.
	g := newLegacyFixture(t, "adopt-revoke-del")
	d := g.seedLegacyAgent(t, "adopt-revoke-del-c", g.legacy, AgentRoleFull)
	g.withAssignedSA(t, d)
	gToken := g.agentToken(t, d.ID)
	runBootAdoption(t, g.store)
	legacy, err := g.store.GetAgent(ctx, g.legacy.ID)
	require.NoError(t, err)
	legacy.DeletedAt = time.Now()
	require.NoError(t, g.store.UpdateAgent(ctx, legacy))
	assert.Equal(t, http.StatusForbidden, g.createAsParent(t, gToken, g.assignBody("adopt-revoke-del-x")).Code)
}

func TestAdoptedRootUserSuspensionDenies(t *testing.T) {
	f := newLegacyFixture(t, "adopt-susp")
	token := f.agentToken(t, f.legacy.ID)
	runBootAdoption(t, f.store)
	identity := f.agentIdentityFor(t, token)
	ctx := contextWithIdentity(context.Background(), identity)
	check := func() bool {
		return f.srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(f.sa), ActionAssign).Allowed
	}
	require.True(t, check())

	owner, err := f.store.GetUser(context.Background(), f.owner.ID)
	require.NoError(t, err)
	owner.Status = "suspended"
	require.NoError(t, f.store.UpdateUser(context.Background(), owner))
	assert.False(t, check(), "suspending the root denies")

	owner.Status = store.UserStatusActive
	require.NoError(t, f.store.UpdateUser(context.Background(), owner))
	assert.True(t, check(), "restoring the root restores access (live check)")
}

// launchFixture wires a secret backend with a progeny secret of the owner,
// and reports whether the last launched child received it.
func (f *legacyFixture) progenyLaunch(t *testing.T) func(child *store.Agent) bool {
	t.Helper()
	ctx := context.Background()
	backend := secret.NewLocalBackend(f.store, "test-hub-id", "test-secret")
	f.srv.SetSecretBackend(backend)
	disp := f.srv.GetDispatcher().(*HTTPAgentDispatcher)
	disp.SetSecretBackend(backend)
	disp.SetAuthzService(f.srv.authzService)
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name: "PROGENY_KEY", Value: "progeny-value", SecretType: store.SecretTypeEnvironment, Target: "PROGENY_KEY",
		Scope: store.ScopeUser, ScopeID: f.owner.ID, AllowProgeny: true, InjectionMode: store.InjectionModeAlways,
		CreatedBy: f.owner.ID, UpdatedBy: f.owner.ID,
	})
	require.NoError(t, err)
	return func(child *store.Agent) bool {
		req := f.client.lastCreateReq
		require.NotNil(t, req)
		require.Equal(t, child.ID, req.ID)
		if _, ok := req.ResolvedEnv["PROGENY_KEY"]; ok {
			return true
		}
		for _, s := range req.ResolvedSecrets {
			if s.Name == "PROGENY_KEY" {
				return true
			}
		}
		return false
	}
}

func TestAdoptedChainDeliversMaterialToChild(t *testing.T) {
	f := newLegacyFixture(t, "adopt-deliver")
	launched := f.progenyLaunch(t)
	before, _ := f.childOf(t, f.legacy, "adopt-deliver-pre")
	require.False(t, launched(before))

	runBootAdoption(t, f.store)
	child, _ := f.childOf(t, f.legacy, "adopt-deliver-c")
	assert.True(t, launched(child), "an adopted chain launches with the progeny secret")
}

func TestPartiallyAdoptedChainDeliversNoMaterial(t *testing.T) {
	f := newLegacyFixture(t, "adopt-nodeliver")
	c := f.seedLegacyAgent(t, "adopt-nodeliver-c", f.legacy, AgentRoleFull)
	launched := f.progenyLaunch(t)
	adoptOnly(t, f.store, f.legacy.ID)
	child, _ := f.childOf(t, c, "adopt-nodeliver-gc")
	assert.False(t, launched(child), "an unrecorded hop leaves delivery eligibility empty")
}

func TestAdoptedAgentRuntimeSecretFetch(t *testing.T) {
	mf := newMaterialFixture(t, "adopt-fetch")
	ctx := context.Background()
	seedSecret(t, mf.Server.secretBackend, "ADOPT_KEY", "adopt-value", store.SecretTypeEnvironment, "ADOPT_KEY", mf.ProjectID)
	id := tid("agent-adopt-fetch-legacy")
	createDCAgent(t, mf.Store, id, mf.ProjectID, mf.UserID, AgentRoleFull)
	createDCEdge(t, mf.Store, store.DelegationPrincipalUser, mf.UserID, store.DelegationPrincipalAgent, id,
		store.RoleScopeProject, mf.ProjectID, string(AgentRoleFull))
	a, err := mf.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	tok, err := mf.Server.issueAgentTokenForTest(ctx, a)
	require.NoError(t, err)
	assertProjectDenied(t, mf, id, tok, "ADOPT_KEY")

	runBootAdoption(t, mf.Store)
	tok, err = mf.Server.issueAgentTokenForTest(ctx, a)
	require.NoError(t, err)
	rec := doRequestWithAgentToken(t, mf.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"ADOPT_KEY"}}, tok)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "adopt-value")
}

// The automatic cohort excludes scheduled-dispatch rows because scheduled
// dispatch writes every child with applied role none (and NoAuth) and an edge
// of role none. This pins that invariant: if the scheduled-create path ever
// wrote a real role, the role_none exclusion would not cover it.
func TestScheduledDispatchChildEdgeHasRoleNone(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)
	ctx := context.Background()
	require.NoError(t, srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
		ID:        tid("scheduled-dispatch-edge-role"),
		ProjectID: project.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"scheduled-edge-child","task":"scheduled work"}`,
		CreatedBy: user.ID,
		FireAt:    time.Now(),
	}))
	child, err := s.GetAgentBySlug(ctx, project.ID, "scheduled-edge-child")
	require.NoError(t, err)
	require.NotNil(t, child.AppliedConfig)
	assert.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
	assert.True(t, child.AppliedConfig.NoAuth)

	edges := activeEdgesFor(t, s, child.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, string(AgentRoleNone), edges[0].Role)
	assert.Equal(t, 0, edges[0].ProvenanceVersion)

	plan, err := delegationadoption.Build(ctx, s, delegationadoption.Scope{AgentIDs: []string{child.ID}})
	require.NoError(t, err)
	h := plan.Hop(child.ID)
	require.NotNil(t, h)
	assert.Equal(t, delegationadoption.OutcomeExcluded, h.Outcome)
	assert.Equal(t, delegationadoption.ReasonRoleNone, h.Reason)
}
