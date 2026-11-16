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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const maxMessageActionPayloadBytes = 16 * 1024

// messageAction implements FireAction and FirePayloadValidator for deferred
// "message" fires dispatched to agents through the AgentDispatcher.
//
// Each fire runs under the recorded authorization revision: payload validation
// runs first, recorded authority is resolved by fireAction before Authorize is
// entered, and Authorize resolves the target agent, checks live send
// authorization through authorizeAgentMessage (isSystemPlane=false), enforces
// standing, gates in-flight reincarnations, and authorizes explicit wake
// requests before Fire executes any wake or delivery.
type messageAction struct {
	srv *Server
}

type messageAuthorized struct {
	payload  MessageEventPayload
	agent    *store.Agent
	auth     FireAuthority
	autoWake bool
	wake     bool
}

func (a *messageAction) Type() string {
	return "message"
}

func (a *messageAction) Permission() string {
	return scheduledMessagePermission
}

func (a *messageAction) Detaches(req hubfire.FireRequest) bool {
	return scheduledPayloadWakes(req.Payload)
}

func (a *messageAction) Validate(ctx context.Context, caller Identity, projectID string, payload string) (string, error) {
	if a == nil || a.srv == nil || a.srv.store == nil {
		return "", errors.New("message action: server not configured")
	}
	if caller == nil {
		return "", errors.New("caller identity is required")
	}

	var rawObj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &rawObj); err != nil || rawObj == nil {
		if err != nil {
			return "", fmt.Errorf("invalid message payload: %w", err)
		}
		return "", errors.New("invalid message payload: expected JSON object")
	}

	var p MessageEventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", fmt.Errorf("invalid message payload: %w", err)
	}
	if strings.TrimSpace(p.Message) == "" {
		return "", errors.New("message payload is empty")
	}
	if len(p.Message) > maxMessageActionPayloadBytes {
		return "", fmt.Errorf("message payload exceeds maximum length of %d bytes", maxMessageActionPayloadBytes)
	}

	agentID := strings.TrimSpace(p.AgentID)
	agentName := strings.TrimSpace(p.AgentName)
	var target *store.Agent
	var err error
	switch {
	case agentID != "":
		target, err = a.srv.store.GetAgent(ctx, agentID)
	case agentName != "" && projectID != "":
		target, err = a.srv.store.GetAgentBySlug(ctx, projectID, agentName)
	default:
		return "", errors.New("message payload must include agentId or agentName")
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errors.New("target agent not found")
		}
		return "", fmt.Errorf("target agent lookup failed: %w", err)
	}
	if target == nil || !target.DeletedAt.IsZero() ||
		(agentID != "" && agentName != "" && agentName != target.Slug) {
		return "", errors.New("target agent not found")
	}

	if target.ProjectID != projectID {
		ops := a.srv.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			return "", errors.New("cross-project messages require cross-project messaging to be enabled")
		}
	}

	allowed, reason, _ := a.srv.authorizeAgentMessage(ctx, caller, target, false)
	if !allowed {
		return "", fmt.Errorf("not authorized to message agent %q: %s", target.Slug, reason)
	}

	p.AgentID = target.ID
	p.AgentName = target.Slug
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode normalized message payload: %w", err)
	}
	return string(encoded), nil
}

func (a *messageAction) ValidatePayload(req hubfire.FireRequest) error {
	payload, err := unmarshalMessageFirePayload(req.Payload)
	if err != nil {
		return err
	}

	// Log staleness for fires that ran late (e.g. after server downtime).
	staleness := time.Since(req.FireAt)
	if !req.FireAt.IsZero() && staleness > 1*time.Minute {
		slog.Warn("Scheduler: firing stale message event",
			"eventID", req.Authority.FireID,
			"agentName", payload.AgentName,
			"agent_id", payload.AgentID,
			"scheduledFor", req.FireAt.UTC().Format(time.RFC3339),
			"staleness", staleness.Truncate(time.Second).String())
	}

	if payload.AgentID == "" && (payload.AgentName == "" || req.ProjectID == "") {
		return fmt.Errorf("message payload must include agentId or agentName")
	}
	return nil
}

