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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

const (
	// mutationTypeArtifactOwnershipTransfer is the mutation audit event type
	// emitted when a steward transfers artifact ownership to another agent.
	mutationTypeArtifactOwnershipTransfer = "artifact.ownership_transfer"

	// transferTargetsLimit caps the agents returned by TransferTargets.
	transferTargetsLimit = 200
)

var (
	_ artifacts.OwnershipTransfer = (*artifactHost)(nil)
	_ artifacts.TransferAuditor   = (*artifactHost)(nil)
)

// SessionCredentialAllowed implements artifacts.OwnershipTransfer: true only
// for interactive sessions and dev credentials (UATs are refused).
func (h *artifactHost) SessionCredentialAllowed(ctx context.Context) bool {
	return sessionCredentialAllowed(ctx)
}

// OwnerLive implements artifacts.OwnershipTransfer: reports whether agent
// ownerAgentID exists and is not soft-deleted. Any lookup failure answers
// (true, err) so callers fail strict (treat as live).
func (h *artifactHost) OwnerLive(ctx context.Context, ownerAgentID string) (bool, error) {
	if h.server == nil || h.server.store == nil {
		return true, errors.New("artifacts: owner liveness: store not available")
	}
	if _, err := uuid.Parse(ownerAgentID); err != nil {
		return false, nil
	}
	agent, err := h.server.store.GetAgent(ctx, ownerAgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return true, err
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return false, nil
	}
	return true, nil
}

// ControlsAgent implements artifacts.OwnershipTransfer: reports whether the
// caller of ctx controls agentID (issuerControlsAgent on a live row). False
// on any lookup failure.
func (h *artifactHost) ControlsAgent(ctx context.Context, agentID string) bool {
	if h.server == nil || h.server.store == nil || agentID == "" {
		return false
	}
	userID := artifactCallerUserID(ctx)
	if userID == "" {
		return false
	}
	agent, err := h.server.store.GetAgent(ctx, agentID)
	if err != nil || agent == nil || !agent.DeletedAt.IsZero() {
		return false
	}
	return issuerControlsAgent(agent, userID)
}

// eligibleTransferTarget evaluates the shared target-agent eligibility
// predicate for both TransferTarget and TransferTargets (design §4.3):
//  1. live row (exists, DeletedAt.IsZero())
//  2. same project (agent.ProjectID == homeScope)
//  3. caller controls agent (issuerControlsAgent)
//  4. good standing (agentStanding == nil) and no reincarnation in flight
//  5. non-empty DelegatingUser(agent.ID) (becomes new sponsor_ref)
func (h *artifactHost) eligibleTransferTarget(ctx context.Context, userID, homeScope string, agent *store.Agent) (sponsor string, ok bool, reason string, err error) {
	if agent == nil || !agent.DeletedAt.IsZero() {
		return "", false, artifacts.TargetReasonNotFound, nil
	}
	if agent.ProjectID != homeScope {
		return "", false, artifacts.TargetReasonOtherProject, nil
	}
	if !issuerControlsAgent(agent, userID) {
		return "", false, artifacts.TargetReasonNotControlled, nil
	}
	if reincarnationInFlight(agent) {
		return "", false, artifacts.TargetReasonStanding, nil
	}
	if stErr := h.server.agentStanding(ctx, agent.ID); stErr != nil {
		if errors.Is(stErr, errAgentNotInStanding) {
			return "", false, artifacts.TargetReasonStanding, nil
		}
		return "", false, "", stErr
	}
	sponsor, err = h.DelegatingUser(ctx, agent.ID)
	if err != nil {
		return "", false, "", err
	}
	if sponsor == "" {
		return "", false, artifacts.TargetReasonNoRoot, nil
	}
	return sponsor, true, "", nil
}

// TransferTarget implements artifacts.OwnershipTransfer.
func (h *artifactHost) TransferTarget(ctx context.Context, agentID, homeScope string) (sponsor string, ok bool, reason string, err error) {
	if h.server == nil || h.server.store == nil || h.server.authzService == nil {
		return "", false, "", errors.New("artifacts: transfer target: hub services unavailable")
	}
	userID := artifactCallerUserID(ctx)
	if userID == "" {
		return "", false, artifacts.TargetReasonNotControlled, nil
	}
	if _, err := uuid.Parse(agentID); err != nil {
		return "", false, artifacts.TargetReasonNotFound, nil
	}
	agent, err := h.server.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, artifacts.TargetReasonNotFound, nil
		}
		return "", false, "", err
	}
	return h.eligibleTransferTarget(ctx, userID, homeScope, agent)
}

