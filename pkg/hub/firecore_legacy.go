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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// EventHandler processes a scheduled event of a specific type on the legacy
// pre-FireAction signature.
// Removed in PR 2 (ptone/scion#8029).
type EventHandler func(ctx context.Context, evt store.ScheduledEvent) error

// RegisterEventHandler registers a legacy EventHandler on Server.actions when
// s is wired to a Server (or on s's standalone test map when s.srv is nil) so
// existing scheduler test suites remain unchanged in Phase 1a.
// Removed in PR 2 (ptone/scion#8029).
func (s *Scheduler) RegisterEventHandler(eventType string, handler EventHandler) {
	if s.srv != nil {
		s.srv.RegisterAction(newLegacyAction(eventType, handler))
		return
	}
	if s.legacyHandlers == nil {
		s.legacyHandlers = make(map[string]EventHandler)
	}
	s.legacyHandlers[eventType] = handler
}

type legacyScheduledEventKey struct{}

// Removed in PR 2 (ptone/scion#8029).
func withLegacyScheduledEvent(ctx context.Context, evt store.ScheduledEvent) context.Context {
	return context.WithValue(ctx, legacyScheduledEventKey{}, evt)
}

func legacyScheduledEventFromContext(ctx context.Context) (store.ScheduledEvent, bool) {
	evt, ok := ctx.Value(legacyScheduledEventKey{}).(store.ScheduledEvent)
	return evt, ok
}

// scheduledRecordedAuthority projects evt's recorded revision authority into
// hubfire.RecordedAuthority.
func scheduledRecordedAuthority(evt store.ScheduledEvent) hubfire.RecordedAuthority {
	defKind := hubfire.DefinitionKindScheduledEvent
	defID := evt.ID
	if evt.ScheduleID != "" {
		defKind = hubfire.DefinitionKindSchedule
		defID = evt.ScheduleID
	}
	return hubfire.RecordedAuthority{
		Attribution:    evt.InitiatorAttribution,
		Ceiling:        evt.AuthorityCeiling,
		DefinitionKind: defKind,
		DefinitionID:   defID,
		FireID:         evt.ID,
	}
}

// Removed in PR 2 (ptone/scion#8029).
func scheduledStandingAuthority(evt store.ScheduledEvent) FireAuthority {
	return FireAuthority{
		ProjectID:      evt.ProjectID,
		Recorded:       scheduledRecordedAuthority(evt),
		historyCreator: evt.CreatedBy,
	}
}

// legacyAction adapts an EventHandler to FireAction until dispatch_agent,
// exec, and scheduler test overrides migrate in PR 2.
// Removed in PR 2 (ptone/scion#8029).
type legacyAction struct {
	actionType string
	handler    EventHandler
}

// Removed in PR 2 (ptone/scion#8029).
func newLegacyAction(actionType string, handler EventHandler) FireAction {
	return &legacyAction{actionType: actionType, handler: handler}
}

func (a *legacyAction) Type() string       { return a.actionType }
func (a *legacyAction) Permission() string { return scheduledEventPermissions[a.actionType] }

func (a *legacyAction) Validate(_ context.Context, _ Identity, _ string, payload string) (string, error) {
	return payload, nil
}

func (a *legacyAction) Authorize(_ context.Context, _ hubfire.FireRequest, _ FireAuthority, _ Identity) (FireAuthorized, *FireRefusal, error) {
	return nil, nil, fmt.Errorf("legacyAction %q executes through run", a.actionType)
}

func (a *legacyAction) Fire(_ context.Context, _ hubfire.FireRequest, _ FireAuthorized) (hubfire.FireOutcome, error) {
	return hubfire.FireOutcome{}, fmt.Errorf("legacyAction %q executes through run", a.actionType)
}

