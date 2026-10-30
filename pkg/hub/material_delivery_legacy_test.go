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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lgFallbackMsg = "launch material delivered through the legacy fallback"

// lgItems stores one plain env var and one secret per project, hub and
// broker scope for the fixture agent and returns their keys.
func lgItems(t *testing.T, m *ldFixture) []string {
	t.Helper()
	var keys []string
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		up := strings.ToUpper(scope)
		m.env(t, "LG_ENV_"+up, "lg-env-value-"+scope, scope, m.scopeID(scope), false)
		m.secret(t, "LG_SECRET_"+up, "lg-secret-value-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
		keys = append(keys, "LG_ENV_"+up, "LG_SECRET_"+up)
	}
	return keys
}

// lgLogger points the fixture's authorization service and dispatcher at a
// JSON log buffer.
func lgLogger(m *ldFixture) *bytes.Buffer {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m.authz.logger = logger
	m.d.log = logger
	return &buf
}

// lgAncestryWithParent is an ldOptions.ancestry giving the agent the
// ancestry [project owner, lgParent(name)]. The parent's ID is the
// deterministic one lgParent stores later.
func lgAncestryWithParent(name string) func(f *goldenFixture) []string {
	return func(f *goldenFixture) []string { return []string{f.projectOwnerID, tid("lg-parent-" + name)} }
}

// lgRequireStoredAncestry checks that the stored agent has ancestry want, so
// a row never silently tests the default ancestry.
func lgRequireStoredAncestry(t *testing.T, m *ldFixture, want []string) {
	t.Helper()
	ag, err := m.store.GetAgent(context.Background(), m.agent.ID)
	require.NoError(t, err)
	require.Equal(t, len(want), len(ag.Ancestry), "stored ancestry %v", ag.Ancestry)
	for i := range want {
		require.Equal(t, want[i], ag.Ancestry[i], "stored ancestry %v", ag.Ancestry)
	}
}

// lgParent stores a parent agent in the fixture's project with ancestry
// [project owner] and returns its ID.
func lgParent(t *testing.T, m *ldFixture, name string) string {
	t.Helper()
	p := tid("lg-parent-" + name)
	hdAgent(t, m.store, p, m.agent.ProjectID, []string{m.projectOwnerID})
	return p
}

// lgSoftDelete marks a stored agent deleted.
func lgSoftDelete(t *testing.T, m *ldFixture, id string) {
	t.Helper()
	ag, err := m.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	ag.DeletedAt = time.Now()
	require.NoError(t, m.store.UpdateAgent(context.Background(), ag))
}

// lgV1Row is the compatibility policy V1 row of role (no assigned service
// account).
func lgV1Row(t *testing.T, role AgentRole) []string {
	t.Helper()
	ids, ok := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), false)
	require.True(t, ok, "role %s", role)
	return ids
}

// lgV1RowStatic is lgV1Row for table literals built before t exists.
func lgV1RowStatic(role AgentRole) []string {
	ids, _ := permissions.CompatibilityCeiling(permissions.CompatibilityPolicyV1, string(role), false)
	return ids
}

// lgAdopted is an adopted (system_migration) edge with edge role role whose
// bounded ceiling is ceiling.
func lgAdopted(role AgentRole, ceiling []string) func(e *store.DelegationEdge) {
	return func(e *store.DelegationEdge) {
		withBoundedCeiling(ceiling...)(e)
		e.Role = string(role)
		e.SourceCredentialKind = store.SourceCredentialSystemMigration
	}
}

// lgUATCreated is an edge recorded from a user access token, whose bounded
// ceiling cannot carry the deliver permissions.
func lgUATCreated(e *store.DelegationEdge) {
	withBoundedCeiling("agent.read")(e)
	e.SourceCredentialKind = store.SourceCredentialUAT
}

