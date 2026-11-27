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
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Notify-on-failure constants (Stage 2 §10, shared with ptone/scion#7895).
const (
	// DefaultFireNotifyCooldown is the default per-(definition, sink) cooldown
	// preventing flapping targets from flooding notifications.
	DefaultFireNotifyCooldown = time.Hour

	// FireNotifyEventType is the synthetic event type carried on agent-message
	// failure notifications.
	FireNotifyEventType = "trigger.fire_failed"

	// FireNotifySinkUser and FireNotifySinkAgent name the two cooldown-tracked
	// notification sinks.
	FireNotifySinkUser  = "user_notice"
	FireNotifySinkAgent = "agent_message"

	// ExecutorKindTriggerNotify identifies the notify-on-failure agent-message
	// sink in ExecutorContext (catalog entry point triggerNotify:message).
	ExecutorKindTriggerNotify = "trigger_notify"

	maxFireNotifyReasonBytes   = 120
	maxFireNotifyCooldownEntry = 2000
	fireNotifyTimeout          = 30 * time.Second
)

// triggerNotifyEventNamespace is a fixed UUIDv5 namespace used to derive a
// deterministic, idempotent event_id for (fire_id, "notify") trigger_fires rows.
var triggerNotifyEventNamespace = uuid.MustParse("c3b2d9a4-7e1f-4c8b-9a5d-2e6f8a1b4c90")

// FireNotifyPolicy configures hub-authored failure notifications for deferred
// fires (Stage 2 §10). Exported so schedules (ptone/scion#7895) can reuse the
// exact same seam and sinks.
//
// Security inventory (Stage 2 §18 S24):
//   - No payload text: buildFireNotifyMessage formats only the validated
//     definition name, closed-set outcome status, and sanitized generic reason;
//     synthetic trigger.fire_failed events carry {trigger, fire_id, outcome,
//     reason} in attributes and never attach external_payload.
//   - Authority: authoring validates AgentID/AgentName via messageAction.Validate
//     on create, update, and resume; every agent notification executes through
//     fireAction under req.Authority (live authz, standing, frozen ceiling).
//   - No recursion: NotifyFireOutcome rejects ActionType=="notify",
//     Event.Type=="trigger.fire_failed", and ActionIndex==TriggerFireActionIndexNotify;
//     the agent sink calls fireAction directly and never re-enters NotifyFireOutcome.
//   - No wake: agent notifications set Wake=false, Offline="skip", and
//     FireRequest.NoWake=true (suppressing auto-wake too).
//   - Outside WithTx: all user-notice and agent-message I/O runs strictly after
//     the fire outcome transaction has committed.
type FireNotifyPolicy struct {
	OnOutcomes []hubfire.FireStatus // subset of {failed, refused, expired, skipped}; default {failed, refused, expired}
	UserNotice bool                 // notify the definition's creator (user, or agent creator's delegating user)
	AgentID    string               // optional: message this in-project agent under the definition's RecordedAuthority
	AgentName  string               // display name of AgentID (optional)
	Cooldown   time.Duration        // per (definition, sink): 1 h default
}

type fireNotifyCooldownKey struct {
	definitionKind string
	definitionID   string
	sink           string
}

// DefaultFireNotifyOutcomes returns a fresh slice of the default notify
// outcomes: failed, refused, expired (Stage 2 §10).
func DefaultFireNotifyOutcomes() []hubfire.FireStatus {
	return []hubfire.FireStatus{
		hubfire.FireStatusFailed,
		hubfire.FireStatusRefused,
		hubfire.FireStatusExpired,
	}
}

// TriggerNotifyDerivedEventID returns the deterministic UUIDv5 event_id used to
// record an idempotent (fire_id, "notify") trigger_fires row.
func TriggerNotifyDerivedEventID(fireID string) string {
	return uuid.NewSHA1(triggerNotifyEventNamespace, []byte(fireID+"\x00notify")).String()
}

// SetFireNotifyClock overrides the clock used for notify-on-failure cooldowns
// and timestamps (intended for deterministic tests). Passing nil restores
// time.Now.
func (s *Server) SetFireNotifyClock(fn func() time.Time) {
	if s == nil {
		return
	}
	s.triggers.mu.Lock()
	s.triggers.notifyNow = fn
	s.triggers.mu.Unlock()
}

func (s *Server) fireNotifyNow() time.Time {
	if s != nil {
		s.triggers.mu.Lock()
		fn := s.triggers.notifyNow
		s.triggers.mu.Unlock()
		if fn != nil {
			return fn().UTC()
		}
	}
	return time.Now().UTC()
}

// NotifyFireOutcome evaluates policy for a persisted terminal fire outcome and
// dispatches enabled sinks (user notice and/or agent message) subject to
// per-(definition, sink) cooldown. Must be called strictly OUTSIDE any WithTx.
func (s *Server) NotifyFireOutcome(ctx context.Context, req hubfire.FireRequest, out hubfire.FireOutcome, policy FireNotifyPolicy) {
	s.notifyFireOutcome(ctx, req, out, policy)
}

func (s *Server) notifyFireOutcome(ctx context.Context, req hubfire.FireRequest, out hubfire.FireOutcome, policy FireNotifyPolicy) {
	if s == nil || s.store == nil {
		return
	}
	// Never notify about a notification (defense-in-depth against recursion).
	if req.ActionType == store.TriggerActionNotify {
		return
	}
	if req.Event != nil {
		if req.Event.Type == FireNotifyEventType {
			return
		}
		if req.Event.Trigger != nil && req.Event.Trigger.ActionIndex == store.TriggerFireActionIndexNotify {
			return
		}
	}
	if !shouldNotifyFireOutcome(out, policy) {
		return
	}
	if !policy.UserNotice && strings.TrimSpace(policy.AgentID) == "" {
		return
	}

	defKind := req.Authority.DefinitionKind
	defID := req.Authority.DefinitionID
	if defID == "" && req.Event != nil && req.Event.Trigger != nil {
		defID = req.Event.Trigger.ID
		if defKind == "" {
			defKind = hubfire.DefinitionKindTrigger
		}
	}
	if defID == "" {
		return
	}
	if defKind == "" {
		defKind = hubfire.DefinitionKindTrigger
	}

	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = DefaultFireNotifyCooldown
	}

	defName := s.resolveFireDefinitionName(ctx, defKind, defID, req)
	reason := sanitizeFireNotifyReason(out.Status, out.Error)
	msg := buildFireNotifyMessage(defKind, defID, defName, req.ProjectID, req.Authority.FireID, out.Status, reason)

	if policy.UserNotice {
		s.sendUserFireNotification(ctx, req, defKind, defID, msg, cooldown)
	}
	if strings.TrimSpace(policy.AgentID) != "" {
		s.sendAgentFireNotification(ctx, req, out, policy, defKind, defID, defName, reason, msg, cooldown)
	}
}

