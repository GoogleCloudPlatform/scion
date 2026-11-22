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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// actionResponse carries the inputs to Server.dispatchActionResponse for any
// message-action kind (decisions, secret requests, etc.).
type actionResponse struct {
	Asker        *store.Agent
	Message      *store.Message
	Text         string
	Structured   *messages.StructuredMessage
	PopulateMeta func(map[string]string)
	LogPrefix    string
	Label        string
}

// canAutoWakeActionAsker reports whether an action response to asker should
// wake it in the background rather than marking the response unreachable.
// Mirrors ordinary message auto-wake (ptone/scion#7826): lifecycle.idle_suspend
// is on, the hub suspended the agent (idle or stalled) or an idle-suspend claim
// is in flight, and the agent is not deleted, reincarnating, or held.
func (s *Server) canAutoWakeActionAsker(ctx context.Context, asker *store.Agent) bool {
	if asker == nil || !asker.DeletedAt.IsZero() || reincarnationInFlight(asker) {
		return false
	}
	return s.shouldQueueAutoWake(asker) && !s.agentHeldOrUnknown(ctx, asker.ID)
}

// actionResponseAutoWakeItem queues delivery of a persisted action response
// behind a background auto-wake of the asking agent. It captures only immutable
// values so it never races with the HTTP handler returning.
func (s *Server) actionResponseAutoWakeItem(messageID, agentSlug, text string, sm *messages.StructuredMessage, logPrefix, label string) autoWakeItem {
	prefix := logPrefix
	if prefix == "" {
		prefix = "message_action"
	}
	return autoWakeItem{
		deliver: func(ctx context.Context, woken *store.Agent) {
			var err error
			if dispatcher := s.GetDispatcher(); dispatcher == nil {
				err = errors.New("dispatch not available")
			} else {
				dctx, cancel := context.WithTimeout(withDispatchMessageID(ctx, messageID), chatWakeDeliveryBudget)
				_, err = s.holdOrDispatch(dctx, woken, messageID, nil, func() error {
					return dispatchWithBrokerRetry(dctx, s.deliveryMarker(), s.recordLog(), dispatcher, woken, text, false, sm)
				})
				cancel()
			}
			if err != nil {
				fctx, fcancel := finalizationContext(ctx)
				defer fcancel()
				s.messageLog.Error(prefix+": failed to deliver "+label+" after auto-wake", "agent", woken.Slug, "message_id", messageID, "error", err)
				_ = s.markFailed(fctx, messageID, err.Error())
			}
		},
		fail: func(ctx context.Context, dmErr *AgentDMError) {
			fctx, fcancel := finalizationContext(ctx)
			defer fcancel()
			s.messageLog.Error(prefix+": auto-wake failed for "+label, "agent", agentSlug, "message_id", messageID, "code", dmErr.Code, "error", dmErr.Message)
			_ = s.markFailed(fctx, messageID, fmt.Sprintf("auto-wake failed: %s", dmErr.Message))
		},
	}
}

// dispatchActionResponse delivers a persisted message-action response to
// r.Asker, auto-waking a hub-suspended (or idle-suspending) asker in the
// background just like web chat secondaries and mention fan-out. It returns
// (queuedOrHeld, err), where queuedOrHeld is true when delivery is held for
// the next session or queued behind a background auto-wake.
func (s *Server) dispatchActionResponse(ctx context.Context, r actionResponse) (bool, error) {
	// Delivery to the asker is optional (C2): with no asker, message, or
	// structured payload, dispatch is a no-op and never panics.
	if s == nil || r.Asker == nil || r.Message == nil || r.Structured == nil {
		return false, nil
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return false, nil
	}
	prefix := r.LogPrefix
	if prefix == "" {
		prefix = "message_action"
	}
	// Strip reserved metadata keys first, as every dispatch site does, so no
	// value from another source reaches the agent; hub-owned keys are populated
	// after the strip.
	r.Structured.Metadata = messaging.StripReservedMetadata(r.Structured.Metadata)
	if r.Structured.Metadata == nil {
		r.Structured.Metadata = make(map[string]string, 7)
	}
	if r.PopulateMeta != nil {
		r.PopulateMeta(r.Structured.Metadata)
	}
	canAutoWake := func(target *store.Agent) bool {
		hctx, hcancel := finalizationContext(ctx)
		defer hcancel()
		return s.canAutoWakeActionAsker(hctx, target)
	}
	if canAutoWake(r.Asker) &&
		s.queueAutoWake(r.Asker, s.actionResponseAutoWakeItem(r.Message.ID, r.Asker.Slug, r.Text, r.Structured, prefix, r.Label)) {
		return true, nil
	}
	dctx, cancel := context.WithTimeout(withDispatchMessageID(context.WithoutCancel(ctx), r.Message.ID), 30*time.Second)
	defer cancel()
	held, err := s.holdOrDispatch(dctx, r.Asker, r.Message.ID, r.Message, func() error {
		return dispatchWithBrokerRetry(dctx, s.deliveryMarker(), s.recordLog(), dispatcher, r.Asker, r.Text, false, r.Structured)
	})
	if err != nil {
		var idleErr *errAgentIdleSuspending
		if errors.As(err, &idleErr) {
			target := idleErr.agent
			if target == nil {
				target = r.Asker
			}
			if canAutoWake(target) &&
				s.queueAutoWake(target, s.actionResponseAutoWakeItem(r.Message.ID, target.Slug, r.Text, r.Structured, prefix, r.Label)) {
				return true, nil
			}
		}
		s.messageLog.Error(prefix+": failed to deliver "+r.Label, "agent", r.Asker.Slug, "message_id", r.Message.ID, "error", err)
		fctx, fcancel := finalizationContext(ctx)
		_ = s.markFailed(fctx, r.Message.ID, err.Error())
		fcancel()
		return false, err
	}
	return held, nil
}