func (a *legacyAction) run(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if a.handler == nil {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "legacy action handler is nil",
		}
	}
	evt, ok := legacyScheduledEventFromContext(ctx)
	if !ok {
		var scheduleID string
		if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
			scheduleID = req.Authority.DefinitionID
		}
		evt = store.ScheduledEvent{
			ID:                   req.Authority.FireID,
			ProjectID:            req.ProjectID,
			EventType:            req.ActionType,
			FireAt:               req.FireAt,
			Payload:              req.Payload,
			ScheduleID:           scheduleID,
			CreatedBy:            fireHistoryCreatorFromContext(ctx),
			InitiatorAttribution: req.Authority.Attribution,
			AuthorityCeiling:     req.Authority.Ceiling,
		}
	}
	localAfter := &scheduledAfterRecord{}
	handlerCtx := context.WithValue(ctx, scheduledAfterRecordKey{}, localAfter)
	err := a.handler(handlerCtx, evt)

	var afterFn func(context.Context)
	localAfter.mu.Lock()
	hasAfter := len(localAfter.hooks) > 0
	localAfter.mu.Unlock()
	if hasAfter {
		afterFn = func(context.Context) {
			localAfter.run()
		}
	}

	if err == nil {
		return hubfire.FireOutcome{
			Status:      hubfire.FireStatusFired,
			AfterRecord: afterFn,
		}
	}
	if recErr, recorded := asOutcomeRecorded(err); recorded {
		if recErr == nil {
			return hubfire.FireOutcome{
				Status:      hubfire.FireStatusFired,
				AfterRecord: afterFn,
				Detached:    true,
			}
		}
		if reason, skipped := asScheduledSkip(recErr); skipped {
			return hubfire.FireOutcome{
				Status:      hubfire.FireStatusSkipped,
				Error:       reason,
				AfterRecord: afterFn,
				Detached:    true,
			}
		}
		return hubfire.FireOutcome{
			Status:      hubfire.FireStatusFailed,
			Error:       recErr.Error(),
			AfterRecord: afterFn,
			Detached:    true,
		}
	}
	if reason, skipped := asScheduledSkip(err); skipped {
		return hubfire.FireOutcome{
			Status:      hubfire.FireStatusSkipped,
			Error:       reason,
			AfterRecord: afterFn,
		}
	}
	return hubfire.FireOutcome{
		Status:      hubfire.FireStatusFailed,
		Error:       err.Error(),
		AfterRecord: afterFn,
	}
}

func (s *Server) registerFireActions() {
	s.RegisterAction(newLegacyAction("message", s.messageEventHandler()))
	s.RegisterAction(newLegacyAction("dispatch_agent", s.dispatchAgentEventHandler()))
	s.RegisterAction(newLegacyAction(scheduledEventTypeExec, s.execEventHandler()))
}

func scheduledFireRequest(srv *Server, evt store.ScheduledEvent, exec ExecutorContext, run *wakeFireRun) hubfire.FireRequest {
	return hubfire.FireRequest{
		ProjectID:  evt.ProjectID,
		Authority:  scheduledRecordedAuthority(evt),
		ActionType: evt.EventType,
		Payload:    evt.Payload,
		Executor:   exec,
		FireAt:     evt.FireAt,
		Tracked:    newScheduledFireTracker(srv, evt.ID, run),
	}
}

func scheduledFireError(eventType string, out hubfire.FireOutcome) error {
	if out.Detached {
		switch out.Status {
		case hubfire.FireStatusSkipped:
			return outcomeRecorded(skipScheduledFire(out.Error))
		case hubfire.FireStatusFailed, hubfire.FireStatusRefused:
			return outcomeRecorded(errors.New(out.Error))
		default:
			return outcomeRecorded(nil)
		}
	}
	switch out.Status {
	case hubfire.FireStatusFired:
		return nil
	case hubfire.FireStatusSkipped:
		return skipScheduledFire(out.Error)
	default:
		if out.Error == unknownActionTypeError(eventType) {
			return fmt.Errorf("unknown event type: %s", eventType)
		}
		return errors.New(out.Error)
	}
}