func shouldNotifyFireOutcome(out hubfire.FireOutcome, policy FireNotifyPolicy) bool {
	// Stage 2 §7.4 + §10: project live trigger-agent cap skips always route to
	// notify (once per hour per trigger) even when skipped is not opted in.
	if out.Status == hubfire.FireStatusSkipped && strings.TrimSpace(out.Error) == trigger.ErrFireProjectTriggerAgentCap {
		return true
	}
	switch out.Status {
	case hubfire.FireStatusFailed, hubfire.FireStatusRefused, hubfire.FireStatusExpired, hubfire.FireStatusSkipped:
	default:
		return false
	}
	outcomes := policy.OnOutcomes
	if len(outcomes) == 0 {
		outcomes = DefaultFireNotifyOutcomes()
	}
	for _, want := range outcomes {
		if want == out.Status {
			return true
		}
	}
	return false
}

func (s *Server) tryClaimFireNotifyCooldown(defKind, defID, sink string, cooldown time.Duration) (time.Time, bool) {
	now := s.fireNotifyNow()
	key := fireNotifyCooldownKey{
		definitionKind: defKind,
		definitionID:   defID,
		sink:           sink,
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.fireNotifyCooldown == nil {
		rt.fireNotifyCooldown = make(map[fireNotifyCooldownKey]time.Time)
	}
	if last, ok := rt.fireNotifyCooldown[key]; ok && now.Sub(last) < cooldown {
		return now, false
	}
	if len(rt.fireNotifyCooldown) >= maxFireNotifyCooldownEntry {
		for k, ts := range rt.fireNotifyCooldown {
			if now.Sub(ts) >= cooldown {
				delete(rt.fireNotifyCooldown, k)
			}
		}
	}
	rt.fireNotifyCooldown[key] = now
	return now, true
}

func (s *Server) releaseFireNotifyCooldown(defKind, defID, sink string, claimedAt time.Time) {
	key := fireNotifyCooldownKey{
		definitionKind: defKind,
		definitionID:   defID,
		sink:           sink,
	}
	rt := &s.triggers
	rt.mu.Lock()
	if cur, ok := rt.fireNotifyCooldown[key]; ok && cur.Equal(claimedAt) {
		delete(rt.fireNotifyCooldown, key)
	}
	rt.mu.Unlock()
}

func (s *Server) resolveFireDefinitionName(ctx context.Context, defKind, defID string, req hubfire.FireRequest) string {
	if req.Event != nil {
		if req.Event.Trigger != nil && req.Event.Trigger.Name != "" {
			return req.Event.Trigger.Name
		}
		if req.Event.Schedule != nil && req.Event.Schedule.Name != "" {
			return req.Event.Schedule.Name
		}
	}
	if defKind == hubfire.DefinitionKindTrigger {
		if tr, err := s.store.GetTrigger(ctx, defID); err == nil && tr != nil && tr.Name != "" {
			return tr.Name
		}
	} else if defKind == hubfire.DefinitionKindSchedule {
		if sched, err := s.store.GetSchedule(ctx, defID); err == nil && sched != nil && sched.Name != "" {
			return sched.Name
		}
	}
	return defID
}

// sanitizeFireNotifyReason produces a bounded, control-char-free generic reason
// string for failure notifications. It never inspects event payloads.
func sanitizeFireNotifyReason(status hubfire.FireStatus, raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(raw))
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return "fire " + status.String()
	}
	if len(cleaned) > maxFireNotifyReasonBytes {
		cleaned = strings.TrimSpace(cleaned[:maxFireNotifyReasonBytes])
	}
	return cleaned
}

