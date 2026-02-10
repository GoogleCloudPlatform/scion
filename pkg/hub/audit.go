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
	// BrokerAuthEventLink is logged when a broker is linked to a grove.
	BrokerAuthEventLink BrokerAuthEventType = "link"
	// BrokerAuthEventUnlink is logged when a broker is unlinked from a grove.
	BrokerAuthEventUnlink BrokerAuthEventType = "unlink"
)

// BrokerAuthEvent represents an auditable event related to broker authentication.
type BrokerAuthEvent struct {
	EventType  BrokerAuthEventType `json:"eventType"`
	BrokerID string            `json:"brokerId"`
	BrokerName string            `json:"brokerName,omitempty"`
	IPAddress  string            `json:"ipAddress,omitempty"`
	UserAgent  string            `json:"userAgent,omitempty"`
	Success    bool              `json:"success"`
	FailReason string            `json:"failReason,omitempty"`
	ActorID    string            `json:"actorId,omitempty"`   // User ID if admin action
	ActorType  string            `json:"actorType,omitempty"` // "user", "broker", or "system"
	Timestamp  time.Time         `json:"timestamp"`
	Details    map[string]string `json:"details,omitempty"`
}

// AuditLogger defines the interface for logging audit events.
type AuditLogger interface {
	// LogBrokerAuthEvent logs a broker authentication event.
	LogBrokerAuthEvent(ctx context.Context, event *BrokerAuthEvent) error
}

// LogAuditLogger is a simple implementation that logs to the standard logger.
type LogAuditLogger struct {
	prefix string
	debug  bool
}

// NewLogAuditLogger creates a new log-based audit logger.
func NewLogAuditLogger(prefix string, debug bool) *LogAuditLogger {
	if prefix == "" {
		prefix = "[Audit]"
	}
	return &LogAuditLogger{
		prefix: prefix,
		debug:  debug,
	}
}

// LogBrokerAuthEvent logs a broker authentication event to the standard logger.
func (l *LogAuditLogger) LogBrokerAuthEvent(ctx context.Context, event *BrokerAuthEvent) error {
	level := slog.LevelInfo
	if !event.Success {
		level = slog.LevelWarn
	}

	attrs := []slog.Attr{
		slog.String("event_type", string(event.EventType)),
		slog.Bool("success", event.Success),
		slog.String("broker_id", event.BrokerID),
		slog.String("ip_address", event.IPAddress),
	}

	if event.FailReason != "" {
		attrs = append(attrs, slog.String("fail_reason", event.FailReason))
	}

	if event.ActorID != "" {
		attrs = append(attrs, slog.String("actor_id", event.ActorID))
		attrs = append(attrs, slog.String("actor_type", event.ActorType))
	}

	if l.debug && len(event.Details) > 0 {
		for k, v := range event.Details {
			attrs = append(attrs, slog.String(k, v))
		}
	}

	slog.LogAttrs(ctx, level, "Broker auth audit event", attrs...)

	return nil
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
				BrokerID:    brokerID,
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

			// Log success
			event.EventType = BrokerAuthEventAuthSuccess
			event.Success = true

			if logger != nil {
				_ = logger.LogBrokerAuthEvent(r.Context(), event)
			}

			// Set both broker-specific and generic identity contexts
			ctx := contextWithBrokerIdentity(r.Context(), identity)
			ctx = contextWithIdentity(ctx, identity)
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
		EventType: BrokerAuthEventRegister,
		BrokerID:    brokerID,
		BrokerName:  brokerName,
		IPAddress: ipAddress,
		Success:   true,
		ActorID:   actorID,
		ActorType: "user",
		Timestamp: time.Now(),
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
		BrokerID:     brokerID,
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

// LogLinkEvent logs a grove link event (broker linked to grove).
func LogLinkEvent(ctx context.Context, logger AuditLogger, brokerID, brokerName, groveID, actorID, ipAddress string) {
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
			"groveId": groveID,
		},
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}

// LogUnlinkEvent logs a grove unlink event (broker unlinked from grove).
func LogUnlinkEvent(ctx context.Context, logger AuditLogger, brokerID, groveID, actorID, ipAddress string) {
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
			"groveId": groveID,
		},
	}

	_ = logger.LogBrokerAuthEvent(ctx, event)
}
