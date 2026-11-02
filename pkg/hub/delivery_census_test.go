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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLaunchDeliveryCensus_Cohorts seeds one agent per cohort and checks the
// census names each one, agrees with the admission it runs, and counts them.
func TestLaunchDeliveryCensus_Cohorts(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	alpha := f.projectAlpha.ID
	owner := f.projectOwnerID

	ok := tid("census-ok")
	hdAgent(t, f.store, ok, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, ok, alpha, nil)

	noEdge := tid("census-no-edge")
	hdAgent(t, f.store, noEdge, alpha, []string{owner})

	sentinel := tid("census-sentinel")
	hdAgent(t, f.store, sentinel, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, migrationDelegatorID, sentinel, alpha, nil)

	unadopted := tid("census-unadopted")
	hdAgent(t, f.store, unadopted, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, unadopted, alpha, func(e *store.DelegationEdge) {
		e.AuthorityProvenance = store.AuthorityProvenance{}
		e.EffectCeiling = store.EffectCeiling{}
	})

	adopted := tid("census-adopted")
	hdAgent(t, f.store, adopted, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, adopted, alpha, lgAdopted(AgentRoleReadOnly, lgV1RowStatic(AgentRoleReadOnly)))

	// An agent created from a token: the deliver permissions are intrinsic
	// to its hop, so the strict admission delivers.
	uatEdge := func(e *store.DelegationEdge) {
		withBoundedCeiling("agent.read")(e)
		e.SourceCredentialKind = store.SourceCredentialUAT
	}
	uat := tid("census-uat")
	hdAgent(t, f.store, uat, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, uat, alpha, uatEdge)

	// Its child, whose agent edge was recorded without the deliver
	// permissions (before they were intrinsic): uat_descendant_legacy.
	uatChild := tid("census-uat-child")
	hdAgent(t, f.store, uatChild, alpha, []string{owner, uat})
	hdEdge(t, f.store, store.DelegationPrincipalAgent, uat, uatChild, alpha, withBoundedCeiling("agent.read"))

	// An agent created from a token by a user who is not admitted to the
	// project: the strict walk passes and the root admission refuses.
	uatNonMember := tid("census-uat-nonmember")
	hdAgent(t, f.store, uatNonMember, alpha, []string{f.memberNoneID})
	hdEdge(t, f.store, store.DelegationPrincipalUser, f.memberNoneID, uatNonMember, alpha, uatEdge)

	// A scheduler-recorded edge whose schedule was written with a token
	// counted, and unchanged: scheduler-recorded hops are not token hops.
	schedUAT := tid("census-sched-uat")
	hdAgent(t, f.store, schedUAT, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, schedUAT, alpha, func(e *store.DelegationEdge) {
		e.AuthorityProvenance = schedulerProv(store.DelegationPrincipalUser, owner, store.InitiatorCredentialKindUAT)
	})

	// The agent's own edge is fine; its parent agent has no edge.
	parent, child := tid("census-parent"), tid("census-child")
	hdAgent(t, f.store, parent, alpha, []string{owner})
	hdAgent(t, f.store, child, alpha, []string{owner, parent})
	hdEdge(t, f.store, store.DelegationPrincipalAgent, parent, child, alpha, nil)

	devLocal := tid("census-dev-local")
	hdDevLocalAgent(t, f, devLocal, nil)

	// What a start delivers (legacy fallback included), and the strict
	// classification without it.
	want := map[string]string{
		ok:           "delivers",
		noEdge:       legacyDeliveryCohortNoEdge,
		sentinel:     legacyDeliveryCohortSentinelEdge,
		unadopted:    legacyDeliveryCohortUnadoptedEdge,
		adopted:      legacyDeliveryCohortAdoptedReadOnly,
		uat:          "delivers",
		uatChild:     legacyDeliveryCohortUATDescendant,
		uatNonMember: launchDeliveryCohortUATRootNotAdmitted,
		parent:       legacyDeliveryCohortNoEdge,
		child:        legacyDeliveryCohortNoEdge,
		devLocal:     "dev_local_edge",
	}
	wantStrict := map[string]string{
		ok:           "delivers",
		noEdge:       "no_edge",
		sentinel:     "sentinel_edge",
		unadopted:    "unadopted_edge",
		adopted:      "adopted_readonly",
		uat:          "delivers",
		uatChild:     "ancestor_" + string(DenyCauseCeilingEffectExceeded),
		uatNonMember: launchDeliveryCohortUATRootNotAdmitted,
		parent:       "no_edge",
		child:        "ancestor_" + string(DenyCauseCeilingProvenanceMissing),
		devLocal:     "dev_local_edge",
	}

	// Both token-created agents store the full role, which their token's
	// list does not fit.
	setAgentRole(t, f.store, uat, AgentRoleFull)
	setAgentRole(t, f.store, uatNonMember, AgentRoleFull)

	rep, err := f.authz.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	rows := map[string]LaunchDeliveryCensusRow{}
	for _, r := range rep.Rows {
		rows[r.AgentID] = r
	}
	counts := map[string]int{}
	for id, cohort := range want {
		r, found := rows[id]
		require.True(t, found, "agent %s missing from the census", id)
		assert.Equal(t, cohort, r.Cohort, "agent %s: %+v", id, r)
		assert.Equal(t, wantStrict[id], r.StrictCohort, "agent %s: %+v", id, r)
		counts[cohort]++
		// The census verdicts are the admission's own outcomes: strict, and
		// with the fallback a launch selection credential carries.
		h := hdIdentity(t, f.authz, id)
		lh := hdIdentity(t, f.authz, id)
		lh.legacyFallback = true
		for _, p := range LaunchDeliveryCensusPermissions {
			o := f.authz.hubDeliveryChainAdmission(ctx, h, p, id, nil)
			assert.Equal(t, o.ok, r.Verdicts[p].StrictAllowed, "agent %s %s", id, p)
			assert.Equal(t, o.cause, r.Verdicts[p].StrictCause, "agent %s %s", id, p)
			lo := f.authz.hubDeliveryAdmissionOutcomeFor(ctx, lh, p, id, nil)
			assert.Equal(t, lo.ok, r.Verdicts[p].Allowed, "agent %s %s", id, p)
			assert.Equal(t, lo.cause, r.Verdicts[p].Cause, "agent %s %s", id, p)
			assert.Equal(t, lo.legacyCohort, r.Verdicts[p].LegacyCohort, "agent %s %s", id, p)
		}
	}
	for cohort, n := range counts {
		assert.GreaterOrEqual(t, rep.ByCohort[cohort], n, "cohort %s", cohort)
	}
	assert.Equal(t, len(rep.Rows), rep.Agents)
	assert.Equal(t, rep.Agents-rep.ByCohort["delivers"], rep.StrictWithheld)
	assert.Equal(t, rep.StrictWithheld, rep.Withheld+rep.Fallback)
	assert.GreaterOrEqual(t, rep.Fallback, 7, "the six legacy agents above and the token-created agent's child")
	assert.Equal(t, 1, rep.UATDescendantLegacy)
	assert.Equal(t, 1, rep.UATRootNotAdmitted)
	assert.Equal(t, 2, rep.UATCreated, "the two agents whose own edge was recorded from a token")
	assert.Equal(t, 1, rep.SchedulerUATInitiated)
	for _, id := range []string{uat, uatNonMember} {
		require.NotNil(t, rows[id].UAT, id)
		assert.True(t, rows[id].Edge.UATRecorded, id)
	}
	for _, id := range []string{ok, uatChild, schedUAT, adopted} {
		assert.Nil(t, rows[id].UAT, "%s: the own edge is not token-recorded", id)
	}
	assert.Equal(t, store.InitiatorCredentialKindUAT, rows[schedUAT].Edge.InitiatorCredentialKind)
	// The token's list (agent.read) fits no role: the stored role is filtered.
	assert.Equal(t, string(AgentRoleFull), rows[uat].UAT.StoredRole)
	assert.Equal(t, string(AgentRoleNone), rows[uat].UAT.FilteredRole)
	assert.Equal(t, 2, rep.UATRoleFiltered)

	// With dev auth on (the workstation hub), the dev_local agent delivers.
	rep, err = f.authz.LaunchDeliveryCensus(ctx, true)
	require.NoError(t, err)
	for _, r := range rep.Rows {
		if r.AgentID == devLocal {
			assert.Equal(t, "delivers", r.Cohort, "%+v", r)
		}
	}
}

