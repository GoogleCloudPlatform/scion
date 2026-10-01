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

package permissions

import (
	"reflect"
	"sort"
	"testing"
)

// TestLegacyUATScopeToPermissionID_Golden pins legacyUATScopeToPermissionID
// to an independent literal copy. A change to the map fails this test: the
// fix for a real interpretation change is a new CeilingVersion, never an
// edit to this table.
func TestLegacyUATScopeToPermissionID_Golden(t *testing.T) {
	golden := map[string]string{
		"access_constraint:manage":     "access_constraint.admin",
		"access_constraint:read":       "access_constraint.read",
		"agent:attach":                 "agent.attach",
		"agent:create":                 "agent.create",
		"agent:delete":                 "agent.delete",
		"agent:grant_hub_mode":         "agent.grant_hub_mode",
		"agent:identity_token":         "agent.identity_token",
		"agent:lifecycle":              "agent.lifecycle",
		"agent:list":                   "agent.list",
		"agent:log_append":             "agent.log_append",
		"agent:message":                "agent.message",
		"agent:notify":                 "agent.notify",
		"agent:port_access":            "agent.port_access",
		"agent:port_forward":           "agent.port_forward",
		"agent:read":                   "agent.read",
		"agent:set_message_mode":       "agent.set_message_mode",
		"agent:status_update":          "agent.status_update",
		"agent:stop_all":               "agent.stop_all",
		"agent:token_refresh":          "agent.token_refresh",
		"agent:update":                 "agent.update",
		"broker:create":                "broker.create",
		"broker:delete":                "broker.delete",
		"broker:dispatch":              "broker.dispatch",
		"broker:list":                  "broker.list",
		"broker:read":                  "broker.read",
		"broker:update":                "broker.update",
		"gcp_service_account:assign":   "gcp_service_account.assign",
		"gcp_service_account:create":   "gcp_service_account.create",
		"gcp_service_account:delete":   "gcp_service_account.delete",
		"gcp_service_account:list":     "gcp_service_account.list",
		"gcp_service_account:mint":     "gcp_service_account.mint",
		"gcp_service_account:read":     "gcp_service_account.read",
		"gcp_service_account:verify":   "gcp_service_account.verify",
		"group:addMember":              "group.addMember",
		"group:create":                 "group.create",
		"group:delete":                 "group.delete",
		"group:list":                   "group.list",
		"group:read":                   "group.read",
		"group:removeMember":           "group.removeMember",
		"group:update":                 "group.update",
		"harness_config:create":        "harness_config.create",
		"harness_config:delete":        "harness_config.delete",
		"harness_config:list":          "harness_config.list",
		"harness_config:read":          "harness_config.read",
		"harness_config:update":        "harness_config.update",
		"hub:execute":                  "hub.maintenance.execute",
		"hub:manage":                   "hub.audit.read",
		"hub:read":                     "hub.settings.read",
		"hub:update":                   "hub.settings.update",
		"policy:create":                "policy.create",
		"policy:delete":                "policy.delete",
		"policy:list":                  "policy.list",
		"policy:read":                  "policy.read",
		"policy:update":                "policy.update",
		"project:clone":                "project.clone",
		"project:create":               "project.create",
		"project:delete":               "project.delete",
		"project:list":                 "project.list",
		"project:manage":               "project.manage",
		"project:read":                 "project.read",
		"project:register":             "project.register",
		"project:secret_read":          "project.secret_read",
		"project:set_messaging_policy": "project.set_messaging_policy",
		"project:update":               "project.update",
		"quota:create":                 "quota.create",
		"quota:delete":                 "quota.delete",
		"quota:read":                   "quota.read",
		"quota:update":                 "quota.update",
		"role:create":                  "role.create",
		"role:delete":                  "role.delete",
		"role:read":                    "role.read",
		"role:update":                  "role.update",
		"role_binding:create":          "role_binding.create",
		"role_binding:delete":          "role_binding.delete",
		"role_binding:read":            "role_binding.read",
		"scheduled_event:create":       "scheduled_event.create",
		"scheduled_event:delete":       "scheduled_event.delete",
		"scheduled_event:list":         "scheduled_event.list",
		"scheduled_event:read":         "scheduled_event.read",
		"scheduled_event:update":       "scheduled_event.update",
		"skill:create":                 "skill.create",
		"skill:create_global":          "skill.create_global",
		"skill:delete":                 "skill.delete",
		"skill:list":                   "skill.list",
		"skill:read":                   "skill.read",
		"skill:register":               "skill.register",
		"skill:update":                 "skill.update",
		"template:create":              "template.create",
		"template:delete":              "template.delete",
		"template:list":                "template.list",
		"template:read":                "template.read",
		"template:update":              "template.update",
		"user:delete":                  "user.delete",
		"user:invite":                  "user.invite",
		"user:list":                    "user.list",
		"user:promote":                 "user.promote",
		"user:read":                    "user.read",
		"user:suspend":                 "user.suspend",
		"user:update":                  "user.update",
	}
	if !reflect.DeepEqual(golden, legacyUATScopeToPermissionID) {
		t.Fatalf("legacyUATScopeToPermissionID changed from its frozen snapshot.\ngolden: %v\nactual: %v\n"+
			"a real interpretation change needs a new CeilingVersion, not an edit to this table",
			golden, legacyUATScopeToPermissionID)
	}
}

