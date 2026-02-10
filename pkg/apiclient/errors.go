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

package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// APIError represents a structured error response from the API.
type APIError struct {
	StatusCode int                    // HTTP status code
	Code       string                 // Machine-readable error code
	Message    string                 // Human-readable message
	Details    map[string]interface{} // Additional context
	RequestID  string                 // Request tracking ID
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (status: %d, request: %s)",
			e.Code, e.Message, e.StatusCode, e.RequestID)
	}
	return fmt.Sprintf("%s: %s (status: %d)", e.Code, e.Message, e.StatusCode)
}

// IsNotFound returns true if the error is a 404 Not Found.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsConflict returns true if the error is a 409 Conflict.
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// IsUnauthorized returns true if the error is a 401 Unauthorized.
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized
}

// IsForbidden returns true if the error is a 403 Forbidden.
func (e *APIError) IsForbidden() bool {
	return e.StatusCode == http.StatusForbidden
}

// IsRateLimited returns true if the error is a 429 Too Many Requests.
func (e *APIError) IsRateLimited() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// IsServerError returns true if the error is a 5xx error.
func (e *APIError) IsServerError() bool {
	return e.StatusCode >= 500 && e.StatusCode < 600
}

// IsBadRequest returns true if the error is a 400 Bad Request.
func (e *APIError) IsBadRequest() bool {
	return e.StatusCode == http.StatusBadRequest
}

// Standard error codes (matching API specification)
const (
	ErrCodeInvalidRequest  = "invalid_request"
	ErrCodeValidationError = "validation_error"
	ErrCodeUnauthorized    = "unauthorized"
	ErrCodeForbidden       = "forbidden"
	ErrCodeNotFound        = "not_found"
	ErrCodeConflict        = "conflict"
	ErrCodeVersionConflict = "version_conflict"
	ErrCodeUnprocessable   = "unprocessable"
	ErrCodeRateLimited     = "rate_limited"
	ErrCodeInternalError   = "internal_error"
	ErrCodeRuntimeError    = "runtime_error"
	ErrCodeUnavailable     = "unavailable"
)

// errorResponse matches the API error response format.
type errorResponse struct {
	Error struct {
		Code      string                 `json:"code"`
		Message   string                 `json:"message"`
		Details   map[string]interface{} `json:"details,omitempty"`
		RequestID string                 `json:"requestId,omitempty"`
	} `json:"error"`
}

// ParseErrorResponse parses an API error response body.
func ParseErrorResponse(resp *http.Response) *APIError {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Code:       statusToCode(resp.StatusCode),
		Message:    http.StatusText(resp.StatusCode),
	}

	// Try to get the request ID from the header
	apiErr.RequestID = resp.Header.Get("X-Request-ID")

	// Try to parse the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return apiErr
	}

	var errResp errorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		// If we can't parse the error, use the body as the message
		if len(body) > 0 && len(body) < 500 {
			apiErr.Message = string(body)
		}
		return apiErr
	}

	if errResp.Error.Code != "" {
		apiErr.Code = errResp.Error.Code
	}
	if errResp.Error.Message != "" {
		apiErr.Message = errResp.Error.Message
	}
	if errResp.Error.Details != nil {
		apiErr.Details = errResp.Error.Details
	}
	if errResp.Error.RequestID != "" {
		apiErr.RequestID = errResp.Error.RequestID
	}

	return apiErr
}

// statusToCode maps HTTP status codes to error codes.
func statusToCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return ErrCodeInvalidRequest
	case http.StatusUnauthorized:
		return ErrCodeUnauthorized
	case http.StatusForbidden:
		return ErrCodeForbidden
	case http.StatusNotFound:
		return ErrCodeNotFound
	case http.StatusConflict:
		return ErrCodeConflict
	case http.StatusUnprocessableEntity:
		return ErrCodeUnprocessable
	case http.StatusTooManyRequests:
		return ErrCodeRateLimited
	case http.StatusServiceUnavailable:
		return ErrCodeUnavailable
	default:
		if status >= 500 {
			return ErrCodeInternalError
		}
		return ErrCodeInvalidRequest
	}
}

// IsNotFoundError checks if an error is a not found API error.
func IsNotFoundError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsNotFound()
	}
	return false
}

// IsUnauthorizedError checks if an error is an unauthorized API error.
func IsUnauthorizedError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsUnauthorized()
	}
	return false
}

// IsConflictError checks if an error is a conflict API error.
func IsConflictError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsConflict()
	}
	return false
}