func buildFireNotifyMessage(defKind, defID, defName, projectID, fireID string, status hubfire.FireStatus, reason string) string {
	label := "Trigger"
	if defKind == hubfire.DefinitionKindSchedule || defKind == hubfire.DefinitionKindScheduledEvent {
		label = "Schedule"
	}
	base := fmt.Sprintf("%s %q %s: %s", label, defName, status.String(), reason)
	if defKind == hubfire.DefinitionKindTrigger && projectID != "" && defID != "" {
		if fireID != "" {
			return fmt.Sprintf("%s (fire %s, /projects/%s/triggers?tab=history&triggerId=%s)", base, fireID, projectID, defID)
		}
		return fmt.Sprintf("%s (/projects/%s/triggers?tab=history&triggerId=%s)", base, projectID, defID)
	}
	if fireID != "" {
		return fmt.Sprintf("%s (fire %s)", base, fireID)
	}
	return base
}

func (s *Server) sendUserFireNotification(
	ctx context.Context,
	req hubfire.FireRequest,
	defKind, defID, msg string,
	cooldown time.Duration,
) {
	if defKind != hubfire.DefinitionKindTrigger {
		return
	}
	principalID := req.Authority.Attribution.InitiatorPrincipalID
	if principalID == "" {
		principalID = fireHistoryCreatorFromContext(ctx)
	}
	if principalID == "" {
		if tr, err := s.store.GetTrigger(ctx, defID); err == nil && tr != nil {
			principalID = tr.CreatedBy
		}
	}
	recipient := s.fireNotificationRecipient(ctx, principalID)
	if recipient == "" {
		slog.WarnContext(ctx, "trigger notify: no owning user to notify; skipping user notice",
			"trigger_id", defID, "fire_id", req.Authority.FireID, "principal_id", principalID)
		return
	}
	claimedAt, ok := s.tryClaimFireNotifyCooldown(defKind, defID, FireNotifySinkUser, cooldown)
	if !ok {
		return
	}
	err := s.writeUserNotification(ctx, userNotice{
		Kind:             store.NotificationKindTriggerFireFailed,
		ResourceType:     store.NotificationResourceTrigger,
		ResourceID:       defID,
		Status:           store.NotificationStatusTriggerFireFailed,
		ProjectID:        req.ProjectID,
		SubscriberUserID: recipient,
		Message:          msg,
	})
	if err != nil {
		s.releaseFireNotifyCooldown(defKind, defID, FireNotifySinkUser, claimedAt)
		slog.WarnContext(ctx, "trigger notify: failed to write user notification",
			"trigger_id", defID, "fire_id", req.Authority.FireID, "recipient", recipient, "error", err)
	}
}

