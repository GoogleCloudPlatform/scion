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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// APIError represents a standardized error response.
type APIError struct {
	Code      string                 `json:"code"`
	Message   string                 `json:"message"`
	Details   map[string]interface{} `json:"details,omitempty"`
	RequestID string                 `json:"requestId,omitempty"`
}

// ErrorResponse wraps an APIError for JSON responses.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// Error codes matching the Hub API specification.
const (
	ErrCodeInvalidRequest       = "invalid_request"
	ErrCodeValidationError      = "validation_error"
	ErrCodeUnauthorized         = "unauthorized"
	ErrCodeForbidden            = "forbidden"
	ErrCodeNotFound             = "not_found"
	ErrCodeConflict             = "conflict"
	ErrCodeVersionConflict      = "version_conflict"
	ErrCodeUnprocessable        = "unprocessable"
	ErrCodeRateLimited          = "rate_limited"
	ErrCodeInternalError        = "internal_error"
	ErrCodeRuntimeError         = "runtime_error"
	ErrCodeUnavailable          = "unavailable"
	ErrCodeNoRuntimeBroker      = "no_runtime_broker"
	ErrCodeRuntimeBrokerUnavail = "runtime_broker_unavailable"

	// ErrCodeConversationNotResolved is returned when the write-deny switch
	// (or read-switch) is ON but the conversation could not be resolved.
	// Status 409: the request conflicts with the resource's current state
	// and retrying will not help until backfill runs. Consistent with G3's
	// read-path shape.
	ErrCodeConversationNotResolved = "conversation_not_resolved"

	ErrCodeMissingEnvVars = "missing_env_vars"
	ErrCodeCloneFailed    = "clone_failed"
	ErrCodePullFailed     = "pull_failed"
	// ErrCodeDiscoverFailed reports a remote-directory probe that could not be
	// fetched or yielded nothing usable (see handleSkillsDiscoverDirectory).
	ErrCodeDiscoverFailed = "discover_failed"

	// Message authorization error codes
	ErrCodeMessageDenied = "message_denied"

	// Delivery error codes
	ErrCodeAgentNotFound   = "agent_not_found"
	ErrCodeDeliveryFailed  = "delivery_failed"
	ErrCodeAgentNotRunning = "agent_not_running"
	ErrCodeBrokerTimeout   = "broker_timeout"

	// Broker authentication error codes
	ErrCodeInvalidJoinToken = "invalid_join_token"
	ErrCodeExpiredJoinToken = "expired_join_token"
	ErrCodeBrokerAuthFailed = "broker_auth_failed"
	ErrCodeInvalidSignature = "invalid_signature"
	ErrCodeClockSkew        = "clock_skew"
	ErrCodeReplayDetected   = "replay_detected"

	// Quota enforcement error codes
	ErrCodeQuotaExceeded = "quota_exceeded"

	// Conversation resolution error codes (Tranche G read-switch)

	// ErrCodeConversationNotResolved is returned when the read-switch is ON
	// but the conversation could not be resolved from the request parameters.
	// This is a client-visible behaviour change: the endpoint returns a typed
	// error instead of silently falling back to the legacy channel+thread
	// filter. Status 409 — see G3 brief §3.
	ErrCodeConversationNotResolved = "conversation_not_resolved"

	// ErrCodeInvalidDMKey is returned when a DM key does not have exactly 5
	// colon-separated parts. Distinguishable from ErrCodeConversationNotResolved
	// because this is a parse failure, not a lookup miss.
	ErrCodeInvalidDMKey = "invalid_dm_key"

	// ErrCodeThreadProjectRequired is returned when a thread_id query param
	// is present but the agent has no ProjectID, making thread conversation
	// resolution impossible. Distinct from conversation_not_resolved so a VM
	// operator can tell "agent has no project" apart from "conversation row
	// missing". Without this, the request would silently fall through to the
	// DM branch and serve wrong data (G3-f).
	ErrCodeThreadProjectRequired = "thread_project_required"
)

