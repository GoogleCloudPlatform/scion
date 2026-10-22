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

package telemetryemit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gcplog "cloud.google.com/go/logging"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// CloudLoggingTimeout bounds one synchronous Cloud Logging write.
const CloudLoggingTimeout = 10 * time.Second

// MaxCloudLoggingEntryBytes is the payload size above which an entry is
// rejected locally as permanently too large. Cloud Logging's limit is
// 256 KiB per entry including metadata; a margin is left for it.
const MaxCloudLoggingEntryBytes = 250 << 10

// EntryLogger is the part of *gcplog.Logger the sink uses.
type EntryLogger interface {
	LogSync(ctx context.Context, e gcplog.Entry) error
}

// CloudLoggingSink writes each payload synchronously to the
// telemetrycontract.AnalyticsLogID log as a structured jsonPayload, with
// severity INFO and the single entry label event_type.
type CloudLoggingSink struct {
	logger      EntryLogger
	circuitOpen func() bool
	timeout     time.Duration
}

// NewCloudLoggingSink returns a sink on the shared client. circuitOpen,
// when non-nil, is the hub's Cloud Logging circuit breaker: while it
// reports open, Emit fails transiently without writing, so the row is
// retried later and never dropped.
func NewCloudLoggingSink(client *gcplog.Client, circuitOpen func() bool) *CloudLoggingSink {
	return NewCloudLoggingSinkWithLogger(client.Logger(telemetrycontract.AnalyticsLogID), circuitOpen)
}

// NewCloudLoggingSinkWithLogger is NewCloudLoggingSink over any
// EntryLogger, for tests.
func NewCloudLoggingSinkWithLogger(l EntryLogger, circuitOpen func() bool) *CloudLoggingSink {
	return &CloudLoggingSink{logger: l, circuitOpen: circuitOpen, timeout: CloudLoggingTimeout}
}

// Emit implements Sink.
func (s *CloudLoggingSink) Emit(ctx context.Context, payload []byte) error {
	if s.circuitOpen != nil && s.circuitOpen() {
		return Transient(ReasonCircuitOpen, errors.New("cloud logging circuit is open"))
	}
	if len(payload) > MaxCloudLoggingEntryBytes {
		return Permanent(ReasonTooLarge, fmt.Errorf("payload is %d bytes, over %d", len(payload), MaxCloudLoggingEntryBytes))
	}
	var head struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(payload, &head); err != nil || !telemetrycontract.ValidEventType(head.EventType) {
		return Permanent(ReasonInvalidPayload, fmt.Errorf("payload is not an analytics event object: %v", err))
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	err := s.logger.LogSync(ctx, gcplog.Entry{
		Severity: gcplog.Info,
		Payload:  json.RawMessage(payload),
		Labels:   map[string]string{"event_type": head.EventType},
	})
	if err == nil {
		return nil
	}
	return classifyCloudLoggingError(err)
}

// classifyCloudLoggingError maps a Cloud Logging write error to a class
// (design 5.3): InvalidArgument and a size rejection are permanent for the
// payload; PermissionDenied, Unauthenticated and a failed credential
// refresh block the sink; everything else is transient.
func classifyCloudLoggingError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return Blocked(ReasonAuth, err)
	}
	st, ok := status.FromError(err)
	if !ok {
		if errors.Is(err, context.DeadlineExceeded) {
			return Transient("deadline_exceeded", err)
		}
		return Transient(ReasonUnknown, err)
	}
	switch st.Code() {
	case codes.InvalidArgument:
		if strings.Contains(strings.ToLower(st.Message()), "size") {
			return Permanent(ReasonTooLarge, err)
		}
		return Permanent(ReasonInvalidArgument, err)
	case codes.PermissionDenied:
		return Blocked(ReasonPermission, err)
	case codes.Unauthenticated:
		return Blocked(ReasonAuth, err)
	default:
		return Transient(grpcReason(st.Code()), err)
	}
}

// grpcReason is the snake_case metric reason of a transient gRPC code, so
// one cause has one attribute value across the gRPC and non-gRPC paths
// (deadline_exceeded, resource_exhausted, ...).
func grpcReason(c codes.Code) string {
	switch c {
	case codes.DeadlineExceeded:
		return "deadline_exceeded"
	case codes.ResourceExhausted:
		return "resource_exhausted"
	case codes.FailedPrecondition:
		return "failed_precondition"
	case codes.OutOfRange:
		return "out_of_range"
	case codes.AlreadyExists:
		return "already_exists"
	case codes.NotFound:
		return "not_found"
	case codes.DataLoss:
		return "data_loss"
	default: // single-word codes: Unavailable, Aborted, Internal, Canceled, Unknown, Unimplemented
		return strings.ToLower(c.String())
	}
}