func (s *Server) sendAgentFireNotification(
	ctx context.Context,
	req hubfire.FireRequest,
	out hubfire.FireOutcome,
	policy FireNotifyPolicy,
	defKind, defID, defName, reason, msg string,
	cooldown time.Duration,
) {
	if defKind != hubfire.DefinitionKindTrigger {
		return
	}
	origFireID := req.Authority.FireID
	if origFireID == "" {
		origFireID = api.NewUUID()
	}

	claimedAt, ok := s.tryClaimFireNotifyCooldown(defKind, defID, FireNotifySinkAgent, cooldown)
	if !ok {
		return
	}

	now := s.fireNotifyNow()
	derivedEventID := TriggerNotifyDerivedEventID(origFireID)
	notifyFireID := api.NewUUID()

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), fireNotifyTimeout)
	defer cancelWrite()

	notifyFire := store.TriggerFire{
		ID:                   notifyFireID,
		ProjectID:            req.ProjectID,
		TriggerID:            defID,
		EventID:              derivedEventID,
		Subject:              "notify:" + origFireID,
		ActionType:           store.TriggerActionNotify,
		ActionIndex:          store.TriggerFireActionIndexNotify,
		Status:               store.TriggerFireStatusFiring,
		TargetAgentID:        policy.AgentID,
		ExecutorKind:         ExecutorKindTriggerNotify,
		ExecutorID:           trigger.ExecutorIDPrefixTriggerFire + origFireID,
		LeaseUntil:           now.Add(trigger.FireLeaseHorizon),
		StartedAt:            now,
		CreatedAt:            now,
		InitiatorAttribution: req.Authority.Attribution,
	}
	claimed, _, err := s.store.ClaimTriggerFire(writeCtx, &notifyFire)
	if err != nil {
		s.releaseFireNotifyCooldown(defKind, defID, FireNotifySinkAgent, claimedAt)
		slog.WarnContext(ctx, "trigger notify: failed to claim notify fire row",
			"trigger_id", defID, "fire_id", origFireID, "error", err)
		return
	}
	if !claimed {
		// Idempotent per (fire_id, "notify"): another pass or replica already
		// claimed this notification fire row.
		return
	}

	rawPayload, err := json.Marshal(MessageEventPayload{
		AgentID:   policy.AgentID,
		AgentName: policy.AgentName,
		Message:   msg,
		Wake:      false,
		Offline:   MessageOfflineSkip,
	})
	if err != nil {
		_, _ = s.store.FinishTriggerFire(writeCtx, notifyFire.ID, store.TriggerFireStatusFailed, "", policy.AgentID, "message failed", s.fireNotifyNow())
		return
	}

	notifyAuthority := req.Authority
	notifyAuthority.FireID = notifyFire.ID
	notifyReq := hubfire.FireRequest{
		ProjectID:  req.ProjectID,
		Authority:  notifyAuthority,
		ActionType: store.TriggerActionMessage,
		Payload:    string(rawPayload),
		NoWake:     true,
		Event: &hubfire.FireEvent{
			ID:         derivedEventID,
			ProjectID:  req.ProjectID,
			SourceKind: store.TriggerActionNotify,
			SourceID:   defID,
			Type:       FireNotifyEventType,
			Subject:    "notify:" + origFireID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{
				"trigger": defID,
				"fire_id": origFireID,
				"outcome": out.Status.String(),
				"reason":  reason,
			},
			Trigger: &hubfire.FireTriggerRef{
				ID:          defID,
				Name:        defName,
				ActionIndex: store.TriggerFireActionIndexNotify,
			},
		},
		Executor: hubfire.ExecutorContext{
			Kind: ExecutorKindTriggerNotify,
			ID:   trigger.ExecutorIDPrefixTriggerFire + origFireID,
		},
		FireAt:   now,
		Attempts: 1,
	}

	// Execute through fireAction directly (catalog entry point triggerNotify:message)
	// so live RecordedAuthority, standing, frozen ceiling, and target message
	// authorization run without re-entering the trigger dispatcher or rate limits.
	notifyOut, action, rawErr := s.fireActionWithError(writeCtx, notifyReq)
	statusStr := normalizeNotifyFireStatus(notifyOut.Status, notifyOut.Error)
	errStr := notifyOut.Error
	if notifyOut.Status == hubfire.FireStatusFailed && errStr != "" {
		errStr = sanitizeTriggerFireError(writeCtx, action, notifyReq, notifyOut.Status, errStr, rawErr)
	}
	targetID := notifyOut.TargetAgentID
	if targetID == "" {
		targetID = policy.AgentID
	}
	finishedAt := s.fireNotifyNow()
	if _, fErr := s.store.FinishTriggerFire(writeCtx, notifyFire.ID, statusStr, notifyOut.ResultRef, targetID, errStr, finishedAt); fErr != nil {
		slog.WarnContext(ctx, "trigger notify: failed to finish notify fire row",
			"trigger_id", defID, "fire_id", origFireID, "notify_fire_id", notifyFire.ID, "error", fErr)
	}
}

