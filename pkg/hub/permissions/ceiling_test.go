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

import "testing"

// TestFrozenPermissionCeiling_Allows_EmptyDenies pins that an empty
// PermissionIDs list denies every permission, for every recognized version —
// it is never interpreted as "unrestricted" (AC: "empty permission lists
// deny rather than become unrestricted").
func TestFrozenPermissionCeiling_Allows_EmptyDenies(t *testing.T) {
	for _, version := range []CeilingVersion{CeilingVersionUnspecified, CeilingVersionV1} {
		c := FrozenPermissionCeiling{Version: version}
		if c.Allows("agent.read") {
			t.Errorf("version %d: empty ceiling must deny agent.read", version)
		}
		if c.Allows("") {
			t.Errorf("version %d: empty ceiling must deny empty permission ID", version)
		}
	}
}

// TestFrozenPermissionCeiling_Allows_UnknownVersionDenies pins that an
// unrecognized CeilingVersion denies every permission, even one present in
// PermissionIDs (AC: "unknown versions ... deny rather than become
// unrestricted").
func TestFrozenPermissionCeiling_Allows_UnknownVersionDenies(t *testing.T) {
	c := FrozenPermissionCeiling{Version: CeilingVersion(99), PermissionIDs: []string{"agent.read"}}
	if c.Allows("agent.read") {
		t.Error("unknown ceiling version must deny even a listed permission")
	}
}

// TestFrozenPermissionCeiling_Allows_MembershipCheck pins straightforward
// membership behavior for a well-formed V1 ceiling.
func TestFrozenPermissionCeiling_Allows_MembershipCheck(t *testing.T) {
	c := FrozenPermissionCeiling{Version: CeilingVersionV1, PermissionIDs: []string{"agent.attach", "agent.read"}}
	if !c.Allows("agent.attach") {
		t.Error("expected agent.attach to be allowed")
	}
	if !c.Allows("agent.read") {
		t.Error("expected agent.read to be allowed")
	}
	if c.Allows("agent.lifecycle") {
		t.Error("expected agent.lifecycle to be denied (not in PermissionIDs, no implication expansion)")
	}
}

// TestBuildCeilingFromSelectors_ManageAliasExpandsExplicitStayOut pins that
// mint-time manage-alias expansion includes the alias's concrete scopes
// while attach/port_access — explicitly excluded from the alias — require
// separate, explicit selection.
func TestBuildCeilingFromSelectors_ManageAliasExpandsExplicitStayOut(t *testing.T) {
	ceiling, ok := BuildCeilingFromSelectors([]string{"agent:manage"})
	if !ok {
		t.Fatal("expected agent:manage to resolve")
	}
	if ceiling.Version != CeilingVersionV1 {
		t.Errorf("expected CeilingVersionV1, got %d", ceiling.Version)
	}
	if ceiling.Allows("agent.attach") {
		t.Error("agent:manage must not expand to agent.attach")
	}
	if ceiling.Allows("agent.port_access") {
		t.Error("agent:manage must not expand to agent.port_access")
	}
	if !ceiling.Allows("agent.read") {
		t.Error("agent:manage must expand to agent.read")
	}

	explicit, ok := BuildCeilingFromSelectors([]string{"agent:manage", "agent:attach", "agent:port_access"})
	if !ok {
		t.Fatal("expected explicit selectors to resolve")
	}
	if !explicit.Allows("agent.attach") || !explicit.Allows("agent.port_access") {
		t.Error("explicitly selected attach/port_access must be allowed when named directly")
	}
}

// TestBuildCeilingFromSelectors_UnknownSelectorFailsClosed pins that an
// unresolvable selector fails the whole mint rather than silently minting a
// partial ceiling.
func TestBuildCeilingFromSelectors_UnknownSelectorFailsClosed(t *testing.T) {
	if _, ok := BuildCeilingFromSelectors([]string{"not:a-real-selector"}); ok {
		t.Error("expected unknown selector to fail closed")
	}
}

// TestBuildCeilingFromSelectors_RegistryAdditionNeverWidensExistingToken
// pins the AC directly: a ceiling computed and frozen at one point in time
// must not change meaning just because the live Registry later grows. Since
// BuildCeilingFromSelectors is only used at mint time, the actual
// non-widening guarantee for an EXISTING token comes from freezing
// PermissionIDs once and never recomputing from ResolveSelector again — this
// test demonstrates that recomputing the same selector after a Registry
// mutation would (correctly) require a new call, and that a stored ceiling
// value (simulated here as a variable, standing in for a persisted column)
// is untouched by the mutation.
func TestBuildCeilingFromSelectors_RegistryAdditionNeverWidensExistingToken(t *testing.T) {
	frozen, ok := BuildCeilingFromSelectors([]string{"agent:read"})
	if !ok {
		t.Fatal("expected agent:read to resolve")
	}

	// Simulate a later Registry addition that could, if re-resolved live,
	// change what "agent:read" means (e.g. a hypothetical alias retarget).
	originalRegistry := Registry
	t.Cleanup(func() { Registry = originalRegistry })
	mutated := append([]Permission(nil), originalRegistry...)
	mutated = append(mutated, Permission{
		ID: "agent.read.v2", Resource: ResourceAgent, Action: ActionRead, UATScope: "agent:read",
	})
	Registry = mutated

	// The previously-frozen value must be completely unaffected: it holds a
	// plain []string, computed once, with no live dependency on Registry.
	if !frozen.Allows("agent.read") {
		t.Error("frozen ceiling must still allow agent.read")
	}
	if frozen.Allows("agent.read.v2") {
		t.Error("frozen ceiling must not gain the newly-added permission ID")
	}
}
