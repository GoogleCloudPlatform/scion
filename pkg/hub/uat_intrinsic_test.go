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

// The intrinsic set of token-recorded hops (uatIntrinsicPermissionIDs,
// uatHopEffectCeiling): an agent whose delegation edge was recorded from a
// user access token gets secret.deliver, env_var.deliver,
// skill_injection.deliver and secret.use on that hop, as a session-created
// agent does; the token's list still narrows every other permission.
//
// Ported from miller79/scion#199 (its test matrix, without the compiled
// cutoff): TestUATIntrinsic_DeliversOwnerSharedSecrets,
// TestUATIntrinsic_NoCutoff and TestUATIntrinsic_OtherClausesStillApply.
// The start-time user-secret ceiling composition cases are in
// start_delivery_user_secret_ceiling_test.go.

import (
	"context"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// uiTokenList is a token's frozen list as an agent edge records it: API
// actions only, none of the intrinsic permissions.
var uiTokenList = []string{"agent.create", "agent.read", "project.read"}

// uiProv is the provenance of an edge recorded from user userID's access
// token.
func uiProv(userID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
		SourcePrincipalID: userID, SourceCredentialKind: store.SourceCredentialUAT, SourceCredentialID: "uat-" + userID,
	}
}

// uiUATEdge makes a recorded edge the one a create from the delegator's
// access token writes: bounded, the token's list, source uat. The ceiling
// version is kept, so a test can pass a legacy (Unspecified) list.
func uiUATEdge(ids ...string) func(e *store.DelegationEdge) {
	return func(e *store.DelegationEdge) {
		e.EffectCeiling = boundedCeiling(ids...)
		e.SourceCredentialKind = store.SourceCredentialUAT
		e.SourceCredentialID = "uat-" + e.DelegatorID
	}
}

// ---- the helper itself ---------------------------------------------------------

// TestUATIntrinsic_HopCeiling pins uatRecordedHop and uatHopEffectCeiling:
// the set is added for a token-recorded hop, for both stored ceiling
// versions, the stored ceiling is never changed, and a ceiling version
// Allows does not interpret still denies everything.
func TestUATIntrinsic_HopCeiling(t *testing.T) {
	owner := uuid.NewString()
	prov := uiProv(owner)

	for _, v := range []permissions.CeilingVersion{permissions.CeilingVersionUnspecified, permissions.CeilingVersionV1} {
		stored := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: v, PermissionIDs: []string{"project.read", "agent.create"}}
		require.True(t, uatRecordedHop(store.DelegationPrincipalUser, owner, prov, stored))
		hop := uatHopEffectCeiling(store.DelegationPrincipalUser, owner, prov, stored)
		for _, p := range uatIntrinsicPermissionIDs {
			assert.False(t, EffectCeilingAllows(stored, p, false), "version %d: the stored list lacks %s", v, p)
			assert.True(t, EffectCeilingAllows(hop, p, false), "version %d: %s is intrinsic", v, p)
			assert.True(t, uatIntrinsicDecides(store.DelegationPrincipalUser, owner, prov, stored, p), "version %d: %s", v, p)
		}
		for _, p := range []string{"project.read", "agent.create"} {
			assert.True(t, EffectCeilingAllows(hop, p, false), "the token's own list still applies")
			assert.False(t, uatIntrinsicDecides(store.DelegationPrincipalUser, owner, prov, stored, p), "%s is the token's, not the rule's", p)
		}
		assert.Equal(t, v, hop.Version, "the stored version is kept")
		assert.Equal(t, []string{"project.read", "agent.create"}, stored.PermissionIDs, "the stored ceiling is never changed")
	}

	unknown := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersion(99), PermissionIDs: []string{"project.read"}}
	for _, p := range append([]string{"project.read"}, uatIntrinsicPermissionIDs...) {
		assert.False(t, EffectCeilingAllows(uatHopEffectCeiling(store.DelegationPrincipalUser, owner, prov, unknown), p, false), "an uninterpreted version still denies %s", p)
	}

	// An inconsistent edge: the delegator is not the token's user (another
	// user, or an agent). Writers never record it; it gets no intrinsic
	// permission, so it is refused as on main.
	consistent := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"project.read"}}
	for name, d := range map[string][2]string{
		"another user": {store.DelegationPrincipalUser, uuid.NewString()},
		"an agent":     {store.DelegationPrincipalAgent, owner},
	} {
		assert.False(t, uatRecordedHop(d[0], d[1], prov, consistent), name)
		assert.Equal(t, consistent, uatHopEffectCeiling(d[0], d[1], prov, consistent), name)
	}

	assert.ElementsMatch(t, []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver", "secret.use"}, uatIntrinsicPermissionIDs)
	for _, p := range []string{"project.secret_read", "agent.identity_token", "gcp_service_account.use", "gcp_service_account.assign"} {
		assert.False(t, uatIntrinsicPermissionSet[p], "%s is not intrinsic", p)
	}
	for _, p := range uatIntrinsicPermissionIDs {
		assert.False(t, selfOperationSet[p], "%s stays out of the self-operation set", p)
		_, ok := registryPermission(p)
		assert.True(t, ok, "%s is a registry permission", p)
	}
}

