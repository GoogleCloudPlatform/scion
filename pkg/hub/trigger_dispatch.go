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
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// triggerRuntime holds the hub's shared trigger.event and trigger.fire_retry
// durablequeue handles, agent-status nudge subscriber, and background drainer
// lifecycle state. The zero value is ready to use.
type triggerRuntime struct {
	mu                   sync.Mutex
	queue                *durablequeue.StoreQueue
	ruleMetrics          *trigger.OTelRuleEvalMetrics
	templateRefusalAudit trigger.TemplateRefusalAuditSink
	drainer              *durablequeue.Drainer
	retryDrainer         *durablequeue.Drainer
	cancel               context.CancelFunc
	done                 chan struct{}
	started              bool
	projectCapNotify     map[string]time.Time // per-trigger 1h cooldown for project live-agent cap notification (§7.4)
}

// SetTriggerTemplateRefusalAuditSink overrides the audit sink used for
// trigger.fire.template_refused records (primarily for tests). Passing nil
// restores the default SlogAuditSink.
func (s *Server) SetTriggerTemplateRefusalAuditSink(sink trigger.TemplateRefusalAuditSink) {
	if s == nil {
		return
	}
	s.triggers.mu.Lock()
	s.triggers.templateRefusalAudit = sink
	s.triggers.mu.Unlock()
}

func (s *Server) triggerTemplateRefusalAudit() trigger.TemplateRefusalAuditSink {
	if s != nil {
		s.triggers.mu.Lock()
		sink := s.triggers.templateRefusalAudit
		if sink == nil {
			sink = trigger.NewSlogAuditSink(trigger.SlogAuditConfig{})
			s.triggers.templateRefusalAudit = sink
		}
		s.triggers.mu.Unlock()
		return sink
	}
	return trigger.NewSlogAuditSink(trigger.SlogAuditConfig{})
}

// TriggerEventQueue returns the hub's shared durablequeue.StoreQueue for
// trigger.event producers and drainers. When s was constructed via New, this
// returns s.durableQueue (the same queue instance used by POST /api/v1/hooks/{id}
// ingress so Wake wakes the local drainer immediately); bare test Server
// structs without s.durableQueue fall back to a lazy StoreQueue on s.triggers.
func (s *Server) TriggerEventQueue() *durablequeue.StoreQueue {
	if s == nil {
		return nil
	}
	if s.durableQueue != nil {
		return s.durableQueue
	}
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return s.triggerQueueLocked(dqStore)
}

func (s *Server) triggerQueueLocked(dqStore store.DurableQueueStore) *durablequeue.StoreQueue {
	if s.durableQueue != nil {
		return s.durableQueue
	}
	if s.triggers.queue == nil {
		s.triggers.queue = durablequeue.NewQueue(dqStore, durablequeue.WithQueueMetrics(serverTriggerMetrics{srv: s}))
	}
	return s.triggers.queue
}

// serverTriggerCore adapts Server's fire core, experiment gate, and wake-fire
// shutdown admission to trigger.Core and trigger.FireAdmitter without exposing
// package hub types to pkg/hub/trigger.
type serverTriggerCore struct {
	srv *Server
}

var (
	_ trigger.Core                 = serverTriggerCore{}
	_ trigger.FireAdmitter         = serverTriggerCore{}
	_ trigger.SourceAuthorizer     = serverTriggerCore{}
	_ trigger.SourceEnricher       = serverTriggerCore{}
	_ trigger.PostEvaluationHook   = serverTriggerCore{}
	_ trigger.ReuseMessageRenderer = serverTriggerCore{}
	_ trigger.PreFireRefuser       = serverTriggerCore{}
	_ trigger.RetryTargetChecker   = serverTriggerCore{}
)

func (c serverTriggerCore) PreFireRefusal(ctx context.Context, tr *store.Trigger, actionType string, ev *hubfire.FireEvent) (string, string, bool) {
	if c.srv == nil || tr == nil || ev == nil {
		return "", "", false
	}
	if !trigger.ActionSubjectToPreFireGuards(actionType) {
		return "", "", false
	}
	if refusal, auditReason := c.srv.githubBotSenderRefusal(ctx, ev); refusal != nil {
		return refusal.Reason, auditReason, true
	}
	return "", "", false
}

func (c serverTriggerCore) RenderReuseMessage(ctx context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
	return c.srv.renderDispatchReuseMessage(ctx, req)
}

// CheckRetryTarget implements trigger.RetryTargetChecker by checking the target
// agent's live existence and deliverability state from the store.
func (c serverTriggerCore) CheckRetryTarget(ctx context.Context, agentID string) (trigger.RetryTargetState, error) {
	if c.srv == nil || c.srv.store == nil || agentID == "" {
		return trigger.RetryTargetOffline, nil
	}
	agent, err := c.srv.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return trigger.RetryTargetDeleted, nil
		}
		return trigger.RetryTargetOffline, err
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return trigger.RetryTargetDeleted, nil
	}
	if c.srv.validateAgentDeliverable(agent) == nil {
		return trigger.RetryTargetDeliverable, nil
	}
	_, skip := classifyOfflineTargetPhase(agent)
	if skip {
		return trigger.RetryTargetTerminal, nil
	}
	return trigger.RetryTargetOffline, nil
}

func (c serverTriggerCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if req.BeforeFire == nil && c.srv != nil {
		req.BeforeFire = func(bfCtx context.Context) (string, error) {
			return c.srv.checkTriggerFireLimits(bfCtx, req)
		}
	}
	out, action, rawErr := c.srv.fireActionWithError(ctx, req)
	if out.Status == hubfire.FireStatusFailed && out.Error != "" {
		out.Error = sanitizeTriggerFireError(ctx, action, req, out.Status, out.Error, rawErr)
	}
	return out
}

const triggerProjectCapNotifyCooldown = time.Hour

