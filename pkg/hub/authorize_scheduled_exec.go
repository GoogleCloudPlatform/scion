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
	"math"
	"net/http"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Authority of exec scheduled events (ptone/scion#7883, design D1 and
// "Permission wiring"). Authoring and every fire require agent.attach on the
// target agent through authorizeAgentTargetAction, the check live /exec
// uses, so a schedule can never do more than a live /exec call by the same
// principal at that moment.

// Authoring limits (design D5). Constants in v1; settings come later (F6).
const (
	// scheduledExecMaxPerTarget bounds the active exec schedules (pending
	// one-shot events) that target one agent.
	scheduledExecMaxPerTarget = 20
	// scheduledExecMaxPerProject bounds the active exec schedules of one
	// project.
	scheduledExecMaxPerProject = 200
	// scheduledExecAuthoringRate and scheduledExecAuthoringBurst bound exec
	// authoring per principal.
	scheduledExecAuthoringRate  = 1
	scheduledExecAuthoringBurst = 10
)

// errScheduledExecTargetGone denies a fire whose target is missing, deleted
// or in another project.
var errScheduledExecTargetGone = errors.New("exec target agent not found")

// scheduledExecLimiter returns the hub's exec authoring rate limiter,
// creating it on first use.
func (s *Server) scheduledExecLimiter() *keysRateLimiter {
	s.scheduledExecLimiterOnce.Do(func() {
		s.scheduledExecAuthoringLimiter = newKeysRateLimiter(scheduledExecAuthoringRate, scheduledExecAuthoringBurst)
	})
	return s.scheduledExecAuthoringLimiter
}

// authorizeScheduledExecAuthoring decides an exec event's authoring for the
// request's identity and returns the resolved target, with p.AgentID and
// p.AgentName set from it. p must already be validated. On a refusal it
// writes the response and returns ok=false, before anything is stored:
//   - 401 without an identity;
//   - 429 rate_limited when the principal authors exec events too fast;
//   - 404 for a target that does not exist in projectID, or that the caller
//     can neither read nor attach to (no existence oracle);
//   - 403 when the caller may read the target but not attach to it
//     (agent.attach through authorizeAgentTargetAction, the live /exec
//     rule: roles and relationships, UAT agent:attach scope and boundary,
//     the delegation ceiling of every live ancestor, delegated agent
//     credentials refused);
//   - 422 unsupported_capability for a managed runtime;
//   - 429 quota_exceeded over the per-target or per-project limit.
//
// The target need not be running or connected: those are fire-time
// conditions that skip a run.
func (s *Server) authorizeScheduledExecAuthoring(w http.ResponseWriter, r *http.Request, projectID string, p *scheduledExecPayload) (*store.Agent, bool) {
	return s.authorizeScheduledExecAuthoringOpts(w, r, projectID, p, "", true)
}

// authorizeScheduledExecAuthoringOpts runs the exec authoring decision with an
// optional excludeScheduleID (when updating or resuming an existing schedule)
// and checkQuota toggle (false when editing a schedule that remains paused).
func (s *Server) authorizeScheduledExecAuthoringOpts(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	p *scheduledExecPayload,
	excludeScheduleID string,
	checkQuota bool,
) (*store.Agent, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}

	if allowed, retryAfter := s.scheduledExecLimiter().Allow(identity.Type() + ":" + identity.ID()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
			"too many exec changes; retry later", nil)
		return nil, false
	}

	var target *store.Agent
	var err error
	switch {
	case p.AgentID != "":
		target, err = s.store.GetAgent(ctx, p.AgentID)
	case p.AgentName != "":
		target, err = s.store.GetAgentBySlug(ctx, projectID, p.AgentName)
	default:
		ValidationError(w, "agentId or agentName is required for exec events", nil)
		return nil, false
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "exec schedule authoring: target lookup failed", "project_id", projectID, "error", err)
		InternalError(w)
		return nil, false
	}
	if err != nil || target == nil || target.ProjectID != projectID || !target.DeletedAt.IsZero() ||
		(p.AgentID != "" && p.AgentName != "" && p.AgentName != target.Slug) {
		NotFound(w, "Agent")
		return nil, false
	}

	if denial := s.authorizeAgentTargetAction(ctx, identity, target, ActionAttach); denial != nil {
		if denial.status == http.StatusUnauthorized {
			Unauthorized(w)
			return nil, false
		}
		if !s.scheduledTargetReadable(ctx, identity, target) {
			logAuthzDenial(r, identity, agentResource(target), ActionAttach, "exec schedule authoring: "+denial.reason)
			NotFound(w, "Agent")
			return nil, false
		}
		writeAgentTargetDenial(w, r, identity, target, ActionAttach, denial)
		return nil, false
	}

	if isManagedAgentRuntime(target.Runtime) {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUnsupportedCapability,
			"exec schedules are not supported on managed runtimes", nil)
		return nil, false
	}

	if checkQuota {
		if err := s.checkScheduledExecQuota(ctx, projectID, target.ID, excludeScheduleID); err != nil {
			if errors.Is(err, errScheduledExecQuota) {
				writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(), nil)
				return nil, false
			}
			slog.ErrorContext(ctx, "exec schedule authoring: quota check failed", "project_id", projectID, "error", err)
			InternalError(w)
			return nil, false
		}
	}

	p.AgentID = target.ID
	p.AgentName = target.Slug
	return target, true
}

