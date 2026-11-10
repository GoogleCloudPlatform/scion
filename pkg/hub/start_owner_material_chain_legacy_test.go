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

// Package hub — the owner material chain rule on data written by older
// versions: every agent whose chain is not recorded to its owner is
// withheld the owner's rows not shared with progeny, with no transition.
// The owner restores delivery by a reclaim (reincarnate) or a re-create.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// omcPrivateRows reports whether every owner row not shared was delivered
// (all) and whether none was (none).
func omcPrivateRows(d omcDelivery) (all, none bool) {
	rows := []bool{d.privateSecret, d.privateAsNeeded, d.privateEnv, d.privateSecretEnv, d.privateAsNeededEnv}
	all, none = true, true
	for _, r := range rows {
		all = all && r
		none = none && !r
	}
	return all, none
}

// omcOutcomes returns the outcome/reason pairs the metric counted.
func omcOutcomes(m *omcMetrics) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for k := range m.got {
		parts := strings.Split(k, "/")
		if len(parts) == 4 {
			out[parts[1]+"/"+parts[2]] = true
		}
	}
	return out
}

// omcAgentOwnedBy stores a full-role agent with the given owner, creator
// and ancestry, and no edge.
func omcAgentOwnedBy(t *testing.T, f *sdFixture, name, ownerID, createdBy string, ancestry []string) string {
	t.Helper()
	id := tid("omc-owned-" + name)
	require.NoError(t, f.Store.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "omc-" + id[:8], Name: "omc-" + id[:8], ProjectID: f.ProjectID, Phase: "running",
		CreatedBy: createdBy, OwnerID: ownerID, StateVersion: 1,
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}, Ancestry: ancestry,
	}))
	return id
}

// omcOtherUser creates another project owner.
func omcOtherUser(t *testing.T, f *sdFixture, name string) string {
	t.Helper()
	id := tid("omc-user-" + name)
	createDCUser(t, f.Store, id, name+"@omc-legacy.test", f.ProjectID, store.ProjectRoleOwner)
	return id
}

