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

	uat := tid("census-uat")
	hdAgent(t, f.store, uat, alpha, []string{owner})
	hdEdge(t, f.store, store.DelegationPrincipalUser, owner, uat, alpha, func(e *store.DelegationEdge) {
		withBoundedCeiling("agent.read")(e)
		e.SourceCredentialKind = store.SourceCredentialUAT
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
		ok:        "delivers",
		noEdge:    legacyDeliveryCohortNoEdge,
		sentinel:  legacyDeliveryCohortSentinelEdge,
		unadopted: legacyDeliveryCohortUnadoptedEdge,
		adopted:   legacyDeliveryCohortAdoptedReadOnly,
		uat:       deliveryCohortUATNoDeliverMapping,
		parent:    legacyDeliveryCohortNoEdge,
		child:     legacyDeliveryCohortNoEdge,
		devLocal:  "dev_local_edge",
	}
	wantStrict := map[string]string{
		ok:        "delivers",
		noEdge:    "no_edge",
		sentinel:  "sentinel_edge",
		unadopted: "unadopted_edge",
		adopted:   "adopted_readonly",
		uat:       "uat_created",
		parent:    "no_edge",
		child:     "ancestor_" + string(DenyCauseCeilingProvenanceMissing),
		devLocal:  "dev_local_edge",
	}

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
	assert.Equal(t, rep.StrictWithheld, rep.Withheld+rep.Fallback+rep.ProductGap)
	assert.GreaterOrEqual(t, rep.Fallback, 6, "the six legacy agents above")
	assert.GreaterOrEqual(t, rep.ProductGap, 1, "the UAT-created agent, counted apart from the legacy cohorts")

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