func normalizeNotifyFireStatus(st hubfire.FireStatus, errMsg string) string {
	switch st {
	case hubfire.FireStatusFired:
		return store.TriggerFireStatusFired
	case hubfire.FireStatusFailed:
		return store.TriggerFireStatusFailed
	case hubfire.FireStatusRefused:
		return store.TriggerFireStatusRefused
	case hubfire.FireStatusSkipped:
		return store.TriggerFireStatusSkipped
	case hubfire.FireStatusExpired:
		return store.TriggerFireStatusExpired
	default:
		if errMsg != "" {
			return store.TriggerFireStatusFailed
		}
		return store.TriggerFireStatusFired
	}
}

// fireNotificationRecipient resolves a definition's creator/initiator ID to
// the user its notifications go to: the user itself, or for an agent creator
// the agent's delegating user (its ancestry root, else its owner) when that is
// a user row. Returns "" when there is none. Used only to address
// notifications, never for authority (Stage 2 §10).
func (s *Server) fireNotificationRecipient(ctx context.Context, createdBy string) string {
	if createdBy == "" || s == nil || s.store == nil {
		return ""
	}
	if _, err := s.store.GetUser(ctx, createdBy); err == nil {
		return createdBy
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "looking up definition creator as a user failed; skipping notification",
			"createdBy", createdBy, "error", err)
		return ""
	}
	agent, err := s.store.GetAgent(ctx, createdBy)
	if err != nil {
		return ""
	}
	candidates := []string{}
	if len(agent.Ancestry) > 0 {
		candidates = append(candidates, agent.Ancestry[0])
	}
	candidates = append(candidates, agent.OwnerID)
	for _, id := range candidates {
		if id == "" || id == agent.ID {
			continue
		}
		if _, err := s.store.GetUser(ctx, id); err == nil {
			return id
		}
	}
	return ""
}