// TestOwnerMaterialChain_LegacyData (LEGACY DATA) covers each shape written
// by older versions and the recorded shapes that stay delivered. For every
// fixture the delivered set of the owner's rows not shared is a subset of
// main's: main delivered every such row on the owner path, and the rule only
// adds withholds.
func TestOwnerMaterialChain_LegacyData(t *testing.T) {
	const (
		terminal = ownerChainReasonTerminalNotOwner
		root     = ownerChainReasonRootNotOwner
	)
	// Erratum 2b section 3a: an agent whose chain is not recorded also has
	// the owner's SHARED row withheld at start, logged with reason
	// chain_unrecorded beside the unshared rows' own reason. For an agent
	// with a stored ancestry, a recorded chain that ends at another user is
	// held by the parent ceiling (counted under no owner-chain reason here).
	sharedWithheld := map[string]bool{
		"no edge": true, "migration sentinel edge": true, "unadopted edge": true,
		"recorded principal does not type-check": true, "no edge, ancestry starts at another user": true,
		"recorded own edge, unrecorded hop above": true, "no ancestry, no own edge": true,
	}
	// Review H1 (section 3a makes no ancestry exception): an agent with NO
	// stored ancestry has the owner's SHARED row decided on the owner
	// material chain verdict. Only a recorded chain that ends at the owner
	// keeps it; shape (b), recorded to another user, withholds it (counted
	// as terminal_user_not_owner, the same reason as its unshared rows), and
	// with no own edge it is withheld as chain_unrecorded (sharedWithheld).
	noAncestryShared := map[string]bool{ // case name -> shared row delivered
		"shape (b): synthetic row (no ancestry, created by another user)": false,
		"empty ancestry, created by the owner":                            true,
		"no ancestry, no own edge":                                        false,
	}
	seen := map[string]bool{} // noAncestryShared keys that name a case
	uatEdge := func(e *store.DelegationEdge) {
		e.SourceCredentialKind = store.SourceCredentialUAT
		e.EffectCeiling = boundedCeiling("project.read")
	}
	for _, c := range []struct {
		name   string
		build  func(t *testing.T, f *sdFixture) string
		reason string // withheld reason; "" when delivered
	}{
		{"agent created from a user access token, owned by the token's user", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "uat-owned", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, uatEdge)
			return id
		}, ""},
		{"shape (a): agent-created sub-agent (owner is the parent agent)", func(t *testing.T, f *sdFixture) string {
			parent := omcAgentWith(t, f, "a-parent", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, parent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			child := omcAgentOwnedBy(t, f, "a-child", parent, parent, []string{f.UserID, parent})
			f.recordEdge(t, store.DelegationPrincipalAgent, parent, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			// The owner rows a start reads are keyed on the parent agent.
			omcSeedOwnerEnv(t, f, parent)
			sdShareOwnerSecrets(t, f.Server.secretBackend, parent)
			return child
		}, root},
		{"shape (b): synthetic row (no ancestry, created by another user)", func(t *testing.T, f *sdFixture) string {
			other := omcOtherUser(t, f, "b-creator")
			id := omcAgentWith(t, f, "b-synthetic", nil, other)
			f.recordEdge(t, store.DelegationPrincipalUser, other, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}, terminal},
		{"shape (c): re-recorded by another user's session", func(t *testing.T, f *sdFixture) string {
			id, _ := omcRedelegated(t, f, "c-session")
			return id
		}, terminal},
		{"shape (c): re-recorded by another user's access token", func(t *testing.T, f *sdFixture) string {
			other := omcOtherUser(t, f, "c-token")
			id, _ := f.direct(t, "c-token", false)
			f.recordEdge(t, store.DelegationPrincipalUser, other, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, uatEdge)
			return id
		}, terminal},
		{"historical same-role re-record by another user", func(t *testing.T, f *sdFixture) string {
			other := omcOtherUser(t, f, "hist")
			id, _ := f.direct(t, "hist", false)
			f.recordEdge(t, store.DelegationPrincipalUser, other, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.Role = string(AgentRoleFull) // the agent's own role, kept
			})
			return id
		}, terminal},
		{"agent-requester multi-hop chain ending at another root", func(t *testing.T, f *sdFixture) string {
			other := omcOtherUser(t, f, "hop-root")
			z := omcAgentOwnedBy(t, f, "hop-z", other, other, []string{other})
			f.recordEdge(t, store.DelegationPrincipalUser, other, z, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			id := omcAgentWith(t, f, "hop-a", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalAgent, z, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}, terminal},
		{"empty ancestry, created by the owner", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "noanc-owner", nil, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}, ""},
		{"adopted edge from the owner", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "adopted", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lafV1(t, AgentRoleBaseline, false)}, func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialSystemMigration
				e.Role = string(AgentRoleBaseline)
			})
			return id
		}, ""},
		{"adopted capped edge from the owner (backfilled)", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "adopted-capped", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lafV1(t, AgentRoleFull, false)}, func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialSystemMigration
				e.Role = string(AgentRoleFull)
				e.Grandfathered = true
			})
			return id
		}, ""},
		{"no edge", func(t *testing.T, f *sdFixture) string {
			return omcAgentWith(t, f, "noedge", []string{f.UserID}, f.UserID)
		}, legacyDeliveryCohortNoEdge},
		{"no ancestry, no own edge", func(t *testing.T, f *sdFixture) string {
			return omcAgentWith(t, f, "noanc-noedge", nil, f.UserID)
		}, legacyDeliveryCohortNoEdge},
		{"migration sentinel edge", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "sentinel", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, migrationDelegatorID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}, legacyDeliveryCohortSentinelEdge},
		{"unadopted edge", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "unadopted", []string{f.UserID}, f.UserID)
			createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, id,
				store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
			return id
		}, legacyDeliveryCohortUnadoptedEdge},
		{"recorded principal does not type-check", func(t *testing.T, f *sdFixture) string {
			id := omcAgentWith(t, f, "badprincipal", []string{f.UserID}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.SourcePrincipalKind = store.DelegationPrincipalAgent
			})
			return id
		}, ownerChainCohortInconsistentPrincipal},
		{"no edge, ancestry starts at another user", func(t *testing.T, f *sdFixture) string {
			other := omcOtherUser(t, f, "noedge-root")
			return omcAgentWith(t, f, "noedge-root", []string{other}, f.UserID)
		}, root},
		{"recorded own edge, unrecorded hop above", func(t *testing.T, f *sdFixture) string {
			parent := omcAgentWith(t, f, "above-parent", []string{f.UserID}, f.UserID)
			id := omcAgentWith(t, f, "above-child", []string{f.UserID, parent}, f.UserID)
			f.recordEdge(t, store.DelegationPrincipalAgent, parent, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return id
		}, ownerChainReasonUnrecordedAbove},
	} {
		if _, ok := noAncestryShared[c.name]; ok {
			seen[c.name] = true
		}
		t.Run(c.name, func(t *testing.T) {
			f := newOMCFixture(t, "omc-legacy-data")
			id := c.build(t, f)
			m := &omcMetrics{}
			f.Server.SetOwnerMaterialChainMetrics(m)
			var buf bytes.Buffer
			f.disp.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			got := omcStartDelivery(t, f.disp, f.Store, id)
			all, none := omcPrivateRows(got)
			// main delivered every owner row not shared on the owner path.
			for name, v := range map[string]bool{
				"private secret": got.privateSecret, "private as-needed": got.privateAsNeeded,
				"private env": got.privateEnv, "private secret env": got.privateSecretEnv,
				"private as-needed env": got.privateAsNeededEnv,
			} {
				assert.True(t, !v || omcAll.privateSecret, "%s: delivered only where main delivered it", name)
			}
			if want, ok := noAncestryShared[c.name]; ok {
				assert.Equal(t, want, got.sharedSecret, "shared owner secret, no stored ancestry")
				assert.Equal(t, want, got.sharedAsNeeded, "shared owner as-needed secret, no stored ancestry")
			}
			if c.reason == "" {
				assert.True(t, all, "every owner row not shared is delivered")
				assert.Empty(t, omcOutcomes(m), "a delivery counts nothing")
				assert.Empty(t, omcRecords(t, buf.String(), omcWithheldMsg))
				return
			}
			assert.True(t, none, "every owner row not shared is withheld")
			wantOutcomes := map[string]bool{ownerRowOutcomeWithheld + "/" + c.reason: true}
			wantReasons := map[string]bool{c.reason: true}
			if sharedWithheld[c.name] {
				wantOutcomes[ownerRowOutcomeWithheld+"/"+ReasonChainUnrecorded] = true
				wantReasons[ReasonChainUnrecorded] = true
			}
			assert.Equal(t, wantOutcomes, omcOutcomes(m))
			recs := omcRecords(t, buf.String(), omcWithheldMsg)
			require.NotEmpty(t, recs, "the withhold is logged; log: %s", buf.String())
			reasons := map[string]bool{}
			for _, r := range recs {
				assert.Equal(t, id, r["agent_id"])
				reason, _ := r["reason"].(string)
				reasons[reason] = true
			}
			assert.Equal(t, wantReasons, reasons)
		})
	}
	for name := range noAncestryShared {
		assert.True(t, seen[name], "noAncestryShared key %q names no case, so its asserts never ran", name)
	}
}

