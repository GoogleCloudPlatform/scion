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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// holdOrDispatch enforces the per-agent awaiting_session hold gate before a
// persisted message is dispatched to a runtime broker (ptone/scion#7881,
// AC A2-3, A2-S2).
//
// When hub.conduit_deliver is enabled, the target agent uses conduit delivery,
// and messageID names a persisted store.Message row:
//   - PrepareOrHoldMessage atomically stamps the target's current RunID onto
//     hold_launch_id and, if an agent_delivery_holds marker is active (under
//     maxHeldMessagesPerAgent), moves the row directly to awaiting_session
//     without calling dispatchFn.
//   - When the row is held, holdOrDispatch updates storeMsg (if non-nil) to
//     reflect awaiting_session, logs a content-free event, nudges the re-drive
//     runner if a session is already connected, and returns (true, nil).
//
// In all other cases (experiment off, non-conduit runtime, unpersisted message,
// no active hold marker, or per-agent hold cap reached), holdOrDispatch runs
// dispatchFn and returns (false, err).
func (s *Server) holdOrDispatch(
	ctx context.Context,
	agent *store.Agent,
	messageID string,
	storeMsg *store.Message,
	dispatchFn func() error,
) (bool, error) {
	if s == nil || s.store == nil || agent == nil || messageID == "" ||
		!s.conduitDeliverEnabled() || !s.agentUsesConduitDelivery(ctx, agent) {
		return false, dispatchFn()
	}

	finCtx, finCancel := finalizationContext(ctx)
	res, err := s.store.PrepareOrHoldMessage(finCtx, agent.ID, messageID, s.heldMessagesCap())
	finCancel()
	if err != nil {
		return false, fmt.Errorf("prepare or hold message: %w", err)
	}
	if storeMsg != nil {
		storeMsg.HoldLaunchID = res.HoldLaunchID
		if res.Held {
			storeMsg.DispatchState = store.MessageDispatchAwaitingSession
			storeMsg.HoldUntil = res.HoldUntil
			storeMsg.DispatchedAt = nil
		}
	}
	if res.Held {
		if s.messageLog != nil {
			s.messageLog.Info("message queued behind awaiting_session hold",
				"message_id", messageID,
				"agent_id", agent.ID,
				"project_id", agent.ProjectID,
			)
		}
		s.maybeScheduleHeldMessageRedriveIfConnected(agent.ID)
		return true, nil
	}
	return false, dispatchFn()
}

// persistScheduledMessageEvent persists a store.Message row for a scheduler
// "message" event fire before dispatch so Conduit row binding and
// awaiting_session hold/re-drive cover scheduled messages including wake fires
// (ptone/scion#7881, Q5=a, AC A2-10).
//
// Scheduled system messages are timer/cron-originated rather than part of a
// two-party chat conversation, so ConversationID is intentionally empty
// (matching broadcastDirect); ThreadID records evt.ID so re-drive can
// re-evaluate the scheduled event's authority and fire-time authorization.
func (s *Server) persistScheduledMessageEvent(
	ctx context.Context,
	evt store.ScheduledEvent,
	agent *store.Agent,
	payload MessageEventPayload,
	structuredMsg *messages.StructuredMessage,
) (*store.Message, error) {
	createdAt := time.Now().UTC()
	senderProjID := evt.ProjectID
	recipProjID := agent.ProjectID
	storeMsg := &store.Message{
		ID:                 api.NewUUID(),
		ProjectID:          agent.ProjectID,
		Sender:             structuredMsg.Sender,
		SenderID:           structuredMsg.SenderID,
		SenderProjectID:    &senderProjID,
		Recipient:          structuredMsg.Recipient,
		RecipientID:        agent.ID,
		RecipientProjectID: &recipProjID,
		Msg:                payload.Message,
		Type:               structuredMsg.Type,
		Urgent:             payload.Interrupt,
		AgentID:            agent.ID,
		Channel:            "scheduler",
		ThreadID:           evt.ID,
		DispatchState:      store.MessageDispatchDispatched,
		CreatedAt:          createdAt,
	}
	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		return nil, fmt.Errorf("failed to persist scheduled message: %w", err)
	}
	structuredMsg.StoredMessageID = storeMsg.ID
	return storeMsg, nil
}