// TestDispatchDelivery_LegacyCohortsDeliveredWithWarn: an agent whose
// delivery walk fails only because of legacy provenance still gets its
// project, hub and broker material at restart, with one Warn per item that
// names the agent, the cohort and the item, and no value.
func TestDispatchDelivery_LegacyCohortsDeliveredWithWarn(t *testing.T) {
	rows := []struct {
		name       string
		opts       ldOptions
		after      func(t *testing.T, m *ldFixture)
		wantCohort string
		wantDepth  bool
	}{
		{name: "no_edge", opts: ldOptions{noEdge: true}, wantCohort: legacyDeliveryCohortNoEdge},
		{
			name: "sentinel_edge", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				createDCEdge(t, m.store, store.DelegationPrincipalUser, migrationDelegatorID, store.DelegationPrincipalAgent, m.agent.ID,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
			},
			wantCohort: legacyDeliveryCohortSentinelEdge,
		},
		{
			name: "unadopted_edge", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, m.agent.ID,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
			},
			wantCohort: legacyDeliveryCohortUnadoptedEdge,
		},
		{
			// A legacy scheduled-dispatch child: unrecorded, role none.
			name: "scheduled_child_role_none", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, m.agent.ID,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleNone))
			},
			wantCohort: legacyDeliveryCohortUnadoptedEdge,
		},
		{name: "adopted_baseline", opts: ldOptions{edge: lgAdopted(AgentRoleBaseline, lgV1RowStatic(AgentRoleBaseline))}, wantCohort: legacyDeliveryCohortAdoptedBaseline},
		{name: "adopted_readonly", opts: ldOptions{edge: lgAdopted(AgentRoleReadOnly, lgV1RowStatic(AgentRoleReadOnly))}, wantCohort: legacyDeliveryCohortAdoptedReadOnly},
		{
			// Adopted at the lower of edge and applied role: the edge role
			// stays full, the ceiling is the baseline row.
			name: "adopted_full_edge_capped_at_baseline_row", opts: ldOptions{edge: lgAdopted(AgentRoleFull, lgV1RowStatic(AgentRoleBaseline))},
			wantCohort: legacyDeliveryCohortAdoptedBaseline,
		},
		{
			name: "adopted_full_edge_capped_at_readonly_row", opts: ldOptions{edge: lgAdopted(AgentRoleFull, lgV1RowStatic(AgentRoleReadOnly))},
			wantCohort: legacyDeliveryCohortAdoptedReadOnly,
		},
		{
			// The root user is not a member of the project: a fallback chain
			// does not check project membership (as before the check).
			name: "no_edge_root_not_project_member", opts: ldOptions{noEdge: true, rootMemberNone: true},
			wantCohort: legacyDeliveryCohortNoEdge,
		},
		{
			// A recorded target edge whose parent's edge is unrecorded and
			// whose root user is not a member of the project: served, because
			// the provenance root admission does not run on a fallback chain.
			name: "legacy_parent_root_not_project_member", opts: ldOptions{noEdge: true, rootMemberNone: true},
			after: func(t *testing.T, m *ldFixture) {
				p := tid("lg-parent-nonmember")
				hdAgent(t, m.store, p, m.agent.ProjectID, []string{m.memberNoneID})
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.memberNoneID, store.DelegationPrincipalAgent, p,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, nil)
			},
			wantCohort: legacyDeliveryCohortUnadoptedEdge,
			wantDepth:  true,
		},
		{name: "uat_created", opts: ldOptions{edge: lgUATCreated}, wantCohort: deliveryCohortUATNoDeliverMapping},
		{
			// The child of a UAT-created agent: its recorded agent edge
			// lacks the permission only because its parent could not pass
			// it down.
			name: "recorded_child_of_uat_created_agent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "uat-parent")
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, lgUATCreated)
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
			wantCohort: deliveryCohortUATNoDeliverMapping,
			wantDepth:  true,
		},
		{
			// Two inheriting hops below the UAT-created agent.
			name: "recorded_grandchild_of_uat_created_agent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				gp := lgParent(t, m, "uat-grandparent")
				p := lgParent(t, m, "uat-middle")
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, gp, m.agent.ProjectID, lgUATCreated)
				hdEdge(t, m.store, store.DelegationPrincipalAgent, gp, p, m.agent.ProjectID, withBoundedCeiling("agent.read"))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
			wantCohort: deliveryCohortUATNoDeliverMapping,
			wantDepth:  true,
		},
		{
			// The agent's own recorded edge passes; its parent's edge is
			// unrecorded.
			name: "legacy_ancestor", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "ancestor")
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, p,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, nil)
			},
			wantCohort: legacyDeliveryCohortUnadoptedEdge,
			wantDepth:  true,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := newLDFixture(t, "lg-"+r.name, r.opts)
			if r.after != nil {
				r.after(t, m)
			}
			keys := lgItems(t, m)
			buf := lgLogger(m)

			require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
			env := m.client.lastRestartResolvedEnv
			for _, k := range keys {
				assert.NotEmpty(t, env[k], "legacy item %s was not delivered", k)
			}

			seen := map[string]map[string]any{}
			for _, rec := range warnRecords(t, buf.Bytes(), lgFallbackMsg) {
				k, _ := rec["key"].(string)
				seen[k] = rec
			}
			for _, k := range keys {
				rec, ok := seen[k]
				require.True(t, ok, "no fallback Warn for %s; log: %s", k, buf.String())
				assert.Equal(t, m.agent.ID, rec["agent_id"])
				assert.Equal(t, r.wantCohort, rec["cohort"])
				assert.Equal(t, deliveryFallbackException(r.wantCohort), rec["exception"])
				assert.Equal(t, string(PurposeRestart), rec["purpose"])
				assert.NotEmpty(t, rec["scope"])
				assert.NotEmpty(t, rec["kind"])
				reason, _ := rec["reason"].(string)
				assert.Equal(t, r.wantDepth, strings.Contains(reason, "at depth"), "reason %q", reason)
			}
			assert.Empty(t, warnRecords(t, buf.Bytes(), "launch material withheld: not authorized for delivery"))
			assert.NotContains(t, buf.String(), "lg-env-value-", "a value was logged")
			assert.NotContains(t, buf.String(), "lg-secret-value-", "a value was logged")

			// Outside launch selection the walk is still strict: the same
			// item is refused to a credential without the fallback.
			sm := m.secret(t, "LG_STRICT", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			d := m.decideItem(t, m.authz, hdIdentity(t, m.authz, m.agent.ID), MaterialKindSecret, materialRecordSecret, sm.ID, sm.Name, store.ScopeProject, m.agent.ProjectID)
			assert.False(t, d.Allowed, "strict decision: %s", d.Reason)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
		})
	}
}