// TestOwnerMaterialChain_EmptyOwnerUnchanged: an agent with no owner (older
// rows, and scheduled agents, whose OwnerID is empty) is not the rule's to
// decide.
func TestOwnerMaterialChain_EmptyOwnerUnchanged(t *testing.T) {
	for _, c := range []struct {
		name      string
		createdBy func(f *sdFixture) string
	}{
		{"no owner", func(f *sdFixture) string { return f.UserID }},
		{"scheduled (created by the schedule revision principal)", func(*sdFixture) string { return tid("omc-schedule-revision-principal") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newOMCFixture(t, "omc-no-owner")
			id := omcAgentOwnedBy(t, f, "no-owner", "", c.createdBy(f), []string{f.UserID})
			f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			m := &omcMetrics{}
			f.Server.SetOwnerMaterialChainMetrics(m)
			agent, err := f.Store.GetAgent(context.Background(), id)
			require.NoError(t, err)
			assert.Nil(t, newOwnerChainCheck(f.Server.authzService, agent), "no owner: nothing for the rule to decide")
			_, none := omcPrivateRows(omcStartDelivery(t, f.disp, f.Store, id))
			assert.True(t, none, "no owner rows are read for an agent without an owner, as before")
			assert.Empty(t, omcOutcomes(m))
		})
	}
}

// TestOwnerMaterialChain_StoreFaultWithholds: a store fault while following
// the chain withholds every owner row not shared, counted as
// chain_lookup_error.
func TestOwnerMaterialChain_StoreFaultWithholds(t *testing.T) {
	f := newOMCFixture(t, "omc-fault-count")
	id, _ := f.direct(t, "fault-count", true)
	f.Server.authzService.store = omcEdgeFaultStore{Store: f.Server.authzService.store}
	m := &omcMetrics{}
	f.Server.SetOwnerMaterialChainMetrics(m)
	_, none := omcPrivateRows(omcStartDelivery(t, f.disp, f.Store, id))
	assert.True(t, none)
	assert.Equal(t, map[string]bool{ownerRowOutcomeWithheld + "/" + ownerChainReasonLookupError: true}, omcOutcomes(m))
}