// writeError writes a JSON error response.
// For 5xx errors, it logs the error details for debugging.
func writeError(w http.ResponseWriter, statusCode int, code, message string, details map[string]interface{}) {
	// Log 5xx errors at ERROR level, 4xx at DEBUG level for diagnostics
	if statusCode >= 500 {
		slog.Error("API Error", "status", statusCode, "code", code, "message", message)
	} else if statusCode >= 400 {
		slog.Debug("API client error", "status", statusCode, "code", code, "message", message)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	resp := ErrorResponse{
		Error: APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// writeErrorFromErr writes an error response based on a Go error.
// For 5xx errors, it logs the underlying error for debugging.
func writeErrorFromErr(w http.ResponseWriter, err error, requestID string) {
	var statusCode int
	var code, message string

	var permErr *secret.PermissionError

	switch {
	case errors.As(err, &permErr):
		statusCode = http.StatusForbidden
		code = ErrCodeForbidden
		message = permErr.Error()
	case errors.Is(err, store.ErrNotFound):
		statusCode = http.StatusNotFound
		code = ErrCodeNotFound
		message = "Resource not found"
	case errors.Is(err, store.ErrAlreadyExists):
		statusCode = http.StatusConflict
		code = ErrCodeConflict
		message = "Resource already exists"
	case errors.Is(err, store.ErrVersionConflict):
		statusCode = http.StatusConflict
		code = ErrCodeVersionConflict
		message = "Version conflict - resource was modified"
	case errors.Is(err, store.ErrInvalidInput):
		statusCode = http.StatusBadRequest
		code = ErrCodeValidationError
		message = "Invalid input"
	case errors.Is(err, secret.ErrNoSecretBackend):
		statusCode = http.StatusNotImplemented
		code = ErrCodeUnavailable
		message = err.Error()
	default:
		statusCode = http.StatusInternalServerError
		code = ErrCodeInternalError
		message = "Internal server error"
	}

	// Log 5xx errors with the underlying error for debugging, 4xx at DEBUG
	if statusCode >= 500 {
		slog.Error("API Error from Go error",
			"status", statusCode,
			"code", code,
			"requestID", requestID,
			"error", err,
		)
	} else if statusCode >= 400 {
		slog.Debug("API client error from Go error",
			"status", statusCode,
			"code", code,
			"requestID", requestID,
			"error", err,
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	resp := ErrorResponse{
		Error: APIError{
			Code:      code,
			Message:   message,
			RequestID: requestID,
		},
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// NotFound writes a 404 Not Found response.
func NotFound(w http.ResponseWriter, resource string) {
	writeError(w, http.StatusNotFound, ErrCodeNotFound,
		resource+" not found", nil)
}

// BadRequest writes a 400 Bad Request response.
func BadRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, message, nil)
}

// ValidationError writes a 400 Bad Request response for validation failures.
func ValidationError(w http.ResponseWriter, message string, details map[string]interface{}) {
	writeError(w, http.StatusBadRequest, ErrCodeValidationError, message, details)
}

// Unauthorized writes a 401 Unauthorized response.
func Unauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
		"Authentication required", nil)
}

// Forbidden writes a 403 Forbidden response.
func Forbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"Insufficient permissions", nil)
}

// Conflict writes a 409 Conflict response.
func Conflict(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, ErrCodeConflict, message, nil)
}

// InternalError writes a 500 Internal Server Error response.
func InternalError(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
		"Internal server error", nil)
}

// MethodNotAllowed writes a 405 Method Not Allowed response.
// If allowedMethods are provided, an Allow header is set per RFC 9110 §15.5.6.
func MethodNotAllowed(w http.ResponseWriter, allowedMethods ...string) {
	if len(allowedMethods) > 0 {
		w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
	}
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
		"Method not allowed", nil)
}

// RuntimeError writes a 502 Bad Gateway response for runtime broker errors.
func RuntimeError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, message, nil)
}

// GatewayTimeout writes a 504 Gateway Timeout response for runtime broker timeouts.
func GatewayTimeout(w http.ResponseWriter, message string) {
	writeError(w, http.StatusGatewayTimeout, ErrCodeBrokerTimeout, message, nil)
}

// NoRuntimeBroker writes a 422 Unprocessable Entity response when no runtime broker
// is available for agent creation. Includes available brokers as alternatives.
func NoRuntimeBroker(w http.ResponseWriter, message string, availableBrokers []RuntimeBrokerSummary) {
	details := map[string]interface{}{
		"availableBrokers": availableBrokers,
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker, message, details)
}

// ServiceNotReady writes a 503 Service Unavailable response with a Retry-After
// header, indicating the server is still initializing and the client should retry.
func ServiceNotReady(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, message, nil)
}

// RuntimeBrokerUnavailable writes a 503 Service Unavailable response when the
// specified runtime broker is not available.
func RuntimeBrokerUnavailable(w http.ResponseWriter, brokerID string, availableBrokers []RuntimeBrokerSummary) {
	details := map[string]interface{}{
		"requestedBrokerId": brokerID,
		"availableBrokers":  availableBrokers,
	}
	writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
		"Specified runtime broker is unavailable", details)
}

// MissingEnvVars writes a 422 Unprocessable Entity response when required
// environment variables cannot be resolved from available sources.
func MissingEnvVars(w http.ResponseWriter, keys []string, envInfo *EnvGatherResponse) {
	details := map[string]interface{}{
		"missingKeys": keys,
	}
	if envInfo != nil {
		details["envGather"] = envInfo
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeMissingEnvVars,
		fmt.Sprintf("Cannot start agent: %d required environment variable(s) are missing: %s",
			len(keys), strings.Join(keys, ", ")),
		details)
}

// RuntimeBrokerSummary is a minimal representation of a runtime broker for error responses.
type RuntimeBrokerSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	IsDefault bool   `json:"isDefault,omitempty"`
}