// TestDispatchDelivery_LegacyFallbackKeepsRealDenials: a legacy-shaped agent
// whose chain also carries a real denial gets none of its project, hub or
// broker material, and no fallback Warn.
func TestDispatchDelivery_LegacyFallbackKeepsRealDenials(t *testing.T) {
	unrecordedFrom := func(t *testing.T, m *ldFixture, parent string) {
		createDCEdge(t, m.store, store.DelegationPrincipalAgent, parent, store.DelegationPrincipalAgent, m.agent.ID,
			store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
	}
	rows := []struct {
		name  string
		opts  ldOptions
		after func(t *testing.T, m *ldFixture)
	}{
		{
			name: "held_agent_no_edge", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
			},
		},
		{
			name: "held_agent_adopted_baseline", opts: ldOptions{edge: lgAdopted(AgentRoleBaseline, lgV1RowStatic(AgentRoleBaseline))},
			after: func(t *testing.T, m *ldFixture) {
				holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
			},
		},
		{
			name: "unadopted_edge_from_deleted_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "deleted")
				unrecordedFrom(t, m, p)
				lgSoftDelete(t, m, p)
			},
		},
		{
			name: "unadopted_edge_from_held_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "held")
				unrecordedFrom(t, m, p)
				holdAgentFor(t, m.store, p, m.agent.ProjectID, m.projectOwnerID)
			},
		},
		{
			name: "no_edge_deleted_ancestor", opts: ldOptions{noEdge: true, ancestry: lgAncestryWithParent("deleted-ancestor")},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "deleted-ancestor")
				lgRequireStoredAncestry(t, m, []string{m.projectOwnerID, p})
				lgSoftDelete(t, m, p)
			},
		},
		{
			name: "no_edge_held_ancestor", opts: ldOptions{noEdge: true, ancestry: lgAncestryWithParent("held-ancestor")},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "held-ancestor")
				lgRequireStoredAncestry(t, m, []string{m.projectOwnerID, p})
				holdAgentFor(t, m.store, p, m.agent.ProjectID, m.projectOwnerID)
			},
		},
		{
			name: "no_edge_root_user_suspended", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				setUserStatus(t, m.store, m.projectOwnerID, store.UserStatusSuspended)
			},
		},
		{
			name: "unadopted_edge_root_user_suspended", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, m.agent.ID,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
				setUserStatus(t, m.store, m.projectOwnerID, store.UserStatusSuspended)
			},
		},
		{
			name: "no_edge_no_stored_ancestry",
			opts: ldOptions{noEdge: true, ancestry: func(*goldenFixture) []string { return []string{} }},
			after: func(t *testing.T, m *ldFixture) {
				lgRequireStoredAncestry(t, m, nil)
			},
		},
		{
			// Local-development provenance on a hub without dev auth is not
			// a legacy shape.
			name: "dev_local_edge_without_dev_auth",
			opts: ldOptions{edge: func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialDevLocal }},
		},
		{
			// A recorded ceiling that lacks the permission is enforced.
			name: "recorded_session_ceiling_without_deliver",
			opts: ldOptions{edge: withBoundedCeiling("agent.read")},
		},
		{
			// A recorded agent edge that lacks the permission under a parent
			// whose recorded ceiling allows it is a real narrowing.
			name: "recorded_child_narrowed_below_principal_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "principal-parent")
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, nil)
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
		},
		{
			// The UAT gap is the only inherited gap tolerated: under an
			// unadopted parent the child's own recorded deny stands.
			name: "recorded_child_lacking_deliver_below_unadopted_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "unadopted-parent")
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, p,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
		},
		{
			// A pending UAT inheritance must end at a UAT-created hop: an
			// adopted parent does not explain the child's missing permission.
			name: "recorded_child_lacking_deliver_below_adopted_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "adopted-parent")
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, lgAdopted(AgentRoleBaseline, lgV1Row(t, AgentRoleBaseline)))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
		},
		{
			name: "recorded_child_lacking_deliver_below_sentinel_parent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "sentinel-parent")
				createDCEdge(t, m.store, store.DelegationPrincipalUser, migrationDelegatorID, store.DelegationPrincipalAgent, p,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
		},
		{
			name: "recorded_child_lacking_deliver_below_parent_without_edge", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "edgeless-parent")
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
			},
		},
		{
			// An adopted edge whose ceiling is not within the V1 row of its
			// role was not capped by V1: its deny stands.
			name: "adopted_edge_ceiling_outside_v1_row", opts: ldOptions{edge: lgAdopted(AgentRoleBaseline, []string{"agent.read"})},
		},
		{
			// An inheriting child of a UAT-created parent that is held.
			name: "recorded_child_of_held_uat_created_agent", opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				p := lgParent(t, m, "held-uat-parent")
				hdEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, p, m.agent.ProjectID, lgUATCreated)
				hdEdge(t, m.store, store.DelegationPrincipalAgent, p, m.agent.ID, m.agent.ProjectID, withBoundedCeiling("agent.read"))
				holdAgentFor(t, m.store, p, m.agent.ProjectID, m.projectOwnerID)
			},
		},
		{
			// The root user is not admitted to the project: a provenance
			// root denial on a fully recorded chain, never a legacy hop.
			name: "recorded_chain_root_not_admitted", opts: ldOptions{rootMemberNone: true},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := newLDFixture(t, "lg-deny-"+r.name, r.opts)
			if r.after != nil {
				r.after(t, m)
			}
			keys := lgItems(t, m)
			buf := lgLogger(m)

			// The selection itself, for start and restart, so no dispatch
			// precondition can stand in for the selection's own refusal.
			for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
				got := map[string]bool{}
				env, _, err := m.d.selectDeliveryEnv(context.Background(), newDeliverySelector(m.authz, m.agent, purpose), m.agent)
				require.NoError(t, err)
				for k := range env {
					got[k] = true
				}
				secrets, err := m.d.selectDeliverySecrets(context.Background(), newDeliverySelector(m.authz, m.agent, purpose), m.agent)
				require.NoError(t, err)
				for _, rs := range secrets {
					got[rs.Name] = true
				}
				for _, k := range keys {
					assert.False(t, got[k], "%s: item %s was delivered despite a real denial", purpose, k)
				}
			}
			assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), "no fallback for a real denial")
			assert.NotEmpty(t, warnRecords(t, buf.Bytes(), "launch material withheld: not authorized for delivery"))
		})
	}
}

