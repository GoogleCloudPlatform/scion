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

// Consistency and drift tests for permissions.RelationshipPolicies, the one
// relationship allowlist read by both the runtime relationship stage and
// token mint eligibility (ptone/scion#2119).

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knownRelationshipNames lists every relationship name a row may use.
// Association relationships are typed but have no rows until their
// permissions are registered (TestRelationshipPolicy_AssociationRowsRequireRegistryIDs).
var knownRelationshipNames = map[string]bool{
	"owner":                true,
	"ancestor":             true,
	"progeny":              true,
	"hub_member_sa_assign": true,
	"launcher":             true,
	"project_association":  true,
	"hub_association":      true,
	"broker_association":   true,
}

// relationshipPolicyPrincipalKinds is the principal-kind vocabulary rows use.
var relationshipPolicyPrincipalKinds = map[string]bool{"user": true, "agent": true}

// relationshipMintEligibleCells are the only cells that may carry
// MintEligible rows: owner/ancestor user attach and port access on agents.
var relationshipMintEligibleCells = map[relationshipAllowKey][]string{
	{"owner", "user", "agent"}:    {"agent.attach", "agent.port_access"},
	{"ancestor", "user", "agent"}: {"agent.attach", "agent.port_access"},
}

// readClassActions are the registry actions a ReadOnly row may name.
var readClassActions = map[string]bool{"read": true, "list": true, "verify": true, "secret_read": true}

// relationshipOwnerExcluded lists, per characterized cell, same-type registry
// permissions the relationship deliberately does not admit. Together with
// the allowlist it must cover every same-type registry permission, so a newly
// registered permission on one of these resource types fails
// TestRelationshipPolicy_DriftRequiresDecision until it is placed in one list.
var relationshipOwnerExcluded = map[relationshipAllowKey][]string{
	{"owner", "user", "agent"}: {},
	// Project.OwnerID grants nothing (ptone/scion#2586): project authority
	// comes only from project-scoped role bindings, so every project
	// permission is excluded from the owner relationship.
	{"owner", "user", "project"}: {
		"project.create", "project.read", "project.update", "project.delete",
		"project.manage", "project.register", "project.set_messaging_policy",
		"project.clone", "project.list", "project.secret_read",
	},
	{"owner", "user", "template"}:       {},
	{"owner", "user", "harness_config"}: {},
	{"owner", "user", "group"}:          {},
	{"owner", "user", "broker"}:         {},
	// gcp_service_account.use (ptone/scion#2129) is meant for an agent's own
	// token-mint request. It has no AgentScopes: the GCP token scope is per
	// service account and cannot be matched statically, so no credential
	// satisfies it until the slice that wires the token-mint check adds that
	// mapping. No user relationship, including ownership, should grant it.
	{"owner", "user", "gcp_service_account"}: {"gcp_service_account.use"},
	{"owner", "user", "skill"}:               {},
	{"ancestor", "user", "agent"}:            {},
	// Agent ancestors: permissions with no agent JWT scope.
	{"ancestor", "agent", "agent"}: {
		"agent.read", "agent.list", "agent.update", "agent.port_access", "agent.stop_all",
		"agent.message", "agent.grant_hub_mode",
	},
	// Launcher status read (ptone/scion#3409): agent.read only.
	{"launcher", "agent", "agent"}: {
		"agent.create", "agent.list", "agent.update", "agent.delete", "agent.attach",
		"agent.lifecycle", "agent.port_access", "agent.stop_all", "agent.message",
		"agent.set_message_mode", "agent.grant_hub_mode", "agent.status_update",
		"agent.log_append", "agent.notify", "agent.token_refresh", "agent.port_forward",
		"agent.identity_token",
	},
	{"hub_member_sa_assign", "user", "gcp_service_account"}: {
		"gcp_service_account.create", "gcp_service_account.read", "gcp_service_account.delete",
		"gcp_service_account.list", "gcp_service_account.verify", "gcp_service_account.mint",
		"gcp_service_account.use",
	},
	{"progeny", "agent", "skill"}: {
		"skill.create", "skill.create_global", "skill.update", "skill.delete", "skill.list", "skill.register",
	},
	// Progeny material pairs (ptone/scion#2129). Every secret and env_var
	// permission is admitted; skill_injection.deliver is granted by
	// skill_default, never by progeny.
	{"progeny", "agent", "secret"}:          {},
	{"progeny", "agent", "env_var"}:         {},
	{"progeny", "agent", "skill_injection"}: {"skill_injection.deliver"},
}

