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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Audit records for scheduled chat messages (ptone/scion#3666).

// Actions recorded for a scheduled chat message.
const (
	ScheduledAuditCreate      = "create"
	ScheduledAuditCancel      = "cancel"
	ScheduledAuditSendNow     = "send_now"
	ScheduledAuditDismiss     = "dismiss"
	ScheduledAuditFire        = "fire"
	ScheduledAuditInterrupted = "interrupted"
)

// ChatScheduledMessageEvent is the audit record of one change to a
// scheduled chat message: created, cancelled, sent again on request,
// dismissed, delivered or failed at fire time, or found interrupted. The
// principal is always the sender and the executor is scheduled-send. It
// never contains the message text.
type ChatScheduledMessageEvent struct {
	// Action is one of the ScheduledAudit* constants.
	Action string
	// SenderUserID is the principal: the user the message is sent as.
	SenderUserID string
	// Executor is scheduledSendClientType; ExecutorID names the row.
	Executor   string
	ExecutorID string
	// CredentialKind is the caller's credential for a request-driven
	// action; empty for actions taken by the sweeper.
	CredentialKind     string
	ScheduledMessageID string
	ConversationKey    string
	// ProjectID is the project stored with the row (empty for user DMs).
	ProjectID     string
	Status        string
	FailureReason string
	MessageID     string
	RequestID     string
	Timestamp     time.Time
}

// chatScheduledAuditor is implemented by audit loggers that record
// scheduled chat message events (LogAuditLogger does). It is separate from
// AuditLogger so other implementations need no change.
type chatScheduledAuditor interface {
	LogChatScheduledMessageEvent(ctx context.Context, event *ChatScheduledMessageEvent) error
}

func chatScheduledAuditAttrs(e *ChatScheduledMessageEvent) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event_type", "chat_scheduled_message"),
		slog.String("audit_action", "chat.scheduled."+e.Action),
		slog.String("principal_type", "user"),
		slog.String("principal_id", e.SenderUserID),
		slog.String("executor", e.Executor),
		slog.String("executor_id", e.ExecutorID),
		slog.String("scheduled_message_id", e.ScheduledMessageID),
		slog.String("conversation_key", e.ConversationKey),
		slog.String("status", e.Status),
	}
	if e.ProjectID != "" {
		attrs = append(attrs, slog.String("project_id", e.ProjectID))
	}
	if e.FailureReason != "" {
		attrs = append(attrs, slog.String("failure_reason", e.FailureReason))
	}
	if e.MessageID != "" {
		attrs = append(attrs, slog.String("message_id", e.MessageID))
	}
	if e.CredentialKind != "" {
		attrs = append(attrs, slog.String("credential_kind", e.CredentialKind))
	}
	if e.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", e.RequestID))
	}
	return attrs
}

// LogChatScheduledMessageEvent logs a scheduled chat message audit event.
// A nil receiver is safe.
func (l *LogAuditLogger) LogChatScheduledMessageEvent(ctx context.Context, e *ChatScheduledMessageEvent) error {
	if l == nil || e == nil {
		return nil
	}
	l.logger().LogAttrs(ctx, slog.LevelInfo, "chat scheduled message event", chatScheduledAuditAttrs(e)...)
	return nil
}

// newChatScheduledMessageEvent builds the audit record of action on m.
func newChatScheduledMessageEvent(ctx context.Context, action string, m *ScheduledChatMessage) *ChatScheduledMessageEvent {
	return &ChatScheduledMessageEvent{
		Action:             action,
		SenderUserID:       m.SenderUserID,
		Executor:           scheduledSendClientType,
		ExecutorID:         "scheduled_message:" + m.ID,
		CredentialKind:     string(GetCredentialContextFromContext(ctx).Kind),
		ScheduledMessageID: m.ID,
		ConversationKey:    m.ConversationKey,
		ProjectID:          m.ProjectID,
		Status:             m.Status,
		FailureReason:      m.FailureReason,
		MessageID:          m.MessageID,
		RequestID:          logging.RequestIDFromContext(ctx),
		Timestamp:          time.Now().UTC(),
	}
}

// auditScheduledMessage records action on m through the hub audit logger,
// with the sender as principal and scheduled-send as executor. The outcome
// is the row's resulting status, with the failure reason in its own field.
// Without an audit logger that records these events it falls back to a
// structured log record with the same fields.
func (s *Server) auditScheduledMessage(ctx context.Context, action string, m *ScheduledChatMessage) {
	e := newChatScheduledMessageEvent(ctx, action, m)
	if a, ok := s.GetAuditLogger().(chatScheduledAuditor); ok {
		_ = a.LogChatScheduledMessageEvent(ctx, e)
		return
	}
	scheduledSendLog().LogAttrs(ctx, slog.LevelInfo, "chat scheduled message event", chatScheduledAuditAttrs(e)...)
}