// TestOwnerMaterialChain_NoAncestryStoreFaultWithholdsShared (review H1): for
// an agent with no stored ancestry, the owner's SHARED rows follow the owner
// material chain verdict, so a store fault while following the chain
// withholds them too (fail closed), counted as chain_lookup_error, even
// though the chain is recorded to the owner.
func TestOwnerMaterialChain_NoAncestryStoreFaultWithholdsShared(t *testing.T) {
	f := newOMCFixture(t, "omc-noanc-fault")
	id := omcAgentWith(t, f, "noanc-fault", nil, f.UserID)
	f.recordEdge(t, store.DelegationPrincipalUser, f.UserID, id, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	f.Server.authzService.store = omcEdgeFaultStore{Store: f.Server.authzService.store}
	m := &omcMetrics{}
	f.Server.SetOwnerMaterialChainMetrics(m)
	got := omcStartDelivery(t, f.disp, f.Store, id)
	_, none := omcPrivateRows(got)
	assert.True(t, none, "every owner row not shared is withheld")
	assert.False(t, got.sharedSecret, "the shared owner secret is withheld on a lookup error")
	assert.False(t, got.sharedAsNeeded, "the shared owner as-needed secret is withheld on a lookup error")
	assert.Equal(t, map[string]bool{ownerRowOutcomeWithheld + "/" + ownerChainReasonLookupError: true}, omcOutcomes(m))
}

// TestOwnerMaterialChain_AmbiguousOwnEdges: an agent with more than one
// active own edge is refused at start by the standing gate (its chain is
// broken). The rule withholds its owner rows not shared as well
// (legacy_ambiguous_edges), and the read-only reports agree.
func TestOwnerMaterialChain_AmbiguousOwnEdges(t *testing.T) {
	f := newOMCFixture(t, "omc-ambig-own")
	ctx := context.Background()
	omcSeedTZ(t, f, store.ScopeUser, f.UserID, omcUserTZ, false)
	omcSeedTZ(t, f, store.ScopeProject, f.ProjectID, omcProjectTZ, false)
	id, _ := f.direct(t, "ambig-own", false)
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	override := omcEdgesOverrideStore{Store: f.Server.authzService.store, agentID: id, edges: omcTwoEdges(f, f.UserID, id, "omc-ambig-own")}

	// The standing gate's chain check refuses the start.
	_, err = f.Server.resolveChainRoot(ctx, override, agent, true)
	require.ErrorIs(t, err, errAgentNotInStanding)
	assert.Equal(t, standingReasonChainBroken, standingReason(err))
	refusal := standingStartRefusal(agent.ID, err)
	require.NotNil(t, refusal)
	assert.Equal(t, http.StatusConflict, refusal.HTTPStatus)

	// The rule and the read-only reports.
	f.Server.authzService.store = override
	m := &omcMetrics{}
	f.Server.SetOwnerMaterialChainMetrics(m)
	privateSecret, privateEnv, _ := omcCreateDelivery(t, f, id)
	assert.False(t, privateSecret || privateEnv, "withheld")
	// The shared row is withheld too, as chain_unrecorded (erratum 2b section 3a).
	assert.Equal(t, map[string]bool{
		ownerRowOutcomeWithheld + "/" + ownerChainCohortAmbiguousEdges: true,
		ownerRowOutcomeWithheld + "/" + ReasonChainUnrecorded:          true,
	}, omcOutcomes(m))
	for _, key := range []string{omcPrivateEnvKey, sdPrivateKey} {
		found, err := f.Server.hasAnyKey(ctx, agent, []string{key})
		require.NoError(t, err)
		assert.False(t, found, "probe of %s agrees with delivery", key)
	}
	tz, err := f.Server.agentTZ(ctx, agent)
	require.NoError(t, err)
	assert.Equal(t, omcProjectTZ, tz.TZ, "the TZ view agrees with delivery")
}

// omcReincarnate has identity reincarnate the agent (role "" keeps it) and
// returns the response; a real run waits until the reincarnation settles.
func omcReincarnate(t *testing.T, r *sdReincarnation, agentID string, identity Identity, role AgentRole, dryRun bool) ReincarnateAgentResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	r.srv.handleReincarnateAgent(rec, reincarnateRequest(t, agentID, identity, ReincarnateAgentRequest{Handoff: "h", Role: string(role), DryRun: dryRun}), agentID)
	want := http.StatusAccepted
	if dryRun {
		want = http.StatusOK
	}
	require.Equal(t, want, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	if !dryRun {
		waitForReincarnationSettled(t, r.s, agentID)
	}
	return resp
}

// omcReincarnator lets a project member reincarnate (with a patch) and
// delegate in the project.
func omcReincarnator(t *testing.T, r *sdReincarnation, userID string) {
	t.Helper()
	grantAgentLifecycleAtProject(t, r.s, userID, r.project.ID)
	grantAgentDelegationAtProject(t, r.s, userID, r.project.ID)
	grantPermissionViaRoleBinding(t, r.s, userID, "agent.update", store.RoleScopeProject, r.project.ID)
}

// omcActiveEdge returns the agent's single active edge.
func omcActiveEdge(t *testing.T, s store.Store, agentID string) *store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	var active []*store.DelegationEdge
	for _, e := range edges {
		if e.Active {
			active = append(active, e)
		}
	}
	require.Len(t, active, 1)
	return active[0]
}

