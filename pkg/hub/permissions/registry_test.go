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

// expectedSelectorRegistry pins today's full derived selector set. A human
// must update this table — an explicit review act — whenever a Registry
// change adds, removes, or retargets a UATScope or manage-alias member.
var expectedSelectorRegistry = map[string][]string{
	"agent:attach":               {"agent.attach"},
	"agent:create":               {"agent.create"},
	"agent:delete":               {"agent.delete"},
	"agent:lifecycle":            {"agent.lifecycle"},
	"agent:list":                 {"agent.list"},
	"agent:manage":               {"agent.create", "agent.delete", "agent.lifecycle", "agent.list", "agent.message", "agent.read"},
	"agent:message":              {"agent.message"},
	"agent:port_access":          {"agent.port_access"},
	"agent:read":                 {"agent.read"},
	"broker:list":                {"broker.list"},
	"broker:read":                {"broker.read"},
	"gcp_service_account:assign": {"gcp_service_account.assign"},
	"gcp_service_account:list":   {"gcp_service_account.list"},
	"gcp_service_account:read":   {"gcp_service_account.read"},
	"gcp_service_account:verify": {"gcp_service_account.verify"},
	"group:addMember":            {"group.addMember"},
	"group:create":               {"group.create"},
	"group:delete":               {"group.delete"},
	"group:list":                 {"group.list"},
	"group:manage":               {"group.addMember", "group.create", "group.delete", "group.list", "group.read", "group.removeMember", "group.update"},
	"group:read":                 {"group.read"},
	"group:removeMember":         {"group.removeMember"},
	"group:update":               {"group.update"},
	"harness_config:create":      {"harness_config.create"},
	"harness_config:delete":      {"harness_config.delete"},
	"harness_config:list":        {"harness_config.list"},
	"harness_config:manage":      {"harness_config.create", "harness_config.delete", "harness_config.list", "harness_config.read", "harness_config.update"},
	"harness_config:read":        {"harness_config.read"},
	"harness_config:update":      {"harness_config.update"},
	"project:clone":              {"project.clone"},
	"project:manage":             {"project.manage"},
	"project:read":               {"project.read"},
	"project:update":             {"project.update"},
	"skill:create":               {"skill.create"},
	"skill:delete":               {"skill.delete"},
	"skill:list":                 {"skill.list"},
	"skill:manage":               {"skill.create", "skill.delete", "skill.list", "skill.read", "skill.register", "skill.update"},
	"skill:read":                 {"skill.read"},
	"skill:register":             {"skill.register"},
	"skill:update":               {"skill.update"},
	"template:create":            {"template.create"},
	"template:delete":            {"template.delete"},
	"template:list":              {"template.list"},
	"template:manage":            {"template.create", "template.delete", "template.list", "template.read", "template.update"},
	"template:read":              {"template.read"},
	"template:update":            {"template.update"},
	"user:invite":                {"user.invite"},
	"user:list":                  {"user.list"},
	"user:read":                  {"user.read"},
}

func TestValidateSelectorRegistry_PinnedSnapshot(t *testing.T) {
	if err := ValidateSelectorRegistry(expectedSelectorRegistry); err != nil {
		t.Fatalf("selector registry drifted from pinned snapshot: %v\nIf this drift is an intentional, reviewed Registry change, update expectedSelectorRegistry to match.", err)
	}
}

// TestResolveSelector_UnknownSelectorsFailClosed proves unknown, unmapped,
// and malformed-looking selectors are rejected rather than silently
// resolving through a resource:action fallback.
func TestResolveSelector_UnknownSelectorsFailClosed(t *testing.T) {
	for _, selector := range []string{
		"",
		"nonsense",
		"agent:frobnicate",
		"hub:read",          // exactly the resource:action reconstruction a fallback would accept
		"broker:create",     // no UATScope on broker.create today; not yet a selector (D.0a/D.1 add it)
		"hub.settings:read", // no such literal UATScope exists
	} {
		if _, ok := ResolveSelector(selector); ok {
			t.Errorf("ResolveSelector(%q) = ok=true, want ok=false (unknown/unreviewed selector)", selector)
		}
	}
}

// TestResolveSelector_SharedResourceActionCannotCollapse is the direct
// regression for the A.1 acceptance criterion "two hub permissions sharing
// resource/action cannot collapse into one selector." hub.settings.read and
// hub.config.read are real Registry entries that already share
// {Resource: hub, Action: read} today. A resource:action reconstruction
// (like useraccesstoken.go's scopeToPermissionIDs) would map the single
// selector string "hub:read" to BOTH permission IDs at once. ResolveSelector
// must not do that: it has no resource:action path at all, so "hub:read"
// resolves to nothing rather than to an ambiguous pair.
func TestResolveSelector_SharedResourceActionCannotCollapse(t *testing.T) {
	var settingsRead, configRead *Permission
	for i := range Registry {
		switch Registry[i].ID {
		case "hub.settings.read":
			settingsRead = &Registry[i]
		case "hub.config.read":
			configRead = &Registry[i]
		}
	}
	if settingsRead == nil || configRead == nil {
		t.Fatal("expected both hub.settings.read and hub.config.read to exist in Registry")
	}
	if settingsRead.Resource != configRead.Resource || settingsRead.Action != configRead.Action {
		t.Fatalf("test fixture assumption broken: hub.settings.read and hub.config.read no longer share {Resource, Action} (%s:%s vs %s:%s) -- the collision-risk scenario this test guards no longer exists in Registry; update or remove this test with a currently-colliding pair",
			settingsRead.Resource, settingsRead.Action, configRead.Resource, configRead.Action)
	}

	// The naive resource:action reconstruction (what scopeToPermissionIDs
	// does today) WOULD match both permissions for a single scope key.
	// Demonstrate that fact so the contrast with ResolveSelector is legible.
	scopeKey := settingsRead.Resource + ":" + settingsRead.Action
	var naiveMatches []string
	for _, p := range Registry {
		if p.Resource+":"+p.Action == scopeKey {
			naiveMatches = append(naiveMatches, p.ID)
		}
	}
	if len(naiveMatches) < 2 {
		t.Fatalf("expected the naive resource:action reconstruction to collide on %q, got %v", scopeKey, naiveMatches)
	}

	// ResolveSelector must not reproduce that collision: neither permission
	// has a UATScope today, so the selector string built the same way
	// resolves to nothing, not to an ambiguous pair.
	if _, ok := ResolveSelector(scopeKey); ok {
		t.Fatalf("ResolveSelector(%q) unexpectedly resolved; it must never derive from resource:action", scopeKey)
	}
}