// effectiveTriggerNotifyPolicy converts a trigger's stored Notify policy into
// the runtime FireNotifyPolicy, applying Stage 2 §10 defaults (userNotice=true,
// onOutcomes={failed, refused, expired}, cooldown=1h).
func effectiveTriggerNotifyPolicy(tr *store.Trigger) FireNotifyPolicy {
	policy := FireNotifyPolicy{
		OnOutcomes: DefaultFireNotifyOutcomes(),
		UserNotice: true,
		Cooldown:   DefaultFireNotifyCooldown,
	}
	if tr == nil || tr.Notify == nil {
		return policy
	}
	if len(tr.Notify.OnOutcomes) > 0 {
		outcomes := make([]hubfire.FireStatus, 0, len(tr.Notify.OnOutcomes))
		for _, o := range tr.Notify.OnOutcomes {
			outcomes = append(outcomes, hubfire.FireStatus(o))
		}
		policy.OnOutcomes = outcomes
	}
	if tr.Notify.UserNotice != nil {
		policy.UserNotice = *tr.Notify.UserNotice
	}
	policy.AgentID = tr.Notify.AgentID
	policy.AgentName = tr.Notify.AgentName
	return policy
}

// notifyTriggerFire builds the FireRequest and FireNotifyPolicy for a persisted
// terminal trigger fire and invokes NotifyFireOutcome outside any transaction.
func (s *Server) notifyTriggerFire(ctx context.Context, tr *store.Trigger, fire *store.TriggerFire, out hubfire.FireOutcome) {
	if s == nil || s.store == nil || fire == nil {
		return
	}
	if fire.ActionType == store.TriggerActionNotify || fire.ActionIndex == store.TriggerFireActionIndexNotify {
		return
	}
	if tr == nil && fire.TriggerID != "" {
		loaded, err := s.store.GetTrigger(ctx, fire.TriggerID)
		if err != nil || loaded == nil {
			return
		}
		tr = loaded
	}
	if tr == nil {
		return
	}
	req := hubfire.FireRequest{
		ProjectID: tr.ProjectID,
		Authority: hubfire.RecordedAuthority{
			Attribution:    tr.InitiatorAttribution,
			Ceiling:        tr.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   tr.ID,
			FireID:         fire.ID,
		},
		ActionType: fire.ActionType,
		Event: &hubfire.FireEvent{
			ID:        fire.EventID,
			ProjectID: tr.ProjectID,
			Trigger: &hubfire.FireTriggerRef{
				ID:          tr.ID,
				Name:        tr.Name,
				ActionIndex: fire.ActionIndex,
			},
		},
	}
	ctx = withFireHistoryCreator(ctx, tr.CreatedBy)
	s.NotifyFireOutcome(ctx, req, out, effectiveTriggerNotifyPolicy(tr))
}

// notifyTriggerFireOutcomeByID loads a persisted trigger_fires row by ID and
// routes its terminal outcome through NotifyFireOutcome (used by detached exec
// runs that finish outside the dispatcher loop).
func (s *Server) notifyTriggerFireOutcomeByID(ctx context.Context, fireID, status, errMsg string) {
	if s == nil || s.store == nil || fireID == "" {
		return
	}
	fire, err := s.store.GetTriggerFire(ctx, fireID)
	if err != nil || fire == nil {
		return
	}
	s.notifyTriggerFire(ctx, nil, fire, hubfire.FireOutcome{
		Status: hubfire.FireStatus(status),
		Error:  errMsg,
	})
}
