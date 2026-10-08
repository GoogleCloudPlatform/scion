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

package delegationadoption

import "github.com/GoogleCloudPlatform/scion/pkg/store"

// MarkerSection is the completion marker of the
// delegation-provenance adoption migration. It is independent of the edge
// backfill marker (migration_delegation_edge_backfill_v1), which plays no
// part in whether this migration runs.
const MarkerSection = "migration_delegation_provenance_adoption_v1"

// CohortSection is the cohort snapshot header.
// Once it exists, no new snapshot is taken: later runs only work through
// the snapshot's pending records, so no row written after the snapshot can
// enter the automatic cohort.
const CohortSection = "delegation_provenance_adoption_cohort"

// HeaderSchemaVersion is the layout of the header and marker.
const HeaderSchemaVersion = 1

// Header is the JSON value of the cohort header and of the
// completion marker.
type Header struct {
	SchemaVersion int            `json:"schema_version"`
	PolicyVersion int            `json:"policy_version"`
	CohortID      string         `json:"cohort_id"`
	Completed     bool           `json:"completed,omitempty"`
	Counts        map[string]int `json:"counts,omitempty"`
}

// SnapshotRecords returns the records a plan contributes to a cohort:
// adoptable hops as pending, already-adopted hops as recognized (or
// recognized_above_policy), and excluded hops whose edge is not a recorded
// edge. Recorded hops are valid path members but not cohort members, so
// they get no record.
func SnapshotRecords(plan *Plan, cohortID, origin string) []*store.DelegationAdoption {
	var out []*store.DelegationAdoption
	for _, h := range plan.Hops {
		rec := &store.DelegationAdoption{
			CohortID:          cohortID,
			Origin:            origin,
			PolicyVersion:     int(plan.PolicyVersion),
			DelegateID:        h.DelegateID,
			ScopeID:           h.ProjectID,
			Depth:             h.Depth,
			Reason:            string(h.Reason),
			BeforeFingerprint: h.Fingerprint,
		}
		if h.Edge != nil {
			rec.DelegatorType = h.Edge.DelegatorType
			rec.DelegatorID = h.Edge.DelegatorID
			rec.ScopeID = h.Edge.ScopeID
			rec.Role = h.Edge.Role
		}
		switch h.Outcome {
		case OutcomeAdopt:
			rec.Status = store.DelegationAdoptionPending
			rec.OriginalEdgeID = h.Edge.ID
		case OutcomeRecognized, OutcomeRecognizedAbovePolicy:
			rec.Status = store.DelegationAdoptionStatus(h.Outcome)
			rec.OriginalEdgeID = h.OriginalEdgeID
			rec.AdoptedEdgeID = h.Edge.ID
		case OutcomeExcluded:
			if h.Edge != nil && h.Edge.ProvenanceVersion == store.ProvenanceVersionV1 && h.Edge.Kind != store.EffectCeilingUnrecorded {
				continue
			}
			rec.Status = store.DelegationAdoptionExcluded
			if h.Edge != nil {
				rec.OriginalEdgeID = h.Edge.ID
			}
		default:
			continue
		}
		out = append(out, rec)
	}
	return out
}