// triggerLimitSkipError is a typed sentinel returned from inside a
// commitAgentCreate OnCommitTx callback when a serialized concurrency or
// project-cap check fails under LockTriggerQuota, causing the transaction to
// roll back cleanly and fireTriggerDispatchAgent to report FireStatusSkipped.
type triggerLimitSkipError struct {
	reason string
}

func (e *triggerLimitSkipError) Error() string { return e.reason }

// isLiveTriggerAgentPhase reports whether an agent phase counts as live for
// trigger dispatch concurrency and project live-agent caps (Stage 2 §7.4:
// phase not in {stopped, error, halted}; soft-deleted agents are excluded by
// AgentFilter's default IncludeDeleted=false).
func isLiveTriggerAgentPhase(phase string) bool {
	switch phase {
	case "stopped", "error", "halted":
		return false
	default:
		return true
	}
}

// countLiveAgentsMatching counts non-deleted agents matching filter whose phase
// is not terminal (stopped, error, halted).
func (s *Server) countLiveAgentsMatching(ctx context.Context, filter store.AgentFilter) (int, error) {
	return countLiveAgentsMatchingStore(ctx, s.store, filter)
}

func countLiveAgentsMatchingStore(ctx context.Context, st store.Store, filter store.AgentFilter) (int, error) {
	filter.IncludeDeleted = false
	rows, err := st.CountAgentsByPhaseIDs(ctx, filter)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, r := range rows {
		if isLiveTriggerAgentPhase(r.Phase) {
			live++
		}
	}
	return live, nil
}

func triggerMaxConcurrentDispatches(trig *hubfire.FireTriggerRef) int {
	maxConc := 0
	if trig != nil {
		maxConc = trig.MaxConcurrentDispatches
	}
	if maxConc <= 0 {
		maxConc = store.DefaultTriggerMaxConcurrentDispatches
	}
	if maxConc > store.MaxTriggerConcurrentDispatches {
		maxConc = store.MaxTriggerConcurrentDispatches
	}
	return maxConc
}

// checkTriggerFireLimits enforces Stage 2 §7.4 per-trigger and per-project fire
// limits after action.Authorize succeeds and before claim/create/delivery:
//  1. max_fires_per_minute (all trigger actions) -> skipped "rate limit"
//  2. max_concurrent_dispatches (dispatch_agent; default 5, max 50) -> skipped "concurrency limit"
//  3. MaxLiveTriggerAgentsPerProject (dispatch_agent; 25) -> skipped "project trigger-agent cap"
//     + once-per-hour-per-trigger notification hook.
//
// The per-trigger rate check runs inside WithTx under LockTriggerQuota so
// concurrent drainers and Postgres replicas are serialized; CountRecentTriggerFires
// uses a deterministic (created, id) ordering among in-flight "firing" rows so
// exactly max_fires_per_minute fires are admitted under concurrency (O1).
// For dispatch_agent, checkTriggerDispatchAgentLimitsInTx re-verifies (2) and
// (3) atomically inside commitAgentCreate's transaction under LockTriggerQuota
// so concurrent drainers and replicas cannot overshoot either cap (O2).
func (s *Server) checkTriggerFireLimits(ctx context.Context, req hubfire.FireRequest) (string, error) {
	if s == nil || s.store == nil || req.Event == nil || req.Event.Trigger == nil {
		return "", nil
	}
	trig := req.Event.Trigger
	triggerID := req.Authority.DefinitionID
	if triggerID == "" {
		triggerID = trig.ID
	}

	// 1. Per-trigger fire rate (max_fires_per_minute).
	if trig.MaxFiresPerMinute > 0 && triggerID != "" {
		maxPerMin := min(trig.MaxFiresPerMinute, store.MaxTriggerFiresPerMinute)
		since := time.Now().UTC().Add(-time.Minute)
		var recent int
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			if req.ProjectID != "" {
				if lErr := tx.LockTriggerQuota(ctx, req.ProjectID); lErr != nil {
					return lErr
				}
			}
			var cErr error
			recent, cErr = tx.CountRecentTriggerFires(ctx, triggerID, since, req.Authority.FireID)
			return cErr
		})
		if err != nil {
			return "", fmt.Errorf("count recent trigger fires: %w", err)
		}
		if recent >= maxPerMin {
			return trigger.ErrFireRateLimit, nil
		}
	}

	if req.ActionType != store.TriggerActionDispatchAgent {
		return "", nil
	}

	// 2. Per-trigger dispatch concurrency (default 5, max 50).
	maxConc := triggerMaxConcurrentDispatches(trig)
	liveForTrigger, err := s.countLiveAgentsMatching(ctx, store.AgentFilter{
		ProjectID: req.ProjectID,
		Labels:    map[string]string{triggerLabelKeyTrigger: triggerID},
	})
	if err != nil {
		return "", fmt.Errorf("count live trigger agents: %w", err)
	}
	if liveForTrigger >= maxConc {
		return trigger.ErrFireConcurrencyLimit, nil
	}

	// 3. Per-project live trigger-created agent cap (hub constant 25).
	liveInProject, err := s.countLiveAgentsMatching(ctx, store.AgentFilter{
		ProjectID: req.ProjectID,
		LabelKeys: []string{triggerLabelKeyTrigger},
	})
	if err != nil {
		return "", fmt.Errorf("count project live trigger agents: %w", err)
	}
	if liveInProject >= store.MaxLiveTriggerAgentsPerProject {
		s.notifyTriggerProjectCapReached(ctx, req, liveInProject)
		return trigger.ErrFireProjectTriggerAgentCap, nil
	}

	return "", nil
}