// TransferTargets implements artifacts.OwnershipTransfer: lists agents in
// homeScope that TransferTarget would admit, capped at transferTargetsLimit.
func (h *artifactHost) TransferTargets(ctx context.Context, homeScope string) ([]artifacts.TransferCandidate, bool, error) {
	if h.server == nil || h.server.store == nil || h.server.authzService == nil {
		return nil, false, errors.New("artifacts: transfer targets: hub services unavailable")
	}
	userID := artifactCallerUserID(ctx)
	if userID == "" || homeScope == "" {
		return []artifacts.TransferCandidate{}, false, nil
	}
	out := make([]artifacts.TransferCandidate, 0)
	cursor := ""
	truncated := false
	for {
		res, err := h.server.store.ListAgents(ctx, store.AgentFilter{ProjectID: homeScope}, store.ListOptions{
			Limit:          200,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return nil, false, err
		}
		for i := range res.Items {
			agent := &res.Items[i]
			_, ok, _, err := h.eligibleTransferTarget(ctx, userID, homeScope, agent)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
			if len(out) >= transferTargetsLimit {
				truncated = true
				return out, truncated, nil
			}
			out = append(out, artifacts.TransferCandidate{
				ID:    agent.ID,
				Slug:  agent.Slug,
				Name:  agent.Name,
				Phase: store.EffectiveRestingPhase(agent),
			})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, truncated, nil
}

// RecordTransfer implements artifacts.TransferAuditor: best-effort mirror of
// a committed ownership transfer to the hub mutation audit log.
func (h *artifactHost) RecordTransfer(ctx context.Context, artifactID, fromOwner, toOwner, authority string) {
	if h.server == nil {
		return
	}
	beforeSummary, _ := json.Marshal(map[string]any{"owner": fromOwner})
	afterSummary, _ := json.Marshal(map[string]any{"owner": toOwner, "authority": authority})
	rec := &store.MutationAuditRecord{
		MutationType:  mutationTypeArtifactOwnershipTransfer,
		TargetType:    "artifact",
		TargetID:      artifactID,
		BeforeSummary: string(beforeSummary),
		AfterSummary:  string(afterSummary),
	}
	if authority == artifacts.AuthorityAncestorSuccession {
		rec.ActorPrincipalKind = artifacts.ActorKindSystem
		rec.ActorPrincipalID = authority
	}
	h.server.emitMutationAudit(ctx, rec)
}

// notifyArtifactTransfer delivers best-effort system notices after a committed
// ownership transfer: always to the new owner agent, and to the former owner
// agent when it was live. Uses the review-notice dispatch path
// (SystemCategoryArtifactReview): auto-wakes only system-suspended agents,
// never wakes user-suspended agents, and withholds from held agents.
func (s *Server) notifyArtifactTransfer(ctx context.Context, n artifacts.TransferNotice) artifacts.TransferNoticeResult {
	var res artifacts.TransferNoticeResult
	if s == nil || s.store == nil {
		return res
	}
	actorLabel := s.transferActorLabel(ctx, n.ActorUserID)
	fromSlug := s.agentSlugOrID(ctx, n.FromAgentID)
	toSlug := s.agentSlugOrID(ctx, n.ToAgentID)

	newOwnerText := fmt.Sprintf("You now own artifact %q (%s), transferred by %s from agent %s; publish new versions with `scion artifact publish`.",
		n.Title, artifacts.FormatRef(n.ArtifactID, 0), actorLabel, fromSlug)
	res.NewOwner = s.deliverTransferNoticeToAgent(ctx, n.ArtifactID, n.ToAgentID, newOwnerText)

	if n.FormerOwnerLive {
		formerOwnerText := fmt.Sprintf("Ownership of %q was transferred to agent %s by %s; you can no longer publish versions of it.",
			n.Title, toSlug, actorLabel)
		res.FormerOwner = s.deliverTransferNoticeToAgent(ctx, n.ArtifactID, n.FromAgentID, formerOwnerText)
	}
	return res
}

func (s *Server) transferActorLabel(ctx context.Context, userID string) string {
	if userID != "" {
		if u, err := s.store.GetUser(ctx, userID); err == nil && u != nil {
			if label := cleanNotificationText(u.DisplayName, reviewerLabelMaxRunes); label != "" {
				return label
			}
		}
	}
	return "a steward"
}

func (s *Server) agentSlugOrID(ctx context.Context, agentID string) string {
	if agentID == "" {
		return ""
	}
	if a, err := s.store.GetAgent(ctx, agentID); err == nil && a != nil && a.Slug != "" {
		return a.Slug
	}
	return agentID
}

func (s *Server) deliverTransferNoticeToAgent(ctx context.Context, artifactID, agentID, text string) (delivered bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("artifacts: transfer notice panicked",
				"artifact_id", artifactID, "agent_id", agentID, "panic", fmt.Sprint(r))
			delivered = false
		}
	}()
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil || agent == nil || !agent.DeletedAt.IsZero() || reincarnationInFlight(agent) {
		return false
	}
	if heldNoticeWithheld(ctx, s.store, s.recordLog(), agent, heldNoticeArtifactReview) {
		return false
	}
	// System-suspended: auto-wake in the background and deliver once woken.
	if s.canAutoWakeActionAsker(ctx, agent) {
		return s.queueAutoWake(agent, s.artifactTransferAutoWakeItem(artifactID, agentID, text))
	}
	// User-suspended: never wake.
	if state.Phase(store.EffectiveRestingPhase(agent)) == state.PhaseSuspended {
		return false
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artifactReviewNoticeTimeout)
	defer cancel()
	if err := s.dispatchTransferNotice(dctx, agent, text); err != nil {
		var idleErr *errAgentIdleSuspending
		if errors.As(err, &idleErr) {
			target := idleErr.agent
			if target == nil {
				target = agent
			}
			if s.canAutoWakeActionAsker(ctx, target) && s.queueAutoWake(target, s.artifactTransferAutoWakeItem(artifactID, target.ID, text)) {
				return true
			}
		}
		slog.WarnContext(ctx, "artifacts: transfer notice not delivered",
			"artifact_id", artifactID, "agent_id", agentID, "error", err)
		return false
	}
	return true
}

func (s *Server) artifactTransferAutoWakeItem(artifactID, agentID, text string) autoWakeItem {
	return autoWakeItem{
		deliver: func(ctx context.Context, woken *store.Agent) {
			dctx, cancel := context.WithTimeout(ctx, chatWakeDeliveryBudget)
			defer cancel()
			if err := s.dispatchTransferNotice(dctx, woken, text); err != nil {
				slog.WarnContext(dctx, "artifacts: failed to deliver transfer notice after auto-wake",
					"artifact_id", artifactID, "agent_id", woken.ID, "error", err)
			}
		},
		fail: func(ctx context.Context, dmErr *AgentDMError) {
			slog.WarnContext(ctx, "artifacts: auto-wake failed for transfer notice",
				"artifact_id", artifactID, "agent_id", agentID, "code", dmErr.Code, "error", dmErr.Message)
		},
	}
}

func (s *Server) dispatchTransferNotice(ctx context.Context, agent *store.Agent, text string) error {
	if agent == nil || !agent.DeletedAt.IsZero() || reincarnationInFlight(agent) {
		return fmt.Errorf("agent unavailable")
	}
	if heldNoticeWithheld(ctx, s.store, s.recordLog(), agent, heldNoticeArtifactReview) {
		return nil
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return fmt.Errorf("no dispatcher available")
	}
	msg := messages.NewSystemMessage("system", "agent:"+agent.Slug, text, messages.SystemCategoryArtifactReview)
	msg.RecipientID = agent.ID
	var ts time.Time
	if t, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil {
		ts = t
	}
	msg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{Msg: msg, CreatedAt: ts})
	return dispatchWithBrokerRetry(ctx, s.deliveryMarker(), s.recordLog(), dispatcher, agent, text, false, msg)
}
