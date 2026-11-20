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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// FireAuthorized carries action-specific state resolved during Authorize so
// Fire never re-resolves authority or target lookup.
type FireAuthorized any

// FireRefusal represents an authorization or standing refusal of a fire. The
// generic Reason is persisted on the fire record; detailed causes are logged
// by Authorize. Cause, when non-nil, carries the wrapped internal error for
// direct caller inspection without widening FireOutcome.Error.
type FireRefusal struct {
	Reason string
	Cause  error
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
	// Detaches reports, before the fire, whether the schedule adapter must
	// detach: recurring -> firing status + AdvanceScheduleNextRun + tracked
	// run; one-shot -> oneShotFireScope + firing claim. It says nothing about
	// who persists the outcome, which is reported independently by
	// FireOutcome.Detached.
	Detaches(req hubfire.FireRequest) bool
}

// FirePayloadValidator is an optional capability implemented by FireActions
// that validate stored JSON payloads at fire time before recorded-authority
// resolution.
type FirePayloadValidator interface {
	ValidatePayload(req hubfire.FireRequest) error
}

// FirePreauthorizer is an optional capability implemented by FireActions that
// evaluate definition-kind guards, feature gates, or staleness windows after
// ValidatePayload and before recorded-authority resolution, returning either a
// *FireRefusal or a skip sentinel without evaluating authority. Retained after
// legacyAction removal because execAction relies on pre-authority experiment,
// staleness-window, and trigger-kind checks to preserve bit-identical skip/refusal
// semantics and audit ordering (2a review O6).
type FirePreauthorizer interface {
	Preauthorize(ctx context.Context, req hubfire.FireRequest) (*FireRefusal, error)
}

