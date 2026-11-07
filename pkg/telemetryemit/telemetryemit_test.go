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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	gcplog "cloud.google.com/go/logging"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const samplePayload = `{"schema_version":1,"event_type":"agent.lifecycle","event_id":"e1","producer_namespace":"scion-hub/h","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","transition":"created","transition_revision":1,"metadata_revision":1,"metadata":{"labels":[]}}`

func TestClassify(t *testing.T) {
	cases := []struct {
		err    error
		class  ErrorClass
		reason string
	}{
		{Permanent(ReasonTooLarge, errors.New("x")), ClassPermanent, ReasonTooLarge},
		{fmt.Errorf("wrapped: %w", Blocked(ReasonAuth, nil)), ClassBlocked, ReasonAuth},
		{Transient(ReasonCircuitOpen, nil), ClassTransient, ReasonCircuitOpen},
		{errors.New("plain"), ClassTransient, ReasonUnknown},
		{context.DeadlineExceeded, ClassTransient, "deadline_exceeded"},
	}
	for _, tc := range cases {
		c, r := Classify(tc.err)
		if c != tc.class || r != tc.reason {
			t.Errorf("Classify(%v) = %v/%q, want %v/%q", tc.err, c, r, tc.class, tc.reason)
		}
	}
}

func TestMemorySinkFailThenDeliver(t *testing.T) {
	s := NewMemorySink()
	s.SetFailFunc(func(p []byte, attempt int) error {
		if attempt <= 2 {
			return Transient("test", nil)
		}
		return nil
	})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Emit(ctx, []byte(samplePayload)); err == nil {
			t.Fatalf("attempt %d succeeded, want failure", i+1)
		}
	}
	if err := s.Emit(ctx, []byte(samplePayload)); err != nil {
		t.Fatalf("third attempt: %v", err)
	}
	if got := s.Delivered(); len(got) != 1 || string(got[0]) != samplePayload {
		t.Fatalf("delivered %q", got)
	}
	if n := s.Attempts([]byte(samplePayload)); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestStdoutSinkWritesJSONLines(t *testing.T) {
	var buf bytes.Buffer
	s := NewStdoutSink(&buf)
	ctx := context.Background()
	if err := s.Emit(ctx, []byte(samplePayload)); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, []byte(samplePayload)); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), samplePayload+"\n"+samplePayload+"\n"; got != want {
		t.Fatalf("stdout = %q", got)
	}
	if c, _ := Classify(s.Emit(ctx, []byte("{\n}"))); c != ClassPermanent {
		t.Fatalf("multi-line payload class = %v, want permanent", c)
	}
}

type fakeEntryLogger struct {
	entries []gcplog.Entry
	err     error
}

func (f *fakeEntryLogger) LogSync(ctx context.Context, e gcplog.Entry) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("LogSync called without a deadline")
	}
	f.entries = append(f.entries, e)
	return f.err
}

func TestCloudLoggingSinkEntry(t *testing.T) {
	fl := &fakeEntryLogger{}
	s := NewCloudLoggingSinkWithLogger(fl, nil)
	if err := s.Emit(context.Background(), []byte(samplePayload)); err != nil {
		t.Fatal(err)
	}
	if len(fl.entries) != 1 {
		t.Fatalf("entries = %d", len(fl.entries))
	}
	e := fl.entries[0]
	if e.Severity != gcplog.Info {
		t.Errorf("severity = %v, want INFO", e.Severity)
	}
	if len(e.Labels) != 1 || e.Labels["event_type"] != "agent.lifecycle" {
		t.Errorf("labels = %v, want only event_type", e.Labels)
	}
	raw, ok := e.Payload.(json.RawMessage)
	if !ok || string(raw) != samplePayload {
		t.Errorf("payload = %#v, want the exact bytes as json.RawMessage", e.Payload)
	}
}

func TestCloudLoggingSinkClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		class  ErrorClass
		reason string
	}{
		{"invalid argument", status.Error(codes.InvalidArgument, "bad entry"), ClassPermanent, ReasonInvalidArgument},
		{"size", status.Error(codes.InvalidArgument, "Log entry with size 300K exceeds maximum size"), ClassPermanent, ReasonTooLarge},
		{"permission", status.Error(codes.PermissionDenied, "no"), ClassBlocked, ReasonPermission},
		{"unauthenticated", status.Error(codes.Unauthenticated, "no"), ClassBlocked, ReasonAuth},
		{"token refresh", fmt.Errorf("refresh: %w", &oauth2.RetrieveError{}), ClassBlocked, ReasonAuth},
		{"unavailable", status.Error(codes.Unavailable, "down"), ClassTransient, "unavailable"},
		{"resource exhausted", status.Error(codes.ResourceExhausted, "quota"), ClassTransient, "resource_exhausted"},
		{"deadline", status.Error(codes.DeadlineExceeded, "slow"), ClassTransient, "deadline_exceeded"},
		{"network", errors.New("connection reset"), ClassTransient, ReasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewCloudLoggingSinkWithLogger(&fakeEntryLogger{err: tc.err}, nil)
			c, r := Classify(s.Emit(context.Background(), []byte(samplePayload)))
			if c != tc.class || r != tc.reason {
				t.Fatalf("class %v/%q, want %v/%q", c, r, tc.class, tc.reason)
			}
		})
	}
}

func TestCloudLoggingSinkLocalRejections(t *testing.T) {
	fl := &fakeEntryLogger{}
	open := true
	s := NewCloudLoggingSinkWithLogger(fl, func() bool { return open })
	if c, r := Classify(s.Emit(context.Background(), []byte(samplePayload))); c != ClassTransient || r != ReasonCircuitOpen {
		t.Fatalf("open circuit: %v/%q, want transient/circuit_open (retried, never dropped)", c, r)
	}
	open = false
	big := []byte(`{"event_type":"agent.lifecycle","x":"` + strings.Repeat("a", MaxCloudLoggingEntryBytes) + `"}`)
	if c, r := Classify(s.Emit(context.Background(), big)); c != ClassPermanent || r != ReasonTooLarge {
		t.Fatalf("oversize: %v/%q", c, r)
	}
	if c, _ := Classify(s.Emit(context.Background(), []byte(`{"event_type":"nope"}`))); c != ClassPermanent {
		t.Fatalf("non-event payload class = %v, want permanent", c)
	}
	if len(fl.entries) != 0 {
		t.Fatalf("locally rejected payloads reached Cloud Logging: %d", len(fl.entries))
	}
}

func TestMemoryCounters(t *testing.T) {
	m := NewMemoryCounters()
	m.Enqueued("created")
	m.Enqueued("created")
	m.Dropped("age", 3)
	m.SinkBlocked(ReasonAuth, true)
	m.ObserverState(ObserverStateDisabledUnpinnedHubID)
	m.DrainerState(DrainerStateBacklogOnly)
	if m.Get(MetricDrainerState, DrainerStateBacklogOnly) != 1 || m.Get(MetricDrainerState, DrainerStateRunning) != 0 {
		t.Errorf("drainer state: %s", m)
	}
	if got := m.Get(MetricEventsEnqueued, "created"); got != 2 {
		t.Errorf("enqueued = %d", got)
	}
	if got := m.Get(MetricOutboxDropped, "age"); got != 3 {
		t.Errorf("dropped = %d", got)
	}
	if got := m.Get(MetricSinkBlocked, ReasonAuth); got != 1 {
		t.Errorf("blocked = %d", got)
	}
	if m.Get(MetricObserverState, ObserverStateEnabled) != 0 || m.Get(MetricObserverState, ObserverStateDisabledUnpinnedHubID) != 1 {
		t.Errorf("observer state: %s", m)
	}
	var _ LifecycleCounters = m
	var _ LifecycleCounters = NopCounters{}
}

func TestOTelCountersExport(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	c, err := NewOTelCounters(mp, "test")
	if err != nil {
		t.Fatal(err)
	}
	var _ LifecycleCounters = c
	c.Enqueued("deleted")
	c.Dropped("poison", 2)
	c.Outbox(OutboxGauges{Pending: 5})
	c.ObserverState(ObserverStateEnabled)
	c.DrainerState(DrainerStateRunning)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	for _, want := range []string{MetricEventsEnqueued, MetricOutboxDropped, MetricOutboxPending, MetricObserverState, MetricDrainerState} {
		if !names[want] {
			t.Errorf("metric %s not exported; got %v", want, names)
		}
	}
	if _, err := NewOTelCounters(nil, "x"); err == nil {
		t.Error("nil MeterProvider accepted")
	}
}
