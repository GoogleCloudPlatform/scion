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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// BrokerAuthEventType defines the type of broker authentication event.
type BrokerAuthEventType string

const (
	// BrokerAuthEventRegister is logged when a new broker is registered.
	BrokerAuthEventRegister BrokerAuthEventType = "register"
	// BrokerAuthEventDeregister is logged when a broker is deregistered.
	BrokerAuthEventDeregister BrokerAuthEventType = "deregister"
	// BrokerAuthEventJoin is logged when a broker completes join.
	BrokerAuthEventJoin BrokerAuthEventType = "join"
	// BrokerAuthEventAuthSuccess is logged when a broker successfully authenticates.
	BrokerAuthEventAuthSuccess BrokerAuthEventType = "auth_success"
	// BrokerAuthEventAuthFailure is logged when a broker fails to authenticate.
	BrokerAuthEventAuthFailure BrokerAuthEventType = "auth_failure"
	// BrokerAuthEventRotate is logged when a broker secret is rotated.
	BrokerAuthEventRotate BrokerAuthEventType = "rotate"
	// BrokerAuthEventRevoke is logged when a broker secret is revoked.
	BrokerAuthEventRevoke BrokerAuthEventType = "revoke"
	// BrokerAuthEventLink is logged when a broker is linked to a project.
	BrokerAuthEventLink BrokerAuthEventType = "link"
	// BrokerAuthEventUnlink is logged when a broker is unlinked from a project.
	BrokerAuthEventUnlink BrokerAuthEventType = "unlink"
)

// GCPTokenEventType defines the type of GCP token event.
type GCPTokenEventType string

const (
	GCPTokenEventAccessToken   GCPTokenEventType = "gcp_access_token"
	GCPTokenEventIdentityToken GCPTokenEventType = "gcp_identity_token"
	GCPTokenEventMintSA        GCPTokenEventType = "gcp_mint_service_account"
)

// GCPTokenEvent represents an auditable GCP token generation event.
type GCPTokenEvent struct {
	EventType           GCPTokenEventType `json:"eventType"`
	AgentID             string            `json:"agentId"`
	ProjectID           string            `json:"projectId"`
	ServiceAccountEmail string            `json:"serviceAccountEmail"`
	ServiceAccountID    string            `json:"serviceAccountId"`
	Success             bool              `json:"success"`
	FailReason          string            `json:"failReason,omitempty"`
	Timestamp           time.Time         `json:"timestamp"`
}

// BrokerAuthEvent represents an auditable event related to broker authentication.
type BrokerAuthEvent struct {
	EventType  BrokerAuthEventType `json:"eventType"`
	BrokerID   string              `json:"brokerId"`
	BrokerName string              `json:"brokerName,omitempty"`
	IPAddress  string              `json:"ipAddress,omitempty"`
	UserAgent  string              `json:"userAgent,omitempty"`
	Success    bool                `json:"success"`
	FailReason string              `json:"failReason,omitempty"`
	ActorID    string              `json:"actorId,omitempty"`   // User ID if admin action
	ActorType  string              `json:"actorType,omitempty"` // "user", "broker", or "system"
	Timestamp  time.Time           `json:"timestamp"`
	Details    map[string]string   `json:"details,omitempty"`
}

// InviteAuditEventType defines the type of invite/allow-list audit event.
type InviteAuditEventType string

const (
	InviteAuditAllowListAdd     InviteAuditEventType = "allow_list_add"
	InviteAuditAllowListRemove  InviteAuditEventType = "allow_list_remove"
	InviteAuditAllowListBulkAdd InviteAuditEventType = "allow_list_bulk_add"
	InviteAuditInviteCreated    InviteAuditEventType = "invite_created"
	InviteAuditInviteRedeemed   InviteAuditEventType = "invite_redeemed"
	InviteAuditInviteRevoked    InviteAuditEventType = "invite_revoked"
	InviteAuditInviteDeleted    InviteAuditEventType = "invite_deleted"
	InviteAuditLoginDenied      InviteAuditEventType = "login_denied"
	InviteAuditUserActivated    InviteAuditEventType = "user_activated"
	InviteAuditUserInvited      InviteAuditEventType = "user_invited"
	InviteAuditUserInvitedBulk  InviteAuditEventType = "user_invited_bulk"
)