// TestUATIntrinsic_NotOtherHops: no intrinsic permission reaches a hop for
// which uatRecordedHop is false: session, agent, scheduler (including a
// schedule written with a token: scheduler-recorded hops are not token
// hops), system_migration, dev_local, unrecorded provenance, a principal
// ceiling, or a uat source missing its user principal.
func TestUATIntrinsic_NotOtherHops(t *testing.T) {
	user := uuid.NewString()
	bounded := boundedCeiling(uiTokenList...)
	cases := map[string]struct {
		prov store.AuthorityProvenance
		c    store.EffectCeiling
	}{
		"session":          {recordedProv(store.DelegationPrincipalUser, user, store.SourceCredentialSession), bounded},
		"agent":            {recordedProv(store.DelegationPrincipalAgent, uuid.NewString(), store.SourceCredentialAgent), bounded},
		"scheduler":        {schedulerProv(store.DelegationPrincipalUser, user, store.InitiatorCredentialKindSession), bounded},
		"scheduler of uat": {schedulerProv(store.DelegationPrincipalUser, user, store.InitiatorCredentialKindUAT), bounded},
		"system_migration": {recordedProv(store.DelegationPrincipalUser, user, store.SourceCredentialSystemMigration), bounded},
		"dev_local":        {recordedProv(store.DelegationPrincipalUser, DevUserID, store.SourceCredentialDevLocal), bounded},
		"unrecorded provenance": {func() store.AuthorityProvenance {
			p := uiProv(user)
			p.ProvenanceVersion = 0
			return p
		}(), bounded},
		"principal ceiling":  {uiProv(user), store.EffectCeiling{Kind: store.EffectCeilingPrincipal}},
		"unrecorded ceiling": {uiProv(user), store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}},
		"uat source, agent principal": {func() store.AuthorityProvenance {
			p := uiProv(user)
			p.SourcePrincipalKind = store.DelegationPrincipalAgent
			return p
		}(), bounded},
		"uat source, no principal": {func() store.AuthorityProvenance {
			p := uiProv(user)
			p.SourcePrincipalID = ""
			return p
		}(), bounded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.False(t, uatRecordedHop(tc.prov.SourcePrincipalKind, tc.prov.SourcePrincipalID, tc.prov, tc.c))
			assert.Equal(t, tc.c, uatHopEffectCeiling(tc.prov.SourcePrincipalKind, tc.prov.SourcePrincipalID, tc.prov, tc.c), "the ceiling is returned unchanged")
			if tc.c.Kind == store.EffectCeilingBounded {
				for _, p := range uatIntrinsicPermissionIDs {
					assert.False(t, EffectCeilingAllows(uatHopEffectCeiling(tc.prov.SourcePrincipalKind, tc.prov.SourcePrincipalID, tc.prov, tc.c), p, false), p)
					edge := &store.DelegationEdge{DelegatorType: store.DelegationPrincipalUser, DelegatorID: tc.prov.SourcePrincipalID,
						AuthorityProvenance: tc.prov, EffectCeiling: tc.c}
					cause, _ := hopEffectCeilingDeny(edge, p, Resource{Type: "secret"}, "", true)
					assert.NotEmpty(t, cause, "%s: the generic walk still denies", p)
				}
			}
		})
	}
}

// TestUATIntrinsic_RegistryTableNotWidened: for every registry permission
// outside the set and outside the token's list, a token-recorded hop still
// denies, in the predicate, the generic walk's hop check and the chain fold;
// every permission in the set is allowed on it.
func TestUATIntrinsic_RegistryTableNotWidened(t *testing.T) {
	user := uuid.NewString()
	prov := uiProv(user)
	stored := boundedCeiling(uiTokenList...)
	edge := &store.DelegationEdge{DelegatorType: store.DelegationPrincipalUser, DelegatorID: user,
		AuthorityProvenance: prov, EffectCeiling: stored}
	fold := foldCeilings([]permissions.FrozenPermissionCeiling{func() permissions.FrozenPermissionCeiling {
		f, _ := uatHopEffectCeiling(store.DelegationPrincipalUser, user, prov, stored).Frozen()
		return f
	}()}, 0)
	checked := 0
	for _, perm := range permissions.Registry {
		inSet := uatIntrinsicPermissionSet[perm.ID]
		inList := slices.Contains(uiTokenList, perm.ID)
		hop := uatHopEffectCeiling(store.DelegationPrincipalUser, user, prov, stored)
		cause, _ := hopEffectCeilingDeny(edge, perm.ID, Resource{Type: "project"}, "", true)
		if inSet || inList {
			assert.True(t, EffectCeilingAllows(hop, perm.ID, false), perm.ID)
			assert.Empty(t, cause, perm.ID)
			assert.Contains(t, fold.PermissionIDs, perm.ID)
			continue
		}
		checked++
		assert.False(t, EffectCeilingAllows(hop, perm.ID, false), "%s is not widened", perm.ID)
		assert.Equal(t, DenyCauseCeilingEffectExceeded, cause, "%s is not widened in the generic walk", perm.ID)
		assert.NotContains(t, fold.PermissionIDs, perm.ID, "%s is not widened in the fold", perm.ID)
	}
	require.Greater(t, checked, 20, "the table covers the registry")
}