// FireAuthorityRefusalHandler is an optional capability implemented by
// FireActions that emit action-specific audit records and return their own
// generic refusal text when resolveRecordedAuthority denies a fire. Retained
// after legacyAction removal because execAction emits schedule.exec.refused
// audit records and records "exec refused" on authority denial (2a review O6).
type FireAuthorityRefusalHandler interface {
	OnAuthorityRefused(ctx context.Context, req hubfire.FireRequest, err error) string
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

// LookupAction returns the FireAction registered on s for actionType.
func (s *Server) LookupAction(actionType string) (FireAction, bool) {
	if s == nil {
		return nil, false
	}
	return s.actions.Lookup(actionType)
}

func unknownActionTypeError(actionType string) string {
	return "unknown action type: " + actionType
}

func fireAuthorityRefusalText(req hubfire.FireRequest, err error) string {
	if errors.Is(err, errScheduledAuthorityUnrecorded) {
		return errScheduledAuthorityUnrecorded.Error()
	}
	if req.ActionType == "message" {
		return errScheduledMessageRefused.Error()
	}
	if req.ActionType == "dispatch_agent" &&
		(req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule ||
			req.Authority.DefinitionKind == hubfire.DefinitionKindScheduledEvent) {
		return err.Error()
	}
	return req.ActionType + " refused"
}

func (s *Server) logFireAuthorityRefusal(req hubfire.FireRequest, permissionID string, err error) {
	var scheduleID string
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = req.Authority.DefinitionID
	}
	msg := "deferred fire authority denied"
	if req.ActionType == "message" &&
		(req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule ||
			req.Authority.DefinitionKind == hubfire.DefinitionKindScheduledEvent) {
		msg = "Scheduler: scheduled message authority denied at fire time"
	}
	slog.Warn(msg,
		"actionType", req.ActionType,
		"permission", permissionID,
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
	out := s.fireActionWith(ctx, req, action)
	if req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger && out.Status == hubfire.FireStatusFailed && out.Error != "" {
		out.Error = sanitizeTriggerFireError(req, out.Status, out.Error)
	}
	return out
}

// sanitizeTriggerFireError maps failed error text persisted to
// trigger_fires.error onto the Stage 1 §5.5 generic set for built-in actions
// so internal details (agent slugs, broker HTTP bodies, raw context errors)
// never leak through trigger history (1b-3 review N4). Refusals and skips
// already carry generic reasons by contract; schedule fires are never passed
// here and retain their existing scheduled_events.error text unchanged.
func sanitizeTriggerFireError(req hubfire.FireRequest, status hubfire.FireStatus, raw string) string {
	if raw == "" {
		return ""
	}
	switch req.ActionType {
	case "message", "dispatch_agent", scheduledEventTypeExec:
	default:
		return raw
	}
	switch raw {
	case errScheduledMessageRefused.Error(),
		"dispatch_agent refused",
		"exec refused",
		req.ActionType + " refused",
		errScheduledAuthorityUnrecorded.Error(),
		"target not running",
		"agent restarted during fire",
		"rule evaluation failed",
		"target wake failed",
		"hub shutting down",
		"message delivery failed",
		"dispatch_agent failed",
		"exec failed",
		"fire failed",
		"invalid message payload",
		"invalid dispatch_agent payload",
		errScheduledExecPayloadInvalid.Error(),
		execReasonDispatchFailed,
		execReasonOverlap,
		execReasonCapacity,
		execReasonStartExpired,
		execReasonMissedWindow:
		return raw
	}
	if strings.HasPrefix(raw, "missing required environment variables (") ||
		strings.Contains(raw, "already exists in project in phase error") {
		return raw
	}

	var mapped string
	switch {
	case raw == "target agent is reincarnating":
		mapped = "agent restarted during fire"
	case raw == errScheduledWakeShuttingDown.Error() || raw == errScheduledWakeShutdown.Error():
		mapped = "hub shutting down"
	case raw == errScheduledWakeNotReady.Error() || strings.HasPrefix(raw, "failed to wake agent "):
		mapped = "target wake failed"
	case strings.Contains(raw, "has no running container") ||
		strings.Contains(raw, "is not running") ||
		strings.Contains(raw, "not in a deliverable phase") ||
		strings.Contains(raw, "cannot receive messages"):
		mapped = "target not running"
	case strings.HasPrefix(raw, "invalid message payload") ||
		raw == "message payload is empty" ||
		strings.HasPrefix(raw, "message payload must include"):
		mapped = "invalid message payload"
	case strings.HasPrefix(raw, "invalid dispatch_agent payload") ||
		strings.HasPrefix(raw, "dispatch_agent payload:"):
		mapped = "invalid dispatch_agent payload"
	default:
		switch req.ActionType {
		case "message":
			mapped = "message delivery failed"
		case "dispatch_agent":
			mapped = "dispatch_agent failed"
		case scheduledEventTypeExec:
			mapped = "exec failed"
		default:
			mapped = "fire failed"
		}
	}

	slog.Warn("trigger fire error mapped to generic text",
		"actionType", req.ActionType,
		"status", status,
		"fireID", req.Authority.FireID,
		"triggerID", req.Authority.DefinitionID,
		"projectID", req.ProjectID,
		"mapped", mapped,
		"raw_error", raw)
	return mapped
}

func (s *Server) fireActionWith(ctx context.Context, req hubfire.FireRequest, action FireAction) hubfire.FireOutcome {
	out, _ := s.fireActionWithRawError(ctx, req, action)
	return out
}

func (s *Server) fireActionWithRawError(ctx context.Context, req hubfire.FireRequest, action FireAction) (hubfire.FireOutcome, error) {
	if req.Executor.Kind != "" || req.Executor.ID != "" {
		ctx = ContextWithExecutor(ctx, req.Executor)
	}
	if pv, ok := action.(FirePayloadValidator); ok {
		if err := pv.ValidatePayload(req); err != nil {
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  err.Error(),
			}, err
		}
	}
	if s == nil {
		err := errors.New("fire action: server not configured")
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  err.Error(),
		}, err
	}
	if pa, ok := action.(FirePreauthorizer); ok {
		refusal, err := pa.Preauthorize(ctx, req)
		if err != nil {
			if reason, skipped := asScheduledSkip(err); skipped {
				return hubfire.FireOutcome{
					Status: hubfire.FireStatusSkipped,
					Error:  reason,
				}, err
			}
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  err.Error(),
			}, err
		}
		if refusal != nil {
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusRefused,
				Error:  refusal.Reason,
			}, scheduledKnownError(refusal.Reason)
		}
	}
	if req.Tracked == nil {
		req.Tracked = newScheduledFireTracker(s, req.Authority.FireID)
	} else if st, ok := req.Tracked.(*scheduledFireTracker); ok && st.srv == nil {
		st.srv = s
	}
	permissionID := action.Permission()
	auth, identity, err := s.resolveRecordedAuthority(ctx, req.Authority, req.ProjectID, permissionID)
	if err != nil {
		if handler, ok := action.(FireAuthorityRefusalHandler); ok {
			refusalText := handler.OnAuthorityRefused(ctx, req, err)
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusRefused,
				Error:  refusalText,
			}, scheduledKnownError(refusalText)
		}
		s.logFireAuthorityRefusal(req, permissionID, err)
		refusalText := fireAuthorityRefusalText(req, err)
		rawErr := scheduledKnownError(refusalText)
		if refusalText == err.Error() {
			rawErr = err
		}
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusRefused,
			Error:  refusalText,
		}, rawErr
	}
	authorized, refusal, err := action.Authorize(ctx, req, auth, identity)
	if err != nil {
		if reason, skipped := asScheduledSkip(err); skipped {
			return hubfire.FireOutcome{
				Status: hubfire.FireStatusSkipped,
				Error:  reason,
			}, err
		}
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  err.Error(),
		}, err
	}
	if refusal != nil {
		rawErr := refusal.Cause
		if rawErr == nil {
			rawErr = scheduledKnownError(refusal.Reason)
		}
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusRefused,
			Error:  refusal.Reason,
		}, rawErr
	}
	out, err := action.Fire(ctx, req, authorized)
	if err != nil {
		if reason, ok := asScheduledSkip(err); ok {
			return hubfire.FireOutcome{
				Status:      hubfire.FireStatusSkipped,
				Error:       reason,
				AfterRecord: out.AfterRecord,
				Detached:    out.Detached,
			}, err
		}
		return hubfire.FireOutcome{
			Status:      hubfire.FireStatusFailed,
			Error:       err.Error(),
			AfterRecord: out.AfterRecord,
			Detached:    out.Detached,
		}, err
	}
	if out.Status == "" {
		out.Status = hubfire.FireStatusFired
	}
	return out, nil
}
