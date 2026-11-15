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

package provider

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// FailReason is the closed, content-free reason vocabulary for Failed
// evaluations and provider errors (runtime-design §6.3).
type FailReason string

const (
	FailTimeout              FailReason = "timeout"
	FailCanceled             FailReason = "canceled"
	FailCapacity             FailReason = "capacity"
	FailCircuitOpen          FailReason = "circuit_open"
	FailUnavailable          FailReason = "unavailable"
	FailRateLimited          FailReason = "rate_limited"
	FailAuth                 FailReason = "auth"
	FailEgressRefused        FailReason = "egress_refused"
	FailMalformedResponse    FailReason = "malformed_response"
	FailUnsupported          FailReason = "unsupported"
	FailInputTooLarge        FailReason = "input_too_large"
	FailProviderRefused      FailReason = "provider_refused"
	FailContentBlocked       FailReason = "content_blocked"
	FailInvalidRequest       FailReason = "invalid_request"
	FailModelMismatch        FailReason = "model_mismatch"
	FailThresholdUnvalidated FailReason = "threshold_unvalidated"
	FailNoEligibleInstance   FailReason = "no_eligible_instance"
	FailInternal             FailReason = "internal"
)

const (
	// UncertainAbstained indicates a required question abstained (via the
	// rule-declared abstain option or adapter-native abstention).
	UncertainAbstained = "abstained"
	// UncertainTie indicates a required question had a top probability tie.
	UncertainTie = "tie"

	// MaxProviderErrorMessageBytes bounds content-free diagnostic text on ProviderError
	// (runtime-design §10).
	MaxProviderErrorMessageBytes = 256
)

var closedFailReasons = []FailReason{
	FailTimeout,
	FailCanceled,
	FailCapacity,
	FailCircuitOpen,
	FailUnavailable,
	FailRateLimited,
	FailAuth,
	FailEgressRefused,
	FailMalformedResponse,
	FailUnsupported,
	FailInputTooLarge,
	FailProviderRefused,
	FailContentBlocked,
	FailInvalidRequest,
	FailModelMismatch,
	FailThresholdUnvalidated,
	FailNoEligibleInstance,
	FailInternal,
}

// AllFailReasons returns a copy of the closed 18-value Failed reason set.
func AllFailReasons() []FailReason {
	out := make([]FailReason, len(closedFailReasons))
	copy(out, closedFailReasons)
	return out
}

// Valid reports whether r is a member of the closed Failed reason set.
func (r FailReason) Valid() bool {
	switch r {
	case FailTimeout,
		FailCanceled,
		FailCapacity,
		FailCircuitOpen,
		FailUnavailable,
		FailRateLimited,
		FailAuth,
		FailEgressRefused,
		FailMalformedResponse,
		FailUnsupported,
		FailInputTooLarge,
		FailProviderRefused,
		FailContentBlocked,
		FailInvalidRequest,
		FailModelMismatch,
		FailThresholdUnvalidated,
		FailNoEligibleInstance,
		FailInternal:
		return true
	default:
		return false
	}
}

// RetryableLater reports whether a consumer-level deferred retry (such as a
// durable trigger queue retry with backoff) may recover from r (runtime-design §6.3).
func (r FailReason) RetryableLater() bool {
	switch r {
	case FailUnavailable, FailRateLimited, FailCapacity, FailCircuitOpen, FailTimeout:
		return true
	default:
		return false
	}
}

// FallbackEligible reports whether r permits failing over to a subsequent
// eligible pool instance when pool fallback is enabled (runtime-design §6.3).
func (r FailReason) FallbackEligible() bool {
	switch r {
	case FailTimeout,
		FailCapacity,
		FailCircuitOpen,
		FailUnavailable,
		FailRateLimited,
		FailAuth,
		FailEgressRefused,
		FailUnsupported,
		FailModelMismatch,
		FailThresholdUnvalidated:
		return true
	default:
		return false
	}
}

// HTTPStatusClass maps an HTTP status code to its coarse content-free class
// ("2xx", "4xx", "5xx", etc.) or "" when code <= 0.
func HTTPStatusClass(code int) string {
	if code < 100 || code >= 600 {
		return ""
	}
	return fmt.Sprintf("%dxx", code/100)
}

// ProviderError is a structured, content-free error returned by an Adapter.
// Error response bodies, model input values, and credentials must never be
// placed in Message (runtime-design §4.3, §10; provider-design §6.4, §7.4).
type ProviderError struct {
	Reason     FailReason
	Retryable  bool
	RetryAfter time.Duration
	HTTPStatus int
	Message    string
}

// NewProviderError constructs a *ProviderError with Message truncated on a
// UTF-8 rune boundary to at most MaxProviderErrorMessageBytes.
func NewProviderError(reason FailReason, retryable bool, retryAfter time.Duration, httpStatus int, msg string) *ProviderError {
	return &ProviderError{
		Reason:     reason,
		Retryable:  retryable,
		RetryAfter: retryAfter,
		HTTPStatus: httpStatus,
		Message:    truncateUTF8(msg, MaxProviderErrorMessageBytes),
	}
}

// StatusClass returns the coarse HTTP status class ("4xx", "5xx", etc.) or "".
func (e ProviderError) StatusClass() string {
	return HTTPStatusClass(e.HTTPStatus)
}

// Error implements the error interface on both ProviderError and *ProviderError.
func (e ProviderError) Error() string {
	msg := truncateUTF8(e.Message, MaxProviderErrorMessageBytes)
	if cls := e.StatusClass(); cls != "" {
		if msg != "" {
			return fmt.Sprintf("system_one provider error (%s, %s): %s", e.Reason, cls, msg)
		}
		return fmt.Sprintf("system_one provider error (%s, %s)", e.Reason, cls)
	}
	if msg != "" {
		return fmt.Sprintf("system_one provider error (%s): %s", e.Reason, msg)
	}
	return fmt.Sprintf("system_one provider error (%s)", e.Reason)
}

// AsProviderError extracts a *ProviderError from err, matching either pointer
// or value ProviderError values wrapped inside err.
func AsProviderError(err error) (*ProviderError, bool) {
	if err == nil {
		return nil, false
	}
	var ptr *ProviderError
	if errors.As(err, &ptr) && ptr != nil {
		cp := *ptr
		cp.Message = truncateUTF8(cp.Message, MaxProviderErrorMessageBytes)
		return &cp, true
	}
	var val ProviderError
	if errors.As(err, &val) {
		val.Message = truncateUTF8(val.Message, MaxProviderErrorMessageBytes)
		return &val, true
	}
	return nil, false
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