// TestUATIntrinsic_CeilingReadersUseHopCeiling pins the reader sites: in
// the package sources, EffectCeilingAllows and childRoleWithinCeiling are
// called on an edge, hop, assignment or source ceiling only through
// uatHopEffectCeiling, and so is every .Frozen() fold of a ceiling. The
// listed exceptions read a fold (already built from hop ceilings), a
// scheduler ceiling (scheduler-recorded hops are not token hops), an
// agent-parent ceiling, or are the helpers themselves.
func TestUATIntrinsic_CeilingReadersUseHopCeiling(t *testing.T) {
	allowed := map[string]map[string]bool{
		"EffectCeilingAllows": {
			"uatIntrinsicDecides":           true, // compares the stored and the hop ceiling
			"deliverIDsAllowedByChain":      true, // a chain fold
			"authorizeScheduledMessageFire": true, // scheduler ceiling: not a token hop
		},
		"childRoleWithinCeiling": {
			"planAgentDelegatorReissue": true, // an agent parent's ceiling (agentRowEffectCeiling)
			"dispatchAgentEventHandler": true, // scheduler edge ceiling with role none: not a token hop
		},
	}
	// Functions whose .Frozen() receiver is a parameter (its callers pass a
	// hop ceiling or a fold), a fold, or a scheduler or agent-row ceiling.
	frozenAllowed := map[string]bool{
		"EffectCeilingAllows":     true, // c: the callers pass uatHopEffectCeiling or a fold
		"ceilingAllowsScope":      true, // c: a chain fold
		"childEffectCeiling":      true, // source: agentRowEffectCeiling passes uatHopEffectCeiling
		"foldWithChain":           true, // chain.Ceiling is a fold; the edge is wrapped
		"intersectEffectCeilings": true, // scheduler revision and agent-row ceilings: not token hops
	}
	_, files := parseHubProduction(t)
	seen := map[string]int{}
	var bad []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Frozen" && len(call.Args) == 0 {
					seen["Frozen"]++
					if inner, ok := sel.X.(*ast.CallExpr); ok {
						if innerID, ok := inner.Fun.(*ast.Ident); ok && innerID.Name == "uatHopEffectCeiling" {
							return true
						}
					}
					if !frozenAllowed[fn.Name.Name] {
						bad = append(bad, "Frozen in "+fn.Name.Name)
					}
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || allowed[id.Name] == nil {
					return true
				}
				seen[id.Name]++
				if inner, ok := call.Args[0].(*ast.CallExpr); ok {
					if innerID, ok := inner.Fun.(*ast.Ident); ok && innerID.Name == "uatHopEffectCeiling" {
						return true
					}
				}
				if !allowed[id.Name][fn.Name.Name] {
					bad = append(bad, id.Name+" in "+fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	assert.Empty(t, bad, "these calls read a hop ceiling without uatHopEffectCeiling; wrap them or list the exception with its reason")
	assert.GreaterOrEqual(t, seen["EffectCeilingAllows"], 8, "control: the scan finds the call sites")
	assert.GreaterOrEqual(t, seen["childRoleWithinCeiling"], 5, "control: the scan finds the call sites")
	assert.GreaterOrEqual(t, seen["Frozen"], 8, "control: the scan finds the .Frozen() folds")
}

// ---- owner shared secrets (ported from miller79/scion#199) --------------------

// uiDirect is the owner's direct agent with the edge a create from the
// owner's access token records.
func (f *sdFixture) uiDirect(t *testing.T, name string, mutate func(*store.DelegationEdge)) (string, string) {
	t.Helper()
	id, token := f.direct(t, name, false)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling(uiTokenList...), func(e *store.DelegationEdge) {
		uiUATEdge(uiTokenList...)(e)
		if mutate != nil {
			mutate(e)
		}
	})
	edges := activeEdgesFor(t, f.Store, id)
	require.Len(t, edges, 1)
	require.False(t, EffectCeilingAllows(edges[0].EffectCeiling, secretUsePermission, false), "the stored token list lacks secret.use")
	return id, token
}

// TestUATIntrinsic_DeliversOwnerSharedSecrets (was
// TestUATLegacyFallback_PreCutoffDeliversOwnerSecrets): an agent created
// from the owner's access token receives the owner's shared secrets at
// create, as_needed, start and restart. The decision names the intrinsic
// rule, every use is counted, nothing is logged at Warn for it, and no value
// appears in any log line.
func TestUATIntrinsic_DeliversOwnerSharedSecrets(t *testing.T) {
	f := newSDFixture(t, "ui-owner")
	id, _ := f.uiDirect(t, "owner", nil)
	log := usaCaptureDefaultLog(t)
	before := f.Server.authzService.UATIntrinsicUses()

	d := f.ceiling(t, id, sdAlwaysKey)
	require.True(t, d.Allowed, "S3c passes at the token hop; reason %q", d.Reason)
	assert.True(t, d.uatIntrinsic)
	assert.Nil(t, d.legacyFallback, "no fallback cohort: the rule is not an exception")

	ctx := context.Background()
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	meta, err := f.Server.secretBackend.GetMeta(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	var steps []DecisionStep
	ed, _ := f.Server.authzService.evaluateUserSecretParentCeiling(ctx,
		newFullAgentIdentity(id, f.ProjectID, agent.Ancestry, []AgentTokenScope{ScopeProjectSecretRead}),
		userSecretResource(meta.ID, meta.ScopeID), &steps)
	require.True(t, ed.Allowed)
	assert.True(t, slices.ContainsFunc(steps, func(s DecisionStep) bool { return s.Step == "secret_parent_ceiling_uat_intrinsic" }),
		"the explain output names the intrinsic rule: %+v", steps)

	assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, f.disp, f.Store, id))
	got := usaDispatch(t, f, id)
	assert.True(t, got.start, "start delivers the owner's shared secret")
	assert.True(t, got.restart, "restart delivers the owner's shared secret")
	assert.Greater(t, f.Server.authzService.UATIntrinsicUses(), before, "every use is counted")

	for _, out := range []string{log.String(), got.logs} {
		assert.NotContains(t, out, sdAlwaysValue, "no value in any log line")
		assert.NotContains(t, out, sdAsNeededValue, "no value in any log line")
		assert.NotContains(t, out, "withheld", "nothing is withheld")
		assert.NotContains(t, out, "legacy", "no fallback is logged for intrinsic delivery")
	}
}

// TestUATIntrinsic_NoCutoff (was TestUATLegacyFallback_PostCutoffStillRefused,
// inverted): the rule has no date. An edge recorded today, long ago, or with
// no recorded time delivers alike.
func TestUATIntrinsic_NoCutoff(t *testing.T) {
	for name, at := range map[string]time.Time{
		"today":       time.Now(),
		"before 4234": time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC),
		"next year":   time.Now().AddDate(1, 0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			f := newSDFixture(t, "ui-cutoff-"+strings.ReplaceAll(name, " ", "-"))
			id, _ := f.uiDirect(t, "cutoff", func(e *store.DelegationEdge) { e.CreatedAt = at })
			d := f.ceiling(t, id, sdAlwaysKey)
			require.True(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, f.disp, f.Store, id))
		})
	}
}