// InviteAuditEvent represents an auditable event for the invite/allow-list system.
type InviteAuditEvent struct {
	EventType  InviteAuditEventType `json:"eventType"`
	Email      string               `json:"email,omitempty"`
	InviteID   string               `json:"inviteId,omitempty"`
	ActorID    string               `json:"actorId,omitempty"`
	ActorEmail string               `json:"actorEmail,omitempty"`
	Success    bool                 `json:"success"`
	FailReason string               `json:"failReason,omitempty"`
	Count      int                  `json:"count,omitempty"`
	Timestamp  time.Time            `json:"timestamp"`
	Details    map[string]string    `json:"details,omitempty"`
}

// ---------------------------------------------------------------------------
// Lifecycle Hook admin audit events
// ---------------------------------------------------------------------------

// LifecycleHookEventType defines the type of lifecycle-hook admin event.
type LifecycleHookEventType string

const (
	LifecycleHookEventCreate  LifecycleHookEventType = "lifecycle_hook_create"
	LifecycleHookEventUpdate  LifecycleHookEventType = "lifecycle_hook_update"
	LifecycleHookEventEnable  LifecycleHookEventType = "lifecycle_hook_enable"
	LifecycleHookEventDisable LifecycleHookEventType = "lifecycle_hook_disable"
	LifecycleHookEventDelete  LifecycleHookEventType = "lifecycle_hook_delete"
)