func policyCellPermissions() map[relationshipAllowKey]map[string]bool {
	out := map[relationshipAllowKey]map[string]bool{}
	for _, row := range permissions.RelationshipPolicies {
		for _, kind := range row.PrincipalKinds {
			key := relationshipAllowKey{row.Relationship, kind, row.ResourceType}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			for _, id := range row.PermissionIDs {
				out[key][id] = true
			}
		}
	}
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRelationshipPolicy_MatchesCharacterization asserts the policy rows and
// the characterization table describe exactly the same cells and permissions.
func TestRelationshipPolicy_MatchesCharacterization(t *testing.T) {
	cells := policyCellPermissions()
	for key, ids := range relationshipCharacterizedAllowlist {
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		assert.Equal(t, sortedBoolKeys(want), sortedBoolKeys(cells[key]), "cell %v", key)
	}
	for key := range cells {
		_, ok := relationshipCharacterizedAllowlist[key]
		assert.True(t, ok, "policy cell %v has no characterization entry", key)
	}
}

// TestRelationshipPolicy_RuntimeMatchesTable asserts RelationshipPolicyAllows
// answers exactly the table: every listed permission is admitted, and every
// other registry permission is not, for each cell.
func TestRelationshipPolicy_RuntimeMatchesTable(t *testing.T) {
	for key := range relationshipCharacterizedAllowlist {
		want := characterizedSet(key)
		for _, p := range permissions.Registry {
			got := permissions.RelationshipPolicyAllows(key.Relationship, key.PrincipalKind, key.ResourceType, p.ID)
			assert.Equal(t, want[p.ID], got, "cell %v permission %s", key, p.ID)
		}
		for id := range relationshipUnregisteredPermissions {
			got := permissions.RelationshipPolicyAllows(key.Relationship, key.PrincipalKind, key.ResourceType, id)
			assert.Equal(t, want[id], got, "cell %v unregistered permission %s", key, id)
		}
	}
	// Principal kinds outside the vocabulary match nothing.
	for _, kind := range []string{"dev", "federated_user", "federated_agent", "broker", ""} {
		assert.False(t, permissions.RelationshipPolicyAllows("owner", kind, "agent", "agent.read"), kind)
	}
}

// TestRelationshipPolicy_Consistency checks every row's shape.
func TestRelationshipPolicy_Consistency(t *testing.T) {
	registered := map[string]permissions.Permission{}
	for _, p := range permissions.Registry {
		registered[p.ID] = p
	}
	resolverUnregistered := map[string]bool{}
	for _, id := range unregisteredResourcePermissions {
		resolverUnregistered[id] = true
	}

	type rowCell struct {
		key      relationshipAllowKey
		mint     bool
		readOnly bool
	}
	seenCells := map[rowCell]bool{}
	cellRows := map[relationshipAllowKey]int{}
	cellPerms := map[relationshipAllowKey]map[string]bool{}

	for i, row := range permissions.RelationshipPolicies {
		assert.True(t, knownRelationshipNames[row.Relationship], "row %d: unknown relationship %q", i, row.Relationship)
		assert.NotEmpty(t, row.ResourceType, "row %d: resource type must be explicit", i)
		assert.NotEqual(t, "*", row.ResourceType, "row %d", i)
		require.NotEmpty(t, row.PrincipalKinds, "row %d: principal kinds", i)
		require.NotEmpty(t, row.PermissionIDs, "row %d: permissions", i)

		for _, kind := range row.PrincipalKinds {
			assert.True(t, relationshipPolicyPrincipalKinds[kind], "row %d: principal kind %q", i, kind)
			key := relationshipAllowKey{row.Relationship, kind, row.ResourceType}

			// At most two rows per cell, at most one row per (cell,
			// MintEligible, ReadOnly), and rows sharing a cell name
			// disjoint permissions (the RelationshipPolicy doc contract).
			cellRows[key]++
			assert.LessOrEqual(t, cellRows[key], 2, "row %d: more than two rows for %v", i, key)
			rc := rowCell{key, row.MintEligible, row.ReadOnly}
			assert.False(t, seenCells[rc], "row %d: duplicate row for %v mint=%v readOnly=%v", i, key, row.MintEligible, row.ReadOnly)
			seenCells[rc] = true
			if cellPerms[key] == nil {
				cellPerms[key] = map[string]bool{}
			}
			for _, id := range row.PermissionIDs {
				assert.False(t, cellPerms[key][id], "row %d: %s listed twice for %v", i, id, key)
				cellPerms[key][id] = true
			}

			if row.MintEligible {
				allowed := map[string]bool{}
				for _, id := range relationshipMintEligibleCells[key] {
					allowed[id] = true
				}
				for _, id := range row.PermissionIDs {
					assert.True(t, allowed[id], "row %d: %s may not be mint eligible for %v", i, id, key)
				}
			}
		}

		for _, id := range row.PermissionIDs {
			p, ok := registered[id]
			if !ok {
				_, reviewed := relationshipUnregisteredPermissions[id]
				assert.True(t, reviewed, "row %d: %s is not registered", i, id)
				assert.True(t, resolverUnregistered[id], "row %d: %s is not a resolver-reviewed unregistered ID", i, id)
				assert.False(t, row.ReadOnly, "row %d: read-only rows name registered permissions only", i)
				continue
			}
			if p.Resource != row.ResourceType {
				reviewed := false
				for _, kind := range row.PrincipalKinds {
					if _, ok := relationshipCrossTypePermissions[relationshipAllowKey{row.Relationship, kind, row.ResourceType}][id]; ok {
						reviewed = true
					}
				}
				assert.True(t, reviewed, "row %d: %s is registered on %s, row resource type is %s", i, id, p.Resource, row.ResourceType)
			}
			if row.ReadOnly {
				assert.True(t, readClassActions[p.Action], "row %d: read-only row names %s (action %s)", i, id, p.Action)
			}
		}
	}

	// Progeny rows (including personal skills) are read only, or name only
	// reviewed exact pairs (progenyExactPairRowViolations).
	assert.Empty(t, progenyExactPairRowViolations(permissions.RelationshipPolicies))
}

// reviewedProgenyExactPairs is the reviewed table of permissions a
// non-read-only progeny row may name, per cell, with the action the
// permission carries (F design f2-material-selection section 4.6 and 4.8;
// ptone/scion#2129). It must equal the runtime gate progenyExactPairs, and
// every entry must be used by a row (TestProgenyExactPairs_TableMatchesRowsAndGate).
var reviewedProgenyExactPairs = map[relationshipAllowKey]map[string]Action{
	{Relationship: "progeny", PrincipalKind: "agent", ResourceType: "secret"}: {
		"secret.use":     ActionUse,
		"secret.deliver": ActionDeliver,
	},
	{Relationship: "progeny", PrincipalKind: "agent", ResourceType: "env_var"}: {
		"env_var.deliver": ActionDeliver,
	},
}

// progenyExactPairRowViolations lists every permission in a non-read-only
// progeny row that is not a reviewed exact pair for its cell, or whose
// registry action or resource type differs from the reviewed entry.
func progenyExactPairRowViolations(rows []permissions.RelationshipPolicy) []string {
	var out []string
	for i, row := range rows {
		if row.Relationship != "progeny" || row.ReadOnly {
			continue
		}
		for _, kind := range row.PrincipalKinds {
			key := relationshipAllowKey{row.Relationship, kind, row.ResourceType}
			for _, id := range row.PermissionIDs {
				want, ok := reviewedProgenyExactPairs[key][id]
				if !ok {
					out = append(out, fmt.Sprintf("row %d: %s is not a reviewed progeny pair for %v", i, id, key))
					continue
				}
				p, registered := registryPermission(id)
				if !registered || Action(p.Action) != want || p.Resource != row.ResourceType {
					out = append(out, fmt.Sprintf("row %d: %s does not match its registry entry (%s/%s)", i, id, p.Resource, p.Action))
				}
			}
		}
	}
	return out
}

// TestProgenyExactPairs_TableMatchesRowsAndGate pins the reviewed table: it
// equals the runtime gate progenyExactPairs, and every entry is named by a
// non-read-only progeny row (no stale entries).
func TestProgenyExactPairs_TableMatchesRowsAndGate(t *testing.T) {
	flat := map[string]Action{}
	for key, pairs := range reviewedProgenyExactPairs {
		for id, action := range pairs {
			flat[id] = action
			used := false
			for _, row := range permissions.RelationshipPolicies {
				if row.Relationship != key.Relationship || row.ResourceType != key.ResourceType || row.ReadOnly {
					continue
				}
				if containsTestString(row.PrincipalKinds, key.PrincipalKind) && containsTestString(row.PermissionIDs, id) {
					used = true
				}
			}
			assert.True(t, used, "stale reviewed progeny pair %v %s", key, id)
		}
	}
	assert.Equal(t, flat, progenyExactPairs, "reviewed table and runtime gate differ")
}

// A non-read-only progeny row naming a permission outside the reviewed
// table, or a reviewed permission in the wrong cell, is reported.
func TestProgenyExactPairs_UnreviewedRowRejected(t *testing.T) {
	rows := []permissions.RelationshipPolicy{
		{Relationship: "progeny", PrincipalKinds: []string{"agent"}, ResourceType: "secret", PermissionIDs: []string{"secret.use"}},
		{Relationship: "progeny", PrincipalKinds: []string{"agent"}, ResourceType: "skill_injection", PermissionIDs: []string{"skill_injection.deliver"}},
		{Relationship: "progeny", PrincipalKinds: []string{"agent"}, ResourceType: "gcp_service_account", PermissionIDs: []string{"gcp_service_account.use"}},
		{Relationship: "progeny", PrincipalKinds: []string{"agent"}, ResourceType: "env_var", PermissionIDs: []string{"secret.deliver"}},
	}
	got := progenyExactPairRowViolations(rows)
	assert.Len(t, got, 3, "%v", got)
	for _, v := range got {
		assert.NotContains(t, v, "row 0:", "the reviewed pair is accepted")
	}
}

func containsTestString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestRelationshipPolicy_MintEligibleUnchanged pins mint eligibility to the
// owner/ancestor user attach and port access cells.
func TestRelationshipPolicy_MintEligibleUnchanged(t *testing.T) {
	for _, p := range permissions.Registry {
		for _, rel := range []string{"owner", "ancestor", "progeny", "hub_member_sa_assign"} {
			for _, kind := range []string{"user", "agent"} {
				for _, rt := range []string{"agent", "project", "template", "harness_config", "group", "broker", "gcp_service_account", "skill", "secret", "env_var", "skill_injection"} {
					want := false
					for _, id := range relationshipMintEligibleCells[relationshipAllowKey{rel, kind, rt}] {
						if id == p.ID {
							want = true
						}
					}
					got := permissions.RelationshipPolicyMintEligible(rel, kind, rt, p.ID)
					assert.Equal(t, want, got, "%s/%s/%s %s", rel, kind, rt, p.ID)
				}
			}
		}
	}
}

// TestRelationshipPolicy_DriftRequiresDecision fails when a registry
// permission on a characterized resource type is neither admitted nor
// explicitly excluded for that cell.
func TestRelationshipPolicy_DriftRequiresDecision(t *testing.T) {
	for key, excluded := range relationshipOwnerExcluded {
		allowed := characterizedSet(key)
		ex := map[string]bool{}
		for _, id := range excluded {
			ex[id] = true
			assert.False(t, allowed[id], "cell %v: %s is both admitted and excluded", key, id)
		}
		for _, p := range sameTypeRegistryPermissions(key.ResourceType) {
			assert.True(t, allowed[p.ID] || ex[p.ID],
				"cell %v: registry permission %s needs a decision (admit it in relationshipCharacterizedAllowlist and RelationshipPolicies, or list it in relationshipOwnerExcluded)", key, p.ID)
		}
	}
	// Every same-type characterized cell has a drift entry.
	for key := range relationshipCharacterizedAllowlist {
		if _, crossType := relationshipCrossTypePermissions[key]; crossType {
			continue
		}
		_, ok := relationshipOwnerExcluded[key]
		assert.True(t, ok, "cell %v has no drift entry", key)
	}
}

// TestRelationshipPolicy_AssociationRowsRequireRegistryIDs guards the
// association relationships: a row may exist only when every permission it
// names is registered on its resource type.
func TestRelationshipPolicy_AssociationRowsRequireRegistryIDs(t *testing.T) {
	registered := map[string]permissions.Permission{}
	for _, p := range permissions.Registry {
		registered[p.ID] = p
	}
	for _, row := range permissions.RelationshipPolicies {
		switch row.Relationship {
		case "project_association", "hub_association", "broker_association":
			for _, id := range row.PermissionIDs {
				p, ok := registered[id]
				assert.True(t, ok, "%s row names unregistered %s", row.Relationship, id)
				if ok {
					assert.Equal(t, row.ResourceType, p.Resource, "%s row %s", row.Relationship, id)
				}
			}
		}
	}
}

// TestUnregisteredResourcePermissions_NotRegistered asserts every reviewed
// unregistered pair is really absent from the registry, so the registry
// answer is never shadowed.
func TestUnregisteredResourcePermissions_NotRegistered(t *testing.T) {
	for k, id := range unregisteredResourcePermissions {
		for _, p := range permissions.Registry {
			assert.False(t, p.Resource == k.ResourceType && p.Action == string(k.Action),
				"(%s, %s) is registered as %s", k.ResourceType, k.Action, p.ID)
			assert.NotEqual(t, id, p.ID)
		}
	}
}

// TestDecide_UnresolvablePermissionDenied asserts a request whose (resource
// type, action) pair names no single permission is denied, including for the
// resource owner.
func TestDecide_UnresolvablePermissionDenied(t *testing.T) {
	authz, s := authzTestSetup(t)
	owner := createCharacterizationUser(t, s, tid("unresolvable-owner"))
	res := Resource{Type: "template", ID: tid("unresolvable-tpl"), OwnerID: owner.ID()}

	d := authz.CheckAccess(context.Background(), owner, res, Action("frobnicate"))
	assert.False(t, d.Allowed)
	assert.Equal(t, unresolvablePermissionReason, d.Reason)

	d = authz.CheckAccess(context.Background(), owner, Resource{Type: "hub", ID: "hub"}, ActionRead)
	assert.False(t, d.Allowed)
	assert.Equal(t, unresolvablePermissionReason, d.Reason)

	// A resolvable pair on the same resource is still admitted.
	d = authz.CheckAccess(context.Background(), owner, res, ActionRead)
	assert.True(t, d.Allowed, d.Reason)
}
