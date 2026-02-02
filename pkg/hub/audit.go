// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"log"
	"net/http"
	"time"
)

// HostAuthEventType defines the type of host authentication event.
type HostAuthEventType string

const (
	// HostAuthEventRegister is logged when a new host is registered.
	HostAuthEventRegister HostAuthEventType = "register"
	// HostAuthEventJoin is logged when a host completes join.
	HostAuthEventJoin HostAuthEventType = "join"
	// HostAuthEventAuthSuccess is logged when a host successfully authenticates.
	HostAuthEventAuthSuccess HostAuthEventType = "auth_success"
	// HostAuthEventAuthFailure is logged when a host fails to authenticate.
	HostAuthEventAuthFailure HostAuthEventType = "auth_failure"
	// HostAuthEventRotate is logged when a host secret is rotated.
	HostAuthEventRotate HostAuthEventType = "rotate"
	// HostAuthEventRevoke is logged when a host secret is revoked.
	HostAuthEventRevoke HostAuthEventType = "revoke"
)

// HostAuthEvent represents an auditable event related to host authentication.
type HostAuthEvent struct {
	EventType  HostAuthEventType `json:"eventType"`
	HostID     string            `json:"hostId"`
	HostName   string            `json:"hostName,omitempty"`
	IPAddress  string            `json:"ipAddress,omitempty"`
	UserAgent  string            `json:"userAgent,omitempty"`
	Success    bool              `json:"success"`
	FailReason string            `json:"failReason,omitempty"`
	ActorID    string            `json:"actorId,omitempty"`   // User ID if admin action
	ActorType  string            `json:"actorType,omitempty"` // "user", "host", or "system"
	Timestamp  time.Time         `json:"timestamp"`
	Details    map[string]string `json:"details,omitempty"`
}

// AuditLogger defines the interface for logging audit events.
type AuditLogger interface {
	// LogHostAuthEvent logs a host authentication event.
	LogHostAuthEvent(ctx context.Context, event *HostAuthEvent) error
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

// LogHostAuthEvent logs a host authentication event to the standard logger.
func (l *LogAuditLogger) LogHostAuthEvent(ctx context.Context, event *HostAuthEvent) error {
	var status string
	if event.Success {
		status = "SUCCESS"
	} else {
		status = "FAILURE"
	}

	msg := "%s %s: %s host=%s ip=%s"
	args := []interface{}{l.prefix, event.EventType, status, event.HostID, event.IPAddress}

	if event.FailReason != "" {
		msg += " reason=%q"
		args = append(args, event.FailReason)
	}

	if event.ActorID != "" {
		msg += " actor=%s(%s)"
		args = append(args, event.ActorType, event.ActorID)
	}

	log.Printf(msg, args...)

	if l.debug && len(event.Details) > 0 {
		for k, v := range event.Details {
			log.Printf("%s   detail: %s=%s", l.prefix, k, v)
		}
	}

	return nil
}

// AuditableHostAuthMiddleware creates middleware that logs authentication events.
// This wraps HostAuthMiddleware with audit logging.
func AuditableHostAuthMiddleware(svc *HostAuthService, logger AuditLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip if host auth service is not configured
			if svc == nil || !svc.config.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			// Skip if not a host-authenticated request
			hostID := r.Header.Get(HeaderHostID)
			if hostID == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Create base event
			event := &HostAuthEvent{
				HostID:    hostID,
				IPAddress: getClientIP(r),
				UserAgent: r.UserAgent(),
				Timestamp: time.Now(),
			}

			// Validate HMAC signature
			identity, err := svc.ValidateHostSignature(r.Context(), r)
			if err != nil {
				event.EventType = HostAuthEventAuthFailure
				event.Success = false
				event.FailReason = err.Error()

				if logger != nil {
					_ = logger.LogHostAuthEvent(r.Context(), event)
				}

				writeHostAuthError(w, err.Error())
				return
			}

			// Log success
			event.EventType = HostAuthEventAuthSuccess
			event.Success = true

			if logger != nil {
				_ = logger.LogHostAuthEvent(r.Context(), event)
			}

			// Set both host-specific and generic identity contexts
			ctx := contextWithHostIdentity(r.Context(), identity)
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

// LogRegistrationEvent logs a host registration event.
func LogRegistrationEvent(ctx context.Context, logger AuditLogger, hostID, hostName, actorID, ipAddress string) {
	if logger == nil {
		return
	}

	event := &HostAuthEvent{
		EventType: HostAuthEventRegister,
		HostID:    hostID,
		HostName:  hostName,
		IPAddress: ipAddress,
		Success:   true,
		ActorID:   actorID,
		ActorType: "user",
		Timestamp: time.Now(),
	}

	_ = logger.LogHostAuthEvent(ctx, event)
}

// LogJoinEvent logs a host join event.
func LogJoinEvent(ctx context.Context, logger AuditLogger, hostID, ipAddress string, success bool, failReason string) {
	if logger == nil {
		return
	}

	event := &HostAuthEvent{
		EventType:  HostAuthEventJoin,
		HostID:     hostID,
		IPAddress:  ipAddress,
		Success:    success,
		FailReason: failReason,
		Timestamp:  time.Now(),
	}

	_ = logger.LogHostAuthEvent(ctx, event)
}

// LogRotateEvent logs a secret rotation event.
func LogRotateEvent(ctx context.Context, logger AuditLogger, hostID, actorID, actorType, ipAddress string) {
	if logger == nil {
		return
	}

	event := &HostAuthEvent{
		EventType: HostAuthEventRotate,
		HostID:    hostID,
		IPAddress: ipAddress,
		Success:   true,
		ActorID:   actorID,
		ActorType: actorType,
		Timestamp: time.Now(),
	}

	_ = logger.LogHostAuthEvent(ctx, event)
}
