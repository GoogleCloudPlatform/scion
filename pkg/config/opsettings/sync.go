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

package opsettings

import "encoding/json"

// The every-boot hub_settings re-sync rule (cmd syncHubSettings), as pure
// functions, so the server and the rebuild preflight (ptone/scion#5057)
// decide the same way.

// Origins of a hub_settings row.
const (
	OriginSeeded  = "seeded"
	OriginManaged = "managed"
)

// SectionRow is the part of a hub_settings row the re-sync rule reads.
type SectionRow struct {
	Origin string
	// UpdatedBy is nil when the column is NULL.
	UpdatedBy *string
	Value     json.RawMessage
}

// SettingsRevisionSection is the reserved hub_settings row that holds the
// operational-settings revision counter. It equals
// store.HubSettingsRevisionSection (a test in the store adapter checks
// this); it is repeated here so this package does not import the store.
const SettingsRevisionSection = "_settings_revision"

// EffectiveOrigin is a row's origin after the store's BackfillOrigin: a row
// still marked seeded but last written by someone other than the seed
// (updated_by set and not "seed"), outside the _meta section and the
// reserved SettingsRevisionSection row, counts as managed. A NULL
// updated_by is not "not seed" (SQL compares it as unknown), so such a row
// stays seeded.
func EffectiveOrigin(section, origin string, updatedBy *string) string {
	if origin == OriginSeeded && section != "_meta" && section != SettingsRevisionSection &&
		updatedBy != nil && *updatedBy != "seed" {
		return OriginManaged
	}
	return origin
}

// SyncedSection is the row the every-boot re-sync leaves for section, given
// the current row (nil when absent) and the current bootstrap document:
//   - a managed row is kept as it is;
//   - an absent catalog section whose bootstrap document is empty stays
//     absent (present false; ptone/scion#5007);
//   - otherwise (absent, or seeded) the row holds the bootstrap document,
//     including an empty one.
func SyncedSection(section string, row *SectionRow, bootstrapDoc json.RawMessage) (doc json.RawMessage, present bool) {
	if row == nil {
		if IsEmptyCatalogSeed(section, bootstrapDoc) {
			return nil, false
		}
		return bootstrapDoc, true
	}
	if EffectiveOrigin(section, row.Origin, row.UpdatedBy) == OriginManaged {
		return row.Value, true
	}
	return bootstrapDoc, true
}

// IsEmptyCatalogSeed reports whether doc is an empty bootstrap document for
// one of the map-of-objects catalog sections (runtimes, profiles,
// harness_configs), which the re-sync does not create a row for.
func IsEmptyCatalogSeed(section string, doc json.RawMessage) bool {
	switch section {
	case "runtimes", "profiles", "harness_configs":
	default:
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		return false
	}
	return len(m) == 0
}
