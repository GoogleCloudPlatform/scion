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

// Notify-on-failure constants (Stage 2 §10, shared seam for ptone/scion#7895).
const (
	// DefaultFireNotifyCooldown is the default fixed-window bucket duration per
	// (definition, sink, outcome, reason) preventing flapping targets from
	// flooding notifications.
	DefaultFireNotifyCooldown = time.Hour

	// FireNotifyEventType is the synthetic event type carried on agent-message
	// failure notifications.
	FireNotifyEventType = "trigger.fire_failed"

	// FireNotifySinkUser and FireNotifySinkAgent name the two cooldown-tracked
	// notification sinks.
	FireNotifySinkUser  = "user_notice"
	FireNotifySinkAgent = "agent_message"

	// ExecutorKindTriggerNotify identifies the notify-on-failure agent-message
	// sink in ExecutorContext (catalog entry point sendAgentFireNotification:message).
	ExecutorKindTriggerNotify = "trigger_notify"

	maxFireNotifyReasonBytes = 120
	fireNotifyTimeout        = 30 * time.Second
)

// triggerNotifyEventNamespace is a fixed UUIDv5 namespace used to derive
// deterministic event_id values for notify trigger_fires rows.
var triggerNotifyEventNamespace = uuid.MustParse("c3b2d9a4-7e1f-4c8b-9a5d-2e6f8a1b4c90")

