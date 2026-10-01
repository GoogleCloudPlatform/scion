// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditevent

// PhaseOutcome is one exact phase/result pair declared by the catalog.
type PhaseOutcome struct {
	Phase   Phase
	Outcome Outcome
}

// Destination is a declared output for an event family/action.
type Destination string

const (
	DestinationStructuredLog Destination = "structured_log"
	DestinationHistory       Destination = "history"
)

// CatalogEntry is the machine-readable schema for one action.
type CatalogEntry struct {
	Family                 string
	Action                 string
	AllowedPairs           []PhaseOutcome
	ResourceKind           string
	RequiredEnvelopeLeaves []string
	RequiredPayloadLeaves  []string
	OptionalPayloadLeaves  []string
	Destinations           []Destination
}

var catalog = []CatalogEntry{{
	Family:                 "access_boundary",
	Action:                 "create",
	AllowedPairs:           []PhaseOutcome{{Phase: PhaseCommit, Outcome: OutcomeSucceeded}},
	ResourceKind:           "access_constraint",
	RequiredEnvelopeLeaves: []string{"schema_version", "event_id", "occurred_at", "family", "action", "phase", "outcome", "severity", "correlation_id", "principal", "resource", "resource.project_id"},
	RequiredPayloadLeaves:  []string{"classification"},
	OptionalPayloadLeaves:  []string{"before_revision", "after_revision", "preview_id", "draft_hash", "impact_counts", "changed_fields"},
	Destinations:           []Destination{DestinationStructuredLog, DestinationHistory},
}}

// Catalog returns a defensive snapshot of the schemas implemented in this
// milestone. Later family adapters add literal entries here.
func Catalog() []CatalogEntry {
	result := make([]CatalogEntry, len(catalog))
	for i, entry := range catalog {
		result[i] = entry
		result[i].AllowedPairs = append([]PhaseOutcome(nil), entry.AllowedPairs...)
		result[i].RequiredEnvelopeLeaves = append([]string(nil), entry.RequiredEnvelopeLeaves...)
		result[i].RequiredPayloadLeaves = append([]string(nil), entry.RequiredPayloadLeaves...)
		result[i].OptionalPayloadLeaves = append([]string(nil), entry.OptionalPayloadLeaves...)
		result[i].Destinations = append([]Destination(nil), entry.Destinations...)
	}
	return result
}

func catalogEntry(family, action string) (CatalogEntry, bool) {
	for _, entry := range catalog {
		if entry.Family == family && entry.Action == action {
			return entry, true
		}
	}
	return CatalogEntry{}, false
}
