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
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	gcplog "cloud.google.com/go/logging"
)

// circuitGatedHandler wraps any slog.Handler and skips Handle calls when
// the circuit breaker is open. Used by request/message loggers to share
// circuit breaker state with the main handler.
type circuitGatedHandler struct {
	inner       slog.Handler
	circuitOpen func() bool
}

func (h *circuitGatedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *circuitGatedHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.circuitOpen() {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *circuitGatedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &circuitGatedHandler{inner: h.inner.WithAttrs(attrs), circuitOpen: h.circuitOpen}
}

func (h *circuitGatedHandler) WithGroup(name string) slog.Handler {
	return &circuitGatedHandler{inner: h.inner.WithGroup(name), circuitOpen: h.circuitOpen}
}

// circuitState represents the state of the circuit breaker.
type circuitState int32

const (
	// circuitClosed means Cloud Logging is operating normally.
	circuitClosed circuitState = iota
	// circuitOpen means Cloud Logging is unavailable; entries are dropped
	// (local logging continues via the multiHandler).
	circuitOpen
	// circuitHalfOpen means the circuit breaker is probing to see if
	// Cloud Logging has recovered.
	circuitHalfOpen
)

// Default circuit breaker configuration values.
const (
	DefaultMaxFailures    = 3
	DefaultOpenDuration   = 60 * time.Second
	DefaultProbeInterval  = 30 * time.Second
	DefaultProbeTimeout   = 10 * time.Second
)

// ResilientCloudHandlerConfig configures the circuit breaker behavior.
type ResilientCloudHandlerConfig struct {
	// MaxFailures is the number of consecutive flush failures before the
	// circuit opens. Default: 3.
	MaxFailures int
	// OpenDuration is how long the circuit stays open before transitioning
	// to half-open for a probe. Default: 60s.
	OpenDuration time.Duration
	// ProbeInterval is how often the background goroutine checks health
	// when the circuit is closed. Default: 30s.
	ProbeInterval time.Duration
	// ProbeTimeout is the context timeout for probe flush calls. Default: 10s.
	ProbeTimeout time.Duration
}

func (c *ResilientCloudHandlerConfig) applyDefaults() {
	if c.MaxFailures <= 0 {
		c.MaxFailures = DefaultMaxFailures
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = DefaultOpenDuration
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = DefaultProbeInterval
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = DefaultProbeTimeout
	}
}

// circuitBreaker holds the shared circuit breaker state. It is allocated
// once and shared by pointer across all handlers derived via WithAttrs /
// WithGroup so that opening the circuit affects all of them.
type circuitBreaker struct {
	state atomic.Int32 // circuitState

	mu              sync.Mutex
	failures        int
	lastStateChange time.Time
}

// ResilientCloudHandler wraps a CloudHandler with circuit breaker logic.
//
// When Cloud Logging is healthy (circuit closed), log entries are forwarded
// to the inner handler normally. A background goroutine periodically flushes
// the Cloud Logging buffer; consecutive flush failures cause the circuit to
// open.
//
// When the circuit is open, Handle() returns nil immediately — entries are
// silently dropped from the Cloud Logging path. Since the caller uses a
// multiHandler, local logging continues unaffected. After OpenDuration
// elapses, the circuit transitions to half-open and a probe flush is
// attempted. If the probe succeeds, the circuit closes and Cloud Logging
// resumes. If it fails, the circuit reopens.
type ResilientCloudHandler struct {
	inner         *CloudHandler
	logger        *gcplog.Logger
	config        ResilientCloudHandlerConfig
	cb            *circuitBreaker
	flushInFlight atomic.Bool
	flushFn       func() error // overridable for testing; defaults to h.logger.Flush

	done chan struct{}
	wg   sync.WaitGroup
}

// NewResilientCloudHandler wraps the given CloudHandler with circuit breaker
// protection. Call the returned cleanup function to stop the background
// health-check goroutine.
func NewResilientCloudHandler(inner *CloudHandler, cfg ResilientCloudHandlerConfig) (*ResilientCloudHandler, func()) {
	cfg.applyDefaults()

	cb := &circuitBreaker{}
	cb.state.Store(int32(circuitClosed))

	h := &ResilientCloudHandler{
		inner:  inner,
		logger: inner.logger,
		config: cfg,
		cb:     cb,
		done:   make(chan struct{}),
	}
	h.flushFn = func() error { return h.logger.Flush() }

	h.wg.Add(1)
	go h.healthCheckLoop()

	cleanup := func() {
		close(h.done)
		h.wg.Wait()
	}
	return h, cleanup
}

// Enabled implements slog.Handler.
func (h *ResilientCloudHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// When the circuit is open we still report as enabled — the multiHandler
	// needs to query us, and we handle the gating in Handle(). Returning
	// false here would cause the multiHandler to skip *all* handlers if we
	// were the only one enabled at that level.
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
//
// When the circuit is open, the entry is silently dropped (returns nil).
// The caller's multiHandler ensures local logging still occurs.
func (h *ResilientCloudHandler) Handle(ctx context.Context, r slog.Record) error {
	state := circuitState(h.cb.state.Load())
	if state == circuitOpen || state == circuitHalfOpen {
		// Circuit is open or probing — don't feed more entries into the
		// Cloud Logging buffer to avoid resource accumulation.
		return nil
	}
	// Circuit is closed — forward to inner handler.
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *ResilientCloudHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newInner := h.inner.WithAttrs(attrs).(*CloudHandler)
	return &ResilientCloudHandler{
		inner:  newInner,
		logger: h.logger,
		config: h.config,
		cb:     h.cb, // share circuit state across derived handlers
		done:   h.done,
	}
}

// WithGroup implements slog.Handler.
func (h *ResilientCloudHandler) WithGroup(name string) slog.Handler {
	newInner := h.inner.WithGroup(name).(*CloudHandler)
	return &ResilientCloudHandler{
		inner:  newInner,
		logger: h.logger,
		config: h.config,
		cb:     h.cb,
		done:   h.done,
	}
}

// Client returns the underlying Cloud Logging client for reuse.
func (h *ResilientCloudHandler) Client() *gcplog.Client {
	return h.inner.Client()
}

// CircuitOpen returns true if the circuit breaker is currently open.
// Exposed for observability and testing.
func (h *ResilientCloudHandler) CircuitOpen() bool {
	return circuitState(h.cb.state.Load()) != circuitClosed
}

// healthCheckLoop runs in a background goroutine. It periodically flushes
// the Cloud Logging buffer to detect backend failures. When enough
// consecutive failures accumulate, it opens the circuit. When the circuit
// is open, it waits for OpenDuration then probes to check recovery.
func (h *ResilientCloudHandler) healthCheckLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(h.config.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.runHealthCheck()
		}
	}
}