func unmarshalMessageFirePayload(raw string) (MessageEventPayload, error) {
	var payload MessageEventPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return MessageEventPayload{}, fmt.Errorf("invalid message payload: %w", err)
	}
	if payload.Message == "" {
		return MessageEventPayload{}, fmt.Errorf("message payload is empty")
	}
	return payload, nil
}

func (a *messageAction) Authorize(
	ctx context.Context,
	req hubfire.FireRequest,
	auth FireAuthority,
	identity Identity,
) (FireAuthorized, *FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, nil, errors.New("message action: server not configured")
	}
	s := a.srv
	payload, err := unmarshalMessageFirePayload(req.Payload)
	if err != nil {
		return nil, nil, err
	}
	if payload.AgentID == "" && (payload.AgentName == "" || req.ProjectID == "") {
		return nil, nil, fmt.Errorf("message payload must include agentId or agentName")
	}

	targetName := payload.AgentName
	if targetName == "" {
		targetName = payload.AgentID
	}
	fireID := req.Authority.FireID

	var agent *store.Agent
	if payload.AgentID != "" {
		agent, err = s.store.GetAgent(ctx, payload.AgentID)
	} else {
		agent, err = s.store.GetAgentBySlug(ctx, req.ProjectID, payload.AgentName)
	}
	if err != nil {
		// The returned refusal text is persisted on the fire record, which
		// project readers can see. A missing target, a lookup failure, and an
		// authorization refusal below all record errScheduledMessageRefused;
		// the specific cause is logged here.
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("Scheduler: target agent no longer exists",
				"eventID", fireID,
				"agentName", payload.AgentName,
				"agent_id", payload.AgentID,
				"projectID", req.ProjectID,
				"cause", "target agent deleted")
		} else {
			slog.Warn("Scheduler: target agent lookup failed",
				"eventID", fireID,
				"agentName", targetName,
				"projectID", req.ProjectID,
				"error", err)
		}
		return nil, &FireRefusal{Reason: errScheduledMessageRefused.Error()}, nil
	}

	// ---- Fire-time authorization ----
	// Authorize the send for the revision's identity through the production
	// choke point (authorizeAgentMessage, isSystemPlane=false). No external
	// effect occurs on denial.
	if authErr := s.authorizeMessageFire(ctx, req.ProjectID, auth, identity, agent); authErr != nil {
		slog.Warn("Scheduler: scheduled message refused at fire time",
			"eventID", fireID,
			"agent_id", agent.ID,
			"projectID", req.ProjectID,
			"principal_kind", auth.PrincipalKind,
			"principal_id", auth.PrincipalID,
			"authorization_revision", auth.Revision,
			"error", authErr)
		return nil, &FireRefusal{Reason: errScheduledMessageRefused.Error()}, nil
	}
	if err := s.fireStanding(ctx, auth, agent); err != nil {
		return nil, &FireRefusal{Reason: err.Error()}, nil
	}

	// Migration gate (design agent-reincarnate §3.7, R3 p2a-r1 review).
	// O-a (p2a-r2 review): checked AFTER send authorization and standing, not
	// before — a denied caller must learn nothing about the recipient's
	// migration state — and BEFORE wake authorization so an in-flight
	// reincarnation fails loudly rather than entering wake permission or
	// resume handling.
	if reincarnationInFlight(agent) {
		return nil, nil, fmt.Errorf("target agent is reincarnating")
	}

	// Wake (ptone/scion#7734): after authorization, standing, and the migration
	// gate, so a refused or migrating fire resumes nothing. A target the hub
	// suspended (ptone/scion#7826) wakes without the payload's wake flag and
	// without the wake permission: the send authorization above suffices.
	autoWake := s.autoWakeModeOf(agent) != autoWakeNone
	wake := payload.Wake || autoWake
	if wake && !autoWake {
		if wakeErr := s.authorizeScheduledWake(ctx, auth, identity, agent); wakeErr != nil {
			slog.Warn("Scheduler: scheduled message wake refused at fire time",
				"eventID", fireID,
				"agent_id", agent.ID,
				"projectID", req.ProjectID,
				"principal_kind", auth.PrincipalKind,
				"principal_id", auth.PrincipalID,
				"authorization_revision", auth.Revision,
				"error", wakeErr)
			return nil, &FireRefusal{Reason: errScheduledMessageRefused.Error()}, nil
		}
	}

	return &messageAuthorized{
		payload:  payload,
		agent:    agent,
		auth:     auth,
		autoWake: autoWake,
		wake:     wake,
	}, nil, nil
}

