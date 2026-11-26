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
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Review notices (ptone/scion#3229, design §8.2, ptone/scion#8946). When a
// review version of an artifact is finalized, the hub tells the artifact's
// owning agent with a system message (category artifact-review) that names
// the version and how to fetch it. The notice is the hub's own message: it
// is sent as "system", never as the reviewer, and it carries the reference
// in its body only (not in the artifacts metadata key, which is admitted
// only under a sender's credential). Whether the owner may read the version
// is decided when it runs the command, as for every reference. A user owner
// gets an in-app notification instead, for a review by an authorized
// reviewer other than the owner (notifyArtifactReviewSummary); the artifact
// page shows the review-on-latest state as well.
//
// Suspension handling (ptone/scion#8946):
//   - System-suspended (idle/stalled under lifecycle.idle_suspend): auto-wakes
//     in the background via the shared auto-wake queue (canAutoWakeActionAsker
//     + queueAutoWake) and delivers the review notice once ready.
//   - User-suspended: offerArtifactReviewWake offers a 409 wake confirmation
//     using the exact chat wake-offer payload when the caller may wake the
//     agent (suspendedPrimaryWakeable). On finalize with Wake=true, the hub
//     checks agentLifecycleAllowed and resumes via wakeAgentForDM; a refused
//     or failed wake never discards the already-saved review.

// artifactReviewNoticeTimeout bounds the background delivery of one notice.
const artifactReviewNoticeTimeout = 30 * time.Second

// offerArtifactReviewWake implements the pre-finalize wake-offer check for
// reviews of user-suspended agents. It returns nil for non-agent owners,
// running agents, system-suspended agents (which auto-wake without prompting),
// and callers without lifecycle permission on the owner.
func (s *Server) offerArtifactReviewWake(ctx context.Context, a *artifacts.Artifact) *artifacts.ReviewWakeOffer {
	if a == nil || a.OwnerKind != artifacts.PrincipalKindAgent || a.OwnerRef == "" {
		return nil
	}
	user := GetUserIdentityFromContext(ctx)
	if isNilIdentity(user) {
		return nil
	}
	agent, err := s.store.GetAgent(ctx, a.OwnerRef)
	if err != nil || agent == nil {
		return nil
	}
	if s.canAutoWakeActionAsker(ctx, agent) {
		return nil
	}
	if !s.suspendedPrimaryWakeable(ctx, user, agent) {
		return nil
	}
	return &artifacts.ReviewWakeOffer{
		AgentID:   agent.ID,
		AgentSlug: agent.Slug,
		Phase:     agent.Phase,
	}
}

// notifyArtifactReview is the artifact service's review notifier. For a
// user owner it stores the review summary notification. For an agent owner
// it delegates to notifyArtifactReviewWithDelivery.
func (s *Server) notifyArtifactReview(ctx context.Context, n artifacts.ReviewNotice) {
	_ = s.notifyArtifactReviewWithDelivery(ctx, nil, n)
}

// canReadArtifactOwnerAgent reports whether the caller holds ActionRead on
// agent (or authz is unset). Both reviewDelivery.status and
// reviewDelivery.agentSlug are gated on this check so a cross-project review
// grantee who cannot read the owning agent learns neither its slug nor its
// suspension state via reviewDelivery.
func (s *Server) canReadArtifactOwnerAgent(ctx context.Context, agent *store.Agent) bool {
	if agent == nil {
		return false
	}
	if s.authzService == nil {
		return true
	}
	caller := GetIdentityFromContext(ctx)
	if isNilIdentity(caller) {
		return false
	}
	return s.authzService.CheckAccess(ctx, caller, agentResource(agent), ActionRead).Allowed
}

// notifyArtifactReviewWithDelivery handles post-finalize review notification
// and returns optional delivery/wake metadata when the owning agent was
// suspended. For running agents, user owners, and callers without ActionRead
// on the owning agent it returns nil.
//
// Authorization invariants (ptone/scion#8946):
//   - System-suspended (idle/stalled under lifecycle.idle_suspend): auto-wake
//     needs no lifecycle permission because the hub suspended the agent
//     (same rule as chat and Decisions #8517).
//   - User-suspended without Wake=true: never wakes the agent.
//   - User-suspended with Wake=true: requires agentLifecycleAllowed(ctx,
//     caller, agent), the exact same check as chat. A refused or failed wake
//     never discards the already-committed review.
func (s *Server) notifyArtifactReviewWithDelivery(ctx context.Context, w http.ResponseWriter, n artifacts.ReviewNotice) *artifacts.ReviewDeliveryInfo {
	if n.OwnerKind == artifacts.PrincipalKindUser {
		s.notifyArtifactReviewSummary(ctx, n)
		return nil
	}
	if n.OwnerKind != artifacts.PrincipalKindAgent || n.OwnerRef == "" {
		return nil
	}
	agent, err := s.store.GetAgent(ctx, n.OwnerRef)
	if err != nil || agent == nil {
		if err != nil {
			slog.WarnContext(ctx, "artifacts: review notice owner lookup failed",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", n.OwnerRef, "error", err)
		}
		return nil
	}
	if !agent.DeletedAt.IsZero() || reincarnationInFlight(agent) {
		return nil
	}
	// A held owner gets no notice (ptone/scion#7145); the drop is logged
	// on the message record log.
	if heldNoticeWithheld(ctx, s.store, s.recordLog(), agent, heldNoticeArtifactReview) {
		return nil
	}

	canRead := s.canReadArtifactOwnerAgent(ctx, agent)
	deliveryInfo := func(status, message string) *artifacts.ReviewDeliveryInfo {
		if !canRead {
			return nil
		}
		return &artifacts.ReviewDeliveryInfo{
			Status:    status,
			AgentID:   agent.ID,
			AgentSlug: agent.Slug,
			Message:   message,
		}
	}

	// System-suspended (idle/stalled) or idle-suspending: auto-wake in the
	// background using the same helper and queue as Decisions (#8517).
	if s.canAutoWakeActionAsker(ctx, agent) {
		if s.queueAutoWake(agent, s.artifactReviewAutoWakeItem(n)) {
			return deliveryInfo(artifacts.ReviewDeliveryAutoWaking, "")
		}
		return deliveryInfo(artifacts.ReviewDeliveryWakeFailed, "hub is shutting down; could not wake agent")
	}

	// User-suspended (or suspended while lifecycle.idle_suspend is off):
	// never auto-wake. Honor explicit Wake=true only when authorized; a
	// refused or failed wake leaves the saved review intact and reports why.
	if state.Phase(store.EffectiveRestingPhase(agent)) == state.PhaseSuspended {
		if !n.Wake {
			return deliveryInfo(artifacts.ReviewDeliverySuspended, "")
		}
		caller := GetIdentityFromContext(ctx)
		if isNilIdentity(caller) || !s.agentLifecycleAllowed(ctx, caller, agent) {
			return deliveryInfo(artifacts.ReviewDeliveryWakeRefused, "You do not have permission to wake this agent")
		}
		if s.agentHeldOrUnknown(ctx, agent.ID) {
			return deliveryInfo(artifacts.ReviewDeliveryWakeRefused, "Agent is held and cannot be woken")
		}
		if w != nil {
			extendWriteDeadlineForWake(w, 1)
		}
		wakeCtx, cancelWake := context.WithTimeout(context.WithoutCancel(ctx), chatWakeResumeBudget)
		defer cancelWake()
		wakeRes, wakeErr := s.wakeAgentForDM(wakeCtx, agent)
		if wakeErr != nil {
			status := artifacts.ReviewDeliveryWakeFailed
			if isWakeRefusal(wakeErr) {
				status = artifacts.ReviewDeliveryWakeRefused
			}
			slog.WarnContext(ctx, "artifacts: wake for review failed",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", agent.ID, "code", wakeErr.Code, "error", wakeErr.Message)
			return deliveryInfo(status, wakeErr.Message)
		}
		if wakeRes != nil && wakeRes.Outcome == WakeDeferred {
			if agent.StartClaimKind == store.StartClaimStop {
				return deliveryInfo(artifacts.ReviewDeliveryWakeFailed, s.deferredReason(agent))
			}
			target := *agent
			if !s.autoWakeWork.goTracked(func(bgCtx context.Context) {
				// A notice is best effort: a panic while waiting or sending it
				// must not take the hub down.
				defer func() {
					if r := recover(); r != nil {
						slog.Error("artifacts: panic delivering review notice after deferred wake",
							"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", target.ID, "panic", fmt.Sprint(r))
					}
				}()
				waitCtx, cancelWait := context.WithTimeout(bgCtx, chatWakeResumeBudget)
				defer cancelWait()
				err := s.awaitStartInProgress(waitCtx, &target)
				if err != nil {
					slog.WarnContext(bgCtx, "artifacts: deferred wake did not become ready for review notice",
						"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", target.ID, "error", err)
					return
				}
				dctx, cancelDispatch := context.WithTimeout(bgCtx, artifactReviewNoticeTimeout)
				defer cancelDispatch()
				if err := s.deliverArtifactReview(dctx, n); err != nil {
					slog.WarnContext(dctx, "artifacts: failed to deliver review notice after deferred wake",
						"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", target.ID, "error", err)
				}
			}) {
				return deliveryInfo(artifacts.ReviewDeliveryWakeFailed, "hub is shutting down; could not deliver review after wake")
			}
			return deliveryInfo(artifacts.ReviewDeliveryWaking, "")
		}
		dctx, cancelDispatch := context.WithTimeout(context.WithoutCancel(ctx), artifactReviewNoticeTimeout)
		defer cancelDispatch()
		err := s.deliverArtifactReview(dctx, n)
		if err != nil {
			slog.WarnContext(ctx, "artifacts: review notice not delivered after wake",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", agent.ID, "error", err)
			return deliveryInfo(artifacts.ReviewDeliveryWakeFailed, "agent was woken, but the review notice could not be delivered")
		}
		return deliveryInfo(artifacts.ReviewDeliveryWoken, "")
	}

	// Running (or other non-suspended phase): keep existing async delivery
	// unchanged so finalize does not wait on a broker.
	go func() {
		// A notice is best effort: a panic while sending it must not take
		// the hub down.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("artifacts: review notice panicked",
					"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", n.OwnerRef, "panic", fmt.Sprint(r))
			}
		}()
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artifactReviewNoticeTimeout)
		defer cancel()
		if err := s.deliverArtifactReview(dctx, n); err != nil {
			slog.WarnContext(dctx, "artifacts: review notice not delivered",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", n.OwnerRef, "error", err)
		}
	}()
	return nil
}

// artifactReviewAutoWakeItem queues delivery of a review notice behind a
// background auto-wake of the owning agent.
func (s *Server) artifactReviewAutoWakeItem(n artifacts.ReviewNotice) autoWakeItem {
	return autoWakeItem{
		deliver: func(ctx context.Context, woken *store.Agent) {
			dctx, cancel := context.WithTimeout(ctx, chatWakeDeliveryBudget)
			defer cancel()
			if err := s.deliverArtifactReview(dctx, n); err != nil {
				slog.WarnContext(dctx, "artifacts: failed to deliver review notice after auto-wake",
					"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", woken.ID, "error", err)
			}
		},
		fail: func(ctx context.Context, dmErr *AgentDMError) {
			slog.WarnContext(ctx, "artifacts: auto-wake failed for review notice",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", n.OwnerRef, "code", dmErr.Code, "error", dmErr.Message)
		},
	}
}

// deliverArtifactReview dispatches the review notice to the owning agent.
func (s *Server) deliverArtifactReview(ctx context.Context, n artifacts.ReviewNotice) error {
	agent, err := s.store.GetAgent(ctx, n.OwnerRef)
	if err != nil {
		return fmt.Errorf("owner agent: %w", err)
	}
	if reincarnationInFlight(agent) {
		return fmt.Errorf("owner agent is reincarnating")
	}
	// A held owner gets no notice (ptone/scion#7145); the drop is logged
	// on the message record log.
	if heldNoticeWithheld(ctx, s.store, s.recordLog(), agent, heldNoticeArtifactReview) {
		return nil
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return fmt.Errorf("no dispatcher available")
	}
	text := artifactReviewNoticeText(n)
	msg := messages.NewSystemMessage("system", "agent:"+agent.Slug, text, messages.SystemCategoryArtifactReview)
	msg.RecipientID = agent.ID
	var ts time.Time
	if t, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil {
		ts = t
	}
	msg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{Msg: msg, CreatedAt: ts})
	err = dispatchWithBrokerRetry(ctx, s.deliveryMarker(), s.recordLog(), dispatcher, agent, text, false, msg)
	if err != nil {
		var idleErr *errAgentIdleSuspending
		if errors.As(err, &idleErr) {
			target := idleErr.agent
			if target == nil {
				target = agent
			}
			if s.canAutoWakeActionAsker(ctx, target) && s.queueAutoWake(target, s.artifactReviewAutoWakeItem(n)) {
				return nil
			}
		}
	}
	return err
}

// artifactReviewNoticeText is the body of a review notice. It is built from
// the notice alone, with the same fetch-hint line as a message reference.
func artifactReviewNoticeText(n artifacts.ReviewNotice) string {
	ref := n.Ref()
	var b strings.Builder
	by := "someone else"
	switch n.ReviewerKind {
	case artifacts.PrincipalKindAgent:
		by = "an agent"
	case artifacts.PrincipalKindUser:
		by = "a user"
	}
	fmt.Fprintf(&b, "A review (v%d, by %s) was published on your artifact; it is now the current version.\n", n.Seq, by)
	fmt.Fprintf(&b, "Artifact: v%d - scion artifact get %s\n", n.Seq, ref)
	fmt.Fprintf(&b, "Without the marks: scion artifact get %s --clean\n", ref)
	fmt.Fprintf(&b, "Review comments: scion artifact comments %s\n", ref)
	fmt.Fprintf(&b, "Apply or answer the marks, then publish the result as the next version: scion artifact publish <file> --version-of %s",
		artifacts.FormatRef(n.ArtifactID, 0))
	return b.String()
}