// runHealthCheck performs a single health check cycle.
func (h *ResilientCloudHandler) runHealthCheck() {
	state := circuitState(h.cb.state.Load())

	switch state {
	case circuitClosed:
		// Flush to detect failures.
		if err := h.flushWithTimeout(); err != nil {
			h.recordFailure(err)
		} else {
			h.recordSuccess()
		}

	case circuitOpen:
		// Check if enough time has passed to probe.
		h.cb.mu.Lock()
		elapsed := time.Since(h.cb.lastStateChange)
		h.cb.mu.Unlock()
		if elapsed >= h.config.OpenDuration {
			h.transitionTo(circuitHalfOpen)
			// Immediately try a probe.
			if err := h.flushWithTimeout(); err != nil {
				h.transitionTo(circuitOpen)
				fmt.Fprintf(os.Stderr, "cloud logging probe failed, circuit remains open: %v\n", err)
			} else {
				h.transitionTo(circuitClosed)
			}
		}

	case circuitHalfOpen:
		// Shouldn't normally be here (half-open is transient), but handle
		// it the same as a probe.
		if err := h.flushWithTimeout(); err != nil {
			h.transitionTo(circuitOpen)
		} else {
			h.transitionTo(circuitClosed)
		}
	}
}

// flushWithTimeout calls Flush on the Cloud Logging logger with a timeout.
// Only one flush runs at a time; concurrent calls return an error immediately.
func (h *ResilientCloudHandler) flushWithTimeout() error {
	if !h.flushInFlight.CompareAndSwap(false, true) {
		return fmt.Errorf("a previous flush is still in progress")
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.config.ProbeTimeout)
	defer cancel()

	// Logger.Flush() doesn't accept a context, so we race it against our
	// timeout. This prevents Flush from hanging indefinitely when the
	// metadata service is unreachable.
	errCh := make(chan error, 1)
	go func() {
		defer h.flushInFlight.Store(false)
		errCh <- h.flushFn()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("cloud logging flush timed out after %v", h.config.ProbeTimeout)
	}
}

// recordFailure tracks a flush failure and opens the circuit if the
// threshold is reached.
func (h *ResilientCloudHandler) recordFailure(err error) {
	h.cb.mu.Lock()
	h.cb.failures++
	opened := false
	failures := h.cb.failures
	if h.cb.failures >= h.config.MaxFailures && circuitState(h.cb.state.Load()) == circuitClosed {
		h.cb.state.Store(int32(circuitOpen))
		h.cb.lastStateChange = time.Now()
		opened = true
	}
	h.cb.mu.Unlock()

	if opened {
		fmt.Fprintf(os.Stderr,
			"WARNING: Cloud Logging circuit breaker opened after %d consecutive failures (last error: %v). Falling back to local-only logging.\n",
			failures, err)
		slog.Warn("Cloud Logging unavailable, falling back to local-only logging",
			"consecutive_failures", failures,
			"error", err.Error(),
		)
	}
}

// recordSuccess resets the failure counter. Called when a flush succeeds
// while the circuit is closed.
func (h *ResilientCloudHandler) recordSuccess() {
	h.cb.mu.Lock()
	defer h.cb.mu.Unlock()
	h.cb.failures = 0
}

// transitionTo atomically transitions the circuit to the given state and
// logs the transition.
func (h *ResilientCloudHandler) transitionTo(newState circuitState) {
	h.cb.mu.Lock()
	oldState := circuitState(h.cb.state.Load())
	if oldState == newState {
		h.cb.mu.Unlock()
		return
	}

	h.cb.state.Store(int32(newState))
	h.cb.lastStateChange = time.Now()
	if newState == circuitClosed {
		h.cb.failures = 0
	}
	h.cb.mu.Unlock()

	switch newState {
	case circuitClosed:
		slog.Info("Cloud Logging circuit breaker closed: Cloud Logging resumed")
	case circuitOpen:
		slog.Warn("Cloud Logging circuit breaker opened: falling back to local-only logging")
	case circuitHalfOpen:
		slog.Info("Cloud Logging circuit breaker half-open: probing Cloud Logging")
	}
}