// LifecycleHookEvent represents an auditable lifecycle-hook admin event.
type LifecycleHookEvent struct {
	EventType  LifecycleHookEventType `json:"eventType"`
	HookID     string                 `json:"hookId"`
	HookName   string                 `json:"hookName"`
	Actor      string                 `json:"actor"`
	Success    bool                   `json:"success"`
	FailReason string                 `json:"failReason,omitempty"`
	Timestamp  time.Time              `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// Lifecycle Hook execution audit events (used by M5 evaluator)
// ---------------------------------------------------------------------------

// LifecycleHookExecutionEventType defines the type of lifecycle-hook execution event.
type LifecycleHookExecutionEventType string

const (
	LifecycleHookExecEventExecute LifecycleHookExecutionEventType = "lifecycle_hook_execute"
)

// LifecycleHookExecutionEvent represents an auditable lifecycle-hook execution event.
// Security: this event MUST NOT contain response bodies, rendered Authorization
// header values, or any secret material. Only request metadata (method, host,
// hook id) and outcome (status code, latency, error class) are recorded.
type LifecycleHookExecutionEvent struct {
	EventType         LifecycleHookExecutionEventType `json:"eventType"`
	HookID            string                          `json:"hookId"`
	HookName          string                          `json:"hookName"`
	Trigger           string                          `json:"trigger"`
	AgentID           string                          `json:"agentId"`
	ExecutionIdentity string                          `json:"executionIdentity"` // SA email or record ID
	ActionType        string                          `json:"actionType"`        // "http" | "webhook"
	Method            string                          `json:"method"`
	Host              string                          `json:"host"` // URL host only, not full URL (avoid leaking path tokens)
	Success           bool                            `json:"success"`
	HTTPStatusCode    int                             `json:"httpStatusCode,omitempty"`
	FailReason        string                          `json:"failReason,omitempty"`
	LatencyMs         int64                           `json:"latencyMs"`
	Attempt           int                             `json:"attempt"`
	Timestamp         time.Time                       `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// Agent Secret Read audit events
// ---------------------------------------------------------------------------

// AgentSecretReadEvent represents an auditable agent secret read event.
type AgentSecretReadEvent struct {
	AgentID    string    `json:"agentId"`
	ProjectID  string    `json:"projectId"`
	SecretKey  string    `json:"secretKey"`
	Success    bool      `json:"success"`
	FailReason string    `json:"failReason,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// AuditLogger defines the interface for logging audit events.
type AuditLogger interface {
	// LogBrokerAuthEvent logs a broker authentication event.
	LogBrokerAuthEvent(ctx context.Context, event *BrokerAuthEvent) error
	// LogGCPTokenEvent logs a GCP token generation event.
	LogGCPTokenEvent(ctx context.Context, event *GCPTokenEvent) error
	// LogInviteAuditEvent logs an invite/allow-list audit event.
	LogInviteAuditEvent(ctx context.Context, event *InviteAuditEvent) error
	// LogLifecycleHookEvent logs a lifecycle-hook admin event.
	LogLifecycleHookEvent(ctx context.Context, event *LifecycleHookEvent) error
	// LogLifecycleHookExecutionEvent logs a lifecycle-hook execution event (M5).
	LogLifecycleHookExecutionEvent(ctx context.Context, event *LifecycleHookExecutionEvent) error
	// LogAgentSecretReadEvent logs an agent secret read event.
	LogAgentSecretReadEvent(ctx context.Context, event *AgentSecretReadEvent) error
	// RecordSAAssignment logs a service-account assignment decision or binding
	// (svc-accnt design §7). Named to match store.SAAssignmentAuditSink, which
	// this method exists to satisfy: pkg/lifecyclehooks emits these too and
	// cannot import pkg/hub.
	RecordSAAssignment(ctx context.Context, event *store.SAAssignmentEvent) error
}

// Compile-time assertion that an AuditLogger is usable as the store-side sink.
// If these drift, the lifecycle-hook surface silently loses its audit trail —
// it would fall back to the nil-sink warning path rather than fail to build.
var _ store.SAAssignmentAuditSink = AuditLogger(nil)

// LogAuditLogger is a simple implementation that logs to the standard logger.
type LogAuditLogger struct {
	prefix string
	debug  bool
	log    *slog.Logger
}

// NewLogAuditLogger creates a new log-based audit logger.
func NewLogAuditLogger(prefix string, debug bool) *LogAuditLogger {
	if prefix == "" {
		prefix = "[Audit]"
	}
	return &LogAuditLogger{
		prefix: prefix,
		debug:  debug,
		log:    logging.Subsystem("hub.audit"),
	}
}

// logger returns the audit subsystem logger, falling back to slog.Default()
// when the field is nil (e.g. in tests that construct LogAuditLogger directly).
func (l *LogAuditLogger) logger() *slog.Logger {
	if l.log != nil {
		return l.log
	}
	return slog.Default()
}

// LogBrokerAuthEvent logs a broker authentication event to the standard logger.
//
// ⚠️ HISTORY, BECAUSE THE NO-OP THIS REPLACES WAS DELIBERATE. Commit 500efd1a
// ("fix: remove broker auth audit log") deleted the body of this method and
// left `return nil`. The commit message gives no rationale, but the cause is
// legible from the call graph: AuditableBrokerAuthMiddleware calls this on
// EVERY broker-authenticated request, and the deleted code logged success at
// LevelInfo. A broker polls the Hub continuously, so that was an INFO line per
// request — untenable, and worth fixing.
//
// The fix was over-broad. Nine event types route through this method, and only
// ONE of them — auth_success — is per-request. The other eight (register,
// deregister, join, rotate, revoke, link, unlink, and auth_FAILURE) are emitted
// by explicit helper calls at administrative moments, are low-volume, and are
// the security-relevant ones. Silencing the method silenced all nine to quiet
// one. The result was that broker authentication FAILURES left no trace
// anywhere, which is the single event here most worth having.
//
// So the volume problem is solved where it actually lives, at the level of the
// one noisy event type, rather than by discarding the other eight:
//
//   - auth_success  -> Debug. Per-request. Available when investigating,
//     absent from normal operation. This is the case 500efd1a was fixing.
//   - any failure   -> Warn. Includes auth_failure. Never suppressed: a
//     credential being rejected is the reason this event exists.
//   - everything else -> Info. Administrative, low-volume, and each one
//     changes who can talk to the Hub.
func (l *LogAuditLogger) LogBrokerAuthEvent(ctx context.Context, event *BrokerAuthEvent) error {
	if event == nil {
		return nil
	}

	level := slog.LevelInfo
	switch {
	case !event.Success:
		// Warn regardless of type. A failed rotate or a failed join is as
		// interesting as a failed auth.
		level = slog.LevelWarn
	case event.EventType == BrokerAuthEventAuthSuccess:
		level = slog.LevelDebug
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.Bool("success", event.Success),
		slog.String("broker_id", event.BrokerID),
		slog.String("ip_address", event.IPAddress),
	}

	if event.BrokerName != "" {
		attrs = append(attrs, slog.String("broker_name", event.BrokerName))
	}
	if event.UserAgent != "" {
		attrs = append(attrs, slog.String("user_agent", event.UserAgent))
	}
	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}
	if event.ActorID != "" {
		// Emitted as a pair: an actor id without its type is ambiguous between
		// a user and a broker acting on its own behalf.
		attrs = append(attrs, slog.String("actor_id", event.ActorID))
		attrs = append(attrs, slog.String("actor_type", event.ActorType))
	}

	// Details is emitted unconditionally, NOT debug-gated as it was before
	// 500efd1a. That gating was a defect the no-op hid: the only in-tree
	// producers are LogLinkEvent and LogUnlinkEvent, and the only key they set
	// is projectId. Debug-gating it means a link record says "broker B was
	// linked" without saying to what — an event stripped of the one field that
	// makes it mean anything.
	//
	// Security: Details is free-form and this method does not vet it. Callers
	// must not put secret material in it — the same rule that governs
	// LifecycleHookExecutionEvent.
	for k, v := range event.Details {
		attrs = append(attrs, slog.String(k, v))
	}

	l.logger().LogAttrs(ctx, level, "Broker auth audit event", attrs...)

	return nil
}

// LogInviteAuditEvent logs an invite/allow-list audit event to the standard logger.
func (l *LogAuditLogger) LogInviteAuditEvent(ctx context.Context, event *InviteAuditEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.Bool("success", event.Success),
	}

	if event.Email != "" {
		attrs = append(attrs, slog.String("email", event.Email))
	}
	if event.InviteID != "" {
		attrs = append(attrs, slog.String("invite_id", event.InviteID))
	}
	if event.ActorID != "" {
		attrs = append(attrs, slog.String("actor_id", event.ActorID))
	}
	if event.ActorEmail != "" {
		attrs = append(attrs, slog.String("actor_email", event.ActorEmail))
	}
	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}
	if event.Count > 0 {
		attrs = append(attrs, slog.Int("count", event.Count))
	}
	for k, v := range event.Details {
		attrs = append(attrs, slog.String(k, v))
	}

	l.logger().LogAttrs(ctx, level, "authz: "+string(event.EventType), attrs...)

	return nil
}

// LogGCPTokenEvent logs a GCP token generation event to the standard logger.
func (l *LogAuditLogger) LogGCPTokenEvent(ctx context.Context, event *GCPTokenEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.Bool("success", event.Success),
		slog.String("agent_id", event.AgentID),
		slog.String("project_id", event.ProjectID),
		slog.String("sa_email", event.ServiceAccountEmail),
		// sa_id is emitted unconditionally, like its siblings above. Every
		// caller of LogGCPTokenGeneration already supplies it and it was
		// populated on the struct all along — it was simply never written out,
		// so the field existed in the schema and in memory but not in any log
		// anyone could read. Emitting it only when non-empty would make the key
		// come and go between records and defeat the point of a stable schema.
		slog.String("sa_id", event.ServiceAccountID),
	}

	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}

	l.logger().LogAttrs(ctx, level, "GCP token audit event", attrs...)

	return nil
}

// LogLifecycleHookEvent logs a lifecycle-hook admin event to the standard logger.
func (l *LogAuditLogger) LogLifecycleHookEvent(ctx context.Context, event *LifecycleHookEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.String("hook_id", event.HookID),
		slog.String("hook_name", event.HookName),
		slog.String("actor", event.Actor),
		slog.Bool("success", event.Success),
	}
	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}

	l.logger().LogAttrs(ctx, level, "lifecycle hook audit event", attrs...)

	return nil
}

// LogLifecycleHookExecutionEvent logs a lifecycle-hook execution event to the standard logger.
func (l *LogAuditLogger) LogLifecycleHookExecutionEvent(ctx context.Context, event *LifecycleHookExecutionEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.String("hook_id", event.HookID),
		slog.String("hook_name", event.HookName),
		slog.String("trigger", event.Trigger),
		slog.String("agent_id", event.AgentID),
		slog.String("execution_identity", event.ExecutionIdentity),
		slog.String("action_type", event.ActionType),
		slog.String("method", event.Method),
		slog.String("host", event.Host),
		slog.Bool("success", event.Success),
		slog.Int("http_status_code", event.HTTPStatusCode),
		slog.Int64("latency_ms", event.LatencyMs),
		slog.Int("attempt", event.Attempt),
	}
	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}

	l.logger().LogAttrs(ctx, level, "lifecycle hook execution event", attrs...)

	return nil
}

// LogAgentSecretReadEvent logs an agent secret read event to the standard logger.
func (l *LogAuditLogger) LogAgentSecretReadEvent(ctx context.Context, event *AgentSecretReadEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", "agent_secret_read"),
		slog.String("agent_id", event.AgentID),
		slog.String("project_id", event.ProjectID),
		slog.String("secret_key", event.SecretKey),
		slog.Bool("success", event.Success),
	}
	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}

	l.logger().LogAttrs(ctx, level, "agent secret read event", attrs...)

	return nil
}

// RecordSAAssignment logs a service-account assignment record (design §7).
//
// Both record kinds come through here and they are NOT the same event, so the
// attributes differ rather than being padded to a common shape:
//
//   - A DECISION record carries the permission that was checked and the verdict.
//   - A BINDING record carries neither, because neither exists. Nothing was
//     checked and nothing was decided — the account came from project settings,
//     not from the caller. Emitting decision="indeterminate" to fill the slot
//     would be a fabricated verdict, and worse, ActAsIndeterminate DENIES, so
//     anyone later driving enforcement from these records would break routine
//     agent creation. The absence of the attribute is the honest encoding.
//
// The nil-receiver case is safe: nothing here dereferences l.
func (l *LogAuditLogger) RecordSAAssignment(ctx context.Context, event *store.SAAssignmentEvent) error {
	if event == nil {
		return nil
	}

	// Denials and indeterminates are warnings; allows and plain bindings are
	// informational. A binding has no decision and is never a warning — nothing
	// was refused.
	level := slog.LevelInfo
	if event.Decision != nil && *event.Decision != store.ActAsAllowed {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.Type)),
		slog.String("surface", event.Surface),
		slog.String("caller_kind", event.Caller.Kind.String()),
		slog.String("caller_id", event.Caller.ID),
		slog.String("target_sa_id", event.TargetSAID),
		slog.String("target_sa_email", event.TargetSAEmail),
		slog.String("mechanism", event.Mechanism),
	}

	// The caller's own GCP principal, when it has one. This is what an IAM
	// binding would have to name, so it is the field that makes a denial
	// actionable.
	if principal := event.Caller.GCPPrincipalID(); principal != "" {
		attrs = append(attrs, slog.String("caller_gcp_principal", principal))
	}
	if event.Permission != "" {
		attrs = append(attrs, slog.String("permission", event.Permission))
	}
	if event.Decision != nil {
		attrs = append(attrs, slog.String("decision", event.Decision.String()))
	}
	if event.Reason != "" {
		attrs = append(attrs, slog.String("reason", event.Reason))
	}
	// ⚠️ Omitted entirely when nil. Not serialised as null and not coerced to
	// false: false asserts that a cache was consulted and missed, which would
	// be a fabricated live IAM call. See store.SAAssignmentEvent.CacheHit.
	if event.CacheHit != nil {
		attrs = append(attrs, slog.Bool("cache_hit", *event.CacheHit))
	}

	slog.LogAttrs(ctx, level, "SA assignment audit event", attrs...)

	return nil
}

// LogAgentSecretRead logs an agent secret read event through the AuditLogger interface.
func LogAgentSecretRead(ctx context.Context, logger AuditLogger, agentID, projectID, secretKey string, success bool, failReason string) {
	if logger == nil {
		return
	}

	event := &AgentSecretReadEvent{
		AgentID:    agentID,
		ProjectID:  projectID,
		SecretKey:  secretKey,
		Success:    success,
		FailReason: failReason,
		Timestamp:  time.Now(),
	}

	_ = logger.LogAgentSecretReadEvent(ctx, event)
}

// AuditableBrokerAuthMiddleware creates middleware that logs authentication events.
// This wraps BrokerAuthMiddleware with audit logging.
func AuditableBrokerAuthMiddleware(svc *BrokerAuthService, logger AuditLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip if broker auth service is not configured
			if svc == nil || !svc.config.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			// Skip if not a broker-authenticated request
			brokerID := r.Header.Get(HeaderBrokerID)
			if brokerID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Create base event
			event := &BrokerAuthEvent{
				BrokerID:  brokerID,
				IPAddress: getClientIP(r),
				UserAgent: r.UserAgent(),
				Timestamp: time.Now(),
			}

			// Validate HMAC signature
			identity, err := svc.ValidateBrokerSignature(r.Context(), r)
			if err != nil {
				event.EventType = BrokerAuthEventAuthFailure
				event.Success = false
				event.FailReason = err.Error()

				if logger != nil {
					_ = logger.LogBrokerAuthEvent(r.Context(), event)
				}

				writeBrokerAuthError(w, err.Error())
				return
			}

			// Set broker-specific identity context and resolve on-behalf-of
			ctx := contextWithBrokerIdentity(r.Context(), identity)
			ctx, userIdent, ok := svc.applyOnBehalfOf(ctx, w, r, identity)
			if !ok {
				return
			}

			// Populate on-behalf-of details in the audit event before logging
			event.EventType = BrokerAuthEventAuthSuccess
			event.Success = true
			if userIdent != nil {
				if event.Details == nil {
					event.Details = make(map[string]string)
				}
				event.Details["on_behalf_of_email"] = userIdent.Email()
				event.Details["on_behalf_of_user_id"] = userIdent.ID()
			}

			if logger != nil {
				_ = logger.LogBrokerAuthEvent(ctx, event)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// getClientIP extracts the client IP address from the request.
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the chain
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}

	// Check X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	return r.RemoteAddr
}

// LogRegistrationEvent logs a broker registration event.
func LogRegistrationEvent(ctx context.Context, logger AuditLogger, brokerID, brokerName, actorID, ipAddress string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType:  BrokerAuthEventRegister,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		IPAddress:  ipAddress,
		Success:    true,
		ActorID:    actorID,
		ActorType:  "user",
		Timestamp:  time.Now(),
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogJoinEvent logs a broker join event.
func LogJoinEvent(ctx context.Context, logger AuditLogger, brokerID, ipAddress string, success bool, failReason string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType:  BrokerAuthEventJoin,
		BrokerID:   brokerID,
		IPAddress:  ipAddress,
		Success:    success,
		FailReason: failReason,
		Timestamp:  time.Now(),
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogRotateEvent logs a secret rotation event.
func LogRotateEvent(ctx context.Context, logger AuditLogger, brokerID, actorID, actorType, ipAddress string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType: BrokerAuthEventRotate,
		BrokerID:  brokerID,
		IPAddress: ipAddress,
		Success:   true,
		ActorID:   actorID,
		ActorType: actorType,
		Timestamp: time.Now(),
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogDeregisterEvent logs a broker deregistration event.
func LogDeregisterEvent(ctx context.Context, logger AuditLogger, brokerID, brokerName, actorID, ipAddress string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType:  BrokerAuthEventDeregister,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		IPAddress:  ipAddress,
		Success:    true,
		ActorID:    actorID,
		ActorType:  "user",
		Timestamp:  time.Now(),
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogLinkEvent logs a project link event (broker linked to project).
func LogLinkEvent(ctx context.Context, logger AuditLogger, brokerID, brokerName, projectID, actorID, ipAddress string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType:  BrokerAuthEventLink,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		IPAddress:  ipAddress,
		Success:    true,
		ActorID:    actorID,
		ActorType:  "user",
		Timestamp:  time.Now(),
		Details: map[string]string{
			"projectId": projectID,
		},
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogUnlinkEvent logs a project unlink event (broker unlinked from project).
func LogUnlinkEvent(ctx context.Context, logger AuditLogger, brokerID, projectID, actorID, ipAddress string) {
	if logger == nil {
		return
	}

	event := &BrokerAuthEvent{
		EventType: BrokerAuthEventUnlink,
		BrokerID:  brokerID,
		IPAddress: ipAddress,
		Success:   true,
		ActorID:   actorID,
		ActorType: "user",
		Timestamp: time.Now(),
		Details: map[string]string{
			"projectId": projectID,
		},
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogGCPTokenGeneration logs a GCP token generation event.
func LogGCPTokenGeneration(ctx context.Context, logger AuditLogger, eventType GCPTokenEventType, agentID, projectID, saEmail, saID string, success bool, failReason string) {
	if logger == nil {
		return
	}

	event := &GCPTokenEvent{
		EventType:           eventType,
		AgentID:             agentID,
		ProjectID:           projectID,
		ServiceAccountEmail: saEmail,
		ServiceAccountID:    saID,
		Success:             success,
		FailReason:          failReason,
		Timestamp:           time.Now(),
	}

	_ = logger.LogGCPTokenEvent(ctx, event)
}

// LogInviteAudit logs an invite/allow-list audit event.
func LogInviteAudit(ctx context.Context, logger AuditLogger, eventType InviteAuditEventType, email, inviteID, actorID, actorEmail string, details map[string]string) {
	if logger == nil {
		return
	}

	event := &InviteAuditEvent{
		EventType:  eventType,
		Email:      email,
		InviteID:   inviteID,
		ActorID:    actorID,
		ActorEmail: actorEmail,
		Success:    true,
		Timestamp:  time.Now(),
		Details:    details,
	}

	_ = logger.LogInviteAuditEvent(ctx, event)
}

// LogInviteAuditFailure logs a failed invite/allow-list audit event.
func LogInviteAuditFailure(ctx context.Context, logger AuditLogger, eventType InviteAuditEventType, email, failReason string) {
	if logger == nil {
		return
	}

	event := &InviteAuditEvent{
		EventType:  eventType,
		Email:      email,
		Success:    false,
		FailReason: failReason,
		Timestamp:  time.Now(),
	}

	_ = logger.LogInviteAuditEvent(ctx, event)
}

// LogLifecycleHookEvent logs a lifecycle-hook admin event through the
// AuditLogger interface so custom logger implementations can capture it.
func LogLifecycleHookEvent(ctx context.Context, logger AuditLogger, eventType LifecycleHookEventType, hookID, hookName, actor string, success bool, failReason string) {
	if logger == nil {
		return
	}

	event := &LifecycleHookEvent{
		EventType:  eventType,
		HookID:     hookID,
		HookName:   hookName,
		Actor:      actor,
		Success:    success,
		FailReason: failReason,
		Timestamp:  time.Now(),
	}

	_ = logger.LogLifecycleHookEvent(ctx, event)
}

// LogLifecycleHookExecutionEvent logs a lifecycle-hook execution event through
// the AuditLogger interface. Used by M5 evaluator.
func LogLifecycleHookExecutionEvent(ctx context.Context, logger AuditLogger, event *LifecycleHookExecutionEvent) {
	if logger == nil {
		return
	}
	_ = logger.LogLifecycleHookExecutionEvent(ctx, event)
}
