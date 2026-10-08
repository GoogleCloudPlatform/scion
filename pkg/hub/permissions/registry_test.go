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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
)

var (
	_ credentialmeta.BoundaryKind = BoundaryKindProject
	_ BoundaryKind                = credentialmeta.BoundaryProject
)

func TestBoundaryKindsAliasCanonicalContract(t *testing.T) {
	t.Parallel()

	serverKinds := []credentialmeta.BoundaryKind{BoundaryKindProject, BoundaryKindHub}
	if want := credentialmeta.BoundaryKinds(); !reflect.DeepEqual(serverKinds, want) {
		t.Fatalf("server boundary kinds = %v, canonical kinds = %v", serverKinds, want)
	}
}

// expectedSelectorRegistry pins today's full derived selector set. A human
// must update this table — an explicit review act — whenever a Registry
// change adds, removes, or retargets a UATScope or manage-alias member.
var expectedSelectorRegistry = map[string][]string{
	"agent:attach":                {"agent.attach"},
	"agent:create":                {"agent.create"},
	"agent:delete":                {"agent.delete"},
	"agent:lifecycle":             {"agent.lifecycle"},
	"agent:list":                  {"agent.list"},
	"agent:manage":                {"agent.create", "agent.delete", "agent.lifecycle", "agent.list", "agent.message", "agent.read"},
	"agent:message":               {"agent.message"},
	"agent:port_access":           {"agent.port_access"},
	"agent:read":                  {"agent.read"},
	"artifact:create":             {"artifact.create"},
	"artifact:delete":             {"artifact.delete"},
	"artifact:manage":             {"artifact.manage"},
	"artifact:read":               {"artifact.read"},
	"artifact:update":             {"artifact.update"},
	"broker:create":               {"broker.create"},
	"broker:list":                 {"broker.list"},
	"broker:read":                 {"broker.read"},
	"gcp_service_account:assign":  {"gcp_service_account.assign"},
	"gcp_service_account:list":    {"gcp_service_account.list"},
	"gcp_service_account:read":    {"gcp_service_account.read"},
	"gcp_service_account:verify":  {"gcp_service_account.verify"},
	"group:addMember":             {"group.addMember"},
	"group:create":                {"group.create"},
	"group:delete":                {"group.delete"},
	"group:list":                  {"group.list"},
	"group:manage":                {"group.addMember", "group.create", "group.delete", "group.list", "group.read", "group.removeMember", "group.update"},
	"group:read":                  {"group.read"},
	"group:removeMember":          {"group.removeMember"},
	"group:update":                {"group.update"},
	"harness_config:create":       {"harness_config.create"},
	"harness_config:delete":       {"harness_config.delete"},
	"harness_config:list":         {"harness_config.list"},
	"harness_config:manage":       {"harness_config.create", "harness_config.delete", "harness_config.list", "harness_config.read", "harness_config.update"},
	"harness_config:read":         {"harness_config.read"},
	"harness_config:update":       {"harness_config.update"},
	"hub_config:read":             {"hub.config.read"},
	"hub_config:update":           {"hub.config.update"},
	"hub_experiments:update":      {"hub.experiments.update"},
	"hub_lifecycle_hooks:read":    {"hub.lifecycle_hooks.read"},
	"hub_lifecycle_hooks:update":  {"hub.lifecycle_hooks.update"},
	"hub_messaging:update":        {"hub.messaging.update"},
	"hub_project_defaults:read":   {"hub.project_defaults.read"},
	"hub_project_defaults:update": {"hub.project_defaults.update"},
	"hub_settings:update":         {"hub.settings.update"},
	"inbox:read":                  {"inbox.read"},
	"inbox:write":                 {"inbox.write"},
	"project:clone":               {"project.clone"},
	"project:manage":              {"project.manage"},
	"project:read":                {"project.read"},
	"project:update":              {"project.update"},
	"skill:create":                {"skill.create"},
	"skill:delete":                {"skill.delete"},
	"skill:list":                  {"skill.list"},
	"skill:manage":                {"skill.create", "skill.delete", "skill.list", "skill.read", "skill.register", "skill.update"},
	"skill:read":                  {"skill.read"},
	"skill:register":              {"skill.register"},
	"skill:update":                {"skill.update"},
	"template:create":             {"template.create"},
	"template:delete":             {"template.delete"},
	"template:list":               {"template.list"},
	"template:manage":             {"template.create", "template.delete", "template.list", "template.read", "template.update"},
	"template:read":               {"template.read"},
	"template:update":             {"template.update"},
	"user:invite":                 {"user.invite"},
	"user:list":                   {"user.list"},
	"user:read":                   {"user.read"},
	"user_skill_injection:update": {"user_skill_injection.update"},

	// Project messaging policy (owner rule applies on top of the selector).
	"project:set_messaging_policy": {"project.set_messaging_policy"},
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
// would map the single selector string "hub:read" to BOTH permission IDs
// at once. ResolveSelector must not do that: it has no resource:action
// path at all, so "hub:read" resolves to nothing rather than to an
// ambiguous pair.
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

	// A naive resource:action reconstruction WOULD match both permissions
	// for a single scope key. Demonstrate that fact so the contrast with
	// ResolveSelector is legible.
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

	// ResolveSelector must not reproduce that collision: selectors are
	// literal UATScope values (hub.config.read's is hub_config:read), so the
	// selector string built the same way resolves to nothing, not to an
	// ambiguous pair.
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
// pins the rule "an arbitrary system permission is not project access":
// broker.create, project.create, and skill.create_global must all be
// reviewed false, since none of them apply to an existing project target.
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

// permissionAllowedBoundariesPreReviewedWithoutUATScope is the explicit
// allowlist for a PermissionAllowedBoundaries key that has no
// Permission.UATScope: a boundary entry that precedes its selector. A key
// on neither this list nor a Registry row with a non-empty UATScope is
// stale and must be removed.
var permissionAllowedBoundariesPreReviewedWithoutUATScope = map[string]bool{
	// No entries.
}

// TestPermissionAllowedBoundaries_NoStaleKeys is the reverse of the coverage
// test above: every PermissionAllowedBoundaries key must correspond to a
// real, current Registry permission with a non-empty UATScope, or be an
// explicit pre-reviewed exception — never a permission that was renamed,
// removed, or never had a selector at all.
func TestPermissionAllowedBoundaries_NoStaleKeys(t *testing.T) {
	byID := make(map[string]Permission, len(Registry))
	for _, p := range Registry {
		byID[p.ID] = p
	}
	for key := range PermissionAllowedBoundaries {
		if permissionAllowedBoundariesPreReviewedWithoutUATScope[key] {
			if _, ok := byID[key]; !ok {
				t.Errorf("PermissionAllowedBoundaries pre-reviewed key %q is not a Registry permission at all", key)
			}
			continue
		}
		p, ok := byID[key]
		if !ok {
			t.Errorf("PermissionAllowedBoundaries has stale key %q with no matching Registry permission", key)
			continue
		}
		if p.UATScope == "" {
			t.Errorf("PermissionAllowedBoundaries key %q has no UATScope and is not in the pre-reviewed allowlist; add it there or remove the entry", key)
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

// TestIntersectAllowedBoundaries_EmptyIDsYieldsNoBoundaries proves
// intersectAllowedBoundaries' contract holds on its own terms, independent
// of any caller: an empty ids has nothing to intersect, so it must return
// nil, false rather than the vacuous "every boundary agrees" answer that
// falls out of comparing a zero count to a zero length. A reviewed ID is
// kept as a positive control alongside it.
func TestIntersectAllowedBoundaries_EmptyIDsYieldsNoBoundaries(t *testing.T) {
	if boundaries, ok := intersectAllowedBoundaries(nil); ok || boundaries != nil {
		t.Errorf("intersectAllowedBoundaries(nil) = (%v, %v), want (nil, false)", boundaries, ok)
	}
	if boundaries, ok := intersectAllowedBoundaries([]string{}); ok || boundaries != nil {
		t.Errorf("intersectAllowedBoundaries([]string{}) = (%v, %v), want (nil, false)", boundaries, ok)
	}

	// Positive control: a reviewed ID still resolves its boundaries with
	// ok=true, so the empty-ids guard above isn't masking a broader
	// regression.
	boundaries, ok := intersectAllowedBoundaries([]string{"agent.create"})
	if !ok {
		t.Fatal("expected agent.create to be reviewed")
	}
	foundProject, foundHub := false, false
	for _, b := range boundaries {
		if b == BoundaryKindProject {
			foundProject = true
		}
		if b == BoundaryKindHub {
			foundHub = true
		}
	}
	if !foundProject || !foundHub {
		t.Errorf("intersectAllowedBoundaries([\"agent.create\"]) = %v, want both project and hub", boundaries)
	}
}

// TestResolveSelector_ReturnsIndependentCopy proves ResolveSelector's
// PermissionIDs/AllowedBoundaries are copies, not aliases of the
// process-wide cached selectorRegistry: mutating the returned slices (and
// appending to them) must not affect what a later, independent call to
// ResolveSelector for the same selector returns.
func TestResolveSelector_ReturnsIndependentCopy(t *testing.T) {
	m1, ok := ResolveSelector("agent:manage")
	if !ok {
		t.Fatal("expected agent:manage to resolve")
	}
	if len(m1.PermissionIDs) < 2 || len(m1.AllowedBoundaries) < 1 {
		t.Fatalf("test assumption broken: agent:manage needs at least 2 PermissionIDs and 1 AllowedBoundaries entry, got %+v", m1)
	}

	// Mutate in place and append -- if ResolveSelector returned aliases of
	// the cached map's backing arrays, this would corrupt them for every
	// later caller.
	m1.PermissionIDs[0] = "corrupted.permission.id"
	m1.AllowedBoundaries[0] = BoundaryKind("corrupted-boundary")
	m1.PermissionIDs = append(m1.PermissionIDs, "appended.permission.id")
	m1.AllowedBoundaries = append(m1.AllowedBoundaries, BoundaryKind("appended-boundary"))

	m2, ok := ResolveSelector("agent:manage")
	if !ok {
		t.Fatal("expected agent:manage to resolve on the second call")
	}
	for _, id := range m2.PermissionIDs {
		if id == "corrupted.permission.id" || id == "appended.permission.id" {
			t.Errorf("a mutation of the first call's PermissionIDs leaked into a later call: %v", m2.PermissionIDs)
		}
	}
	for _, b := range m2.AllowedBoundaries {
		if b == BoundaryKind("corrupted-boundary") || b == BoundaryKind("appended-boundary") {
			t.Errorf("a mutation of the first call's AllowedBoundaries leaked into a later call: %v", m2.AllowedBoundaries)
		}
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

// TestSupportedTargetClasses_CoversEveryUATScope is the drift coverage this
// table requires: every mintable permission (non-empty UATScope) must have
// an explicit, reviewed SupportedTargetClasses entry -- never a guessed
// default from ProjectTargetApplicability or PermissionAllowedBoundaries.
func TestSupportedTargetClasses_CoversEveryUATScope(t *testing.T) {
	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		if classes := SupportedTargetClassesFor(p.ID); len(classes) == 0 {
			t.Errorf("permission %q (UATScope %q) has no SupportedTargetClasses entry", p.ID, p.UATScope)
		}
	}
}

// TestSupportedTargetClasses_NoStaleEntries ensures every key in
// SupportedTargetClasses still names a real Registry permission ID --
// catching a renamed or removed permission the table wasn't updated for.
func TestSupportedTargetClasses_NoStaleEntries(t *testing.T) {
	known := make(map[string]bool, len(Registry))
	for _, p := range Registry {
		known[p.ID] = true
	}
	for id := range SupportedTargetClasses {
		if !known[id] {
			t.Errorf("SupportedTargetClasses has a stale entry for %q, which is not a Registry permission ID", id)
		}
	}
}

// TestSupportedTargetClasses_HubOnlyMintablePermissionReviewed is the
// explicit super-admin hub-only mint case this table must cover: user.invite
// (hub-only, ProjectTargetApplicability false) must still have a reviewed
// SupportedTargetClasses entry so a super-admin can mint it under a hub
// boundary.
func TestSupportedTargetClasses_HubOnlyMintablePermissionReviewed(t *testing.T) {
	applies, reviewed := AppliesToExistingProjectTarget("user.invite")
	if !reviewed || applies {
		t.Fatalf("test assumption broken: user.invite ProjectTargetApplicability = (%v, reviewed=%v), want (false, true)", applies, reviewed)
	}
	classes := SupportedTargetClassesFor("user.invite")
	if len(classes) == 0 {
		t.Fatal("user.invite must have a reviewed SupportedTargetClasses entry despite being hub-only")
	}
}

// TestSupportedTargetClasses_UnknownPermissionDeniesRatherThanGuess proves
// this table's core requirement: an unreviewed permission ID returns no
// classes at all (deny), never a class inferred from ProjectTargetApplicability
// or PermissionAllowedBoundaries.
func TestSupportedTargetClasses_UnknownPermissionDeniesRatherThanGuess(t *testing.T) {
	if classes := SupportedTargetClassesFor("totally.unreviewed.permission"); classes != nil {
		t.Errorf("unreviewed permission must return nil classes, got %v", classes)
	}
	// hub.settings.read is project-applicable=false AND has no
	// SupportedTargetClasses entry (it has no UATScope, so it is outside
	// today's mintable universe) -- confirms absence denies rather than
	// falling back to a guessed hub_resource class.
	if classes := SupportedTargetClassesFor("hub.settings.read"); classes != nil {
		t.Errorf("hub.settings.read has no reviewed entry and must return nil, got %v", classes)
	}
}

// TestCollectionTargetClasses_CoversEveryRegistryPermission is the drift
// coverage this table requires: ResolveTargetScope's collection-evidence
// cross-check
// needs a reviewed class set for every Registry permission, not just
// mintable ones (project.create has no UATScope at all).
func TestCollectionTargetClasses_CoversEveryRegistryPermission(t *testing.T) {
	for _, p := range Registry {
		if _, reviewed := CollectionTargetClassesFor(p.ID); !reviewed {
			t.Errorf("permission %q has no CollectionTargetClasses entry", p.ID)
		}
	}
}

// TestCollectionTargetClasses_NoStaleEntries mirrors
// TestSupportedTargetClasses_NoStaleEntries for this table.
func TestCollectionTargetClasses_NoStaleEntries(t *testing.T) {
	known := make(map[string]bool, len(Registry))
	for _, p := range Registry {
		known[p.ID] = true
	}
	for id := range CollectionTargetClasses {
		if !known[id] {
			t.Errorf("CollectionTargetClasses has a stale entry for %q, which is not a Registry permission ID", id)
		}
	}
}

// TestCollectionTargetClasses_SkillListSupportsBothClasses pins the exact
// example this table exists for: skill.list is ProjectTargetApplicability=true AND separately,
// legitimately, supports Hub-scope collection evidence for the global
// catalog -- a single boolean cannot represent both.
func TestCollectionTargetClasses_SkillListSupportsBothClasses(t *testing.T) {
	applies, reviewed := AppliesToExistingProjectTarget("skill.list")
	if !reviewed || !applies {
		t.Fatalf("test assumption broken: skill.list ProjectTargetApplicability = (%v, reviewed=%v), want (true, true)", applies, reviewed)
	}
	classes, reviewed := CollectionTargetClassesFor("skill.list")
	if !reviewed {
		t.Fatal("skill.list must have a reviewed CollectionTargetClasses entry")
	}
	hasProject, hasGlobal := false, false
	for _, c := range classes {
		if c == TargetClassKindProjectScoped {
			hasProject = true
		}
		if c == TargetClassKindGlobalCatalog {
			hasGlobal = true
		}
	}
	if !hasProject || !hasGlobal {
		t.Errorf("skill.list must support BOTH ProjectScoped and GlobalCatalog collection classes, got %v", classes)
	}
}