func (a *messageAction) Fire(ctx context.Context, req hubfire.FireRequest, authorized FireAuthorized) (hubfire.FireOutcome, error) {
	if a == nil || a.srv == nil {
		return hubfire.FireOutcome{}, errors.New("message action: server not configured")
	}
	ma, ok := authorized.(*messageAuthorized)
	if !ok || ma == nil {
		return hubfire.FireOutcome{}, errors.New("message action: missing authorized state")
	}
	return a.srv.deliverMessageAction(ctx, req, ma)
}

func buildMessageActionMessage(agent *store.Agent, payload MessageEventPayload) *messages.StructuredMessage {
	structuredMsg := messages.NewSystemMessage("scheduler", "agent:"+agent.Slug, payload.Message, messages.SystemCategoryScheduler)
	structuredMsg.SenderID = "SCHEDULER"
	structuredMsg.RecipientID = agent.ID
	structuredMsg.Plain = payload.Plain
	structuredMsg.Urgent = payload.Interrupt
	return structuredMsg
}

func (s *Server) deliverMessageAction(ctx context.Context, req hubfire.FireRequest, ma *messageAuthorized) (hubfire.FireOutcome, error) {
	payload := ma.payload
	agent := ma.agent
	auth := ma.auth
	fireID := req.Authority.FireID
	var scheduleID string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
	}

	if reincarnationInFlight(agent) {
		return hubfire.FireOutcome{}, fmt.Errorf("target agent is reincarnating")
	}

	if ma.wake {
		if ma.autoWake {
			slog.Info("Scheduler: waking a hub-suspended target for a scheduled message",
				"eventID", fireID, "agent_id", agent.ID, "suspend_reason", agent.SuspendReason)
		}
		tracker := req.Tracked
		if tracker == nil {
			tracker = newScheduledFireTracker(s, fireID, nil)
		}
		trackedCtx, endTracking, admitted := tracker.Admit(ctx)
		if !admitted {
			return hubfire.FireOutcome{}, errScheduledWakeShuttingDown
		}
		defer endTracking()
		ctx = trackedCtx
		if err := s.wakeFireTarget(ctx, fireID, scheduleID, agent); err != nil {
			if scheduledFireAborted(ctx) {
				return hubfire.FireOutcome{}, errScheduledWakeShutdown
			}
			return hubfire.FireOutcome{}, err
		}
	} else if phaseErr := validateAgentDeliverable(agent); phaseErr != nil {
		slog.Warn("Scheduler: scheduled message target is not running",
			"eventID", fireID,
			"scheduleID", scheduleID,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"phase", store.EffectiveRestingPhase(agent),
			"error", phaseErr.Message)
		return hubfire.FireOutcome{}, errors.New(phaseErr.Message)
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return hubfire.FireOutcome{}, fmt.Errorf("no dispatcher available to deliver message")
	}

	// Reconstruct structured message from payload to preserve traits like Plain.
	structuredMsg := buildMessageActionMessage(agent, payload)

	// A wake fire may have spent the handler's deadline on the resume
	// (wakeFireTarget runs on its own budget); its persist and dispatch get a
	// fresh bound, keeping the handler's values.
	dispatchBase := ctx
	if ma.wake {
		tracker := req.Tracked
		if tracker == nil {
			tracker = newScheduledFireTracker(s, fireID, nil)
		}
		detached, cancelDetached := tracker.Detach(ctx)
		defer cancelDetached()
		dispatchBase = detached
	}
	retryCtx, retryCancel := context.WithTimeout(dispatchBase, scheduledMessageDispatchTimeout)
	defer retryCancel()

	// When hub.conduit_deliver is enabled, persist a store.Message row
	// before dispatch so Conduit row binding and awaiting_session
	// hold/re-drive cover scheduled messages including wake fires
	// (ptone/scion#7881, Q5=a, REQ-4). When hub.conduit_deliver is off,
	// preserve exact Phase A no-row / omitted-message_id behavior.
	var storeMsg *store.Message
	holdMsgID := ""
	renderMessageID := ""
	renderCreatedAt := time.Now().UTC()
	dispatchCtx := retryCtx
	if s.conduitDeliverEnabled() {
		var persistErr error
		storeMsg, persistErr = s.persistScheduledMessageEvent(retryCtx, req.ProjectID, fireID, agent, payload, structuredMsg)
		if persistErr != nil {
			return hubfire.FireOutcome{}, persistErr
		}
		holdMsgID = storeMsg.ID
		renderMessageID = storeMsg.ID
		renderCreatedAt = storeMsg.CreatedAt
		dispatchCtx = withDispatchMessageID(retryCtx, storeMsg.ID)
	}
	if s.writeDenyEnabled() {
		structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID:  renderMessageID,
			ConvResult: nil,
			Msg:        structuredMsg,
			CreatedAt:  renderCreatedAt,
		})
	}

	if _, err := s.holdOrDispatch(dispatchCtx, agent, holdMsgID, storeMsg, func() error {
		return dispatchWithBrokerRetry(dispatchCtx, s.recordLog(), dispatcher, agent, payload.Message, payload.Interrupt, structuredMsg)
	}); err != nil {
		if storeMsg != nil {
			_ = s.markFailed(retryCtx, storeMsg.ID, err.Error())
		}
		if ma.wake && scheduledFireAborted(ctx) {
			return hubfire.FireOutcome{}, errScheduledWakeShutdown
		}
		if isBrokerAgentNotFound(err) {
			logArgs := []any{
				"eventID", fireID,
				"scheduleID", scheduleID,
				"agent_id", agent.ID,
				"agentName", agent.Name,
				"wake", ma.wake,
				"error", err,
			}
			var se *brokerStatusError
			if errors.As(err, &se) {
				logArgs = append(logArgs,
					"broker_status", se.StatusCode,
					"broker_body_len", len(se.Body),
					"broker_body", brokerBodyForLog(se.Body),
				)
			}
			slog.Warn("Scheduler: broker reported agent not found on message dispatch", logArgs...)
			if !ma.wake {
				if fresh, gerr := s.store.GetAgent(retryCtx, agent.ID); gerr == nil && fresh != nil {
					if phaseErr := validateAgentDeliverable(fresh); phaseErr != nil {
						return hubfire.FireOutcome{}, errors.New(phaseErr.Message)
					}
				}
			}
			return hubfire.FireOutcome{}, fmt.Errorf("agent %q has no running container; message was not delivered", agentTargetLabel(agent))
		}
		return hubfire.FireOutcome{}, fmt.Errorf("failed to dispatch message to agent %s: %w", agent.Name, err)
	}

	// Log the revision authority the send ran under alongside the executor
	// context set by the caller so a deferred message is distinguishable in
	// logs from a live send.
	executor, _ := ExecutorContextFromContext(ctx)
	slog.Info("Scheduler: message delivered to agent",
		"eventID", fireID, "agent_id", agent.ID, "agentName", agent.Name,
		"initiator_principal_kind", auth.PrincipalKind,
		"initiator_credential_kind", auth.CredentialKind,
		"initiator_credential_id", auth.CredentialID,
		"authorization_revision", auth.Revision,
		"executor_kind", executor.Kind,
		"executor_id", executor.ID)
	return hubfire.FireOutcome{
		Status:    hubfire.FireStatusFired,
		ResultRef: holdMsgID,
	}, nil
}
