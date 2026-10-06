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

// relationshipPolicyExceptionKey identifies one permission in one
// RelationshipPolicies row for one principal kind.
type relationshipPolicyExceptionKey struct {
	Relationship  string
	PrincipalKind string
	ResourceType  string
	PermissionID  string
}

// relationshipPolicyReviewedExceptions lists row permissions that the two
// row-shape tests below accept although the Registry entry does not match
// the row's ResourceType or read-class rule. Each entry is reviewed and
// scoped to exactly one row and principal kind; it is not a global
// reclassification. TestRelationshipPolicies_ReviewedExceptionsMatchRows
// fails for any entry that no longer matches a row.
var relationshipPolicyReviewedExceptions = map[relationshipPolicyExceptionKey]string{
	// Progeny secret reads are decided with Resource.Type "secret" and
	// Permission "project.secret_read" (pkg/hub/httpdispatcher.go). The row's
	// ResourceType is that Decide resource type, and secret_read is treated
	// as read-class for this row only.
	{"progeny", "agent", "secret", "project.secret_read"}: "Decide resource type is secret; secret_read is read-class for this row only",
}

// relationshipPolicyExcepted reports whether permID in row is a reviewed
// exception for every principal kind the row declares.
func relationshipPolicyExcepted(row RelationshipPolicy, permID string) bool {
	if len(row.PrincipalKinds) == 0 {
		return false
	}
	for _, kind := range row.PrincipalKinds {
		if _, ok := relationshipPolicyReviewedExceptions[relationshipPolicyExceptionKey{row.Relationship, kind, row.ResourceType, permID}]; !ok {
			return false
		}
	}
	return true
}

// TestRelationshipPolicies_ReviewedExceptionsMatchRows pins that every
// reviewed exception names an existing row, principal kind and permission.
func TestRelationshipPolicies_ReviewedExceptionsMatchRows(t *testing.T) {
	for key := range relationshipPolicyReviewedExceptions {
		found := false
		for _, row := range RelationshipPolicies {
			if row.Relationship != key.Relationship || row.ResourceType != key.ResourceType {
				continue
			}
			kindOK := false
			for _, k := range row.PrincipalKinds {
				kindOK = kindOK || k == key.PrincipalKind
			}
			if !kindOK {
				continue
			}
			for _, id := range row.PermissionIDs {
				found = found || id == key.PermissionID
			}
		}
		if !found {
			t.Errorf("reviewed exception %+v matches no RelationshipPolicies row", key)
		}
	}
}

// TestRelationshipPolicies_PermissionIDsMatchRegistryResourceType pins that
// every RelationshipPolicies row's PermissionIDs are real Registry IDs whose
// own Resource matches the row's ResourceType — a row can never grant a
// permission belonging to a different resource family.
func TestRelationshipPolicies_PermissionIDsMatchRegistryResourceType(t *testing.T) {
	byID := make(map[string]Permission, len(Registry))
	for _, p := range Registry {
		byID[p.ID] = p
	}
	for _, row := range RelationshipPolicies {
		for _, permID := range row.PermissionIDs {
			if relationshipPolicyExcepted(row, permID) {
				continue
			}
			p, ok := byID[permID]
			if !ok {
				t.Errorf("RelationshipPolicies row %q/%q references %q, which is not a Registry permission", row.Relationship, row.ResourceType, permID)
				continue
			}
			if p.Resource != row.ResourceType {
				t.Errorf("RelationshipPolicies row %q declares ResourceType %q but PermissionIDs contains %q, whose Registry Resource is %q", row.Relationship, row.ResourceType, permID, p.Resource)
			}
		}
	}
}

// TestRelationshipPolicies_ReadOnlyRowsContainOnlyReadActions pins the
// ReadOnly field's contract: every permission listed in a ReadOnly==true row
// must be a read, list or verify action.
func TestRelationshipPolicies_ReadOnlyRowsContainOnlyReadActions(t *testing.T) {
	byID := make(map[string]Permission, len(Registry))
	for _, p := range Registry {
		byID[p.ID] = p
	}
	for _, row := range RelationshipPolicies {
		if !row.ReadOnly {
			continue
		}
		for _, permID := range row.PermissionIDs {
			if relationshipPolicyExcepted(row, permID) {
				continue
			}
			p, ok := byID[permID]
			if !ok {
				continue // reported by the sibling test above
			}
			switch p.Action {
			case ActionRead, ActionList, ActionVerify:
			default:
				t.Errorf("RelationshipPolicies row %q/%q is ReadOnly but lists %q, whose action is %q (not read/list/verify)", row.Relationship, row.ResourceType, permID, p.Action)
			}
		}
	}
}

// TestMintEligibilityRegistry_RelationshipTypesResolveToMintEligibleRow pins
// the doc comment's own claim: every MintEligibilityRelationship source's
// RelationshipTypes must resolve to at least one MintEligible==true
// RelationshipPolicies row with a matching ResourceType/PermissionID, for
// every PrincipalKind that row declares.
func TestMintEligibilityRegistry_RelationshipTypesResolveToMintEligibleRow(t *testing.T) {
	for permID, descriptor := range MintEligibilityRegistry {
		resourceType := ""
		for _, p := range Registry {
			if p.ID == permID {
				resourceType = p.Resource
				break
			}
		}
		if resourceType == "" {
			t.Errorf("MintEligibilityRegistry key %q is not a Registry permission", permID)
			continue
		}
		for _, src := range descriptor.Sources {
			if src.Kind != MintEligibilityRelationship {
				continue
			}
			for _, relType := range src.RelationshipTypes {
				found := false
				for _, row := range RelationshipPolicies {
					if !row.MintEligible || row.Relationship != relType || row.ResourceType != resourceType {
						continue
					}
					if containsString(row.PermissionIDs, permID) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("MintEligibilityRegistry[%q]'s relationship type %q has no MintEligible RelationshipPolicies row for resource %q granting %q", permID, relType, resourceType, permID)
				}
			}
		}
	}
}

// TestMintEligibilityRegistry_KeyMatchesDescriptorPermissionID pins that
// every MintEligibilityRegistry map key equals its own descriptor's
// PermissionID and names a real Registry permission — a mismatched key
// would make MintEligibilityRegistry[wrongKey] silently describe the wrong
// permission to every caller that looks it up by ID.
func TestMintEligibilityRegistry_KeyMatchesDescriptorPermissionID(t *testing.T) {
	registryIDs := make(map[string]bool, len(Registry))
	for _, p := range Registry {
		registryIDs[p.ID] = true
	}
	for key, descriptor := range MintEligibilityRegistry {
		if descriptor.PermissionID != key {
			t.Errorf("MintEligibilityRegistry key %q has descriptor.PermissionID %q; they must match", key, descriptor.PermissionID)
		}
		if !registryIDs[key] {
			t.Errorf("MintEligibilityRegistry key %q is not a Registry permission", key)
		}
	}
}

// TestRelationshipPrincipalKind pins the exact mapping both the flat and
// relationship mint paths must share: user/dev/federated_user collapse to
// "user", agent/federated_agent collapse to "agent", and any other value
// passes through unchanged rather than being silently dropped.
func TestRelationshipPrincipalKind(t *testing.T) {
	cases := map[string]string{
		"user":            "user",
		"dev":             "user",
		"federated_user":  "user",
		"agent":           "agent",
		"federated_agent": "agent",
		"broker":          "broker",
		"":                "",
	}
	for kind, want := range cases {
		if got := RelationshipPrincipalKind(kind); got != want {
			t.Errorf("RelationshipPrincipalKind(%q) = %q, want %q", kind, got, want)
		}
	}
}