// TestUATIntrinsic_OtherClausesStillApply ports the negative cases of
// miller79/scion#199's predicate: each is refused, or unaffected, through S3c's
// other hops and clauses or the uatRecordedHop conditions.
func TestUATIntrinsic_OtherClausesStillApply(t *testing.T) {
	type out struct {
		allowed bool
		cause   DenyCause
	}
	cases := []struct {
		name  string
		build func(t *testing.T, f *sdFixture) string
		want  out
	}{
		{"depth 1: the token agent's child, its own agent edge lacking secret.use", func(t *testing.T, f *sdFixture) string {
			p, _ := f.uiDirect(t, "d1-parent", nil)
			c, _ := f.agent(t, "d1-child", []string{f.UserID, p})
			f.recordEdge(t, store.DelegationPrincipalAgent, p, c, boundedCeiling(uiTokenList...), nil)
			return c
		}, out{false, DenyCauseCeilingEffectExceeded}},
		{"agent delegator recorded with a uat source", func(t *testing.T, f *sdFixture) string {
			p, _ := f.uiDirect(t, "ad-parent", nil)
			c, _ := f.agent(t, "ad-child", []string{f.UserID, p})
			f.recordEdge(t, store.DelegationPrincipalAgent, p, c, boundedCeiling(uiTokenList...), func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialUAT
			})
			return c
		}, out{allowed: false}},
		{"delegator is not the owner", func(t *testing.T, f *sdFixture) string {
			other := tid("ui-other-delegator")
			createDCUser(t, f.Store, other, "ui-other-delegator@test.com", f.ProjectID, store.ProjectRoleOwner)
			id, _ := f.direct(t, "not-owner", false)
			f.recordEdge(t, store.DelegationPrincipalUser, other, id, boundedCeiling(), uiUATEdge(uiTokenList...))
			return id
		}, out{false, DenyCauseCeilingSourceNotAllowed}},
		{"source principal is another user", func(t *testing.T, f *sdFixture) string {
			other := tid("ui-other-source")
			createDCUser(t, f.Store, other, "ui-other-source@test.com", f.ProjectID, store.ProjectRoleOwner)
			id, _ := f.uiDirect(t, "other-source", func(e *store.DelegationEdge) { e.SourcePrincipalID = other })
			return id
		}, out{allowed: false}},
		{"session source, bounded", func(t *testing.T, f *sdFixture) string {
			id, _ := f.direct(t, "session", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling(uiTokenList...), nil)
			return id
		}, out{false, DenyCauseCeilingEffectExceeded}},
		{"adoption source (its own cohort decides)", func(t *testing.T, f *sdFixture) string {
			id, _ := f.direct(t, "adopted", false)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling(uiTokenList...), func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialSystemMigration
			})
			return id
		}, out{allowed: false}},
		{"principal ceiling (unaffected)", func(t *testing.T, f *sdFixture) string {
			id, _ := f.direct(t, "principal", true)
			return id
		}, out{allowed: true}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSDFixture(t, "ui-neg-"+string(rune('a'+i)))
			id := tc.build(t, f)
			d := f.ceiling(t, id, sdAlwaysKey)
			assert.Equal(t, tc.want.allowed, d.Allowed, "reason %q", d.Reason)
			if tc.want.cause != "" {
				assert.Equal(t, tc.want.cause, d.DenyCause, "reason %q", d.Reason)
			}
			assert.False(t, d.uatIntrinsic, "the intrinsic rule decided nothing here")
			got := sdStartDelivery(t, f.disp, f.Store, id)
			assert.Equal(t, tc.want.allowed, got.always, "start agrees with the ceiling")
		})
	}
}

// ---- per site, classifications ----------------------------------------------------------

// TestUATIntrinsic_PerSiteDispatch: the restored owner secret is delivered
// on start, resume and restart and is classified SecretFetchable in
// extras.EnvClassifications on each (GoogleCloudPlatform/scion#4338); create (resolveSecrets) and
// as_needed are covered by sdStartDelivery.
func TestUATIntrinsic_PerSiteDispatch(t *testing.T) {
	f := newSDFixture(t, "ui-sites")
	id, _ := f.uiDirect(t, "sites", nil)
	assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, f.disp, f.Store, id), "create and as_needed")

	ctx := context.Background()
	brokerID := tid("ui-sites-broker")
	require.NoError(t, f.Store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "ui-sites-broker", Slug: "ui-sites-broker", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, f.Store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.ProjectID, BrokerID: brokerID, BrokerName: "ui-sites-broker",
		LocalPath: "/home/user/projects/ui/.scion", Status: store.BrokerStatusOnline,
	}))
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	agent.RuntimeBrokerID = brokerID
	agent.AppliedConfig.HarnessConfig = "claude"
	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(f.Store, client, false, f.disp.log)
	d.SetTokenGenerator(runTokenGenerator{svc: svc})
	d.SetSecretBackend(f.Server.secretBackend)
	d.SetAuthzService(f.Server.authzService)

	has := func(rs []ResolvedSecret) bool {
		return slices.ContainsFunc(rs, func(r ResolvedSecret) bool {
			return r.Source == store.ScopeUser && r.Name == sdAlwaysKey && r.Value == sdAlwaysValue
		})
	}
	classified := func(cls map[string]api.EnvKind) bool {
		kind, ok := api.ClassifyEnvKey(cls, sdAlwaysKey)
		return ok && kind == api.EnvKindSecretFetchable
	}
	for _, resume := range []bool{false, true} {
		require.NoError(t, d.DispatchAgentStart(ctx, agent, "", resume))
		assert.True(t, has(client.lastStartResolvedSecrets), "resume=%t delivers the owner's shared secret", resume)
		assert.True(t, classified(client.lastStartExtras.EnvClassifications), "resume=%t classifies it SecretFetchable: %v", resume, client.lastStartExtras.EnvClassifications)
	}
	require.NoError(t, d.DispatchAgentRestart(ctx, agent))
	assert.True(t, has(client.lastRestartExtras.ResolvedSecrets), "restart delivers the owner's shared secret")
	assert.True(t, classified(client.lastRestartExtras.EnvClassifications), "restart classifies it SecretFetchable: %v", client.lastRestartExtras.EnvClassifications)
}

// ---- launch material -----------------------------------------------------------