// FireNotifyPolicy configures hub-authored failure notifications for deferred
// fires (Stage 2 §10). Currently implemented for trigger definitions only;
// ptone/scion#7895 adds schedule sinks on the same seam.
//
// Cooldown semantics (durable fixed window):
//   - Each sink claims a durable trigger_fires row keyed on the existing unique
//     (event_id, trigger_id, action_index) constraint with reserved per-sink
//     action_index values (TriggerFireActionIndexNotify = -1 for agent_message,
//     TriggerFireActionIndexNotifyUser = -2 for user_notice) and
//     event_id = UUIDv5(trigger_id, sink, outcome, reason, floor(now/cooldown)).
//   - This enforces at most one notice per sink, outcome, and reason per hour
//     bucket across replicas and hub restarts without any schema change. Because
//     it is a fixed window, two notices for the same key may land close together
//     across a bucket boundary (e.g., 10:59 and 11:01).
//
// Security inventory (Stage 2 §18 S24):
//   - No payload text: buildFireNotifyMessage formats only the validated
//     definition name, closed-set outcome status, and sanitized generic reason;
//     synthetic trigger.fire_failed events carry {trigger, fire_id, outcome,
//     reason} in attributes and never attach external_payload.
//   - Authority: authoring validates AgentID/AgentName via messageAction.Validate
//     on create, update, and resume; every agent notification executes through
//     fireAction under req.Authority (live authz, standing, frozen ceiling).
//     User notices check live trigger.read on the project before delivery.
//   - No recursion: NotifyFireOutcome rejects ActionType=="notify",
//     Event.Type=="trigger.fire_failed", and negative notify ActionIndex values;
//     the agent sink calls fireAction directly and never re-enters NotifyFireOutcome.
//   - No wake: agent notifications set Wake=false, Offline="skip", and
//     FireRequest.NoWake=true (suppressing auto-wake too).
//   - Outside WithTx: all claim, authz, user-notice, and agent-message I/O runs
//     strictly after the fire outcome transaction has committed.
type FireNotifyPolicy struct {
	OnOutcomes []hubfire.FireStatus // subset of {failed, refused, expired, skipped}; default {failed, refused, expired}
	UserNotice bool                 // notify the definition's creator (user, or agent creator's delegating user)
	AgentID    string               // optional: message this in-project agent under the definition's RecordedAuthority
	AgentName  string               // display name of AgentID (optional)
	Cooldown   time.Duration        // fixed-window bucket per (definition, sink, outcome, reason): 1 h default
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

// TriggerNotifyDerivedEventID returns the deterministic UUIDv5 event_id for a
// single (fire_id, "notify") pair.
func TriggerNotifyDerivedEventID(fireID string) string {
	return uuid.NewSHA1(triggerNotifyEventNamespace, []byte(fireID+"\x00notify")).String()
}

// fireNotifyCooldownWindow returns the fixed-window bucket index
// floor(now / cooldown) in UTC.
func fireNotifyCooldownWindow(now time.Time, cooldown time.Duration) int64 {
	if cooldown <= 0 {
		cooldown = DefaultFireNotifyCooldown
	}
	utc := now.UTC()
	if cooldown >= time.Second {
		if sec := int64(cooldown / time.Second); sec > 0 {
			u := utc.Unix()
			if u >= 0 {
				return u / sec
			}
			return (u - sec + 1) / sec
		}
	}
	ns := int64(cooldown)
	un := utc.UnixNano()
	if un >= 0 {
		return un / ns
	}
	return (un - ns + 1) / ns
}

// TriggerNotifyCooldownEventID returns the deterministic UUIDv5 event_id used
// to atomically claim a durable fixed-window cooldown slot in trigger_fires
// via the unique (event_id, trigger_id, action_index) index.
//
// Fixed-window semantics: at most one notice per (trigger, sink, outcome,
// reason) per hour bucket (floor(now / cooldown)); two notices for the same
// key may land close together across a bucket boundary.
func TriggerNotifyCooldownEventID(triggerID, sink, outcome, reason string, window int64) string {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", triggerID, sink, outcome, reason, window)
	return uuid.NewSHA1(triggerNotifyEventNamespace, []byte(key)).String()
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
// dispatches enabled sinks (user notice and/or agent message) subject to a
// durable fixed-window cooldown per (definition, sink, outcome, reason).
// Currently implemented for trigger definitions only; ptone/scion#7895 adds
// schedule sinks on the same seam. Must be called strictly OUTSIDE any WithTx.
func (s *Server) NotifyFireOutcome(ctx context.Context, req hubfire.FireRequest, out hubfire.FireOutcome, policy FireNotifyPolicy) {
	s.notifyFireOutcome(ctx, req, out, policy)
}

func isTriggerNotifyActionIndex(idx int) bool {
	return idx == store.TriggerFireActionIndexNotify || idx == store.TriggerFireActionIndexNotifyUser
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
		if req.Event.Trigger != nil && isTriggerNotifyActionIndex(req.Event.Trigger.ActionIndex) {
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
	if defKind != hubfire.DefinitionKindTrigger {
		slog.DebugContext(ctx, "fire notify: unsupported definition kind; skipping notification (trigger definitions only; ptone/scion#7895 adds schedule sinks)",
			"definition_kind", defKind, "definition_id", defID)
		return
	}

	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = DefaultFireNotifyCooldown
	}

	now := s.fireNotifyNow()
	window := fireNotifyCooldownWindow(now, cooldown)
	reason := sanitizeFireNotifyReason(out.Status, out.Error)

	if policy.UserNotice {
		s.sendUserFireNotification(ctx, req, out, defKind, defID, reason, now, window)
	}
	if strings.TrimSpace(policy.AgentID) != "" {
		s.sendAgentFireNotification(ctx, req, out, policy, defKind, defID, reason, now, window)
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

func (s *Server) resolveFireDefinitionName(ctx context.Context, defKind, defID string, req hubfire.FireRequest) string {
	if req.Event != nil {
		if req.Event.Trigger != nil && req.Event.Trigger.Name != "" {
			return req.Event.Trigger.Name
		}
		if req.Event.Schedule != nil && req.Event.Schedule.Name != "" {
			return req.Event.Schedule.Name
		}
	}
	switch defKind {
	case hubfire.DefinitionKindTrigger:
		if tr, err := s.store.GetTrigger(ctx, defID); err == nil && tr != nil && tr.Name != "" {
			return tr.Name
		}
	case hubfire.DefinitionKindSchedule:
		if sched, err := s.store.GetSchedule(ctx, defID); err == nil && sched != nil && sched.Name != "" {
			return sched.Name
		}
	}
	return defID
}

// sanitizeFireNotifyReason produces a bounded, control-char-free generic reason
// string for failure notifications, truncating cleanly on a UTF-8 rune
// boundary. It never inspects event payloads.
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
		cleaned = strings.TrimSpace(truncateUTF8Bytes(cleaned, maxFireNotifyReasonBytes))
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
	out hubfire.FireOutcome,
	defKind, defID, reason string,
	now time.Time,
	window int64,
) {
	origFireID := req.Authority.FireID
	if origFireID == "" {
		origFireID = api.NewUUID()
	}

	// Claim durable fixed-window cooldown slot FIRST, before any recipient
	// lookup, authz check, or notification write (R1/R2/O1).
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), fireNotifyTimeout)
	defer cancelWrite()

	cooldownEventID := TriggerNotifyCooldownEventID(defID, FireNotifySinkUser, out.Status.String(), reason, window)
	notifyFire := store.TriggerFire{
		ID:                   api.NewUUID(),
		ProjectID:            req.ProjectID,
		TriggerID:            defID,
		EventID:              cooldownEventID,
		Subject:              "notify-user:" + origFireID,
		ActionType:           store.TriggerActionNotify,
		ActionIndex:          store.TriggerFireActionIndexNotifyUser,
		Status:               store.TriggerFireStatusFiring,
		ExecutorKind:         ExecutorKindTriggerNotify,
		ExecutorID:           trigger.ExecutorIDPrefixTriggerFire + origFireID,
		LeaseUntil:           now.Add(trigger.FireLeaseHorizon),
		StartedAt:            now,
		CreatedAt:            now,
		InitiatorAttribution: req.Authority.Attribution,
	}
	claimed, _, err := s.store.ClaimTriggerFire(writeCtx, &notifyFire)
	if err != nil {
		slog.WarnContext(ctx, "trigger notify: failed to claim user-notice cooldown row",
			"trigger_id", defID, "fire_id", origFireID, "error", err)
		return
	}
	if !claimed {
		return
	}

	principalID := req.Authority.Attribution.InitiatorPrincipalID
	if principalID == "" {
		principalID = fireHistoryCreatorFromContext(ctx)
	}
	if principalID == "" {
		if tr, err := s.store.GetTrigger(writeCtx, defID); err == nil && tr != nil {
			principalID = tr.CreatedBy
		}
	}
	recipient := s.fireNotificationRecipientUser(writeCtx, principalID, "trigger")
	if recipient == nil {
		slog.WarnContext(ctx, "trigger notify: no owning user to notify; skipping user notice",
			"trigger_id", defID, "fire_id", origFireID, "principal_id", principalID)
		_, _ = s.store.FinishTriggerFire(writeCtx, notifyFire.ID, store.TriggerFireStatusSkipped, "", "", "no owning user to notify", s.fireNotifyNow())
		return
	}
	// O4: verify recipient still holds trigger.read on the project (outside any tx).
	if s.authzService != nil && req.ProjectID != "" {
		userIdent := NewAuthenticatedUser(recipient.ID, recipient.Email, recipient.DisplayName, recipient.Role, "notify")
		decision := s.authzService.Decide(writeCtx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   triggerResource(req.ProjectID),
			Action:     ActionRead,
			Permission: "trigger.read",
		})
		if !decision.Allowed {
			slog.InfoContext(ctx, "trigger notify: recipient no longer has trigger.read on project; skipping user notice",
				"trigger_id", defID, "fire_id", origFireID, "recipient", recipient.ID, "project_id", req.ProjectID)
			_, _ = s.store.FinishTriggerFire(writeCtx, notifyFire.ID, store.TriggerFireStatusSkipped, "", "", "recipient lacks project trigger.read", s.fireNotifyNow())
			return
		}
	}

	defName := s.resolveFireDefinitionName(writeCtx, defKind, defID, req)
	msg := buildFireNotifyMessage(defKind, defID, defName, req.ProjectID, req.Authority.FireID, out.Status, reason)

	err = s.writeUserNotification(writeCtx, userNotice{
		Kind:             store.NotificationKindTriggerFireFailed,
		ResourceType:     store.NotificationResourceTrigger,
		ResourceID:       defID,
		Status:           store.NotificationStatusTriggerFireFailed,
		ProjectID:        req.ProjectID,
		SubscriberUserID: recipient.ID,
		Message:          msg,
	})
	if err != nil {
		slog.WarnContext(ctx, "trigger notify: failed to write user notification",
			"trigger_id", defID, "fire_id", origFireID, "recipient", recipient.ID, "error", err)
		_, _ = s.store.FinishTriggerFire(writeCtx, notifyFire.ID, store.TriggerFireStatusFailed, "", "", "user notification write failed", s.fireNotifyNow())
		return
	}
	_, _ = s.store.FinishTriggerFire(writeCtx, notifyFire.ID, store.TriggerFireStatusFired, "", "", "", s.fireNotifyNow())
}

