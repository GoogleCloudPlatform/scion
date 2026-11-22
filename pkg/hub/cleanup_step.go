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

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// errCleanupStepPanicked is returned by runBestEffortCleanupStep when fn panics
// and is recovered. Callers that distinguish clean completion from failure
// (for example settleLifecycleWrite) see a non-nil error without re-panicking.
var errCleanupStepPanicked = errors.New("best-effort cleanup step panicked")

// runBestEffortCleanupStep executes fn as a named best-effort post-status
// cleanup step with nil-safe ctx and logger normalization, a detached bounded
// context (context.WithoutCancel + timeout, defaulting to finalizationTimeout
// when timeout <= 0), and panic recovery.
//
// Caller-provided attrs must never include secrets. When fn panics, it logs
// panicMsg (or a default message naming step) at Warn with step, attrs, and the
// recovered value, and returns errCleanupStepPanicked. When fn returns a
// non-nil error, it logs at Warn with step, attrs, and error, and returns err.
// Callers that log errors with richer context or ignore benign sentinels (such
// as store.ErrNotFound) may handle them inside fn and return nil.
func runBestEffortCleanupStep(
	ctx context.Context,
	timeout time.Duration,
	logger *slog.Logger,
	step, panicMsg string,
	attrs []any,
	fn func(stepCtx context.Context) error,
) (err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = finalizationTimeout
	}
	defer func() {
		if r := recover(); r != nil {
			msg := panicMsg
			if msg == "" {
				msg = "Recovered panic in best-effort cleanup step"
			}
			logAttrs := make([]any, 0, len(attrs)+4)
			logAttrs = append(logAttrs, "step", step)
			logAttrs = append(logAttrs, attrs...)
			logAttrs = append(logAttrs, "panic", fmt.Sprint(r))
			logger.Warn(msg, logAttrs...)
			err = errCleanupStepPanicked
		}
	}()
	stepCtx, cancel := detachedContext(ctx, timeout)
	defer cancel()
	if err = fn(stepCtx); err != nil {
		logAttrs := make([]any, 0, len(attrs)+4)
		logAttrs = append(logAttrs, "step", step)
		logAttrs = append(logAttrs, attrs...)
		logAttrs = append(logAttrs, "error", err)
		logger.Warn("Best-effort cleanup step failed", logAttrs...)
	}
	return err
}

// lifecycleLogger returns s.agentLifecycleLog when configured, falling back to
// slog.Default() so best-effort cleanup and error paths never nil-dereference.
func (s *Server) lifecycleLogger() *slog.Logger {
	if s != nil && s.agentLifecycleLog != nil {
		return s.agentLifecycleLog
	}
	return slog.Default()
}