// TestUATIntrinsic_LaunchMaterialStrictWalk: project, hub and broker
// material for an agent created from a token passes the STRICT walk for
// every deliver permission and every launch purpose (dispatch, restart,
// provision credential, availability probe), with no fallback cohort, no
// Warn, and the explain step naming the intrinsic rule.
func TestUATIntrinsic_LaunchMaterialStrictWalk(t *testing.T) {
	m := newLDFixture(t, "ui-launch", ldOptions{edge: uiUATEdge(uiTokenList...)})
	keys := lgItems(t, m)
	buf := lgLogger(m)
	ctx := context.Background()

	h := hdIdentity(t, m.authz, m.agent.ID)
	for _, p := range LaunchDeliveryCensusPermissions {
		o := m.authz.hubDeliveryChainAdmission(ctx, h, p, m.agent.ID, nil)
		assert.True(t, o.ok, "%s passes the strict admission: %s", p, o.reason)
		assert.True(t, o.uatIntrinsic, p)
		assert.Empty(t, o.legacyCohort, p)
		var steps []DecisionStep
		m.authz.hubDeliveryChainAdmission(ctx, h, p, m.agent.ID, &steps)
		assert.True(t, slices.ContainsFunc(steps, func(s DecisionStep) bool { return s.Step == "delegation_ceiling_delivery_uat_intrinsic" }),
			"%s: the explain output names the intrinsic rule: %+v", p, steps)
	}

	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart, PurposeProvisionCredential, PurposeAvailabilityProbe} {
		env, _, err := m.d.selectDeliveryEnv(ctx, newDeliverySelector(m.authz, m.agent, purpose), m.agent)
		require.NoError(t, err)
		for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
			k := "LG_ENV_" + strings.ToUpper(scope)
			assert.NotEmpty(t, env[k], "%s: %s delivered", purpose, k)
		}
		selected, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, purpose), m.agent)
		require.NoError(t, err)
		for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
			k := "LG_SECRET_" + strings.ToUpper(scope)
			assert.True(t, slices.ContainsFunc(selected, func(r ResolvedSecret) bool { return r.Name == k }), "%s: %s delivered", purpose, k)
		}
	}

	require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
	for _, k := range keys {
		assert.NotEmpty(t, m.client.lastRestartResolvedEnv[k], "%s delivered at restart", k)
	}
	assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), "no fallback Warn: the strict walk delivers")
	assert.NotContains(t, buf.String(), "product_gap")
}

// TestUATIntrinsic_RootNotAdmittedWithheld: the
// strict walk applies the provenance root admission, as for a
// session-created agent, so an agent created from a token by a user who is
// no longer admitted to the project is withheld its launch material. The
// withheld Warn names the cause, and no fallback serves it.
func TestUATIntrinsic_RootNotAdmittedWithheld(t *testing.T) {
	const parentName = "ui-root-token-parent"
	cases := []struct {
		name string
		opts ldOptions
		// after builds the rest of the chain; nil for a direct agent.
		after func(t *testing.T, m *ldFixture)
		// fallback is true for a chain the strict walk refuses before the
		// root admission and the fallback then refuses at it.
		fallback bool
	}{
		{name: "token", opts: ldOptions{rootMemberNone: true, edge: uiUATEdge(uiTokenList...)}},
		// The comparator: a session-created agent in the same state.
		{name: "session", opts: ldOptions{rootMemberNone: true}},
		// A child of a token-created agent, recorded before the deliver
		// permissions were intrinsic to the token hop (uat_descendant_legacy):
		// withheld like its parent.
		{
			name: "pre-existing child of a token-created agent",
			opts: ldOptions{noEdge: true, rootMemberNone: true, ancestry: func(f *goldenFixture) []string {
				return []string{f.memberNoneID, tid(parentName)}
			}},
			after: func(t *testing.T, m *ldFixture) {
				p := tid(parentName)
				hdAgent(t, m.store, p, m.agent.ProjectID, []string{m.memberNoneID})
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.memberNoneID, p, m.agent.ProjectID, uiUATEdge(uiTokenList...))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
			fallback: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newLDFixture(t, "ui-root-"+strings.ReplaceAll(tc.name, " ", "-"), tc.opts)
			if tc.after != nil {
				tc.after(t, m)
			}
			m.env(t, "UI_ROOT_ENV", "ui-root-value", store.ScopeProject, m.agent.ProjectID, false)
			buf := lgLogger(m)
			ctx := context.Background()

			h := hdIdentity(t, m.authz, m.agent.ID)
			o := m.authz.hubDeliveryChainAdmission(ctx, h, "env_var.deliver", m.agent.ID, nil)
			require.False(t, o.ok)
			if tc.fallback {
				assert.Equal(t, DenyCauseCeilingEffectExceeded, o.cause, "the strict walk refuses the child's own edge; reason %q", o.reason)
				lh := hdIdentity(t, m.authz, m.agent.ID)
				lh.legacyFallback = true
				o = m.authz.hubDeliveryAdmissionOutcomeFor(ctx, lh, "env_var.deliver", m.agent.ID, nil)
				require.False(t, o.ok, "the fallback applies the root admission; reason %q", o.reason)
				assert.Empty(t, o.legacyCohort)
				assert.Equal(t, legacyDeliveryCohortUATDescendant, o.deniedCohort)
			}
			assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, o.cause)
			assert.True(t, strings.HasPrefix(o.reason, deliveryRootNotAdmittedReason), "reason %q", o.reason)

			require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
			assert.Empty(t, m.client.lastRestartResolvedEnv["UI_ROOT_ENV"], "withheld")
			assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), "no fallback serves it")
			recs := warnRecords(t, buf.Bytes(), "launch material withheld: not authorized for delivery")
			require.NotEmpty(t, recs, "log: %s", buf.String())
			var named bool
			for _, rec := range recs {
				if rec["key"] == "UI_ROOT_ENV" {
					reason, _ := rec["reason"].(string)
					named = strings.Contains(reason, "root user not admitted to the project")
				}
			}
			assert.True(t, named, "the withheld Warn names the cause; log: %s", buf.String())

			if tc.fallback {
				rep, err := m.authz.LaunchDeliveryCensus(ctx, false)
				require.NoError(t, err)
				for _, r := range rep.Rows {
					if r.AgentID == m.agent.ID {
						assert.Equal(t, launchDeliveryCohortUATRootNotAdmitted, r.Cohort, "the census lists it")
					}
				}
			}
		})
	}
}