// lgDupEdgeStore reports two active edges for one agent, the malformed
// state the partial unique index forbids, as legacy data may still hold.
type lgDupEdgeStore struct {
	store.Store
	agentID   string
	projectID string
	ownerID   string
}

func (s *lgDupEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateType != store.DelegationPrincipalAgent || delegateID != s.agentID {
		return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	}
	edge := func(id string) *store.DelegationEdge {
		return &store.DelegationEdge{
			ID: id, DelegatorType: store.DelegationPrincipalUser, DelegatorID: s.ownerID,
			DelegateType: store.DelegationPrincipalAgent, DelegateID: s.agentID,
			ScopeType: store.RoleScopeProject, ScopeID: s.projectID, Role: string(AgentRoleFull), Active: true,
		}
	}
	return []*store.DelegationEdge{edge(tid("lg-dup-1-" + s.agentID)), edge(tid("lg-dup-2-" + s.agentID))}, nil
}

// TestDispatchDelivery_DuplicateEdgesWithheldWithCause: an agent with two
// active edges stays withheld (no fallback, pending a decision on the census
// numbers), and each item's Warn names the cause.
func TestDispatchDelivery_DuplicateEdgesWithheldWithCause(t *testing.T) {
	m := newLDFixture(t, "lg-dup", ldOptions{noEdge: true})
	m.env(t, "LG_DUP_ENV", "v", store.ScopeProject, m.agent.ProjectID, false)
	buf := lgLogger(m)
	hdWithStore(t, m.authz, &lgDupEdgeStore{Store: m.store, agentID: m.agent.ID, projectID: m.agent.ProjectID, ownerID: m.projectOwnerID})

	env, _, err := m.d.selectDeliveryEnv(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	assert.NotContains(t, env, "LG_DUP_ENV")
	assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg))
	recs := warnRecords(t, buf.Bytes(), "launch material withheld: not authorized for delivery")
	require.Len(t, recs, 1, "log: %s", buf.String())
	assert.Equal(t, "LG_DUP_ENV", recs[0]["key"])
	assert.Equal(t, string(DenyCauseCeilingProvenanceAmbiguous), recs[0]["deny_cause"])
}