func (s *Server) sendAgentFireNotification(
	ctx context.Context,
	req hubfire.FireRequest,
	out hubfire.FireOutcome,
	policy FireNotifyPolicy,
	defKind, defID, reason string,
	now time.Time,
	window int64,
) {
	origFireID := req.Authority.FireID
	if origFireID == "" {
		origFireID = api.NewUUID()
	}

	// Claim durable fixed-window cooldown slot FIRST via the unique
	// (event_id, trigger_id, action_index=-1) index (R1/R2/O1).
	cooldownEventID := TriggerNotifyCooldownEventID(defID, FireNotifySinkAgent, out.Status.String(), reason, window)
	notifyFireID := api.NewUUID()

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), fireNotifyTimeout)
	defer cancelWrite()

	notifyFire := store.TriggerFire{
		ID:                   notifyFireID,
		ProjectID:            req.ProjectID,
		TriggerID:            defID,
		EventID:              cooldownEventID,
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
		slog.WarnContext(ctx, "trigger notify: failed to claim notify fire row",
			"trigger_id", defID, "fire_id", origFireID, "error", err)
		return
	}
	if !claimed {
		// Another pass or replica already claimed this (trigger, sink,
		// outcome, reason, window) bucket.
		return
	}

	defName := s.resolveFireDefinitionName(writeCtx, defKind, defID, req)
	msg := buildFireNotifyMessage(defKind, defID, defName, req.ProjectID, req.Authority.FireID, out.Status, reason)

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
			ID:         TriggerNotifyDerivedEventID(origFireID),
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

	// Execute through fireAction directly (catalog entry point sendAgentFireNotification:message)
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
// the user ID its notifications go to: the user itself, or for an agent creator
// the agent's delegating user (its ancestry root, else its owner) when that is
// a user row. Returns "" when there is none. Used only to address
// notifications, never for authority (Stage 2 §10).
func (s *Server) fireNotificationRecipient(ctx context.Context, createdBy, kindLabel string) string {
	u := s.fireNotificationRecipientUser(ctx, createdBy, kindLabel)
	if u == nil {
		return ""
	}
	return u.ID
}

func (s *Server) fireNotificationRecipientUser(ctx context.Context, createdBy, kindLabel string) *store.User {
	if createdBy == "" || s == nil || s.store == nil {
		return nil
	}
	if u, err := s.store.GetUser(ctx, createdBy); err == nil && u != nil {
		return u
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		if kindLabel == "schedule" {
			slog.WarnContext(ctx, "Scheduler: looking up the schedule creator as a user failed; skipping notification",
				"subsystem", "scheduler", "createdBy", createdBy, "error", err)
		} else {
			if kindLabel == "" {
				kindLabel = "definition"
			}
			slog.WarnContext(ctx, "looking up the "+kindLabel+" creator as a user failed; skipping notification",
				"createdBy", createdBy, "error", err)
		}
		return nil
	}
	agent, err := s.store.GetAgent(ctx, createdBy)
	if err != nil {
		return nil
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
		if u, err := s.store.GetUser(ctx, id); err == nil && u != nil {
			return u
		}
	}
	return nil
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
	if fire.ActionType == store.TriggerActionNotify || isTriggerNotifyActionIndex(fire.ActionIndex) {
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