// TestUATIntrinsic_DescendantInactiveRootWithheld: a sub-agent of a
// token-created agent, recorded before the deliver permissions were
// intrinsic to the token hop, is withheld when the creating user is
// suspended, as its token-created parent is on the strict walk. The deny
// names the cause.
func TestUATIntrinsic_DescendantInactiveRootWithheld(t *testing.T) {
	const parentName = "ui-inactive-token-parent"
	m := newLDFixture(t, "ui-inactive-child", ldOptions{noEdge: true, ancestry: func(f *goldenFixture) []string {
		return []string{f.projectOwnerID, tid(parentName)}
	}})
	p := tid(parentName)
	hdAgent(t, m.store, p, m.agent.ProjectID, []string{m.projectOwnerID})
	hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, uiUATEdge(uiTokenList...))
	hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
	m.env(t, "UI_INACTIVE_ENV", "ui-inactive-value", store.ScopeProject, m.agent.ProjectID, false)
	ctx := context.Background()

	lh := hdIdentity(t, m.authz, m.agent.ID)
	lh.legacyFallback = true
	control := m.authz.hubDeliveryAdmissionOutcomeFor(ctx, lh, "env_var.deliver", m.agent.ID, nil)
	require.True(t, control.ok, "control: served while the creator is active; reason %q", control.reason)
	assert.Equal(t, legacyDeliveryCohortUATDescendant, control.legacyCohort)

	setUserStatus(t, m.store, m.projectOwnerID, store.UserStatusSuspended)

	parent := m.authz.hubDeliveryChainAdmission(ctx, hdIdentity(t, m.authz, p), "env_var.deliver", p, nil)
	require.False(t, parent.ok, "the token-created parent is withheld on the strict walk")

	lh = hdIdentity(t, m.authz, m.agent.ID)
	lh.legacyFallback = true
	o := m.authz.hubDeliveryAdmissionOutcomeFor(ctx, lh, "env_var.deliver", m.agent.ID, nil)
	require.False(t, o.ok, "the descendant is withheld like its parent; reason %q", o.reason)
	assert.Empty(t, o.legacyCohort)
	assert.Equal(t, legacyDeliveryCohortUATDescendant, o.deniedCohort)
	assert.Equal(t, parent.cause, o.cause, "parent and descendant agree")
	assert.Contains(t, o.reason, string(store.UserStatusSuspended), "the deny names the cause")

	buf := lgLogger(m)
	require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
	assert.Empty(t, m.client.lastRestartResolvedEnv["UI_INACTIVE_ENV"], "withheld")
	assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), "no fallback serves it")

	rep, err := m.authz.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	for _, r := range rep.Rows {
		if r.AgentID == m.agent.ID {
			v := r.Verdicts["env_var.deliver"]
			assert.False(t, v.Allowed)
			assert.Contains(t, v.Reason, string(store.UserStatusSuspended), "the census names the cause")
		}
	}
}

// TestUATIntrinsic_InconsistentEdgeGetsNoIntrinsic: a token-sourced edge
// whose delegator is not the token's user (delegator U, source principal X)
// is inconsistent data no writer records. It gets no intrinsic permission at
// S3c (withheld, as on main), in the chain fold, in the hub delivery walk
// and in the role cap.
func TestUATIntrinsic_InconsistentEdgeGetsNoIntrinsic(t *testing.T) {
	f := newSDFixture(t, "ui-inconsistent")
	other := tid("ui-inconsistent-source")
	createDCUser(t, f.Store, other, "ui-inconsistent-source@test.com", f.ProjectID, store.ProjectRoleOwner)
	id, _ := f.uiDirect(t, "inconsistent", func(e *store.DelegationEdge) { e.SourcePrincipalID = other })
	edge := activeEdgesFor(t, f.Store, id)[0]
	require.False(t, uatRecordedHop(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling))
	ctx := context.Background()

	// S3c: withheld.
	d := f.ceiling(t, id, sdAlwaysKey)
	require.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause, "reason %q", d.Reason)
	assert.False(t, sdStartDelivery(t, f.disp, f.Store, id).always)

	// The fold: no deliver permission, so a child's edge carries none.
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	deliver, err := f.Server.authzService.parentDeliverEligibility(ctx, agent)
	require.NoError(t, err)
	assert.Empty(t, deliver)

	// The hub delivery walk: the strict walk refuses, and no fallback cohort
	// serves it.
	h := hdIdentity(t, f.Server.authzService, id)
	h.legacyFallback = true
	o := f.Server.authzService.hubDeliveryAdmissionOutcomeFor(ctx, h, "env_var.deliver", id, nil)
	assert.False(t, o.ok, "reason %q", o.reason)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, o.cause)
	assert.Empty(t, o.legacyCohort)

	// The role cap: the ceiling it reads is the stored one.
	assert.Equal(t, edge.EffectCeiling, uatHopEffectCeiling(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling))
	assert.Equal(t, edge.EffectCeiling, uatHopEffectCeiling(store.DelegationPrincipalUser, f.UserID, uiProv(other), edge.EffectCeiling),
		"a requester other than the token's user gets the stored ceiling")
}

// TestUATIntrinsic_DescendantRootErrorWithheld: any provenance root error
// withholds a uat_descendant_legacy chain, as the strict walk withholds its
// token-created ancestor. Here the child's agent edge records a source kind
// that does not match its delegator (a mismatch the tolerant walk does not
// read but the root admission does).
func TestUATIntrinsic_DescendantRootErrorWithheld(t *testing.T) {
	const parentName = "ui-mismatch-token-parent"
	m := newLDFixture(t, "ui-mismatch-child", ldOptions{noEdge: true, ancestry: func(f *goldenFixture) []string {
		return []string{f.projectOwnerID, tid(parentName)}
	}})
	p := tid(parentName)
	hdAgent(t, m.store, p, m.agent.ProjectID, []string{m.projectOwnerID})
	hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, uiUATEdge(uiTokenList...))
	hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, func(e *store.DelegationEdge) {
		withBoundedCeiling("agent.read")(e)
		e.SourcePrincipalKind = store.DelegationPrincipalUser
	})
	ctx := context.Background()
	_, rootErr := m.authz.ResolveProvenanceRoot(ctx, m.agent.ID, ResolveProvenanceOptions{PermissionID: "env_var.deliver"})
	require.ErrorIs(t, rootErr, ErrProvenanceMismatch, "control: the root admission reads the mismatch")

	lh := hdIdentity(t, m.authz, m.agent.ID)
	lh.legacyFallback = true
	o := m.authz.hubDeliveryAdmissionOutcomeFor(ctx, lh, "env_var.deliver", m.agent.ID, nil)
	require.False(t, o.ok, "reason %q", o.reason)
	assert.Equal(t, legacyDeliveryCohortUATDescendant, o.deniedCohort)
	assert.Equal(t, DenyCauseCeilingOrphaned, o.cause)
	assert.True(t, strings.HasPrefix(o.reason, "delivery provenance root denied"), "the deny names the cause: %q", o.reason)
}

