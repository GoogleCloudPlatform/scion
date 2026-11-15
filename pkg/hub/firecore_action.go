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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// FireAuthorized carries action-specific state resolved during Authorize so
// Fire never re-resolves authority or target lookup.
type FireAuthorized any

// FireRefusal represents an authorization or standing refusal of a fire. The
// generic Reason is persisted on the fire record; detailed causes are logged
// by Authorize.
type FireRefusal struct {
	Reason string
}

// FireAction defines one deferred action type handled by the fire core. Name
// avoids colliding with package hub's authz Action vocabulary type.
type FireAction interface {
	Type() string
	// Validate parses and bounds payload at authoring time and resolves any
	// target within projectID, returning the normalized payload to store.
	Validate(ctx context.Context, caller Identity, projectID string, payload string) (normalized string, err error)
	// Permission returns the base project permission checked by
	// resolveRecordedAuthority (agent.message, agent.create, agent.attach).
	Permission() string
	// Authorize checks frozen ceiling, live authorization for identity, and
	// standing. A denial returns a non-nil *FireRefusal (not an error).
	Authorize(ctx context.Context, req hubfire.FireRequest, auth FireAuthority, identity Identity) (FireAuthorized, *FireRefusal, error)
	// Fire executes the authorized action.
	Fire(ctx context.Context, req hubfire.FireRequest, a FireAuthorized) (hubfire.FireOutcome, error)
}

// FirePayloadValidator is an optional capability implemented by FireActions
// that validate stored JSON payloads at fire time before recorded-authority
// resolution.
type FirePayloadValidator interface {
	ValidatePayload(req hubfire.FireRequest) error
}

// fireServerAction is implemented by server-owned FireActions so standalone
// Scheduler unit tests without Scheduler.srv set still resolve authority and
// shutdown tracking against the action's Server.
type fireServerAction interface {
	server() *Server
}

type actionRegistry struct {
	mu      sync.RWMutex
	actions map[string]FireAction
}

func (r *actionRegistry) Register(a FireAction) {
	if a == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.actions == nil {
		r.actions = make(map[string]FireAction)
	}
	r.actions[a.Type()] = a
}

func (r *actionRegistry) Lookup(actionType string) (FireAction, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[actionType]
	return a, ok
}

func (r *actionRegistry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.actions))
	for t := range r.actions {
		out = append(out, t)
	}
	return out
}

// RegisterAction registers a FireAction on the server's fire-core registry.
func (s *Server) RegisterAction(a FireAction) {
	s.actions.Register(a)
}

// LookupAction returns the FireAction registered for actionType. When s has a
// scheduler with an explicit action registration (including test overrides via
// RegisterEventHandler), that registration takes precedence over the server
// defaults.
func (s *Server) LookupAction(actionType string) (FireAction, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	sched := s.scheduler
	s.mu.RUnlock()
	if sched != nil {
		if a, ok := sched.LookupAction(actionType); ok {
			return a, true
		}
	}
	return s.actions.Lookup(actionType)
}

func unknownActionTypeError(actionType string) string {
	return "unknown action type: " + actionType
}

func fireAuthorityRefusalText(actionType string, err error) string {
	if errors.Is(err, errScheduledAuthorityUnrecorded) {
		return errScheduledAuthorityUnrecorded.Error()
	}
	if actionType == "message" {
		return errScheduledMessageRefused.Error()
	}
	return actionType + " refused"
}

func (s *Server) logFireAuthorityRefusal(req hubfire.FireRequest, err error) {
	var scheduleID string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
	}
	slog.Warn("Scheduler: scheduled message authority denied at fire time",
		"eventID", req.Authority.FireID,
		"scheduleID", scheduleID,
		"definitionKind", req.Authority.DefinitionKind,
		"definitionID", req.Authority.DefinitionID,
		"projectID", req.ProjectID,
		"authorization_revision", req.Authority.Attribution.AuthorizationRevision,
		"error", err)
}

// fireAction is the single entry point that turns a FireRequest into a
// FireOutcome for both scheduler adapters and the trigger dispatcher.
func (s *Server) fireAction(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	action, ok := s.LookupAction(req.ActionType)
	if !ok {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  unknownActionTypeError(req.ActionType),
		}
	}
	return s.fireActionWith(ctx, req, action)
}

func (s *Server) fireActionWith(ctx context.Context, req hubfire.FireRequest, action FireAction) hubfire.FireOutcome {
	if req.Executor.Kind != "" || req.Executor.ID != "" {
		ctx = ContextWithExecutor(ctx, req.Executor)
	}
	if legacy, ok := action.(*legacyAction); ok {
		return legacy.run(ctx, req)
	}
	if pv, ok := action.(FirePayloadValidator); ok {
		if err := pv.ValidatePayload(req); err != nil {
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  err.Error(),
			}
		}
	}
	if s == nil {
		if sa, ok := action.(fireServerAction); ok {
			s = sa.server()
		}
	}
	if s == nil {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "fire action: server not configured",
		}
	}
	if req.Tracked == nil {
		req.Tracked = newScheduledFireTracker(s, req.Authority.FireID, nil)
	} else if st, ok := req.Tracked.(*scheduledFireTracker); ok && st.srv == nil {
		st.srv = s
	}
	auth, identity, err := s.resolveRecordedAuthority(ctx, req.Authority, req.ProjectID, action.Permission())
	if err != nil {
		s.logFireAuthorityRefusal(req, err)
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusRefused,
			Error:  fireAuthorityRefusalText(req.ActionType, err),
		}
	}
	authorized, refusal, err := action.Authorize(ctx, req, auth, identity)
	if err != nil {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  err.Error(),
		}
	}
	if refusal != nil {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusRefused,
			Error:  refusal.Reason,
		}
	}
	out, err := action.Fire(ctx, req, authorized)
	if err != nil {
		if reason, ok := asScheduledSkip(err); ok {
			return hubfire.FireOutcome{
				Status:      hubfire.FireStatusSkipped,
				Error:       reason,
				AfterRecord: out.AfterRecord,
				Detached:    out.Detached,
			}
		}
		return hubfire.FireOutcome{
			Status:      hubfire.FireStatusFailed,
			Error:       err.Error(),
			AfterRecord: out.AfterRecord,
			Detached:    out.Detached,
		}
	}
	if out.Status == "" {
		out.Status = hubfire.FireStatusFired
	}
	return out
}