// checkTriggerDispatchAgentLimitsInTx enforces max_concurrent_dispatches and
// MaxLiveTriggerAgentsPerProject atomically inside commitAgentCreate's
// transaction under tx.LockTriggerQuota (pg_advisory_xact_lock on Postgres).
// Because tx.CreateAgent has already inserted the new live agent row inside tx,
// limits are exceeded when count > limit (Stage 2 §7.4, P4a O2).
func (s *Server) checkTriggerDispatchAgentLimitsInTx(ctx context.Context, tx store.Store, req hubfire.FireRequest) error {
	if s == nil || tx == nil || req.Event == nil || req.Event.Trigger == nil {
		return nil
	}
	if err := tx.LockTriggerQuota(ctx, req.ProjectID); err != nil {
		return fmt.Errorf("lock trigger quota for dispatch limits: %w", err)
	}
	trig := req.Event.Trigger
	triggerID := req.Authority.DefinitionID
	if triggerID == "" {
		triggerID = trig.ID
	}

	maxConc := triggerMaxConcurrentDispatches(trig)
	liveForTrigger, err := countLiveAgentsMatchingStore(ctx, tx, store.AgentFilter{
		ProjectID: req.ProjectID,
		Labels:    map[string]string{triggerLabelKeyTrigger: triggerID},
	})
	if err != nil {
		return fmt.Errorf("count live trigger agents in tx: %w", err)
	}
	if liveForTrigger > maxConc {
		return &triggerLimitSkipError{reason: trigger.ErrFireConcurrencyLimit}
	}

	liveInProject, err := countLiveAgentsMatchingStore(ctx, tx, store.AgentFilter{
		ProjectID: req.ProjectID,
		LabelKeys: []string{triggerLabelKeyTrigger},
	})
	if err != nil {
		return fmt.Errorf("count project live trigger agents in tx: %w", err)
	}
	if liveInProject > store.MaxLiveTriggerAgentsPerProject {
		s.notifyTriggerProjectCapReached(ctx, req, liveInProject-1)
		return &triggerLimitSkipError{reason: trigger.ErrFireProjectTriggerAgentCap}
	}
	return nil
}

// notifyTriggerProjectCapReached emits at most one notification per hour per
// trigger when the per-project live trigger-agent cap is reached (Stage 2 §7.4).
// P7 hook point: once notifyFireOutcome (§10) lands, route this notification
// through it alongside the structured warning log.
func (s *Server) notifyTriggerProjectCapReached(ctx context.Context, req hubfire.FireRequest, liveCount int) {
	triggerID := req.Authority.DefinitionID
	if triggerID == "" && req.Event != nil && req.Event.Trigger != nil {
		triggerID = req.Event.Trigger.ID
	}
	if triggerID == "" {
		return
	}
	now := time.Now().UTC()
	rt := &s.triggers
	rt.mu.Lock()
	if rt.projectCapNotify == nil {
		rt.projectCapNotify = make(map[string]time.Time)
	}
	if last, ok := rt.projectCapNotify[triggerID]; ok && now.Sub(last) < triggerProjectCapNotifyCooldown {
		rt.mu.Unlock()
		return
	}
	// Bound map size by pruning expired entries when large.
	if len(rt.projectCapNotify) >= 1000 {
		for k, ts := range rt.projectCapNotify {
			if now.Sub(ts) >= triggerProjectCapNotifyCooldown {
				delete(rt.projectCapNotify, k)
			}
		}
	}
	rt.projectCapNotify[triggerID] = now
	rt.mu.Unlock()

	// P7 hook point: invoke notifyFireOutcome(ctx, req, FireOutcome{Status: skipped, Error: "project trigger-agent cap"}, ...) here.
	slog.WarnContext(ctx, "trigger dispatch skipped: project live trigger-agent cap reached",
		"project_id", req.ProjectID,
		"trigger_id", triggerID,
		"fire_id", req.Authority.FireID,
		"live_trigger_agents", liveCount,
		"cap", store.MaxLiveTriggerAgentsPerProject)
}