// TestDispatchDelivery_LegacyFallbackKeepsEarlierDenials: the fallback runs
// inside step 10 only, so an item a stage before it refuses (here the
// association's record check) stays withheld for a legacy agent, while the
// other items are served.
func TestDispatchDelivery_LegacyFallbackKeepsEarlierDenials(t *testing.T) {
	m := newLDFixture(t, "lg-earlier", ldOptions{noEdge: true})
	m.env(t, "LG_PROJECT_ENV", "p", store.ScopeProject, m.agent.ProjectID, false)
	m.env(t, "LG_HUB_ENV", "h", store.ScopeHub, m.hubID, false)
	hdWithStore(t, m.authz, &ldProjectEnvHiddenStore{Store: m.store})

	sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
	env, _, err := m.d.selectDeliveryEnv(context.Background(), sel, m.agent)
	require.NoError(t, err)
	assert.NotContains(t, env, "LG_PROJECT_ENV", "an association refusal is not overridden")
	assert.Equal(t, "h", env["LG_HUB_ENV"])
}

// TestDispatchDelivery_RecordedEdgeNeedsNoFallback: a recorded principal
// chain delivers through the strict admission, with no fallback Warn.
func TestDispatchDelivery_RecordedEdgeNeedsNoFallback(t *testing.T) {
	m := newLDFixture(t, "lg-recorded", ldOptions{})
	keys := lgItems(t, m)
	buf := lgLogger(m)
	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	for _, k := range keys {
		assert.NotEmpty(t, m.client.lastRestartResolvedEnv[k], k)
	}
	assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg))
}

