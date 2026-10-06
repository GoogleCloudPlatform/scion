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

// CeilingVersion identifies the interpretation rules that were in effect
// when a FrozenPermissionCeiling's PermissionIDs were computed. Pinning the
// version on the ceiling itself (rather than re-deriving permission IDs from
// mutable state every time) means a later Registry or alias change can never
// silently widen or narrow what an already-minted credential means.
type CeilingVersion int

const (
	// CeilingVersionUnspecified marks a row with no recorded version — every
	// credential minted before this ceiling existed. Its raw stored scopes
	// are mapped through the frozen legacy selector snapshot (see
	// NormalizeLegacyUATScopes): each scope grants exactly the permission it
	// names, and no scope implies another. This is deliberately distinct
	// from an explicit CeilingVersionV1+ ceiling whose PermissionIDs list
	// happens to be empty: both deny via Allows, but only the latter
	// represents an intentionally-issued, permission-less credential.
	CeilingVersionUnspecified CeilingVersion = 0
	// CeilingVersionV1 is the first explicit ceiling version: PermissionIDs
	// is the exact, alias-expanded, deduplicated set of canonical permission
	// IDs resolved (via SelectorRegistry) at mint time. An empty list under
	// this version denies, the same as every other version — it never means
	// "unrestricted."
	CeilingVersionV1 CeilingVersion = 1
)

// FrozenPermissionCeiling is the normalized, persisted ceiling on what a
// credential (today: a UAT) may ever be authorized for, independent of the
// live role/relationship graph. It is computed once — at mint time for new
// credentials, or via frozen normalization for pre-existing rows — and
// never re-derived from the current Registry at evaluation time.
type FrozenPermissionCeiling struct {
	Version CeilingVersion
	// PermissionIDs holds canonical Registry permission IDs, already
	// alias-expanded and deduplicated under Version's rules. An empty list
	// denies every permission; it is never treated as "unrestricted" (see
	// Allows).
	PermissionIDs []string
}

// Allows reports whether the ceiling permits permissionID. This is the one
// place "empty PermissionIDs denies" is encoded, for every version,
// including versions this package does not yet know about: an unrecognized
// CeilingVersion fails closed rather than falling through to a membership
// check against whatever PermissionIDs happens to contain.
func (c FrozenPermissionCeiling) Allows(permissionID string) bool {
	if permissionID == "" {
		return false
	}
	switch c.Version {
	case CeilingVersionUnspecified, CeilingVersionV1:
		for _, id := range c.PermissionIDs {
			if id == permissionID {
				return true
			}
		}
		return false
	default:
		// Unknown/malformed version: deny rather than guess at an
		// interpretation this code was not built to apply.
		return false
	}
}

// BuildCeilingFromSelectors resolves a set of requested UAT selectors
// (ordinary scopes or manage aliases) into a CeilingVersionV1 ceiling via
// the live SelectorRegistry. Live resolution is correct here — unlike
// normalizing a pre-existing row (see NormalizeLegacyUATScopes), mint time
// is exactly when today's registry rules are supposed to apply, and the
// resulting ceiling freezes that resolution for the credential's lifetime.
//
// ok is false, with a zero-value ceiling, if any selector fails to resolve;
// callers must fail closed rather than mint a partially-resolved
// credential.
func BuildCeilingFromSelectors(selectors []string) (ceiling FrozenPermissionCeiling, ok bool) {
	seen := make(map[string]bool, len(selectors))
	ids := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		m, resolved := ResolveSelector(selector)
		if !resolved {
			return FrozenPermissionCeiling{}, false
		}
		for _, id := range m.PermissionIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return FrozenPermissionCeiling{Version: CeilingVersionV1, PermissionIDs: ids}, true
}
