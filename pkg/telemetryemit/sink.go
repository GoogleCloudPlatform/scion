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

// Package telemetryemit delivers serialized analytics events (see
// pkg/telemetrycontract) to a sink: Cloud Logging's scion-analytics log in
// production, stdout on a workstation, memory in tests. It imports neither
// pkg/hub nor sciontool, so every producer can use it.
//
// A sink receives the exact bytes a producer persisted. It never
// re-serializes, so a retry sends byte-identical payloads with the same
// event_id.
package telemetryemit

import (
	"context"
	"errors"
	"fmt"
)

// Sink delivers one serialized analytics event. Emit returns nil once the
// destination has acknowledged the event. A failure is classified with
// Classify: a permanent failure is specific to this payload, a blocked
// failure affects every payload (credentials, permissions), anything else
// is transient and retried.
type Sink interface {
	Emit(ctx context.Context, payload []byte) error
}

// ErrorClass is how a delivery failure is treated by a retrying caller.
type ErrorClass int

const (
	// ClassTransient: retry with backoff; never counts toward poison.
	ClassTransient ErrorClass = iota
	// ClassPermanent: this payload can never be delivered as is (rejected
	// as invalid or oversized, or it fails local re-validation).
	ClassPermanent
	// ClassBlocked: the sink rejects every payload (permission denied,
	// unauthenticated). Stop sending until a probe succeeds; keep rows.
	ClassBlocked
)

func (c ErrorClass) String() string {
	switch c {
	case ClassPermanent:
		return "permanent"
	case ClassBlocked:
		return "blocked"
	default:
		return "transient"
	}
}

// Failure reasons used by the sinks in this package. Reasons are short,
// low-cardinality metric attribute values.
const (
	ReasonInvalidArgument = "invalid_argument"
	ReasonTooLarge        = "too_large"
	ReasonInvalidPayload  = "invalid_payload"
	ReasonPermission      = "permission"
	ReasonAuth            = "auth"
	ReasonCircuitOpen     = "circuit_open"
	ReasonUnknown         = "unknown"
)

// SinkError is a classified delivery failure.
type SinkError struct {
	Class  ErrorClass
	Reason string
	Err    error
}

func (e *SinkError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("telemetryemit: %s failure (%s)", e.Class, e.Reason)
	}
	return fmt.Sprintf("telemetryemit: %s failure (%s): %v", e.Class, e.Reason, e.Err)
}

func (e *SinkError) Unwrap() error { return e.Err }

// Permanent marks err as permanent for this payload.
func Permanent(reason string, err error) error {
	return &SinkError{Class: ClassPermanent, Reason: reason, Err: err}
}

// Blocked marks err as blocking the sink for every payload.
func Blocked(reason string, err error) error {
	return &SinkError{Class: ClassBlocked, Reason: reason, Err: err}
}

// Transient marks err as transient with a reason.
func Transient(reason string, err error) error {
	return &SinkError{Class: ClassTransient, Reason: reason, Err: err}
}

// Classify returns the class and reason of a delivery error. An error that
// was not classified by a sink is transient with reason "unknown"; so is a
// context deadline or cancellation.
func Classify(err error) (ErrorClass, string) {
	var se *SinkError
	if errors.As(err, &se) {
		reason := se.Reason
		if reason == "" {
			reason = ReasonUnknown
		}
		return se.Class, reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTransient, "deadline_exceeded"
	}
	return ClassTransient, ReasonUnknown
}

// KindOf names the kind of sink s for the lifecycle_sink_kind gauge
// (design section 22 item 4, condition b): cloud_logging, stdout, or none
// for no sink. Any other sink (tests) reports "other", which appears in
// the sink log line only: the gauge records the three wire kinds, so an
// "other" sink publishes all three as 0.
func KindOf(s Sink) string {
	switch s.(type) {
	case nil:
		return SinkKindNone
	case *CloudLoggingSink:
		return SinkKindCloudLogging
	case *StdoutSink:
		return SinkKindStdout
	}
	return "other"
}