// TestDispatchDelivery_LegacyFallbackWarnRateLimited: a second restart
// within the window logs no second fallback Warn for the same items, and
// still delivers them.
func TestDispatchDelivery_LegacyFallbackWarnRateLimited(t *testing.T) {
	m := newLDFixture(t, "lg-ratelimit", ldOptions{noEdge: true})
	m.env(t, "LG_RL_ENV", "v", store.ScopeProject, m.agent.ProjectID, false)
	buf := lgLogger(m)
	for i := 0; i < 2; i++ {
		require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
		assert.Equal(t, "v", m.client.lastRestartResolvedEnv["LG_RL_ENV"], "restart %d", i)
	}
	assert.Len(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), 1)
}

// TestLegacyDeliveryFallback_Scope pins which purposes take the fallback
// and which strict causes can.
func TestLegacyDeliveryFallback_Scope(t *testing.T) {
	assert.True(t, legacyDeliveryFallbackPurpose(PurposeDispatch))
	assert.True(t, legacyDeliveryFallbackPurpose(PurposeRestart))
	assert.True(t, legacyDeliveryFallbackPurpose(PurposeAvailabilityProbe))
	assert.True(t, legacyDeliveryFallbackPurpose(PurposeProvisionCredential))
	assert.False(t, legacyDeliveryFallbackPurpose(PurposeRuntimeRead))

	for _, c := range []DenyCause{
		DenyCauseCeilingProvenanceAmbiguous, DenyCauseCeilingSourceNotAllowed,
		DenyCauseCeilingDelegatorLacksPermission, DenyCauseCeilingError, "",
	} {
		assert.False(t, legacyFallbackCauses[c], "cause %q must never take the fallback", c)
	}
	for _, p := range LaunchDeliveryCensusPermissions {
		assert.False(t, uatCarriesPermission(p), "%s: the UAT-created cohort assumes no token scope maps it", p)
	}
}