// TestNormalizeLegacyUATScopes_AttachOnlyStaysAttachOnly pins that an
// unversioned row holding only agent:attach normalizes to exactly
// agent.attach — no lifecycle implication.
func TestNormalizeLegacyUATScopes_AttachOnlyStaysAttachOnly(t *testing.T) {
	ids := NormalizeLegacyUATScopes([]string{"agent:attach"})
	if len(ids) != 1 || ids[0] != "agent.attach" {
		t.Fatalf("expected exactly [agent.attach], got %v", ids)
	}
}

// TestNormalizeLegacyUATScopes_ManageAliasExpansion characterizes legacy
// manage-alias expansion separately: a legacy row's stored scopes always
// already reflect the mint-time expandScopes output (manage aliases were
// never stored raw), so normalizing them recovers exactly the resource's
// manage-alias scopes, still excluding attach/port_access.
func TestNormalizeLegacyUATScopes_ManageAliasExpansion(t *testing.T) {
	storedAfterMintTimeExpansion := UATManageScopesFor(ResourceAgent) // what expandScopes("agent:manage") would have persisted
	ids := NormalizeLegacyUATScopes(storedAfterMintTimeExpansion)
	sort.Strings(ids)

	want := make([]string, 0, len(storedAfterMintTimeExpansion))
	for _, scope := range storedAfterMintTimeExpansion {
		want = append(want, legacyUATScopeToPermissionID[scope])
	}
	sort.Strings(want)

	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, ids)
		}
	}
	for _, excluded := range []string{"agent.attach", "agent.port_access"} {
		for _, id := range ids {
			if id == excluded {
				t.Errorf("legacy agent:manage expansion must not include %s", excluded)
			}
		}
	}
}

// TestNormalizeLegacyUATScopes_UnrecognizedScopeDropped pins that an
// unrecognized scope narrows (is dropped) rather than denying or erroring
// the whole ceiling.
func TestNormalizeLegacyUATScopes_UnrecognizedScopeDropped(t *testing.T) {
	ids := NormalizeLegacyUATScopes([]string{"agent:read", "not:a-real-scope"})
	if len(ids) != 1 || ids[0] != "agent.read" {
		t.Fatalf("expected exactly [agent.read], got %v", ids)
	}
}

// TestNormalizeLegacyUATScopes_ImmuneToRegistryChanges proves — not just
// asserts — that normalizing a legacy row never consults the live, mutable
// selector registry. It mutates UATManageAliases so that, if
// NormalizeLegacyUATScopes were switched to call the live ResolveSelector,
// "agent:attach" would resolve to a materially different (and wider) set of
// permission IDs; it then confirms that mutation actually took effect on
// ResolveSelector before trusting the negative result on
// NormalizeLegacyUATScopes. Without the effectiveness check, this test
// would pass vacuously if the mutation were a no-op.
func TestNormalizeLegacyUATScopes_ImmuneToRegistryChanges(t *testing.T) {
	before := NormalizeLegacyUATScopes([]string{"agent:attach", "agent:read"})

	// agent:attach already exists as an ordinary UATScope (mapping only to
	// agent.attach). Retargeting it as a manage alias too makes the alias
	// candidate — built and inserted into the selector table AFTER the
	// plain UATScope entries — win the same map key, so a live resolution
	// of "agent:attach" would jump from {agent.attach} to the full
	// agent:manage expansion, which includes agent.lifecycle. That is
	// exactly the widening a live-resolution regression would produce.
	mutatedAliases := make(map[string]string, len(UATManageAliases)+1)
	for k, v := range UATManageAliases {
		mutatedAliases[k] = v
	}
	mutatedAliases["agent:attach"] = ResourceAgent
	t.Cleanup(OverrideSelectorInputsForTest(Registry, mutatedAliases))

	mutatedResolution, ok := ResolveSelector("agent:attach")
	if !ok {
		t.Fatal("test setup: expected agent:attach to still resolve after the alias mutation")
	}
	foundLifecycle := false
	for _, id := range mutatedResolution.PermissionIDs {
		if id == "agent.lifecycle" {
			foundLifecycle = true
		}
	}
	if !foundLifecycle {
		t.Fatalf("test setup: mutation was not effective — ResolveSelector(\"agent:attach\") = %v, expected it to include agent.lifecycle", mutatedResolution.PermissionIDs)
	}

	after := NormalizeLegacyUATScopes([]string{"agent:attach", "agent:read"})
	if len(before) != len(after) {
		t.Fatalf("alias mutation changed normalization result: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("alias mutation changed normalization result: before=%v after=%v", before, after)
		}
	}
	for _, id := range after {
		if id == "agent.lifecycle" {
			t.Error("legacy attach-only normalization must not pick up a live alias retarget that would add lifecycle")
		}
	}
}

// TestNormalizeLegacyUATScopes_TableShapes pins two specific legacy-row
// shapes the frozen-table comment relies on.
func TestNormalizeLegacyUATScopes_TableShapes(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   []string
	}{
		{
			// hub:settings:read was briefly mintable (b09e7f49b, removed the
			// same day by 943241adb) but is not a key in the frozen
			// snapshot, so a row that somehow stored it contributes nothing.
			name:   "briefly mintable hub scope contributes nothing",
			scopes: []string{"hub:settings:read"},
			want:   []string{},
		},
		{
			// agent:update is the Resource:Action pair of exactly one
			// Registry permission, which has no UATScope, so it maps to
			// that single permission.
			name:   "registry pair without a UATScope maps to its single permission",
			scopes: []string{"agent:update"},
			want:   []string{"agent.update"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeLegacyUATScopes(tt.scopes)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("expected %v, got %v", tt.want, got)
				}
			}
		})
	}
}
