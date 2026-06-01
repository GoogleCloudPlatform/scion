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

package logging

import (
	"context"
	"errors"
	"log/slog"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// mockCloudHandler is a test double for CloudHandler that doesn't require
// a real Cloud Logging connection.
func newTestResilientHandler(t *testing.T, cfg ResilientCloudHandlerConfig) *ResilientCloudHandler {
	t.Helper()
	// Create a minimal CloudHandler without a real client/logger.
	inner := &CloudHandler{
		level:     slog.LevelInfo,
		component: "test",
		hostname:  "test-host",
	}
	cfg.applyDefaults()
	cb := &circuitBreaker{}
	cb.state.Store(int32(circuitClosed))
	h := &ResilientCloudHandler{
		inner:  inner,
		config: cfg,
		cb:     cb,
		done:   make(chan struct{}),
	}
	// Don't start the health check loop — tests drive it explicitly.
	return h
}

func TestResilientCloudHandler_DefaultConfig(t *testing.T) {
	cfg := ResilientCloudHandlerConfig{}
	cfg.applyDefaults()

	if cfg.MaxFailures != DefaultMaxFailures {
		t.Errorf("MaxFailures = %d, want %d", cfg.MaxFailures, DefaultMaxFailures)
	}
	if cfg.OpenDuration != DefaultOpenDuration {
		t.Errorf("OpenDuration = %v, want %v", cfg.OpenDuration, DefaultOpenDuration)
	}
	if cfg.ProbeInterval != DefaultProbeInterval {
		t.Errorf("ProbeInterval = %v, want %v", cfg.ProbeInterval, DefaultProbeInterval)
	}
	if cfg.ProbeTimeout != DefaultProbeTimeout {
		t.Errorf("ProbeTimeout = %v, want %v", cfg.ProbeTimeout, DefaultProbeTimeout)
	}
}

func TestResilientCloudHandler_CustomConfig(t *testing.T) {
	cfg := ResilientCloudHandlerConfig{
		MaxFailures:   10,
		OpenDuration:  2 * time.Minute,
		ProbeInterval: time.Minute,
		ProbeTimeout:  5 * time.Second,
	}
	cfg.applyDefaults()

	if cfg.MaxFailures != 10 {
		t.Errorf("MaxFailures = %d, want 10", cfg.MaxFailures)
	}
	if cfg.OpenDuration != 2*time.Minute {
		t.Errorf("OpenDuration = %v, want 2m", cfg.OpenDuration)
	}
	if cfg.ProbeInterval != time.Minute {
		t.Errorf("ProbeInterval = %v, want 1m", cfg.ProbeInterval)
	}
	if cfg.ProbeTimeout != 5*time.Second {
		t.Errorf("ProbeTimeout = %v, want 5s", cfg.ProbeTimeout)
	}
}

func TestResilientCloudHandler_InitialState(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	if h.CircuitOpen() {
		t.Error("circuit should be closed initially")
	}
	if circuitState(h.cb.state.Load()) != circuitClosed {
		t.Errorf("state = %d, want %d (circuitClosed)", h.cb.state.Load(), circuitClosed)
	}
}

func TestResilientCloudHandler_Enabled(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	ctx := context.Background()

	// Should mirror inner handler's Enabled behavior.
	if !h.Enabled(ctx, slog.LevelInfo) {
		t.Error("should be enabled for Info level")
	}
	if !h.Enabled(ctx, slog.LevelError) {
		t.Error("should be enabled for Error level")
	}
	if h.Enabled(ctx, slog.LevelDebug) {
		t.Error("should not be enabled for Debug level when inner level is Info")
	}

	// Should still report enabled when circuit is open (Handle gates, not Enabled).
	h.cb.state.Store(int32(circuitOpen))
	if !h.Enabled(ctx, slog.LevelInfo) {
		t.Error("should still report enabled when circuit is open")
	}
}

func TestResilientCloudHandler_HandleWhenClosed(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	// Circuit is closed — Handle should try to forward.
	// Since our test handler has no real logger, Handle on the inner
	// handler will panic. We test the gating logic by verifying the
	// circuit state check works.
	if circuitState(h.cb.state.Load()) != circuitClosed {
		t.Fatal("expected circuit to be closed")
	}
	// We can't call Handle with a nil logger, but we can verify the
	// state-based routing by testing the open/half-open paths.
}

func TestResilientCloudHandler_HandleWhenOpen(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	h.cb.state.Store(int32(circuitOpen))

	// Handle should return nil immediately (skip Cloud Logging).
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test message", 0)
	err := h.Handle(context.Background(), r)
	if err != nil {
		t.Errorf("Handle() returned error when circuit open: %v", err)
	}
}

func TestResilientCloudHandler_HandleWhenHalfOpen(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	h.cb.state.Store(int32(circuitHalfOpen))

	// Handle should return nil immediately (skip during probe).
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test message", 0)
	err := h.Handle(context.Background(), r)
	if err != nil {
		t.Errorf("Handle() returned error when circuit half-open: %v", err)
	}
}

func TestResilientCloudHandler_RecordFailureOpensCircuit(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 3,
	})
	defer close(h.done)

	// First two failures should not open the circuit.
	h.recordFailure(errForTest("fail 1"))
	if h.CircuitOpen() {
		t.Error("circuit should not open after 1 failure")
	}

	h.recordFailure(errForTest("fail 2"))
	if h.CircuitOpen() {
		t.Error("circuit should not open after 2 failures")
	}

	// Third failure should open the circuit.
	h.recordFailure(errForTest("fail 3"))
	if !h.CircuitOpen() {
		t.Error("circuit should be open after 3 failures")
	}
	if circuitState(h.cb.state.Load()) != circuitOpen {
		t.Errorf("state = %d, want %d (circuitOpen)", h.cb.state.Load(), circuitOpen)
	}
}