// TestLaunchDeliveryCensus_SkipsDeletedAgents checks that a soft-deleted agent
// is not counted.
func TestLaunchDeliveryCensus_SkipsDeletedAgents(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()
	gone := tid("census-deleted")
	hdAgent(t, f.store, gone, f.projectAlpha.ID, []string{f.projectOwnerID})
	ag, err := f.store.GetAgent(ctx, gone)
	require.NoError(t, err)
	ag.DeletedAt = time.Now()
	require.NoError(t, f.store.UpdateAgent(ctx, ag))

	rep, err := f.authz.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	for _, r := range rep.Rows {
		assert.NotEqual(t, gone, r.AgentID)
	}
}

// TestLaunchDeliveryCensus_EquivalenceWithMain is the census fixture from
// miller79/scion#199's production verification table (uat:bounded,
// system_migration:bounded, session:principal), plus descendants of a
// token-created agent. For every agent the delivered launch item set is the
// one main delivered (pinned below: every agent received every deliver
// permission); only the cohort labels change. Equivalence holds for agents
// whose root user is admitted to the project (the
// non-admitted case is TestUATIntrinsic_RootNotAdmittedWithheld).
func TestLaunchDeliveryCensus_EquivalenceWithMain(t *testing.T) {
	m := newLDFixture(t, "census-eq", ldOptions{noEdge: true})
	f := m.goldenFixture
	ctx := context.Background()
	alpha, owner := f.projectAlpha.ID, f.projectOwnerID
	m.env(t, "EQ_ENV", "eq-value", store.ScopeProject, alpha, false)

	uatEdge := func(e *store.DelegationEdge) {
		withBoundedCeiling("agent.read", "agent.create", "project.read")(e)
		e.SourceCredentialKind = store.SourceCredentialUAT
	}
	agentAt := func(name string, ancestry []string, edgeType, delegator string, mutate func(*store.DelegationEdge)) string {
		id := tid("census-eq-" + name)
		hdAgent(t, f.store, id, alpha, ancestry)
		hdEdge(t, f.store, edgeType, delegator, id, alpha, mutate)
		return id
	}
	uat := agentAt("uat", []string{owner}, store.DelegationPrincipalUser, owner, uatEdge)
	adopted := agentAt("adopted", []string{owner}, store.DelegationPrincipalUser, owner,
		lgAdopted(AgentRoleReadOnly, lgV1RowStatic(AgentRoleReadOnly)))
	session := agentAt("session", []string{owner}, store.DelegationPrincipalUser, owner, nil)
	child := agentAt("uat-child", []string{owner, uat}, store.DelegationPrincipalAgent, uat, withBoundedCeiling("agent.read"))
	grandchild := agentAt("uat-grandchild", []string{owner, uat, child}, store.DelegationPrincipalAgent, child, withBoundedCeiling("agent.read"))

	// main's verdicts, pinned: every agent received every deliver
	// permission; the cohort a start reported on main, and now.
	type pinned struct{ mainCohort, p0Cohort string }
	want := map[string]pinned{
		uat:        {"uat_created_no_deliver_mapping", "delivers"},
		adopted:    {legacyDeliveryCohortAdoptedReadOnly, legacyDeliveryCohortAdoptedReadOnly},
		session:    {"delivers", "delivers"},
		child:      {"uat_created_no_deliver_mapping", legacyDeliveryCohortUATDescendant},
		grandchild: {"uat_created_no_deliver_mapping", legacyDeliveryCohortUATDescendant},
	}

	rep, err := f.authz.LaunchDeliveryCensus(ctx, false)
	require.NoError(t, err)
	rows := map[string]LaunchDeliveryCensusRow{}
	for _, r := range rep.Rows {
		rows[r.AgentID] = r
	}
	require.Len(t, want, 5)
	for id, p := range want {
		r, ok := rows[id]
		require.True(t, ok, id)
		for _, perm := range LaunchDeliveryCensusPermissions {
			assert.True(t, r.Verdicts[perm].Allowed, "%s %s: delivered on main, delivered now", id, perm)
		}
		assert.Equal(t, p.p0Cohort, r.Cohort, "%s: only the label changes (main: %s)", id, p.mainCohort)

		agent, err := f.store.GetAgent(ctx, id)
		require.NoError(t, err)
		env, _, err := m.d.selectDeliveryEnv(ctx, newDeliverySelector(f.authz, agent, PurposeRestart), agent)
		require.NoError(t, err)
		assert.Equal(t, "eq-value", env["EQ_ENV"], "%s: the delivered item set is unchanged", id)
	}
	// Negative control (LEGACY DATA: a descendant of a token hop whose
	// child edge narrowed deliver for another reason): below the token
	// agent, an agent edge recorded since carries deliver, and its child's
	// edge lacks it. The hop above the narrowing allows the permission, so
	// the narrowing is real and is denied, as on main.
	carrying := agentAt("carrying", []string{owner, uat}, store.DelegationPrincipalAgent, uat,
		withBoundedCeiling("agent.read", "secret.deliver", "env_var.deliver", "skill_injection.deliver"))
	narrowed := agentAt("narrowed", []string{owner, uat, carrying}, store.DelegationPrincipalAgent, carrying, withBoundedCeiling("agent.read"))
	agent, err := f.store.GetAgent(ctx, narrowed)
	require.NoError(t, err)
	env, _, err := m.d.selectDeliveryEnv(ctx, newDeliverySelector(f.authz, agent, PurposeRestart), agent)
	require.NoError(t, err)
	assert.Empty(t, env["EQ_ENV"], "control: a real narrowing still denies")
}