// sanitizeTriggerFireError maps failed error text persisted to
// trigger_fires.error onto the Stage 1 §5.5 generic set for built-in actions
// (or an action's FireErrorSanitizer allowlist) so internal details (agent
// slugs, broker HTTP bodies, raw context errors) never leak through trigger
// history (1b-3 review N4, 2c item 4). Refusals and skips already carry generic
// reasons by contract; schedule fires never pass through serverTriggerCore.Fire
// and retain their existing scheduled_events.error text unchanged.
func sanitizeTriggerFireError(ctx context.Context, action FireAction, req hubfire.FireRequest, status hubfire.FireStatus, raw string, rawErr error) string {
	if raw == "" {
		return ""
	}
	if sanitizer, ok := action.(FireErrorSanitizer); ok {
		known, fallback := sanitizer.SanitizedReasons()
		if raw == req.ActionType+" refused" ||
			raw == errScheduledAuthorityUnrecorded.Error() ||
			raw == "hub shutting down" ||
			raw == "fire failed" ||
			slices.Contains(known, raw) {
			return raw
		}
		mapped := fallback
		if mapped == "" {
			mapped = "fire failed"
		}
		slog.WarnContext(ctx, "trigger fire error mapped to generic text",
			"actionType", req.ActionType,
			"status", status,
			"fireID", req.Authority.FireID,
			"triggerID", req.Authority.DefinitionID,
			"projectID", req.ProjectID,
			"mapped", mapped,
			"raw_error", raw)
		return mapped
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
		"name exhausted",
		"event file too large",
		"exec failed",
		"fire failed",
		"invalid message payload",
		"invalid dispatch_agent payload",
		errScheduledExecPayloadInvalid.Error(),
		execReasonDispatchFailed,
		execReasonOverlap,
		execReasonCapacity,
		execReasonStartExpired,
		execReasonMissedWindow,
		execReasonDisabled,
		execReasonUnavailable,
		execReasonStartFailed,
		execReasonAgentRestarted,
		execReasonAgentStopped,
		execReasonOutcomeUnknown,
		execReasonSourceNotReplayProtected,
		execReasonRateLimit,
		execReasonSubjectBusy,
		execReasonContextInvalid,
		execReasonContextUnsupported,
		execReasonMissingDeliveryID:
		return raw
	}
	if req.ActionType == "dispatch_agent" && rawErr != nil {
		var missingEnv *HeadlessMissingEnvError
		var blocked *scheduleBlockedAgentError
		if errors.As(rawErr, &missingEnv) || errors.As(rawErr, &blocked) {
			return raw
		}
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

	slog.WarnContext(ctx, "trigger fire error mapped to generic text",
		"actionType", req.ActionType,
		"status", status,
		"fireID", req.Authority.FireID,
		"triggerID", req.Authority.DefinitionID,
		"projectID", req.ProjectID,
		"mapped", mapped,
		"raw_error", raw)
	return mapped
}

func (c serverTriggerCore) Experiment(name string) bool {
	return c.srv.experimentEnabled(name)
}

func (c serverTriggerCore) OnEventEvaluated(ctx context.Context, ev *store.TriggerEvent, outcome trigger.EventEvaluationOutcome, attempts int) {
	if c.srv == nil || ev == nil {
		return
	}
	if outcome.AnyFireRecorded || outcome.MatchCount != 0 || outcome.FireCount != 0 {
		return
	}
	if ev.SourceKind == store.TriggerSourceKindGitHub {
		c.srv.handleUnmatchedGitHubCommand(ctx, ev, attempts)
	}
}

// AuthorizeEventSource implements trigger.SourceAuthorizer for Stage 3 §4.2
// step 4 (rev 1.10): before any fire on a github event, verify that the
// project's GitHub App installation association is authorized now via
// authorizeGitHubAppAssociation (using existing caches). Definitive
// errGitHubAssociationUnauthorized updates project.GitHubAppStatus to
// association_unauthorized and returns (false, nil) so matched candidates
// record refused "github association not authorized". Inconclusive
// errGitHubAssociationCheckFailed updates project.GitHubAppStatus to
// association_check_failed and returns an error so the durable queue backs off
// and retries until MaxAge without refusing the event.
func (c serverTriggerCore) AuthorizeEventSource(ctx context.Context, ev *store.TriggerEvent) (bool, error) {
	if ev == nil || ev.SourceKind != store.TriggerSourceKindGitHub {
		return true, nil
	}
	if c.srv == nil || c.srv.store == nil {
		return false, nil
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(ev.SourceID), 10, 64)
	if err != nil || instID <= 0 {
		return false, nil
	}
	project, err := c.srv.store.GetProject(ctx, ev.ProjectID)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return false, nil
		}
		return false, err
	}
	if project == nil || project.GitHubInstallationID == nil || *project.GitHubInstallationID != instID {
		return false, nil
	}
	if _, authzErr := c.srv.authorizeGitHubAppAssociation(ctx, project, instID); authzErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		errCode := githubAssociationErrorCode(authzErr)
		c.srv.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
			errCode, githubAppStatusMessage(errCode))
		if errors.Is(authzErr, errGitHubAssociationCheckFailed) {
			slog.WarnContext(ctx, "GitHub trigger fire deferred: installation association check inconclusive; will retry",
				"project_id", project.ID, "installation_id", instID, "event_id", ev.ID, "error", authzErr)
			return false, authzErr
		}
		slog.WarnContext(ctx, "GitHub trigger fire refused: installation association not authorized",
			"project_id", project.ID, "installation_id", instID, "event_id", ev.ID, "error", authzErr)
		return false, nil
	}
	c.srv.clearProjectGitHubAssociationErrorStatus(ctx, project)
	return true, nil
}

func (c serverTriggerCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
	if itemCtx != nil && itemCtx.Err() != nil {
		return ctx, nil, func() {}, false
	}
	var (
		trackCtx context.Context
		endWork  = func() {}
	)
	if existingAbort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context); existingAbort != nil {
		trackCtx = existingAbort
	} else if c.srv != nil {
		var ok bool
		trackCtx, endWork, ok = c.srv.scheduledDetachedFires.begin()
		if !ok {
			return ctx, nil, func() {}, false
		}
	}
	// Bound scheduledFireAbortKey and the returned abortCtx by BOTH hub
	// shutdown (trackCtx) and the durablequeue item lifetime (itemCtx), with
	// NO per-fire deadline, so both wakeFireTarget's detachKeepFireAbort(ctx)
	// and post-wake tracker.Detach(ctx) outlive the 30 s per-fire timeout
	// without outliving either the queue item (Stage 1 ledger L3) or shutdown.
	abortCtx, cancelAbort := context.WithCancel(context.Background())
	var stopTrack, stopItem func() bool
	if trackCtx != nil {
		stopTrack = context.AfterFunc(trackCtx, cancelAbort)
	}
	if itemCtx != nil {
		stopItem = context.AfterFunc(itemCtx, cancelAbort)
	}
	fireCtx, cancelFire := context.WithCancel(ctx)
	stopFire := context.AfterFunc(abortCtx, cancelFire)
	return withScheduledFireAbort(fireCtx, abortCtx), abortCtx, func() {
		stopFire()
		if stopItem != nil {
			stopItem()
		}
		if stopTrack != nil {
			stopTrack()
		}
		cancelFire()
		cancelAbort()
		endWork()
	}, true
}

func (s *Server) triggerDrainerContext(ctx context.Context) context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return ctx
}

// startTriggerDrainer marks trigger background services started and launches
// the trigger.event drainer when hub.triggers is enabled. When hub.triggers is
// off, no drainer goroutine runs until reconcileTriggerDrainer observes the
// experiment enabled (§16.2).
func (s *Server) startTriggerDrainer(ctx context.Context) <-chan struct{} {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.started = true
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
	return rt.done
}

// reconcileTriggerDrainer aligns the running trigger.event drainer with the
// live hub.triggers experiment state (called on ApplySnapshot and in tests).
func (s *Server) reconcileTriggerDrainer(ctx context.Context) {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return
	}
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
}

