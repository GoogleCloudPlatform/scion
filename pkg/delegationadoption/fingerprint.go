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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hopFacts is the canonical, ordered set of facts a hop's fingerprint
// covers. Any change to one of them between a preview and a commit, or
// between the boot snapshot and the adoption write, changes the
// fingerprint.
type hopFacts struct {
	DelegateID        string   `json:"delegate_id"`
	EdgeID            string   `json:"edge_id"`
	EdgeActive        bool     `json:"edge_active"`
	EdgeUpdated       string   `json:"edge_updated"`
	ProvenanceVersion int      `json:"provenance_version"`
	CeilingKind       string   `json:"ceiling_kind"`
	CeilingVersion    int      `json:"ceiling_version"`
	CeilingIDs        []string `json:"ceiling_ids"`
	DelegatorType     string   `json:"delegator_type"`
	DelegatorID       string   `json:"delegator_id"`
	ScopeType         string   `json:"scope_type"`
	ScopeID           string   `json:"scope_id"`
	Role              string   `json:"role"`
	DelegateDeleted   bool     `json:"delegate_deleted"`
	DelegateProjectID string   `json:"delegate_project_id"`
	AppliedRole       string   `json:"applied_role"`
	AppliedConfig     bool     `json:"applied_config"`
	AssignedSA        bool     `json:"assigned_sa"`
	DelegatorState    string   `json:"delegator_state"`
	PolicyVersion     int      `json:"policy_version"`
}

func (p *planner) fingerprint(a *store.Agent, h *Hop) string {
	f := hopFacts{
		DelegateID:        a.ID,
		DelegateDeleted:   !a.DeletedAt.IsZero(),
		DelegateProjectID: a.ProjectID,
		AppliedConfig:     a.AppliedConfig != nil,
		AssignedSA:        hasAssignedSA(a),
		DelegatorState:    h.delegatorState,
		PolicyVersion:     int(PolicyVersion),
	}
	if a.AppliedConfig != nil {
		f.AppliedRole = a.AppliedConfig.AgentRole
	}
	if e := h.Edge; e != nil {
		f.EdgeID = e.ID
		f.EdgeActive = e.Active
		f.EdgeUpdated = e.UpdatedAt.UTC().Format(time.RFC3339Nano)
		f.ProvenanceVersion = e.ProvenanceVersion
		f.CeilingKind = string(e.Kind)
		f.CeilingVersion = int(e.Version)
		f.CeilingIDs = e.PermissionIDs
		f.DelegatorType = e.DelegatorType
		f.DelegatorID = e.DelegatorID
		f.ScopeType = e.ScopeType
		f.ScopeID = e.ScopeID
		f.Role = e.Role
	}
	return digest(f)
}

func digest(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Marshalling a struct of strings, ints, bools and string slices
		// cannot fail; an empty digest never matches a computed one.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Fingerprint returns the plan fingerprint for operation: SHA-256 over the
// canonical JSON of the policy version, the operation, and every hop's
// fingerprint and outcome in plan order. A commit recomputes it from current
// state, so a client cannot supply a plan of its own.
func (p *Plan) Fingerprint(operation string) string {
	type hopEntry struct {
		DelegateID  string `json:"delegate_id"`
		Fingerprint string `json:"fingerprint"`
		Outcome     string `json:"outcome"`
		Reason      string `json:"reason"`
	}
	entries := make([]hopEntry, 0, len(p.Hops))
	for _, h := range p.Hops {
		entries = append(entries, hopEntry{h.DelegateID, h.Fingerprint, string(h.Outcome), string(h.Reason)})
	}
	return digest(struct {
		PolicyVersion int        `json:"policy_version"`
		Operation     string     `json:"operation"`
		Hops          []hopEntry `json:"hops"`
	}{int(p.PolicyVersion), operation, entries})
}