// ---- not unlocked ---------------------------------------------------------------

// TestUATIntrinsic_NotUnlocked: the rule does not open the runtime read or
// the full role. A token holding every selector still cannot request the
// full role, a defaulted role is still capped, the minted scopes never
// carry project:secret:read (so the runtime read and list stop at check 4),
// even after the stored role is raised to full.
func TestUATIntrinsic_NotUnlocked(t *testing.T) {
	f := newUATCreateFixture(t, "ui-locked")
	f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleFull))
	// Every per-permission selector (aliases expand to these).
	var all []string
	for _, p := range permissions.Registry {
		if p.UATScope != "" {
			all = append(all, p.UATScope)
		}
	}
	all = sortedUniqueIDs(all)
	tokenCeiling := uatCeilingFromSelectors(t, all...)
	require.NotContains(t, tokenCeiling.PermissionIDs, "project.secret_read", "no selector maps project.secret_read")

	// Explicit full: refused at the delegation ceiling.
	rec := f.create(t, f.uat(t, all...), CreateAgentRequest{Name: "ui-locked-full", AgentRole: string(AgentRoleFull)})
	assert.Contains(t, assertCeilingDenial(t, rec), `agent role "full"`)
	assertCreateWroteNothing(t, f.store, f.proj.ID, "ui-locked-full", f.creator.ID)

	// Defaulted full: capped below full.
	rec = f.create(t, f.uat(t, all...), CreateAgentRequest{Name: "ui-locked-default"})
	agent, edge := f.createdAgent(t, rec, "ui-locked-default")
	assert.Equal(t, string(AgentRoleBaseline), agent.AppliedConfig.AgentRole, "a defaulted role is still capped")
	require.True(t, uatRecordedHop(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling))
	assert.Equal(t, tokenCeiling.PermissionIDs, edge.PermissionIDs, "the stored edge is the token's list verbatim")

	// A stored role raised to full still mints without project:secret:read.
	ctx := context.Background()
	agent.AppliedConfig.AgentRole = string(AgentRoleFull)
	require.NoError(t, f.store.UpdateAgent(ctx, agent))
	f.srv.authzService.mintDevAuthOverride = false
	scopes, err := f.srv.authzService.ceilingFilteredAgentScopes(ctx, agent, f.srv.authzService.mintCandidateScopes(agent))
	require.NoError(t, err)
	assert.NotContains(t, scopes, ScopeProjectSecretRead, "check 4 still refuses the runtime read and list")
	assert.Contains(t, scopes, ScopeProjectRead, "control: the filter issues the token's scopes")
}

// TestUATIntrinsic_RuntimeReadRefused: with the token the hub actually
// mints (filtered), the runtime read and the list of the owner's shared
// secrets are refused at check 4, while start delivers.
func TestUATIntrinsic_RuntimeReadRefused(t *testing.T) {
	f := newSDFixture(t, "ui-read")
	id, _ := f.uiDirect(t, "read", nil)
	ctx := context.Background()
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	scopes, err := f.Server.authzService.ceilingFilteredAgentScopes(ctx, agent, f.Server.authzService.mintCandidateScopes(agent))
	require.NoError(t, err)
	require.NotContains(t, scopes, ScopeProjectSecretRead)
	token := mintRecordedMaterialToken(t, f.Server, f.Store, id, f.ProjectID, scopes, agent.Ancestry)

	code, _ := f.readAs(t, id, token, sdAlwaysKey)
	assert.Equal(t, http.StatusForbidden, code, "the runtime read is refused")
	listCode, _ := f.listKeys(t, id, token, "user")
	assert.Equal(t, http.StatusForbidden, listCode, "the list is refused")
	assert.True(t, sdStartDelivery(t, f.disp, f.Store, id).always, "control: start delivers")
}

// TestUATIntrinsic_ProgenyPathStillRefused: a descendant of a token-created
// agent at baseline does not reach its root's shared secrets through the
// progeny path, whose first decision is project.secret_read.
func TestUATIntrinsic_ProgenyPathStillRefused(t *testing.T) {
	f := newSDFixture(t, "ui-progeny")
	p, _ := f.uiDirect(t, "progeny-parent", nil)
	c, _ := f.agent(t, "progeny-child", []string{f.UserID, p})
	setAgentRole(t, f.Store, c, AgentRoleBaseline)
	ctx := context.Background()
	parent, err := f.Store.GetAgent(ctx, p)
	require.NoError(t, err)
	ceiling, err := f.Server.authzService.agentRowEffectCeiling(ctx, parent)
	require.NoError(t, err)
	f.recordEdge(t, store.DelegationPrincipalAgent, p, c, ceiling, func(e *store.DelegationEdge) { e.Role = string(AgentRoleBaseline) })
	assert.Equal(t, sdDelivery{}, sdStartDelivery(t, f.disp, f.Store, c), "the progeny path stays refused")
}

// ---- legacy data ----------------------------------------------------------------

