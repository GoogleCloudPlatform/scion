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

package runtimebroker

import (
	"encoding/json"
	"net/http"
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

// Error codes matching the Runtime Broker API specification.
const (
	ErrCodeInvalidRequest   = "invalid_request"
	ErrCodeValidationError  = "validation_error"
	ErrCodeUnauthorized     = "unauthorized"
	ErrCodeForbidden        = "forbidden"
	ErrCodeAgentNotFound    = "agent_not_found"
	ErrCodeNotFound         = "not_found"
	ErrCodeConflict         = "conflict"
	ErrCodeMethodNotAllowed = "method_not_allowed"
	ErrCodeInternalError    = "internal_error"
	ErrCodeRuntimeError     = "runtime_error"
	ErrCodeHubUnreachable   = "hub_unreachable"
	ErrCodeTemplateError    = "template_error"
)

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, statusCode int, code, message string, details map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	resp := ErrorResponse{
		Error: APIError{
			Code:    code,
			Message: message,
			Details: details,
		},
	}

	json.NewEncoder(w).Encode(resp)
}

// NotFound writes a 404 Not Found response.
func NotFound(w http.ResponseWriter, resource string) {
	code := ErrCodeNotFound
	if resource == "Agent" {
		code = ErrCodeAgentNotFound
	}
	writeError(w, http.StatusNotFound, code, resource+" not found", nil)
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

// MethodNotAllowed writes a 405 Method Not Allowed response.
func MethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, ErrCodeMethodNotAllowed,
		"Method not allowed", nil)
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

// RuntimeError writes a 500 error for runtime failures.
func RuntimeError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, message, nil)
}

// HubUnreachableError writes a 503 Service Unavailable response for Hub connectivity issues.
// This indicates that the Hub is temporarily unreachable and the operation should be retried.
func HubUnreachableError(w http.ResponseWriter, details string) {
	writeError(w, http.StatusServiceUnavailable, ErrCodeHubUnreachable,
		"Hub is unreachable. Check Hub connectivity or use solo mode.", map[string]interface{}{
			"details": details,
		})
}

// TemplateError writes a 500 error for template-related failures.
func TemplateError(w http.ResponseWriter, message string) {
	writeError(w, http.StatusInternalServerError, ErrCodeTemplateError, message, nil)
}

// Unprocessable writes a 422 Unprocessable Entity response.
func Unprocessable(w http.ResponseWriter, message string) {
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, message, nil)
}