// TestOwnerMaterialChain_ShapeCReincarnateAndReclaim: another user's
// reincarnate that re-records the owner's agent returns the notice, and the
// owner's unshared rows are withheld; the owner's same-role reincarnate
// reclaims the agent (the edge is re-recorded with the owner as delegator,
// without the notice) and the rows are delivered again.
func TestOwnerMaterialChain_ShapeCReincarnateAndReclaim(t *testing.T) {
	r := newSDReincarnation(t)
	agent := newReincarnateTestAgent(t, r.s, r.project, r.broker, nil)
	require.Equal(t, r.creator, agent.OwnerID)
	seedAgentEdge(t, r.s, r.creator, agent)
	require.True(t, sdStartDelivery(t, r.disp, r.s, agent.ID).private, "the owner's own chain delivers")

	// Owner keeping the role of a clean agent: nothing to reclaim.
	omcReincarnator(t, r, r.creator)
	owner := NewAuthenticatedUser(r.creator, r.creator+"@delegator.test", "Creator", store.UserRoleMember, "web")
	resp := omcReincarnate(t, r, agent.ID, owner, "", false)
	assert.NotContains(t, resp.Plan.Warnings, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause)
	assert.Equal(t, r.creator, omcActiveEdge(t, r.s, agent.ID).DelegatorID)

	// Another user re-records the agent with a role change.
	b := newReincarnateAuthzUser(t, r.s, tidSlugSafe(t.Name()))
	grantProjectRole(t, r.s, b.ID, r.project.ID, store.ProjectRoleOwner)
	omcReincarnator(t, r, b.ID)
	other := NewAuthenticatedUser(b.ID, b.Email, b.DisplayName, b.Role, "web")
	resp = omcReincarnate(t, r, agent.ID, other, AgentRoleReadOnly, false)
	assert.Contains(t, resp.Plan.Warnings, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause, "the non-owner re-record returns the notice")
	require.Equal(t, b.ID, omcActiveEdge(t, r.s, agent.ID).DelegatorID)
	assert.False(t, sdStartDelivery(t, r.disp, r.s, agent.ID).private, "the owner's unshared row is withheld")

	// The owner reclaims: same role, re-recorded to the owner.
	resp = omcReincarnate(t, r, agent.ID, owner, "", false)
	assert.NotContains(t, resp.Plan.Warnings, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause)
	assert.Equal(t, r.creator, omcActiveEdge(t, r.s, agent.ID).DelegatorID, "the reclaim re-records the edge with the owner as delegator")
	assert.True(t, sdStartDelivery(t, r.disp, r.s, agent.ID).private, "the reclaimed agent gets the owner's rows again")
}