// authorizeStoredExecTargetForUpdate verifies that the caller holds live
// agent.attach on an existing exec schedule's stored target before any PATCH
// can modify its dispatch or re-target it to another agent (Phase 2 R1).
// When the stored target has been deleted (for example recovering a schedule
// paused with target_deleted), prevLive is false and ok is true so the caller
// may re-target only by supplying an explicit replacement command/payload.
func (s *Server) authorizeStoredExecTargetForUpdate(
	w http.ResponseWriter,
	r *http.Request,
	projectID string,
	prevTargetID string,
) (prevLive bool, ok bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false, false
	}
	if prevTargetID == "" {
		return false, true
	}
	prevTarget, err := s.store.GetAgent(ctx, prevTargetID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "exec schedule authoring: stored target lookup failed", "project_id", projectID, "error", err)
		InternalError(w)
		return false, false
	}
	if err != nil || prevTarget == nil || prevTarget.ProjectID != projectID || !prevTarget.DeletedAt.IsZero() {
		return false, true
	}
	if denial := s.authorizeAgentTargetAction(ctx, identity, prevTarget, ActionAttach); denial != nil {
		if denial.status == http.StatusUnauthorized {
			Unauthorized(w)
			return false, false
		}
		if !s.scheduledTargetReadable(ctx, identity, prevTarget) {
			logAuthzDenial(r, identity, agentResource(prevTarget), ActionAttach, "exec schedule authoring: "+denial.reason)
			NotFound(w, "Agent")
			return false, false
		}
		writeAgentTargetDenial(w, r, identity, prevTarget, ActionAttach, denial)
		return false, false
	}
	return true, true
}

// errScheduledExecQuota is the error of an authoring over the exec limits.
var errScheduledExecQuota = errors.New("exec schedule quota exceeded")

// checkScheduledExecQuota is the early quota check at authoring: it counts
// the active exec work of projectID (pending one-shot exec events + active
// recurring exec schedules, ptone/scion#7884) and refuses another when the
// project, or targetID, is at its limit. excludeScheduleID omits the schedule
// being updated or resumed so editing an already-active schedule does not
// double-count itself. The binding check is repeated atomically with the
// insert/update under LockScheduledEventQuota.
func (s *Server) checkScheduledExecQuota(ctx context.Context, projectID, targetID, excludeScheduleID string) error {
	events, err := s.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		ProjectID: projectID,
		EventType: scheduledEventTypeExec,
		Status:    store.ScheduledEventPending,
	}, store.ListOptions{Limit: scheduledExecMaxPerProject + 1})
	if err != nil {
		return err
	}
	schedules, err := s.store.ListSchedules(ctx, store.ScheduleFilter{
		ProjectID: projectID,
		EventType: scheduledEventTypeExec,
		Status:    store.ScheduleStatusActive,
	}, store.ListOptions{Limit: scheduledExecMaxPerProject + 1})
	if err != nil {
		return err
	}
	return scheduledExecQuotaCheck(store.ScheduledExecQuotaSnapshot{
		PendingEvents:   events.Items,
		ActiveSchedules: schedules.Items,
	}, targetID, excludeScheduleID)
}

// scheduledExecQuotaCheck refuses another active exec item on targetID given
// the project's pending exec events and active recurring exec schedules.
func scheduledExecQuotaCheck(snap store.ScheduledExecQuotaSnapshot, targetID, excludeScheduleID string) error {
	total := len(snap.PendingEvents)
	perTarget := 0
	for _, evt := range snap.PendingEvents {
		p, err := decodeScheduledExecPayload(evt.Payload)
		if err != nil || p.AgentID == targetID {
			// A stored row that no longer decodes counts against the
			// target too: fail closed.
			perTarget++
		}
	}
	for _, sc := range snap.ActiveSchedules {
		if excludeScheduleID != "" && sc.ID == excludeScheduleID {
			continue
		}
		total++
		p, err := decodeScheduledExecPayload(sc.Payload)
		if err != nil || p.AgentID == targetID {
			perTarget++
		}
	}
	if total >= scheduledExecMaxPerProject {
		return fmt.Errorf("%w: at most %d active exec schedules per project", errScheduledExecQuota, scheduledExecMaxPerProject)
	}
	if perTarget >= scheduledExecMaxPerTarget {
		return fmt.Errorf("%w: at most %d active exec schedules per agent", errScheduledExecQuota, scheduledExecMaxPerTarget)
	}
	return nil
}