// TestUATIntrinsic_LegacyData covers the stored shapes the rule meets:
// token edges of both stored ceiling versions, a scope re-issued edge, a
// --role reincarnate from a token, and agents created by a token before
// GoogleCloudPlatform/scion#2355 (adopted edges, not uat).
func TestUATIntrinsic_LegacyData(t *testing.T) {
	t.Run("pre-ceiling token (Unspecified, normalized legacy list)", func(t *testing.T) {
		f := newSDFixture(t, "ui-legacy-unspec")
		legacy := permissions.NormalizeLegacyUATScopes([]string{"project:read", "agent:create"})
		id, _ := f.uiDirect(t, "unspec", func(e *store.DelegationEdge) {
			e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionUnspecified, PermissionIDs: legacy}
		})
		require.True(t, f.ceiling(t, id, sdAlwaysKey).Allowed)
		assert.True(t, sdStartDelivery(t, f.disp, f.Store, id).always)
	})
	t.Run("V1 token", func(t *testing.T) {
		f := newSDFixture(t, "ui-legacy-v1")
		id, _ := f.uiDirect(t, "v1", nil)
		assert.True(t, sdStartDelivery(t, f.disp, f.Store, id).always)
	})
	t.Run("scope re-issued edge (new CreatedAt, same token)", func(t *testing.T) {
		f := newSDFixture(t, "ui-legacy-reissue")
		id, _ := f.uiDirect(t, "reissue", nil)
		edges := activeEdgesFor(t, f.Store, id)
		require.Len(t, edges, 1)
		now := time.Now()
		ok, err := f.Store.DeactivateDelegationEdgeGuarded(context.Background(), edges[0].ID, store.DelegationEdgeDeactivateGuard{},
			store.EdgeDeactivationScopeReissueReplaced, "ui-reissue")
		require.NoError(t, err)
		require.True(t, ok)
		f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, edges[0].EffectCeiling, func(e *store.DelegationEdge) {
			e.AuthorityProvenance = edges[0].AuthorityProvenance
			e.CreatedAt = now.Add(time.Minute)
		})
		assert.True(t, sdStartDelivery(t, f.disp, f.Store, id).always)
	})
	t.Run("role reincarnate from a token: the role cap reads the hop ceiling", func(t *testing.T) {
		prov := uiProv(uuid.NewString())
		stored := boundedCeiling(uatCeilingFromSelectors(t, minimalSelectors(t)...).PermissionIDs...)
		hop := uatHopEffectCeiling(prov.SourcePrincipalKind, prov.SourcePrincipalID, prov, stored)
		for _, role := range []AgentRole{AgentRoleFull, AgentRoleBaseline, AgentRoleReadOnly} {
			_, _, storedFits := childRoleWithinCeiling(stored, role, true)
			_, _, hopFits := childRoleWithinCeiling(hop, role, true)
			assert.Equal(t, storedFits, hopFits, "%s: the intrinsic set does not change which role fits", role)
		}
	})
	t.Run("role reincarnate from a token through the handler", func(t *testing.T) {
		srv, s, project, agent := reincarnateAuthzFixture(t)
		seedAgentEdge(t, s, agent.OwnerID, agent)
		user := newReincarnateAuthzUser(t, s, "ui-role-pat")
		grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleOwner)
		grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
		// --role is a patch flag: it needs agent.update and the agent:update scope.
		identity := scopedIdentityFor(user, project.ID, append(minimalSelectors(t), "agent:lifecycle", "agent:update"))

		// An explicit full role still does not fit the token's hop ceiling.
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity,
			ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleFull)}), agent.ID)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "delegation_ceiling")

		// A role change the token covers re-records the edge from the token.
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity,
			ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleReadOnly)}), agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		waitForReincarnationSettled(t, s, agent.ID)
		edges := activeEdgesFor(t, s, agent.ID)
		require.Len(t, edges, 1)
		require.Equal(t, user.ID, edges[0].DelegatorID)
		require.True(t, uatRecordedHop(edges[0].DelegatorType, edges[0].DelegatorID, edges[0].AuthorityProvenance, edges[0].EffectCeiling), "the re-recorded edge is a token hop")
		for _, p := range uatIntrinsicPermissionIDs {
			assert.False(t, EffectCeilingAllows(edges[0].EffectCeiling, p, false), "the stored edge stays the token's list (%s)", p)
		}
		reincarnated, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		deliver, err := srv.authzService.parentDeliverEligibility(context.Background(), reincarnated)
		require.NoError(t, err)
		assert.ElementsMatch(t, hubDeliveryPermissionList, deliver, "the set applies to the re-recorded hop")
	})
	t.Run("agent created by a token before 2355: adopted edge, not uat", func(t *testing.T) {
		f := newSDFixture(t, "ui-legacy-adopted")
		id, _ := f.direct(t, "adopted", false)
		f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, boundedCeiling(uiTokenList...), func(e *store.DelegationEdge) {
			e.SourceCredentialKind = store.SourceCredentialSystemMigration
		})
		edge := activeEdgesFor(t, f.Store, id)[0]
		assert.False(t, uatRecordedHop(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling))
		d := f.ceiling(t, id, sdAlwaysKey)
		assert.False(t, d.uatIntrinsic, "its own cohort decides; reason %q", d.Reason)
	})
}

// TestUATIntrinsic_NewChildEdgesCarryDeliver: the fold allows the deliver
// permissions at a token hop, so a new child edge records them
// (parentDeliverEligibility), and needs no exception.
func TestUATIntrinsic_NewChildEdgesCarryDeliver(t *testing.T) {
	f := newSDFixture(t, "ui-newchild")
	p, _ := f.uiDirect(t, "newchild", nil)
	ctx := context.Background()
	parent, err := f.Store.GetAgent(ctx, p)
	require.NoError(t, err)
	deliver, err := f.Server.authzService.parentDeliverEligibility(ctx, parent)
	require.NoError(t, err)
	assert.ElementsMatch(t, hubDeliveryPermissionList, deliver)
	ceiling, err := f.Server.authzService.agentRowEffectCeiling(ctx, parent)
	require.NoError(t, err)
	for _, d := range hubDeliveryPermissionList {
		assert.Contains(t, ceiling.PermissionIDs, d)
	}
	assert.NotContains(t, ceiling.PermissionIDs, "secret.use", "secret.use reaches a child only through the parent's coverage")
}