func TestResilientCloudHandler_RecordSuccessResetsFailures(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 3,
	})
	defer close(h.done)

	// Accumulate 2 failures.
	h.recordFailure(errForTest("fail 1"))
	h.recordFailure(errForTest("fail 2"))

	// Success resets the counter.
	h.recordSuccess()

	h.cb.mu.Lock()
	failures := h.cb.failures
	h.cb.mu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after success", failures)
	}

	// Now 2 more failures should not open (need 3 consecutive).
	h.recordFailure(errForTest("fail 3"))
	h.recordFailure(errForTest("fail 4"))
	if h.CircuitOpen() {
		t.Error("circuit should not open — success reset the counter")
	}
}

func TestResilientCloudHandler_TransitionToClosedResetsFailures(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 2,
	})
	defer close(h.done)

	// Open the circuit.
	h.recordFailure(errForTest("fail 1"))
	h.recordFailure(errForTest("fail 2"))
	if !h.CircuitOpen() {
		t.Fatal("circuit should be open")
	}

	// Transition back to closed.
	h.transitionTo(circuitClosed)
	if h.CircuitOpen() {
		t.Error("circuit should be closed after transition")
	}

	h.cb.mu.Lock()
	failures := h.cb.failures
	h.cb.mu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after closing circuit", failures)
	}
}

func TestResilientCloudHandler_TransitionIdempotent(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	// Transition to the same state should be a no-op.
	h.transitionTo(circuitClosed)
	if h.CircuitOpen() {
		t.Error("should remain closed")
	}

	h.transitionTo(circuitOpen)
	h.transitionTo(circuitOpen) // idempotent
	if !h.CircuitOpen() {
		t.Error("should remain open")
	}
}

func TestResilientCloudHandler_CircuitOpenState(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	if h.CircuitOpen() {
		t.Error("CircuitOpen() should be false when closed")
	}

	h.cb.state.Store(int32(circuitOpen))
	if !h.CircuitOpen() {
		t.Error("CircuitOpen() should be true when open")
	}

	h.cb.state.Store(int32(circuitHalfOpen))
	if !h.CircuitOpen() {
		t.Error("CircuitOpen() should be true when half-open")
	}
}

