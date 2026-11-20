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
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

func (s *Server) registerFireActions() {
	s.RegisterAction(&messageAction{srv: s})
	s.RegisterAction(&dispatchAgentAction{srv: s})
	s.RegisterAction(&execAction{srv: s})
}

func scheduledDetachesRequest(evt store.ScheduledEvent, actionType string) hubfire.FireRequest {
	return hubfire.FireRequest{
		ProjectID:  evt.ProjectID,
		Authority:  scheduledRecordedAuthority(evt),
		ActionType: actionType,
		Payload:    evt.Payload,
		FireAt:     evt.FireAt,
	}
}

func (s *Server) scheduleFireDetaches(sched store.Schedule, evt store.ScheduledEvent) bool {
	actionType := evt.EventType
	if actionType == "" {
		actionType = sched.EventType
	}
	req := scheduledDetachesRequest(evt, actionType)
	action, ok := s.LookupAction(req.ActionType)
	return ok && action.Detaches(req)
}

func (s *Scheduler) eventFireDetaches(evt store.ScheduledEvent) bool {
	req := scheduledDetachesRequest(evt, evt.EventType)
	if s.srv != nil {
		action, ok := s.srv.LookupAction(req.ActionType)
		return ok && action.Detaches(req)
	}
	if action, ok := s.actions.Lookup(req.ActionType); ok {
		return action.Detaches(req)
	}
	return false
}

func scheduledFireRequest(srv *Server, evt store.ScheduledEvent, exec ExecutorContext) hubfire.FireRequest {
	var st store.Store
	if srv != nil {
		st = srv.store
	}
	return scheduledFireRequestWithSchedule(context.Background(), srv, st, evt, nil, exec)
}

func scheduledFireRequestWithSchedule(
	ctx context.Context,
	srv *Server,
	st store.Store,
	evt store.ScheduledEvent,
	sched *store.Schedule,
	exec ExecutorContext,
) hubfire.FireRequest {
	actionType := evt.EventType
	if actionType == "" && sched != nil {
		actionType = sched.EventType
	}
	req := hubfire.FireRequest{
		ProjectID:  evt.ProjectID,
		Authority:  scheduledRecordedAuthority(evt),
		ActionType: actionType,
		Payload:    evt.Payload,
		Executor:   exec,
		FireAt:     evt.FireAt,
		Tracked:    newScheduledFireTracker(srv, evt.ID),
	}
	if actionType == "message" {
		req.Event = scheduledMessageFireEvent(ctx, st, evt, sched)
	}
	return req
}

func scheduledMessageFireEvent(ctx context.Context, st store.Store, evt store.ScheduledEvent, sched *store.Schedule) *hubfire.FireEvent {
	schedID := evt.ScheduleID
	schedName := ""
	if sched != nil {
		if sched.ID != "" {
			schedID = sched.ID
		}
		schedName = sched.Name
	} else if schedID != "" && st != nil {
		if loaded, err := st.GetSchedule(ctx, schedID); err == nil && loaded != nil {
			schedName = loaded.Name
		}
	}

	fe := &hubfire.FireEvent{
		ID:         evt.ID,
		ProjectID:  evt.ProjectID,
		Type:       string(messaging.EventScheduleFired),
		OccurredAt: evt.FireAt,
		Schedule: &hubfire.FireScheduleRef{
			ID:      schedID,
			Name:    schedName,
			EventID: evt.ID,
			FireAt:  evt.FireAt,
		},
	}
	if schedID != "" {
		fe.SourceKind = "schedule"
		fe.SourceID = schedID
		fe.Subject = "schedule:" + schedID
	} else if evt.ID != "" {
		fe.Subject = "scheduled_event:" + evt.ID
	}
	return fe
}

func scheduledKnownError(msg string) error {
	switch msg {
	case errScheduledAuthorityUnrecorded.Error():
		return errScheduledAuthorityUnrecorded
	case errScheduledExecPayloadInvalid.Error():
		return errScheduledExecPayloadInvalid
	case errScheduledMessageRefused.Error():
		return errScheduledMessageRefused
	case errScheduledWakeNotReady.Error():
		return errScheduledWakeNotReady
	case errScheduledWakeShuttingDown.Error():
		return errScheduledWakeShuttingDown
	case errScheduledWakeShutdown.Error():
		return errScheduledWakeShutdown
	case errScheduledWakeAbandoned.Error():
		return errScheduledWakeAbandoned
	case errHoldPrepareFailed.Error():
		return errHoldPrepareFailed
	default:
		return errors.New(msg)
	}
}

func scheduledFireError(eventType string, out hubfire.FireOutcome) error {
	if out.Detached {
		switch out.Status {
		case hubfire.FireStatusSkipped:
			return outcomeRecorded(skipScheduledFire(out.Error))
		case hubfire.FireStatusFailed, hubfire.FireStatusRefused:
			return outcomeRecorded(scheduledKnownError(out.Error))
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
		return scheduledKnownError(out.Error)
	}
}