// TestDispatchDelivery_BootAdoptedFullEdgesWithoutDeliverServed: boot
// adoption keeps the edge role and caps the ceiling by policy V1, so a full
// edge can carry a ceiling without the deliver permissions: (a) an edge of
// role full on an agent applied at baseline, and (b) a full child of an
// adopted baseline parent (intersection). Both are served by the adopted
// cohort, keyed on the ceiling.
func TestDispatchDelivery_BootAdoptedFullEdgesWithoutDeliverServed(t *testing.T) {
	m := newLDFixture(t, "lg-boot", ldOptions{})
	ctx := context.Background()
	owner, alpha := m.projectOwnerID, m.agent.ProjectID

	capped := tid("lg-boot-capped")
	createDCAgent(t, m.store, capped, alpha, owner, AgentRoleBaseline)
	createDCEdge(t, m.store, store.DelegationPrincipalUser, owner, store.DelegationPrincipalAgent, capped,
		store.RoleScopeProject, alpha, string(AgentRoleFull))

	parent := tid("lg-boot-parent")
	createDCAgent(t, m.store, parent, alpha, owner, AgentRoleBaseline)
	createDCEdge(t, m.store, store.DelegationPrincipalUser, owner, store.DelegationPrincipalAgent, parent,
		store.RoleScopeProject, alpha, string(AgentRoleBaseline))
	child := tid("lg-boot-child")
	require.NoError(t, m.store.CreateAgent(ctx, &store.Agent{
		ID: child, Slug: "slug-" + child[:8], Name: "name-" + child[:8], ProjectID: alpha, Phase: "running",
		CreatedBy: parent, OwnerID: parent, Ancestry: []string{owner, parent},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
	createDCEdge(t, m.store, store.DelegationPrincipalAgent, parent, store.DelegationPrincipalAgent, child,
		store.RoleScopeProject, alpha, string(AgentRoleFull))

	runBootAdoption(t, m.store)

	m.env(t, "LG_BOOT_ENV", "boot-value", store.ScopeProject, alpha, false)
	buf := lgLogger(m)
	for _, id := range []string{capped, child} {
		edges := activeEdgesFor(t, m.store, id)
		require.Len(t, edges, 1, id)
		e := edges[0]
		require.Equal(t, store.SourceCredentialSystemMigration, e.SourceCredentialKind, "%s: the edge is adopted", id)
		require.Equal(t, string(AgentRoleFull), e.Role, "%s: adoption keeps the edge role", id)
		require.Equal(t, store.EffectCeilingBounded, e.Kind, id)
		assert.Subset(t, lgV1Row(t, AgentRoleBaseline), e.PermissionIDs, "%s: the ceiling is capped at the baseline row", id)
		assert.NotContains(t, e.PermissionIDs, "env_var.deliver", id)

		agent, err := m.store.GetAgent(ctx, id)
		require.NoError(t, err)
		env, _, err := m.d.selectDeliveryEnv(ctx, newDeliverySelector(m.authz, agent, PurposeRestart), agent)
		require.NoError(t, err)
		assert.Equal(t, "boot-value", env["LG_BOOT_ENV"], "%s: served by the adopted cohort", id)
	}
	cohorts := map[string]any{}
	for _, rec := range warnRecords(t, buf.Bytes(), lgFallbackMsg) {
		if id, _ := rec["agent_id"].(string); id == capped || id == child {
			cohorts[id] = rec["cohort"]
		}
	}
	assert.Equal(t, map[string]any{capped: legacyDeliveryCohortAdoptedBaseline, child: legacyDeliveryCohortAdoptedBaseline}, cohorts, "log: %s", buf.String())
}

// TestDeliveryLogDedup_BoundedLRU: within the window a key logs once; past
// capacity only the least recently logged key is forgotten; past the window
// a key logs again.
func TestDeliveryLogDedup_BoundedLRU(t *testing.T) {
	d := newDeliveryLogDedup(3, time.Hour)
	now := time.Date(2026, 10, 28, 4, 0, 0, 0, time.UTC)
	for _, k := range []string{"a", "b", "c"} {
		assert.True(t, d.first(k, now), k)
	}
	assert.False(t, d.first("b", now.Add(time.Minute)))
	assert.True(t, d.first("d", now.Add(2*time.Minute)), "a fourth key is logged")
	assert.True(t, d.first("a", now.Add(3*time.Minute)), "only the least recently logged key was evicted")
	for _, k := range []string{"c", "d"} {
		assert.False(t, d.first(k, now.Add(4*time.Minute)), "%s is still remembered", k)
	}
	assert.True(t, d.first("d", now.Add(2*time.Minute+time.Hour)), "past the window a key logs again")
	assert.LessOrEqual(t, d.order.Len(), 3)
	assert.Equal(t, d.order.Len(), len(d.byKey))
}

// TestLegacyDeliveryFallback_OnlyDeliverPermissions pins the permission
// bound: the fallback admits only the three deliver permissions. The adopted
// cohort serves them beyond the V1 baseline and readonly rows on purpose
// (those rows never carry them; that is the ruled legacy gap), and a
// permission outside the deliver set is never tolerated, on an adopted hop
// or any other legacy hop.
func TestLegacyDeliveryFallback_OnlyDeliverPermissions(t *testing.T) {
	for _, role := range []AgentRole{AgentRoleReadOnly, AgentRoleBaseline} {
		row := lgV1Row(t, role)
		for _, p := range LaunchDeliveryCensusPermissions {
			assert.NotContains(t, row, p, "the V1 %s row lacks %s: the adopted cohort is the ruled gap", role, p)
		}
	}
	full := lgV1Row(t, AgentRoleFull)
	for _, p := range LaunchDeliveryCensusPermissions {
		assert.Contains(t, full, p)
	}

	adopted := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, Role: string(AgentRoleBaseline), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: store.ProvenanceVersionV1, SourceCredentialKind: store.SourceCredentialSystemMigration},
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: lgV1Row(t, AgentRoleBaseline)},
	}
	uat := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser, Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{ProvenanceVersion: store.ProvenanceVersionV1, SourceCredentialKind: store.SourceCredentialUAT},
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingBounded, PermissionIDs: []string{"agent.read"}},
	}
	for _, p := range LaunchDeliveryCensusPermissions {
		assert.Equal(t, legacyDeliveryCohortAdoptedBaseline, legacyBoundedEdgeCohort(adopted, p), p)
		assert.Equal(t, deliveryCohortUATNoDeliverMapping, legacyBoundedEdgeCohort(uat, p), p)
	}
	// Permissions outside the deliver set, in and out of the V1 rows.
	for _, p := range []string{"secret.use", "project.secret_read", "gcp_service_account.use", "agent.identity_token", "agent.create"} {
		assert.Empty(t, legacyBoundedEdgeCohort(adopted, p), p)
		assert.Empty(t, legacyBoundedEdgeCohort(uat, p), p)
	}

	// The fallback entry point refuses a non-deliver permission even for a
	// legacy agent (no edge) on a launch credential.
	f := newGoldenFixture(t)
	ctx := context.Background()
	id := tid("lg-perm-bound")
	hdAgent(t, f.store, id, f.projectAlpha.ID, []string{f.projectOwnerID})
	h := hdIdentity(t, f.authz, id)
	h.legacyFallback = true
	ok := f.authz.hubDeliveryAdmissionOutcomeFor(ctx, h, "secret.deliver", id, nil)
	assert.True(t, ok.ok, "control: a deliver permission is served: %s", ok.reason)
	for _, p := range []string{"secret.use", "project.secret_read", "agent.create"} {
		o := f.authz.hubDeliveryAdmissionOutcomeFor(ctx, h, p, id, nil)
		assert.False(t, o.ok, "%s: %s", p, o.reason)
		assert.Empty(t, o.legacyCohort, p)
	}

	// The walk itself: with a legacy note, a non-deliver permission gets
	// the strict result and no cohort is noted.
	strictOK, strictReason, strictCause := f.authz.hubDeliveryChainWalk(ctx, id, f.projectAlpha.ID, "secret.use", nil)
	var note hubDeliveryLegacyNote
	walkOK, walkReason, walkCause := f.authz.hubDeliveryWalk(ctx, id, f.projectAlpha.ID, "secret.use", nil, &note)
	assert.False(t, walkOK)
	assert.Equal(t, strictOK, walkOK)
	assert.Equal(t, strictReason, walkReason)
	assert.Equal(t, strictCause, walkCause)
	assert.Equal(t, DenyCauseCeilingProvenanceMissing, walkCause)
	assert.Empty(t, note.cohort, "no legacy hop is noted for a non-deliver permission")
	var deliverNote hubDeliveryLegacyNote
	deliverOK, _, _ := f.authz.hubDeliveryWalk(ctx, id, f.projectAlpha.ID, "secret.deliver", nil, &deliverNote)
	assert.True(t, deliverOK, "control: the tolerant walk serves a deliver permission")
	assert.Equal(t, legacyDeliveryCohortNoEdge, deliverNote.cohort)
}