// TestProjectTargetApplicability_CoversEveryRegistryPermission is the drift
// test for the hand-reviewed applicability table: every permission ID in
// Registry must have an explicit disposition. An unreviewed ID is a bug.
func TestProjectTargetApplicability_CoversEveryRegistryPermission(t *testing.T) {
	for _, p := range Registry {
		if _, reviewed := AppliesToExistingProjectTarget(p.ID); !reviewed {
			t.Errorf("permission %q has no ProjectTargetApplicability entry; every Registry permission must be explicitly reviewed", p.ID)
		}
	}
	for id := range ProjectTargetApplicability {
		found := false
		for _, p := range Registry {
			if p.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProjectTargetApplicability has entry %q with no matching Registry permission (stale entry)", id)
		}
	}
}

// TestProjectTargetApplicability_ArbitrarySystemPermissionNotProjectAccess
// pins the literal fix for "an arbitrary system permission is not project
// access": broker.create, project.create, and skill.create_global must all
// be reviewed false, since none of them apply to an existing project
// target.
func TestProjectTargetApplicability_ArbitrarySystemPermissionNotProjectAccess(t *testing.T) {
	for _, id := range []string{"broker.create", "project.create", "skill.create_global"} {
		applies, reviewed := AppliesToExistingProjectTarget(id)
		if !reviewed {
			t.Fatalf("permission %q must have a reviewed ProjectTargetApplicability entry", id)
		}
		if applies {
			t.Errorf("permission %q must be reviewed false (it does not apply to an existing project target)", id)
		}
	}
	applies, reviewed := AppliesToExistingProjectTarget("agent.delete")
	if !reviewed || !applies {
		t.Errorf("permission agent.delete must be reviewed true (it applies to an existing project target)")
	}
}

// TestPermissionAllowedBoundaries_CoversEveryUATScope is the drift test for
// the hand-reviewed boundary table: every permission with a non-empty
// UATScope must have an explicit SelectorAllowedBoundaries entry.
func TestPermissionAllowedBoundaries_CoversEveryUATScope(t *testing.T) {
	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		if _, reviewed := SelectorAllowedBoundaries(p.ID); !reviewed {
			t.Errorf("permission %q (UATScope %q) has no PermissionAllowedBoundaries entry", p.ID, p.UATScope)
		}
	}
}

// TestSelectorAllowedBoundaries_AliasIsIntersectionOfMembers proves the
// alias boundary rule: a manage alias's AllowedBoundaries is the
// intersection across every expanded member, not a separate guess. skill:
// manage includes skill.register, which is Hub-only, so skill:manage must
// be Hub-only too even though most skill.* permissions allow both.
func TestSelectorAllowedBoundaries_AliasIsIntersectionOfMembers(t *testing.T) {
	m, ok := ResolveSelector("skill:manage")
	if !ok {
		t.Fatal("expected skill:manage to resolve")
	}
	if len(m.AllowedBoundaries) != 1 || m.AllowedBoundaries[0] != BoundaryKindHub {
		t.Errorf("skill:manage AllowedBoundaries = %v, want [hub] (skill.register is Hub-only and must constrain the alias)", m.AllowedBoundaries)
	}

	m, ok = ResolveSelector("agent:manage")
	if !ok {
		t.Fatal("expected agent:manage to resolve")
	}
	foundProject, foundHub := false, false
	for _, b := range m.AllowedBoundaries {
		if b == BoundaryKindProject {
			foundProject = true
		}
		if b == BoundaryKindHub {
			foundHub = true
		}
	}
	if !foundProject || !foundHub {
		t.Errorf("agent:manage AllowedBoundaries = %v, want both project and hub (no Hub-only member in this alias)", m.AllowedBoundaries)
	}
}

// TestMintEligibilityRegistry_AttachAndPortAccess pins the reviewed
// disposition #2092 depends on: agent.attach and agent.port_access are
// mintable before any target exists, via a relationship source only (no
// stock role grants them flatly).
func TestMintEligibilityRegistry_AttachAndPortAccess(t *testing.T) {
	for _, id := range []string{"agent.attach", "agent.port_access"} {
		d, ok := MintEligibilityRegistry[id]
		if !ok {
			t.Fatalf("expected MintEligibilityRegistry entry for %q", id)
		}
		if d.RequiresExistingTarget {
			t.Errorf("%q: RequiresExistingTarget = true, want false (mint before first agent exists)", id)
		}
		foundRelationship := false
		for _, src := range d.Sources {
			if src.Kind == MintEligibilityRelationship && len(src.RelationshipTypes) > 0 {
				foundRelationship = true
			}
		}
		if !foundRelationship {
			t.Errorf("%q: expected at least one MintEligibilityRelationship source with non-empty RelationshipTypes", id)
		}
	}
}
