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
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	// artifactSuccessionTimeout bounds the post-delete tail step that passes a
	// deleted child agent's artifacts to its nearest live eligible ancestor.
	artifactSuccessionTimeout = 60 * time.Second

	// maxSuccessionWalk caps how many ancestry entries are inspected per child.
	maxSuccessionWalk = 32
)

type artifactSuccessor struct {
	AgentID            string
	Sponsor            string
	SponsorReviewGrant bool
}

// ancestorCandidates returns the candidate ancestor agent IDs for a, nearest
// first. It iterates a's immutable Ancestry slice in reverse (falling back to
// CreatedBy for legacy rows whose Ancestry was empty), excludes a.ID, drops
// duplicates and non-UUID entries, and caps the result at maxSuccessionWalk.
func ancestorCandidates(a *store.Agent) []string {
	if a == nil {
		return nil
	}
	src := a.Ancestry
	if len(src) == 0 && a.CreatedBy != "" && a.CreatedBy != a.OwnerID {
		src = []string{a.CreatedBy}
	}
	out := make([]string, 0, len(src))
	seen := map[string]bool{strings.ToLower(a.ID): true}
	for i := len(src) - 1; i >= 0 && len(out) < maxSuccessionWalk; i-- {
		id := strings.ToLower(src[i])
		if seen[id] {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// resolveArtifactSuccessor finds the nearest live, eligible ancestor agent in
// child.ProjectID to inherit child's live artifacts. It returns (zero, false,
// nil) when no ancestor qualifies, and a non-nil error on a transient store or
// authorization fault (stopping the walk rather than skipping to a wrong
// ancestor).
func (s *Server) resolveArtifactSuccessor(ctx context.Context, child *store.Agent) (artifactSuccessor, bool, error) {
	if child == nil || child.ProjectID == "" {
		return artifactSuccessor{}, false, nil
	}
	if s == nil || s.store == nil || s.authzService == nil {
		return artifactSuccessor{}, false, errors.New("artifacts: resolve successor: hub services unavailable")
	}
	host := newArtifactHost(s)
	for _, candID := range ancestorCandidates(child) {
		cand, err := s.store.GetAgent(ctx, candID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return artifactSuccessor{}, false, err
		}
		if cand == nil || !cand.DeletedAt.IsZero() {
			continue
		}
		if cand.ProjectID != child.ProjectID {
			continue
		}
		if cand.DeletionState != store.DeletionStateNone {
			continue
		}
		if reincarnationInFlight(cand) {
			continue
		}
		if stErr := s.agentStanding(ctx, cand.ID); stErr != nil {
			if errors.Is(stErr, errAgentNotInStanding) {
				continue
			}
			return artifactSuccessor{}, false, stErr
		}
		sponsor, err := host.DelegatingUser(ctx, cand.ID)
		if err != nil {
			return artifactSuccessor{}, false, err
		}
		if sponsor == "" {
			continue
		}
		sponsorReviewGrant := host.SponsorReviewGrant(ctx, child.ProjectID, sponsor) == nil
		return artifactSuccessor{
			AgentID:            cand.ID,
			Sponsor:            sponsor,
			SponsorReviewGrant: sponsorReviewGrant,
		}, true, nil
	}
	return artifactSuccessor{}, false, nil
}

// succeedAgentArtifacts transfers live artifacts owned by child in
// child.ProjectID to its nearest live eligible ancestor agent. Errors are
// logged and swallowed; the caller's agent deletion is never rolled back.
func (s *Server) succeedAgentArtifacts(ctx context.Context, child *store.Agent, trigger string) {
	if s == nil || child == nil || child.ID == "" || child.ProjectID == "" {
		return
	}
	if !s.artifactsEnabled() || !s.experimentEnabled(experiments.Artifacts) {
		return
	}
	st := s.ArtifactStore()
	if st == nil {
		return
	}
	succ, found, err := s.resolveArtifactSuccessor(ctx, child)
	if err != nil {
		slog.WarnContext(ctx, "artifacts: resolve successor failed on agent delete",
			"agent_id", child.ID, "project_id", child.ProjectID, "trigger", trigger, "error", err)
		return
	}
	if !found {
		return
	}
	host := newArtifactHost(s)
	fromRef := artifacts.PrincipalRef(artifacts.PrincipalKindAgent, child.ID)
	toRef := artifacts.PrincipalRef(artifacts.PrincipalKindAgent, succ.AgentID)
	res, err := artifacts.TransferToSuccessor(ctx, st, artifacts.Succession{
		From:               artifacts.Owner{Kind: artifacts.PrincipalKindAgent, Ref: child.ID},
		ProjectID:          child.ProjectID,
		ToAgentID:          succ.AgentID,
		Sponsor:            succ.Sponsor,
		SponsorReviewGrant: succ.SponsorReviewGrant,
		Trigger:            trigger,
		OnTransferred: func(a *artifacts.Artifact, _ artifacts.TransferResult) {
			host.RecordTransfer(ctx, a.ID, fromRef, toRef, artifacts.AuthorityAncestorSuccession)
		},
	}, time.Now(), s.artifactLog())
	if err != nil {
		slog.WarnContext(ctx, "artifacts: ancestor succession stopped with error",
			"agent_id", child.ID, "successor_id", succ.AgentID, "project_id", child.ProjectID,
			"trigger", trigger, "transferred", res.Transferred, "skipped", res.Skipped, "error", err)
		return
	}
	if res.Transferred > 0 {
		slog.InfoContext(ctx, "artifacts: transferred deleted agent artifacts to ancestor",
			"agent_id", child.ID, "successor_id", succ.AgentID, "project_id", child.ProjectID,
			"trigger", trigger, "transferred", res.Transferred, "key_cleared", res.KeyCleared, "skipped", res.Skipped)
	}
}

var _ artifacts.OwnerSuccessorResolver = (*artifactHost)(nil)

// ResolveOwnerSuccessor implements artifacts.OwnerSuccessorResolver
// (ptone/scion#9846). It serves as the reconciler backstop for soft-deleted
// agents whose post-delete tail step did not run or did not finish.
func (h *artifactHost) ResolveOwnerSuccessor(ctx context.Context, agentID string) (artifacts.Succession, bool, error) {
	if h == nil || h.server == nil || !h.server.artifactsEnabled() || !h.server.experimentEnabled(experiments.Artifacts) {
		return artifacts.Succession{}, false, nil
	}
	if h.server.store == nil {
		return artifacts.Succession{}, false, errors.New("artifacts: resolve owner successor: store not available")
	}
	if _, err := uuid.Parse(agentID); err != nil {
		return artifacts.Succession{}, false, nil
	}
	row, err := h.server.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return artifacts.Succession{}, false, nil
		}
		return artifacts.Succession{}, false, err
	}
	if row == nil || row.DeletedAt.IsZero() {
		// Live row (restore race): do not transfer.
		return artifacts.Succession{}, false, nil
	}
	succ, found, err := h.server.resolveArtifactSuccessor(ctx, row)
	if err != nil {
		return artifacts.Succession{}, false, err
	}
	if !found {
		return artifacts.Succession{}, false, nil
	}
	fromRef := artifacts.PrincipalRef(artifacts.PrincipalKindAgent, row.ID)
	toRef := artifacts.PrincipalRef(artifacts.PrincipalKindAgent, succ.AgentID)
	return artifacts.Succession{
		From:               artifacts.Owner{Kind: artifacts.PrincipalKindAgent, Ref: row.ID},
		ProjectID:          row.ProjectID,
		ToAgentID:          succ.AgentID,
		Sponsor:            succ.Sponsor,
		SponsorReviewGrant: succ.SponsorReviewGrant,
		Trigger:            artifacts.SuccessionTriggerReconciler,
		OnTransferred: func(a *artifacts.Artifact, _ artifacts.TransferResult) {
			h.RecordTransfer(ctx, a.ID, fromRef, toRef, artifacts.AuthorityAncestorSuccession)
		},
	}, true, nil
}