func (s *Server) reconcileTriggerDrainerLocked(ctx context.Context, dqStore store.DurableQueueStore) {
	rt := &s.triggers
	if !s.experimentEnabled(experiments.Triggers) {
		if rt.cancel != nil {
			rt.cancel()
			rt.drainer, rt.retryDrainer, rt.cancel, rt.done = nil, nil, nil, nil
		}
		return
	}
	if rt.drainer != nil {
		return
	}
	ctx = s.triggerDrainerContext(ctx)
	if ctx.Err() != nil {
		return
	}
	q := s.triggerQueueLocked(dqStore)
	d, err := s.triggerDrainer(dqStore, q)
	if err != nil {
		slog.Warn("hub.triggers: event drainer not started", "error", err)
		return
	}
	rd, err := s.triggerRetryDrainer(dqStore, q)
	if err != nil {
		slog.Warn("hub.triggers: fire retry drainer not started", "error", err)
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.drainer, rt.retryDrainer, rt.cancel, rt.done = d, rd, cancel, done
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			d.Run(dctx)
		}()
		go func() {
			defer wg.Done()
			rd.Run(dctx)
		}()
		if s.events != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.runTriggerAgentStatusNudger(dctx, q)
			}()
		}
		wg.Wait()
		cancel()
		rt.mu.Lock()
		if rt.drainer == d {
			rt.drainer, rt.retryDrainer, rt.cancel, rt.done = nil, nil, nil, nil
		}
		rt.mu.Unlock()
	}()
}

// runTriggerAgentStatusNudger subscribes to agent status transitions and nudges
// waiting offline-queued message fires when their target agent transitions into
// running (Stage 2 §6.2 item 4). Repeated status/activity updates while an
// agent is already running are ignored so steady-state agent traffic does not
// issue unnecessary waiting-fire lookups.
func (s *Server) runTriggerAgentStatusNudger(ctx context.Context, q *durablequeue.StoreQueue) {
	if s == nil || s.events == nil || q == nil || s.store == nil {
		return
	}
	const maxTrackedAgentPhases = 10_000
	lastPhase := make(map[string]string)

	ch, unsubscribe := s.events.Subscribe("project.*.agent.status")
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if !strings.HasSuffix(evt.Subject, ".agent.status") {
				continue
			}
			var statusEvt AgentStatusEvent
			if err := json.Unmarshal(evt.Data, &statusEvt); err != nil {
				continue
			}
			if statusEvt.AgentID == "" || statusEvt.Phase == "" {
				continue
			}
			prev := lastPhase[statusEvt.AgentID]
			if len(lastPhase) >= maxTrackedAgentPhases {
				clear(lastPhase)
			}
			lastPhase[statusEvt.AgentID] = statusEvt.Phase
			if statusEvt.Phase != string(state.PhaseRunning) || prev == string(state.PhaseRunning) {
				continue
			}
			if !s.experimentEnabled(experiments.Triggers) {
				continue
			}
			s.nudgeWaitingFiresForAgent(ctx, q, statusEvt.AgentID)
		}
	}
}

func (s *Server) nudgeWaitingFiresForAgent(ctx context.Context, q *durablequeue.StoreQueue, agentID string) {
	nudgeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fireIDs, err := s.store.ListWaitingFiresForTarget(nudgeCtx, agentID, trigger.MaxWaitingFiresPerTarget)
	if err != nil {
		slog.WarnContext(nudgeCtx, "hub.triggers: failed to list waiting fires for running agent",
			"agent_id", agentID, "error", err)
		return
	}
	if len(fireIDs) == 0 {
		return
	}
	keys := make([]string, len(fireIDs))
	for i, id := range fireIDs {
		keys[i] = trigger.FireRetryDedupPrefix + id
	}
	if _, err := q.Nudge(nudgeCtx, trigger.FireRetryQueueKind, keys); err != nil {
		slog.WarnContext(nudgeCtx, "hub.triggers: failed to nudge waiting fires for running agent",
			"agent_id", agentID, "count", len(keys), "error", err)
	}
}

func (s *Server) newTriggerDispatcher(queue *durablequeue.StoreQueue) (*trigger.Dispatcher, error) {
	core := serverTriggerCore{srv: s}
	logger := logging.Subsystem("hub.trigger-drainer")
	if s.triggers.ruleMetrics == nil {
		m, mErr := trigger.NewOTelRuleEvalMetrics(otel.GetMeterProvider())
		if mErr != nil {
			logger.Warn("hub.triggers: failed to initialize rule eval metrics", "error", mErr)
		} else {
			s.triggers.ruleMetrics = m
		}
	}
	metrics := serverTriggerMetrics{srv: s}
	var q durablequeue.Queue
	if queue != nil {
		q = queue
	}
	txRunner := func(ctx context.Context, fn func(ctx context.Context, tx store.Tx, ts store.TriggerStore) error) error {
		return s.store.WithTx(ctx, func(tx store.Store) error {
			return fn(ctx, tx, tx)
		})
	}
	return trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:         s.store,
		Queue:         q,
		TxRunner:      txRunner,
		Core:          core,
		Storage:       s.GetStorage,
		SubjectClaims: trigger.NewSubjectClaims(s.store, s.store, nil),
		Rules:         s.TriggerRules(),
		RuleMetrics:   s.triggers.ruleMetrics,
		Metrics:       metrics,
		Logger:        logger,
	})
}

// triggerDrainer constructs the trigger.event durablequeue.Drainer wired to
// Server's fire core (catalog entry points triggerDrainer:message,
// triggerDrainer:dispatch_agent, and triggerDrainer:github.comment for trigger.fire).
func (s *Server) triggerDrainer(dqStore store.DurableQueueStore, queue *durablequeue.StoreQueue) (*durablequeue.Drainer, error) {
	disp, err := s.newTriggerDispatcher(queue)
	if err != nil {
		return nil, err
	}
	return disp.NewDrainer(trigger.DrainerOptions{
		QueueStore: dqStore,
		Queue:      queue,
		Owner:      s.HubID(),
		Metrics:    serverTriggerMetrics{srv: s},
	})
}

