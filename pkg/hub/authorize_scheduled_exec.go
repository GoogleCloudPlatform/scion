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
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return nil, false
	}

	if allowed, retryAfter := s.scheduledExecLimiter().Allow(identity.Type() + ":" + identity.ID()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
			"too many exec schedule changes; retry later", nil)
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

	if err := s.checkScheduledExecQuota(ctx, projectID, target.ID); err != nil {
		if errors.Is(err, errScheduledExecQuota) {
			writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(), nil)
			return nil, false
		}
		slog.ErrorContext(ctx, "exec schedule authoring: quota check failed", "project_id", projectID, "error", err)
		InternalError(w)
		return nil, false
	}

	p.AgentID = target.ID
	p.AgentName = target.Slug
	return target, true
}

// errScheduledExecQuota is the error of an authoring over the exec limits.
var errScheduledExecQuota = errors.New("exec schedule quota exceeded")

// checkScheduledExecQuota counts the active exec work of projectID (pending
// one-shot exec events; recurring exec schedules join in Phase 2,
// ptone/scion#7884) and refuses another when the project, or targetID,
// is at its limit.
func (s *Server) checkScheduledExecQuota(ctx context.Context, projectID, targetID string) error {
	result, err := s.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		ProjectID: projectID,
		EventType: scheduledEventTypeExec,
		Status:    store.ScheduledEventPending,
	}, store.ListOptions{Limit: scheduledExecMaxPerProject + 1})
	if err != nil {
		return err
	}
	if len(result.Items) >= scheduledExecMaxPerProject {
		return fmt.Errorf("%w: at most %d active exec schedules per project", errScheduledExecQuota, scheduledExecMaxPerProject)
	}
	perTarget := 0
	for _, evt := range result.Items {
		p, err := decodeScheduledExecPayload(evt.Payload)
		if err != nil || p.AgentID == targetID {
			// A stored row that no longer decodes counts against the
			// target too: fail closed.
			perTarget++
		}
	}
	if perTarget >= scheduledExecMaxPerTarget {
		return fmt.Errorf("%w: at most %d active exec schedules per agent", errScheduledExecQuota, scheduledExecMaxPerTarget)
	}
	return nil
}

// authorizeScheduledExecFire authorizes an exec event at fire time,
// immediately before dispatch, under the event's authorization revision
// (design "Fire-time check"):
//
//  1. resolveScheduledAuthority: the revision principal's liveness, account
//     status, project admission and (UAT) token state;
//  2. the target by the stored agentId only (never the name): it must exist,
//     not be deleted and be in the event's project;
//  3. the revision's frozen ceiling must allow agent.attach (for user and
//     agent principals alike: an agent identity is rebuilt from its current
//     role, so only the frozen ceiling keeps a revision authored without
//     attach authority from running after that role widens);
//  4. the live rule: authorizeAgentTargetAction(identity, target,
//     ActionAttach), with selfTarget false;
//  5. standing: the target is not held and the authoring agent is in good
//     standing (scheduledFireStanding).
//
// Any denial returns an error naming the cause, for the audit log and slog
// only; the event records the generic execReasonRefused.
func (s *Server) authorizeScheduledExecFire(ctx context.Context, evt store.ScheduledEvent, p *scheduledExecPayload) (ScheduledAuthority, Identity, *store.Agent, error) {
	auth, identity, err := s.resolveScheduledAuthority(ctx, evt)
	if err != nil {
		return auth, nil, nil, err
	}
	target, err := s.store.GetAgent(ctx, p.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth, identity, nil, errScheduledExecTargetGone
		}
		return auth, identity, nil, fmt.Errorf("exec target lookup: %w", err)
	}
	if target == nil || !target.DeletedAt.IsZero() || target.ProjectID != evt.ProjectID {
		return auth, identity, nil, errScheduledExecTargetGone
	}
	if !EffectCeilingAllows(auth.Ceiling, scheduledExecPermission, false) {
		return auth, identity, target, fmt.Errorf("%w: revision ceiling does not allow %s", errScheduledAuthorityDenied, scheduledExecPermission)
	}
	if denial := s.authorizeAgentTargetAction(ctx, identity, target, ActionAttach); denial != nil {
		return auth, identity, target, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, denial.reason)
	}
	if err := s.scheduledFireStanding(ctx, evt, target); err != nil {
		return auth, identity, target, fmt.Errorf("%w: standing: %v", errScheduledAuthorityDenied, err)
	}
	return auth, identity, target, nil
}
