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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	mutationTypeAgentLabelsUpdate = "agent.labels.update"
	maxLabelMergeRetries          = 8
	errMsgAgentsCannotEditLabels  = "agents cannot modify agent labels"
)

// PatchAgentLabelsRequest is the request body for PATCH /api/v1/agents/{id}/labels
// and PATCH /api/v1/projects/{projectId}/agents/{id}/labels.
// It applies merge semantics: Set adds or overwrites keys and Remove deletes keys
// without replacing unrelated labels. StateVersion is optional: when non-zero, the
// update fails with 409 Conflict if the agent's current stateVersion differs; when
// zero, the server retries on concurrent version conflicts so concurrent merges do
// not lose updates.
type PatchAgentLabelsRequest struct {
	Set          map[string]string `json:"set,omitempty"`
	Remove       []string          `json:"remove,omitempty"`
	StateVersion int64             `json:"stateVersion,omitempty"`
}

// patchBodyHasLabelsField reports whether a raw JSON PATCH body contains a
// non-null "labels" field (matching encoding/json's case-insensitive key
// matching for updates.Labels).
func patchBodyHasLabelsField(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var raw struct {
		Labels json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	trimmed := bytes.TrimSpace(raw.Labels)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// isAgentDerivedCaller reports whether ctx carries any agent-derived principal
// kind — local agent JWT, federated agent, or delegated agent — using the same
// concrete-type classification as the authz kernel (principalContextForIdentity)
// rather than Identity.Type() string matching.
func isAgentDerivedCaller(ctx context.Context) bool {
	id := GetIdentityFromContext(ctx)
	if id == nil {
		return false
	}
	kind := principalContextForIdentity(id).Kind
	return isAgentPrincipal(kind) || kind == PrincipalKindAgentDelegated
}

// handleAgentLabels handles PATCH /api/v1/agents/{id}/labels.
func (s *Server) handleAgentLabels(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPatch {
		MethodNotAllowed(w, http.MethodPatch)
		return
	}
	if isAgentDerivedCaller(r.Context()) {
		writeForbidden(w, errMsgAgentsCannotEditLabels)
		return
	}
	agent, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.applyAgentLabelsMerge(w, r, agent)
}

// handleProjectAgentLabels handles PATCH /api/v1/projects/{projectId}/agents/{id}/labels.
func (s *Server) handleProjectAgentLabels(w http.ResponseWriter, r *http.Request, projectID, agentID string) {
	if r.Method != http.MethodPatch {
		MethodNotAllowed(w, http.MethodPatch)
		return
	}
	if isAgentDerivedCaller(r.Context()) {
		writeForbidden(w, errMsgAgentsCannotEditLabels)
		return
	}
	agent, err := s.resolveProjectAgent(r.Context(), projectID, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	s.applyAgentLabelsMerge(w, r, agent)
}

func (s *Server) applyAgentLabelsMerge(w http.ResponseWriter, r *http.Request, agent *store.Agent) {
	ctx := r.Context()

	if isAgentDerivedCaller(ctx) {
		writeForbidden(w, errMsgAgentsCannotEditLabels)
		return
	}
	if !s.authorize(w, r, agentResource(agent), ActionUpdate) {
		return
	}

	var req PatchAgentLabelsRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if err := validateLabelMergeRequest(&req); err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	attempts := maxLabelMergeRetries
	if req.StateVersion != 0 {
		attempts = 1
	}

	var updated *store.Agent
	for attempt := 0; attempt < attempts; attempt++ {
		var validationErr error
		var deletedErr bool
		var versionConflict bool

		err := s.store.WithTx(ctx, func(tx store.Store) error {
			fresh, err := tx.GetAgent(ctx, agent.ID)
			if err != nil {
				return err
			}
			if !fresh.DeletedAt.IsZero() {
				deletedErr = true
				return nil
			}
			if req.StateVersion != 0 && req.StateVersion != fresh.StateVersion {
				versionConflict = true
				return nil
			}

			before := maps.Clone(fresh.Labels)
			merged := mergeAgentLabels(fresh.Labels, req.Set, req.Remove)
			if err := labels.Validate(merged); err != nil {
				validationErr = err
				return nil
			}

			if maps.Equal(before, merged) {
				updated = fresh
				return nil
			}

			fresh.Labels = merged
			if err := tx.UpdateAgent(ctx, fresh); err != nil {
				return err
			}
			if err := writeAgentLabelsAuditTx(ctx, tx, fresh.ID, before, merged); err != nil {
				return err
			}
			updated = fresh
			return nil
		})

		if deletedErr {
			Conflict(w, "The agent is deleted and cannot be edited")
			return
		}
		if versionConflict {
			Conflict(w, "Version conflict - resource was modified")
			return
		}
		if validationErr != nil {
			ValidationError(w, "Invalid labels: "+validationErr.Error(), nil)
			return
		}
		if err == nil {
			writeJSON(w, http.StatusOK, redactedAgentCopy(ctx, s, updated))
			return
		}
		if errors.Is(err, store.ErrVersionConflict) && req.StateVersion == 0 && attempt+1 < attempts {
			backoff := time.Duration(attempt+1) * 5 * time.Millisecond
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				writeErrorFromErr(w, ctx.Err(), "")
				return
			case <-timer.C:
			}
			continue
		}
		writeErrorFromErr(w, err, "")
		return
	}

	Conflict(w, "Version conflict - resource was modified")
}

// validateLabelMergeRequest validates the shape of a PatchAgentLabelsRequest
// before reading or merging against the stored labels.
func validateLabelMergeRequest(req *PatchAgentLabelsRequest) error {
	for _, k := range req.Remove {
		if k == "" {
			return errors.New("label key to remove must not be empty")
		}
		if _, overlap := req.Set[k]; overlap {
			return fmt.Errorf("label key %q cannot be both set and removed in the same request", k)
		}
	}
	return nil
}

// mergeAgentLabels returns a new label map with set keys added/overwritten and
// remove keys deleted. Removing a key that is not present is an idempotent no-op.
func mergeAgentLabels(current, set map[string]string, remove []string) map[string]string {
	merged := make(map[string]string, len(current)+len(set))
	maps.Copy(merged, current)
	for k, v := range set {
		merged[k] = v
	}
	for _, k := range remove {
		delete(merged, k)
	}
	return merged
}

// writeAgentLabelsAuditTx writes an atomic MutationAuditRecord for a label change
// inside tx. BeforeSummary and AfterSummary are deterministic JSON objects holding
// the old and new values for every key that was added, changed, or removed.
// Labels are short, printable-ASCII metadata (not secrets), so values are recorded
// directly.
func writeAgentLabelsAuditTx(ctx context.Context, tx store.Store, agentID string, before, after map[string]string) error {
	if maps.Equal(before, after) {
		return nil
	}
	beforeDiff := make(map[string]string)
	afterDiff := make(map[string]string)
	for k, bv := range before {
		if av, ok := after[k]; !ok {
			beforeDiff[k] = bv
		} else if av != bv {
			beforeDiff[k] = bv
			afterDiff[k] = av
		}
	}
	for k, av := range after {
		if _, ok := before[k]; !ok {
			afterDiff[k] = av
		}
	}

	beforeJSON, err := json.Marshal(beforeDiff)
	if err != nil {
		return fmt.Errorf("marshal labels audit before summary: %w", err)
	}
	afterJSON, err := json.Marshal(afterDiff)
	if err != nil {
		return fmt.Errorf("marshal labels audit after summary: %w", err)
	}

	rec := &store.MutationAuditRecord{
		MutationType:  mutationTypeAgentLabelsUpdate,
		TargetType:    "agent",
		TargetID:      agentID,
		BeforeSummary: string(beforeJSON),
		AfterSummary:  string(afterJSON),
		Timestamp:     time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	applyHubActorFallback(rec)
	if err := tx.CreateMutationAudit(ctx, rec); err != nil {
		return fmt.Errorf("agent labels audit: %w", err)
	}
	return nil
}