// TestOwnerMaterialChain_TokenReclaimBounded: an owner reclaim made from the
// owner's user access token (not a session) records a token edge whose
// ceiling is the token's: it carries the token's source, is bounded, and
// does not allow what the token lacks. A token reincarnate cannot record a
// wider chain: asking for the full role, which no token can delegate, is
// refused and leaves the reclaimed edge in place.
func TestOwnerMaterialChain_TokenReclaimBounded(t *testing.T) {
	r := newSDReincarnation(t)
	ctx := context.Background()
	agent := newReincarnateTestAgent(t, r.s, r.project, r.broker, nil)
	seedAgentEdge(t, r.s, r.creator, agent)

	// Another user re-records the agent: the owner's rows are withheld.
	b := newReincarnateAuthzUser(t, r.s, tidSlugSafe(t.Name()))
	grantProjectRole(t, r.s, b.ID, r.project.ID, store.ProjectRoleOwner)
	omcReincarnator(t, r, b.ID)
	other := NewAuthenticatedUser(b.ID, b.Email, b.DisplayName, b.Role, "web")
	omcReincarnate(t, r, agent.ID, other, AgentRoleReadOnly, false)
	require.Equal(t, b.ID, omcActiveEdge(t, r.s, agent.ID).DelegatorID)

	// The owner reclaims from a token.
	omcReincarnator(t, r, r.creator)
	ownerUser, err := r.s.GetUser(ctx, r.creator)
	require.NoError(t, err)
	scopes := append(permissions.UATManageScopes(), minimalSelectors(t)...)
	token := scopedIdentityFor(ownerUser, r.project.ID, append(scopes, "agent:update"))
	resp := omcReincarnate(t, r, agent.ID, token, "", false)
	assert.NotContains(t, resp.Plan.Warnings, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause)

	edge := omcActiveEdge(t, r.s, agent.ID)
	assert.Equal(t, r.creator, edge.DelegatorID, "the reclaim re-records the edge with the owner as delegator")
	assert.Equal(t, store.SourceCredentialUAT, edge.SourceCredentialKind, "the reclaim records a token edge")
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind, "the token edge is bounded by the token's ceiling")
	hop := uatHopEffectCeiling(edge.DelegatorType, edge.DelegatorID, edge.AuthorityProvenance, edge.EffectCeiling)
	assert.False(t, EffectCeilingAllows(hop, "project.secret_read", false), "the edge does not allow what the token lacks")
	assert.True(t, sdStartDelivery(t, r.disp, r.s, agent.ID).private, "the reclaimed agent gets the owner's rows again")

	// A wider re-record from the token is refused and changes nothing.
	rec := httptest.NewRecorder()
	r.srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, token, ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleFull)}), agent.ID)
	assert.NotEqual(t, http.StatusAccepted, rec.Code, "a token cannot delegate the full role: %s", rec.Body.String())
	assert.Equal(t, edge.ID, omcActiveEdge(t, r.s, agent.ID).ID, "the reclaimed edge stays in place")
}

// TestOwnerMaterialChain_ReRecordNoticeText: the re-record notice offers
// the reclaim only where it restores delivery (the new chain ends at
// another user and the ancestry starts at the owner); it is absent when
// the rows stay delivered.
func TestOwnerMaterialChain_ReRecordNoticeText(t *testing.T) {
	f := newOMCFixture(t, "omc-notice-text")
	ctx := context.Background()
	other := omcOtherUser(t, f, "notice-text")
	get := func(id string) *store.Agent {
		a, err := f.Store.GetAgent(ctx, id)
		require.NoError(t, err)
		return a
	}
	own := get(omcAgentWith(t, f, "notice-own", []string{f.UserID}, f.UserID))
	foreign := get(omcAgentWith(t, f, "notice-foreign", []string{other}, f.UserID))

	assert.Equal(t, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause,
		f.Server.ownerMaterialReRecordNotice(ctx, own, store.DelegationPrincipalUser, other), "another user's re-record: notice and remedy")
	assert.Empty(t, f.Server.ownerMaterialReRecordNotice(ctx, own, store.DelegationPrincipalUser, f.UserID), "the owner's re-record: no notice")
	assert.Equal(t, ownerMaterialReRecordNoticeText,
		f.Server.ownerMaterialReRecordNotice(ctx, foreign, store.DelegationPrincipalUser, f.UserID),
		"ancestry at another user: a reclaim cannot restore delivery, so no remedy is offered")
}

