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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func firstTriggerAction(tr store.Trigger) store.TriggerAction {
	actions, err := triggerActions(tr)
	if err != nil || len(actions) == 0 {
		return store.TriggerAction{Type: tr.ActionType, Payload: tr.ActionPayload}
	}
	return actions[0]
}

// fireScheduledActionForTest runs a scheduled event through the native fire
// core (Server.fireActionWithRawError) using the registered FireAction (or the
// default built-in action for actionType on bare test servers that did not call
// registerFireActions).
func fireScheduledActionForTest(ctx context.Context, srv *Server, actionType string, evt store.ScheduledEvent) error {
	if evt.EventType == "" {
		evt.EventType = actionType
	}
	exec, _ := ExecutorContextFromContext(ctx)
	var st store.Store
	if srv != nil {
		st = srv.store
	}
	req := scheduledFireRequestWithSchedule(ctx, srv, st, evt, nil, exec)
	req.ActionType = actionType
	if actionType == "message" && req.Event == nil {
		req.Event = scheduledMessageFireEvent(ctx, st, evt, nil)
	}
	action, ok := srv.LookupAction(actionType)
	if !ok {
		switch actionType {
		case "message":
			action = &messageAction{srv: srv}
		case "dispatch_agent":
			action = &dispatchAgentAction{srv: srv}
		case scheduledEventTypeExec:
			action = &execAction{srv: srv}
		}
	}
	out, rawErr := srv.fireActionWithRawError(withFireHistoryCreator(ctx, evt.CreatedBy), req, action)
	if out.AfterRecord != nil {
		after := out.AfterRecord
		afterScheduledRecord(ctx, func() {
			after(context.WithoutCancel(ctx))
		})
	}
	if rawErr != nil && !out.Detached {
		return rawErr
	}
	return scheduledFireError(actionType, out)
}

// testFireAction is a lightweight FireAction test double for scheduler and
// fire-core unit tests that need to intercept or customize action execution.
type testFireAction struct {
	actionType string
	permission string
	detachesFn func(req hubfire.FireRequest) bool
	fireFn     func(ctx context.Context, req hubfire.FireRequest) (hubfire.FireOutcome, error)
}

func newTestFireAction(actionType string, fireFn func(ctx context.Context, req hubfire.FireRequest) error) *testFireAction {
	perm := scheduledMessagePermission
	switch actionType {
	case "dispatch_agent":
		perm = dispatchAgentPermission
	case scheduledEventTypeExec:
		perm = scheduledExecPermission
	}
	return &testFireAction{
		actionType: actionType,
		permission: perm,
		fireFn: func(ctx context.Context, req hubfire.FireRequest) (hubfire.FireOutcome, error) {
			if fireFn == nil {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
			}
			if err := fireFn(ctx, req); err != nil {
				if recErr, recorded := asOutcomeRecorded(err); recorded {
					if recErr == nil {
						return hubfire.FireOutcome{Status: hubfire.FireStatusFired, Detached: true}, nil
					}
					if reason, skipped := asScheduledSkip(recErr); skipped {
						return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: reason, Detached: true}, nil
					}
					return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: recErr.Error(), Detached: true}, nil
				}
				return hubfire.FireOutcome{}, err
			}
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
		},
	}
}

func (a *testFireAction) Type() string       { return a.actionType }
func (a *testFireAction) Permission() string { return a.permission }

func (a *testFireAction) Validate(_ context.Context, _ Identity, _ string, payload string) (string, error) {
	return payload, nil
}

func (a *testFireAction) Authorize(_ context.Context, _ hubfire.FireRequest, _ FireAuthority, _ Identity) (FireAuthorized, *FireRefusal, error) {
	return nil, nil, nil
}

func (a *testFireAction) Fire(ctx context.Context, req hubfire.FireRequest, _ FireAuthorized) (hubfire.FireOutcome, error) {
	if a.fireFn == nil {
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
	}
	return a.fireFn(ctx, req)
}

func (a *testFireAction) Detaches(req hubfire.FireRequest) bool {
	if a.detachesFn != nil {
		return a.detachesFn(req)
	}
	switch a.actionType {
	case "message":
		return scheduledPayloadWakes(req.Payload)
	case scheduledEventTypeExec:
		return req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule
	default:
		return false
	}
}

// testDispatchCreator holds the resolved authority and identity returned by
// authorizeDispatchAgentForTest.
type testDispatchCreator struct {
	Authority FireAuthority
	Identity  Identity
	Name      string
}

func resolveScheduledAuthorityForTest(ctx context.Context, srv *Server, evt store.ScheduledEvent) (FireAuthority, Identity, error) {
	perm := ""
	switch evt.EventType {
	case "message":
		perm = scheduledMessagePermission
	case "dispatch_agent":
		perm = dispatchAgentPermission
	case scheduledEventTypeExec:
		perm = scheduledExecPermission
	}
	return srv.resolveRecordedAuthority(withFireHistoryCreator(ctx, evt.CreatedBy), scheduledRecordedAuthority(evt), evt.ProjectID, perm)
}

func authorizeDispatchAgentForTest(ctx context.Context, srv *Server, evt store.ScheduledEvent) (testDispatchCreator, error) {
	auth, identity, err := srv.resolveRecordedAuthority(withFireHistoryCreator(ctx, evt.CreatedBy), scheduledRecordedAuthority(evt), evt.ProjectID, dispatchAgentPermission)
	if err != nil {
		return testDispatchCreator{}, err
	}
	var p DispatchAgentEventPayload
	if evt.Payload != "" {
		_ = json.Unmarshal([]byte(evt.Payload), &p)
	}
	rec := scheduledRecordedAuthority(evt)
	if _, _, err := srv.authorizeDispatchAgentFire(ctx, evt.ProjectID, evt.ID, rec.DefinitionKind, auth, identity, AgentRole(p.AgentRole), p.NoAuth); err != nil {
		return testDispatchCreator{}, err
	}
	return testDispatchCreator{
		Authority: auth,
		Identity:  identity,
		Name:      fireCreatorName(auth),
	}, nil
}

func fireStandingForTest(ctx context.Context, srv *Server, evt store.ScheduledEvent, target *store.Agent) error {
	return srv.fireStanding(ctx, FireAuthority{
		ProjectID:      evt.ProjectID,
		Recorded:       scheduledRecordedAuthority(evt),
		historyCreator: evt.CreatedBy,
	}, target)
}