func TestResilientCloudHandler_WithAttrsPreservesCircuitState(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	// Open the circuit.
	h.cb.state.Store(int32(circuitOpen))

	// Create a derived handler with WithAttrs.
	derived := h.WithAttrs([]slog.Attr{slog.String("key", "value")})
	rh, ok := derived.(*ResilientCloudHandler)
	if !ok {
		t.Fatal("WithAttrs should return a *ResilientCloudHandler")
	}

	// Derived handler should share the same atomic state.
	if !rh.CircuitOpen() {
		t.Error("derived handler should share circuit state")
	}

	// The inner handler should have the new attribute.
	if len(rh.inner.attrs) != 1 {
		t.Errorf("expected 1 attr on inner handler, got %d", len(rh.inner.attrs))
	}
}

func TestResilientCloudHandler_WithGroupPreservesCircuitState(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{})
	defer close(h.done)

	h.cb.state.Store(int32(circuitOpen))

	derived := h.WithGroup("mygroup")
	rh, ok := derived.(*ResilientCloudHandler)
	if !ok {
		t.Fatal("WithGroup should return a *ResilientCloudHandler")
	}

	if !rh.CircuitOpen() {
		t.Error("derived handler should share circuit state")
	}

	if len(rh.inner.groups) != 1 || rh.inner.groups[0] != "mygroup" {
		t.Errorf("expected inner handler to have group 'mygroup', got %v", rh.inner.groups)
	}
}

func TestResilientCloudHandler_ConcurrentStateAccess(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 5,
	})
	defer close(h.done)

	// Hammer state transitions from multiple goroutines to verify
	// there are no data races.
	done := make(chan struct{})
	var ops atomic.Int64

	for i := 0; i < 4; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					h.recordFailure(errForTest("test"))
					ops.Add(1)
				}
			}
		}()
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					h.recordSuccess()
					ops.Add(1)
				}
			}
		}()
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = h.CircuitOpen()
					ops.Add(1)
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(done)

	if ops.Load() == 0 {
		t.Error("expected some operations to complete")
	}
}

func TestCircuitState_Constants(t *testing.T) {
	// Verify the circuit state constants are distinct.
	if circuitClosed == circuitOpen {
		t.Error("circuitClosed should differ from circuitOpen")
	}
	if circuitOpen == circuitHalfOpen {
		t.Error("circuitOpen should differ from circuitHalfOpen")
	}
	if circuitClosed == circuitHalfOpen {
		t.Error("circuitClosed should differ from circuitHalfOpen")
	}
}

func TestResilientCloudHandler_FailuresBelowThresholdDontOpen(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 5,
	})
	defer close(h.done)

	for i := 0; i < 4; i++ {
		h.recordFailure(errForTest("fail"))
	}

	if h.CircuitOpen() {
		t.Error("circuit should not open with fewer than MaxFailures")
	}

	// One more should open it.
	h.recordFailure(errForTest("fail"))
	if !h.CircuitOpen() {
		t.Error("circuit should open at MaxFailures")
	}
}

func TestResilientCloudHandler_RecordFailureIdempotentWhenOpen(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures: 2,
	})
	defer close(h.done)

	// Open the circuit.
	h.recordFailure(errForTest("fail 1"))
	h.recordFailure(errForTest("fail 2"))
	if !h.CircuitOpen() {
		t.Fatal("expected circuit to be open")
	}

	// Additional failures should not panic or change state.
	h.recordFailure(errForTest("fail 3"))
	h.recordFailure(errForTest("fail 4"))
	if circuitState(h.cb.state.Load()) != circuitOpen {
		t.Error("circuit should remain open")
	}
}

func TestResilientCloudHandler_RunHealthCheckClosedSuccess(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{MaxFailures: 3})
	defer close(h.done)
	h.flushFn = func() error { return nil }

	h.runHealthCheck()

	h.cb.mu.Lock()
	failures := h.cb.failures
	h.cb.mu.Unlock()
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after successful health check", failures)
	}
	if h.CircuitOpen() {
		t.Error("circuit should remain closed after successful flush")
	}
}