// TestOwnerMaterialChain_TokenReRecordNotice: a role-changing reincarnate
// by another user's access token returns the notice too (dry run).
func TestOwnerMaterialChain_TokenReRecordNotice(t *testing.T) {
	r := newSDReincarnation(t)
	agent := newReincarnateTestAgent(t, r.s, r.project, r.broker, nil)
	seedAgentEdge(t, r.s, r.creator, agent)
	b := newReincarnateAuthzUser(t, r.s, "omc-token-notice")
	grantProjectRole(t, r.s, b.ID, r.project.ID, store.ProjectRoleOwner)
	omcReincarnator(t, r, b.ID)
	// A --role patch needs agent:update besides the lifecycle scopes.
	scopes := append(permissions.UATManageScopes(), minimalSelectors(t)...)
	identity := scopedIdentityFor(b, r.project.ID, append(scopes, "agent:update"))
	resp := omcReincarnate(t, r, agent.ID, identity, AgentRoleReadOnly, true)
	assert.Contains(t, resp.Plan.Warnings, ownerMaterialReRecordNoticeText+ownerMaterialReclaimClause)
}

// TestOwnerMaterialChain_Census: the delivery census lists every live agent
// whose owner's unshared rows the rule withholds, with the reason, its
// depth, the names of those rows (never their values) and the remedy; an
// agent whose chain ends at its owner is not listed.
func TestOwnerMaterialChain_Census(t *testing.T) {
	f := newOMCFixture(t, "omc-census")
	ctx := context.Background()
	redelegated, _ := omcRedelegated(t, f, "census-redeleg")
	noEdge := omcAgentWith(t, f, "census-noedge", []string{f.UserID}, f.UserID)
	// Review H1: an agent with no stored ancestry and no recorded chain.
	noAncestry := omcAgentWith(t, f, "census-noanc", nil, f.UserID)
	clean, _ := f.direct(t, "census-clean", true)

	defaultLog := usaCaptureDefaultLog(t)
	rep, err := f.Server.authzService.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	omc := rep.OwnerMaterialChain
	rows := map[string]OwnerMaterialChainCensusRow{}
	for _, r := range omc.Rows {
		rows[r.AgentID] = r
	}
	assert.NotContains(t, rows, clean, "a chain recorded to the owner is not listed")
	// The owner's shared secrets are listed where start also withholds them
	// (erratum 2b section 3a): a chain that is not recorded, with or without
	// a stored ancestry. A recorded chain that ends at another user, with a
	// stored ancestry, is held by the parent ceiling instead, so none.
	sharedNames := []string{sdAlwaysKey, sdAsNeededKey}
	for _, c := range []struct {
		id, reason string
		depth      int
		shared     []string
	}{
		{redelegated, ownerChainReasonTerminalNotOwner, 1, []string{}},
		{noEdge, legacyDeliveryCohortNoEdge, 1, sharedNames},
		{noAncestry, legacyDeliveryCohortNoEdge, 0, sharedNames},
	} {
		row, ok := rows[c.id]
		require.True(t, ok, "agent %s is listed: %+v", c.id, omc.Rows)
		assert.Equal(t, f.UserID, row.OwnerID)
		assert.Equal(t, c.reason, row.Reason)
		assert.Equal(t, c.depth, row.Depth, "the depth is the stored ancestry's length")
		assert.ElementsMatch(t, []string{sdPrivateKey, omcPrivateAsNeededKey}, row.Secrets)
		assert.ElementsMatch(t, []string{omcPrivateEnvKey, omcPrivateSecretEnvKey, omcPrivateAsNeededEnvKey}, row.EnvVars)
		assert.ElementsMatch(t, c.shared, row.SharedSecrets, "the owner's shared secrets start withholds")
	}
	assert.Equal(t, len(omc.Rows), omc.Withheld)
	assert.GreaterOrEqual(t, omc.Affected, 2)
	assert.GreaterOrEqual(t, omc.ByReason[ownerChainReasonTerminalNotOwner], 1)
	assert.GreaterOrEqual(t, omc.ByReason[legacyDeliveryCohortNoEdge], 1)
	assert.Equal(t, ownerMaterialChainRemedy, omc.Remedy)
	raw, err := json.Marshal(rep)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "epoch", "the census carries no epoch state (erratum 2b section 1)")
	for _, v := range []string{sdPrivateValue, sdAlwaysValue, sdAsNeededValue, omcPrivateAsNeededValue, omcPrivateEnvValue, omcPrivateSecretEnvValue, omcPrivateAsNeededEnvValue} {
		assert.NotContains(t, string(raw), v, "the census never carries a value")
		assert.NotContains(t, defaultLog.String(), v, "the census never logs a value")
	}
}