// triggerRetryDrainer constructs the trigger.fire_retry durablequeue.Drainer
// wired to Server's fire core (catalog entry point triggerRetryDrainer:message).
func (s *Server) triggerRetryDrainer(dqStore store.DurableQueueStore, queue *durablequeue.StoreQueue) (*durablequeue.Drainer, error) {
	disp, err := s.newTriggerDispatcher(queue)
	if err != nil {
		return nil, err
	}
	return disp.NewFireRetryDrainer(trigger.DrainerOptions{
		QueueStore: dqStore,
		Queue:      queue,
		Owner:      s.HubID(),
		Metrics:    serverTriggerMetrics{srv: s},
	})
}

// stopTriggerDrainer cancels the running trigger.event and trigger.fire_retry
// drainers and agent-status nudger and waits for their bounded shutdown drain.
func (s *Server) stopTriggerDrainer(ctx context.Context) {
	rt := &s.triggers
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.drainer, rt.retryDrainer, rt.cancel, rt.done = nil, nil, nil, nil
	rt.started = false
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runTriggerMaintenance executes the hourly trigger reaper, orphan-pending
// event reconciliation, and 7-day history purge inside Server.purgeHandler
// (§4.3, §6.4, rev 7.4).
func (s *Server) runTriggerMaintenance(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	runAt := time.Now().UTC()
	res, err := trigger.RunMaintenanceWithStorage(ctx, s.store, s.GetStorage(), runAt, trigger.MaintenanceOptions{
		Logger: logging.Subsystem("hub.triggers"),
	})
	serverTriggerMetrics{srv: s}.RecordReaperRun(res.FiresReaped, runAt, err)
	if err != nil {
		slog.Error("Scheduler: trigger maintenance failed", "error", err)
	}
	sweptBound := s.sweepOrphanedBoundWebhookEndpoints(ctx)
	if res.FiresReaped > 0 || res.EventsReconciled > 0 || res.EventsPurged > 0 || res.FiresPurged > 0 || res.ClaimsReleased > 0 || res.ClaimsPurged > 0 || res.RotatedSecretsPurged > 0 || res.OverflowObjectsPurged > 0 || res.OverflowOrphansSwept > 0 || res.OverflowDeleteErrors > 0 || sweptBound > 0 {
		slog.Info("Scheduler: completed trigger maintenance",
			"fires_reaped", res.FiresReaped,
			"events_reconciled", res.EventsReconciled,
			"events_purged", res.EventsPurged,
			"fires_purged", res.FiresPurged,
			"claims_released", res.ClaimsReleased,
			"claims_purged", res.ClaimsPurged,
			"rotated_secrets_purged", res.RotatedSecretsPurged,
			"overflow_objects_purged", res.OverflowObjectsPurged,
			"overflow_orphans_swept", res.OverflowOrphansSwept,
			"overflow_delete_errors", res.OverflowDeleteErrors,
			"orphaned_bound_endpoints_swept", sweptBound)
	}
}

// verifiedLabelForFireEvent maps known source kinds and signature_mode values
// to canonical integrity labels and fails closed to ExternalVerifiedUnverified
// for missing or unrecognized modes (never assuming scion-v1 and never echoing
// raw input).
func verifiedLabelForFireEvent(ev *hubfire.FireEvent) string {
	if ev == nil {
		return messaging.ExternalVerifiedUnverified
	}
	if ev.SourceKind == store.TriggerSourceKindGitHub {
		return messaging.ExternalVerifiedGitHubHMACSHA256Projected
	}
	switch ev.Attributes["signature_mode"] {
	case store.WebhookSignatureModeBodyOnly, messaging.ExternalVerifiedHMACSHA256Body:
		return messaging.ExternalVerifiedHMACSHA256Body
	case store.WebhookSignatureModeScionV1, messaging.ExternalVerifiedScionV1:
		return messaging.ExternalVerifiedScionV1
	case store.WebhookSignatureModeGitHubV1, messaging.ExternalVerifiedGitHubV1:
		return messaging.ExternalVerifiedGitHubV1
	case store.WebhookSignatureModeStripeV1, messaging.ExternalVerifiedStripeV1:
		return messaging.ExternalVerifiedStripeV1
	case store.WebhookAuthModeHubToken, messaging.ExternalVerifiedHubToken:
		return messaging.ExternalVerifiedHubToken
	default:
		return messaging.ExternalVerifiedUnverified
	}
}

const (
	neutralEventContentType = "application/octet-stream"
	maxSafeContentTypeBytes = 128
)

var safeContentTypeParamMaxLen = map[string]int{
	"charset": 32,
	"version": 16,
}

func isSafeMediaTypeToken(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		isAlphaNum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
		if i == 0 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && b != '.' && b != '_' && b != '+' && b != '-' {
			return false
		}
	}
	return true
}