func TestResilientCloudHandler_RunHealthCheckClosedFailure(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{MaxFailures: 2})
	defer close(h.done)
	h.flushFn = func() error { return errors.New("flush failed") }

	h.runHealthCheck()
	if h.CircuitOpen() {
		t.Error("circuit should not open after 1 failure")
	}

	h.runHealthCheck()
	if !h.CircuitOpen() {
		t.Error("circuit should open after MaxFailures consecutive failures")
	}
}

func TestResilientCloudHandler_RunHealthCheckOpenProbeSuccess(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures:  2,
		OpenDuration: time.Millisecond,
	})
	defer close(h.done)

	h.flushFn = func() error { return errors.New("flush failed") }
	h.runHealthCheck()
	h.runHealthCheck()
	if !h.CircuitOpen() {
		t.Fatal("circuit should be open")
	}

	time.Sleep(2 * time.Millisecond)

	h.flushFn = func() error { return nil }
	h.runHealthCheck()
	if h.CircuitOpen() {
		t.Error("circuit should close after successful probe")
	}
}

func TestResilientCloudHandler_RunHealthCheckOpenProbeFailure(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		MaxFailures:  2,
		OpenDuration: time.Millisecond,
	})
	defer close(h.done)

	h.flushFn = func() error { return errors.New("flush failed") }
	h.runHealthCheck()
	h.runHealthCheck()
	if !h.CircuitOpen() {
		t.Fatal("circuit should be open")
	}

	time.Sleep(2 * time.Millisecond)

	// Probe also fails — circuit should remain open.
	h.runHealthCheck()
	if !h.CircuitOpen() {
		t.Error("circuit should remain open after failed probe")
	}
}

func TestResilientCloudHandler_FlushWithTimeoutSuccess(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		ProbeTimeout: time.Second,
	})
	defer close(h.done)

	h.flushFn = func() error { return nil }

	if err := h.flushWithTimeout(); err != nil {
		t.Errorf("flushWithTimeout() unexpected error: %v", err)
	}
}

func TestResilientCloudHandler_FlushWithTimeoutTimesOut(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		ProbeTimeout: 10 * time.Millisecond,
	})
	defer close(h.done)

	h.flushFn = func() error {
		time.Sleep(time.Second)
		return nil
	}

	err := h.flushWithTimeout()
	if err == nil {
		t.Fatal("flushWithTimeout() should return error on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		if err.Error() != "cloud logging flush timed out after 10ms" {
			t.Errorf("unexpected error: %v", err)
		}
	}
}

func TestResilientCloudHandler_FlushInFlightPrevents(t *testing.T) {
	h := newTestResilientHandler(t, ResilientCloudHandlerConfig{
		ProbeTimeout: time.Second,
	})
	defer close(h.done)

	// Block flush so flushInFlight stays true.
	blocked := make(chan struct{})
	h.flushFn = func() error {
		<-blocked
		return nil
	}

	// Start first flush in background.
	done := make(chan error, 1)
	go func() {
		done <- h.flushWithTimeout()
	}()
	// Give it a moment to start.
	time.Sleep(5 * time.Millisecond)

	// Second flush should fail immediately.
	err := h.flushWithTimeout()
	if err == nil {
		t.Error("concurrent flushWithTimeout() should return error")
	}

	// Unblock and clean up.
	close(blocked)
	<-done
}

func TestCircuitGatedHandler(t *testing.T) {
	inner := slog.NewJSONHandler(io.Discard, nil)
	open := atomic.Bool{}
	gated := &circuitGatedHandler{
		inner:       inner,
		circuitOpen: func() bool { return open.Load() },
	}

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0)

	// Circuit closed — should forward.
	if err := gated.Handle(context.Background(), r); err != nil {
		t.Errorf("Handle() error: %v", err)
	}

	// Circuit open — should skip.
	open.Store(true)
	if err := gated.Handle(context.Background(), r); err != nil {
		t.Errorf("Handle() with open circuit error: %v", err)
	}
}

// errForTest creates a simple error for testing.
type testError string

func (e testError) Error() string { return string(e) }

func errForTest(msg string) error { return testError(msg) }