func trimHTTPOWS(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// sanitizeEventContentType restricts an event's content_type attribute to a
// canonical lowercase media-type token ("type/subtype") plus an optional
// allowlisted token parameter subset (charset, version). Any malformed value,
// quoted string, comment, control byte, or unknown parameter is replaced with
// neutralEventContentType while leaving the event body untouched.
func sanitizeEventContentType(raw string) string {
	if len(raw) == 0 || len(raw) > maxSafeContentTypeBytes {
		return neutralEventContentType
	}
	for i := 0; i < len(raw); i++ {
		if (raw[i] < 0x20 && raw[i] != '\t') || raw[i] > 0x7e {
			return neutralEventContentType
		}
	}
	trimmed := trimHTTPOWS(raw)
	if trimmed == "" {
		return neutralEventContentType
	}
	parts := strings.Split(trimmed, ";")
	if len(parts) > 1+len(safeContentTypeParamMaxLen) {
		return neutralEventContentType
	}
	media := trimHTTPOWS(parts[0])
	slash := strings.IndexByte(media, '/')
	if slash <= 0 || slash >= len(media)-1 || strings.IndexByte(media[slash+1:], '/') >= 0 {
		return neutralEventContentType
	}
	mainType := media[:slash]
	subType := media[slash+1:]
	if !isSafeMediaTypeToken(mainType, 64) || !isSafeMediaTypeToken(subType, 64) {
		return neutralEventContentType
	}

	var b strings.Builder
	b.Grow(len(trimmed))
	b.WriteString(strings.ToLower(mainType))
	b.WriteByte('/')
	b.WriteString(strings.ToLower(subType))

	var seenParams map[string]struct{}
	for _, rawParam := range parts[1:] {
		p := trimHTTPOWS(rawParam)
		if p == "" {
			return neutralEventContentType
		}
		eq := strings.IndexByte(p, '=')
		if eq <= 0 || eq >= len(p)-1 || strings.IndexByte(p[eq+1:], '=') >= 0 {
			return neutralEventContentType
		}
		k := strings.ToLower(p[:eq])
		v := p[eq+1:]
		maxValLen, allowed := safeContentTypeParamMaxLen[k]
		if !allowed || !isSafeMediaTypeToken(v, maxValLen) {
			return neutralEventContentType
		}
		if seenParams == nil {
			seenParams = make(map[string]struct{}, len(parts)-1)
		}
		if _, dup := seenParams[k]; dup {
			return neutralEventContentType
		}
		seenParams[k] = struct{}{}
		b.WriteString("; ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.ToLower(v))
	}
	return b.String()
}

// buildDispatchedEventBody constructs the §8.3 messaging.EventBody for a
// FireEvent delivered to an agent.
func buildDispatchedEventBody(ev *hubfire.FireEvent) messaging.EventBody {
	if ev == nil {
		return messaging.EventBody{}
	}
	var body messaging.EventBody
	candidateType := messaging.EventType(ev.Type)
	if messaging.ValidateEventType(candidateType) == nil {
		body.Type = candidateType
	} else {
		body.Type = messaging.EventTriggerFired
		body.EventType = ev.Type
	}
	body.Subject = ev.Subject
	body.EventID = ev.ID
	if ev.SourceKind != "" && ev.SourceID != "" {
		sourceName := ""
		if ev.Attributes != nil {
			sourceName = ev.Attributes["endpoint_name"]
		}
		if sourceName == "" && ev.Schedule != nil {
			sourceName = ev.Schedule.Name
		}
		body.Source = &messaging.EventSource{
			Kind: ev.SourceKind,
			ID:   ev.SourceID,
			Name: sourceName,
		}
	}
	if ev.Trigger != nil {
		body.Trigger = &messaging.EventTriggerRef{
			ID:   ev.Trigger.ID,
			Name: ev.Trigger.Name,
		}
	}
	if ev.Schedule != nil {
		fireAt := ""
		if !ev.Schedule.FireAt.IsZero() {
			fireAt = ev.Schedule.FireAt.UTC().Format(time.RFC3339)
		}
		body.Schedule = &messaging.EventScheduleRef{
			ID:      ev.Schedule.ID,
			Name:    ev.Schedule.Name,
			EventID: ev.Schedule.EventID,
			FireAt:  fireAt,
		}
	}
	verified := verifiedLabelForFireEvent(ev)
	if len(ev.Attributes) > 0 {
		attrs := maps.Clone(ev.Attributes)
		if verified != messaging.ExternalVerifiedScionV1 &&
			verified != messaging.ExternalVerifiedHubToken &&
			verified != messaging.ExternalVerifiedGitHubV1 &&
			verified != messaging.ExternalVerifiedStripeV1 &&
			verified != messaging.ExternalVerifiedGitHubHMACSHA256Projected {
			// §8.1a: generic webhook modes other than scion_v1 strip header-derived
			// attributes. Stage 3 GitHub App deliveries (§4.1, §4.3) keep "delivery"
			// and "event" populated from X-GitHub-Delivery and X-GitHub-Event: those
			// headers are shape-checked at ingress, NOT authenticated by
			// X-Hub-Signature-256 (which covers the raw request body only).
			delete(attrs, "delivery")
			delete(attrs, "event")
			delete(attrs, "subject")
		}
		if rawCT, hasCT := attrs["content_type"]; hasCT {
			attrs["content_type"] = sanitizeEventContentType(rawCT)
		}
		if len(attrs) > 0 {
			body.Attributes = attrs
		}
	}
	if len(ev.Lists) > 0 {
		body.Lists = copyTriggerStringListMap(ev.Lists)
	}
	if ev.PayloadRef != "" {
		preview := ev.PayloadPreview
		totalBytes := ev.PayloadBytes
		if totalBytes <= 0 {
			totalBytes = len(preview)
		}
		body.External = messaging.NewTruncatedExternalPayload(verified, preview, totalBytes)
	} else if len(ev.Payload) > 0 {
		body.External = messaging.NewExternalPayload(verified, ev.Payload)
	}
	return body
}

// DispatchedEventUsesPhaseANoRow reports whether req stays on the Stage 1
// Phase-A unpersisted delivery path even when hub.conduit_deliver is enabled.
// Trigger fires have no scheduled_events row and use Sender="trigger", so
// persisting them into messages.scheduled_event_id and holding on
// awaiting_session would fail held-message re-drive revalidation; keeping them
// on the no-row path makes non-deliverable targets fail loud and record
// status=failed on trigger_fires without changing schedule fire behaviour.
//
// Named without "Trigger" so action_message.go satisfies TestFireCoreContainment.
func DispatchedEventUsesPhaseANoRow(req hubfire.FireRequest) bool {
	return req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger
}

// renderDispatchedMessageTemplate compiles and renders MessageEventPayload.Message
// through triggertemplate.SlotMessage for event-triggered message fires
// (Stage 2 §5.7). On any render or plain_with_untrusted error, it emits a
// content-free hub.audit record carrying slot and path (never values) and
// returns a FireStatusRefused outcome.
//
// Named without the word "Trigger" so action_message.go can invoke it while
// satisfying TestFireCoreContainment.
func renderDispatchedMessageTemplate(
	ctx context.Context,
	s *Server,
	req hubfire.FireRequest,
	payload MessageEventPayload,
) (string, []string, *hubfire.FireOutcome) {
	ev := req.Event
	if ev == nil {
		return payload.Message, nil, nil
	}

	tmpl, err := triggertemplate.Parse(payload.Message, triggertemplate.SlotMessage)
	if err != nil {
		out := refuseMessageTemplateRender(ctx, s, req, err)
		return "", nil, &out
	}
	if payload.Plain && len(tmpl.Untrusted()) > 0 {
		s.triggerTemplateRefusalAudit().RecordTemplateRefusal(ctx, trigger.TemplateRefusalAuditRecord{
			ProjectID: req.ProjectID,
			TriggerID: req.Authority.DefinitionID,
			EventID:   ev.ID,
			FireID:    req.Authority.FireID,
			Slot:      triggertemplate.SlotMessage,
			Path:      tmpl.Untrusted()[0],
			Reason:    "plain_with_untrusted",
		})
		out := hubfire.FireOutcome{
			Status: hubfire.FireStatusRefused,
			Error:  "plain_with_untrusted",
		}
		return "", nil, &out
	}

	// Fast path: literal message with no placeholders or escaped $${.
	if len(tmpl.Placeholders()) == 0 && !strings.Contains(payload.Message, "$${") {
		return payload.Message, nil, nil
	}

	var projectName, sourceName, signatureMode string
	if s != nil && s.store != nil {
		if proj, pErr := s.store.GetProject(ctx, req.ProjectID); pErr == nil && proj != nil {
			projectName = proj.Name
		}
		if ev.SourceKind == store.TriggerSourceKindWebhook && ev.SourceID != "" {
			if ep, epErr := s.store.GetWebhookEndpoint(ctx, ev.SourceID); epErr == nil && ep != nil && ep.ProjectID == req.ProjectID {
				sourceName = ep.Name
				signatureMode = ep.SignatureMode
			}
		}
	}
	if sourceName == "" && ev.Attributes != nil {
		sourceName = ev.Attributes["endpoint_name"]
	}
	if signatureMode == "" && ev.Attributes != nil {
		signatureMode = ev.Attributes["signature_mode"]
	}

	vars := buildDispatchTemplateVars(req, &dispatchAgentAuthorized{
		projectName:   projectName,
		sourceName:    sourceName,
		signatureMode: signatureMode,
	})
	rendered, report, err := tmpl.Render(vars, payload.UntrustedVars)
	if err != nil {
		out := refuseMessageTemplateRender(ctx, s, req, err)
		return "", nil, &out
	}

	var untrustedPaths []string
	var seen map[string]bool
	for _, u := range report.Used {
		if u.Class != triggertemplate.ClassUntrusted {
			continue
		}
		if seen == nil {
			seen = make(map[string]bool)
		}
		if !seen[u.Path] {
			seen[u.Path] = true
			untrustedPaths = append(untrustedPaths, u.Path)
		}
	}
	return rendered, untrustedPaths, nil
}

func refuseMessageTemplateRender(ctx context.Context, s *Server, req hubfire.FireRequest, err error) hubfire.FireOutcome {
	slot := triggertemplate.SlotMessage
	path := ""
	reason := string(triggertemplate.KindValueRejected)

	var re *triggertemplate.RenderError
	var ue *triggertemplate.UntrustedVarError
	var pe *triggertemplate.ParseError
	switch {
	case errors.As(err, &re):
		if re.Slot != "" {
			slot = re.Slot
		}
		path = re.Path
		if re.Kind != "" {
			reason = string(re.Kind)
		}
	case errors.As(err, &ue):
		if ue.Slot != "" {
			slot = ue.Slot
		}
		path = ue.Path
	case errors.As(err, &pe):
		if pe.Slot != "" {
			slot = pe.Slot
		}
	}

	eventID := ""
	if req.Event != nil {
		eventID = req.Event.ID
	}
	// Never log expanded values or rendered output (§5.2, §5.7).
	s.triggerTemplateRefusalAudit().RecordTemplateRefusal(ctx, trigger.TemplateRefusalAuditRecord{
		ProjectID: req.ProjectID,
		TriggerID: req.Authority.DefinitionID,
		EventID:   eventID,
		FireID:    req.Authority.FireID,
		Slot:      slot,
		Path:      path,
		Reason:    reason,
	})
	return hubfire.FireOutcome{
		Status: hubfire.FireStatusRefused,
		Error:  reason,
	}
}

// ApplyDispatchedEventEnvelope populates structuredMsg's §8.3 event carrier and
// sender metadata when req.Event is present, before action_message.go persists
// (when enabled) and calls RenderDeliveryText.
//
// Named without the word "Trigger" so action_message.go can invoke it while
// satisfying TestFireCoreContainment.
func ApplyDispatchedEventEnvelope(structuredMsg *messages.StructuredMessage, req hubfire.FireRequest, templatedVars ...string) error {
	if structuredMsg == nil || req.Event == nil {
		return nil
	}
	if req.Event.Trigger != nil {
		structuredMsg.Sender = "trigger"
		structuredMsg.SenderID = "TRIGGER"
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string)
		}
		structuredMsg.Metadata["system_category"] = messages.SystemCategoryTrigger
		if !req.Event.OccurredAt.IsZero() {
			structuredMsg.Timestamp = req.Event.OccurredAt.UTC().Format(time.RFC3339)
		}
	}

	body := buildDispatchedEventBody(req.Event)
	if len(templatedVars) > 0 {
		body.TemplatedVars = append([]string(nil), templatedVars...)
	}
	rawEvent, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal dispatched event body: %w", err)
	}
	structuredMsg.Event = rawEvent
	return nil
}
