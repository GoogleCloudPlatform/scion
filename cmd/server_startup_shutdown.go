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

package cmd

import (
	"context"
	"errors"
	"log/slog"
)

// startupShutdownErr returns the error a startup step of `server start`
// reports, or nil when the step only stopped because the requested
// shutdown cancelled the server context. A `systemctl stop` (or restart)
// during startup, for example during the first boot's migrations
// ("failed to run migrations: context canceled"), then ends the process
// with exit status 0, so systemd records the unit as stopped, not failed,
// and its OnFailure= boot-failure guard does not run.
//
// The decision is keyed on the shutdown, not on the error alone: err is
// cleared only when
//   - shutdownBegan: the shutdown sequence has begun (a signal or the
//     foreground broker's SIGTERM handler claimed it);
//   - ctxErr, the server context's Err(), is context.Canceled: the
//     shutdown's cancel reached the context the step ran under;
//   - err wraps context.Canceled and not context.DeadlineExceeded.
//
// Every other case returns err unchanged: a cancellation with no shutdown
// requested, a deadline, or any other failure still exits non-zero.
func startupShutdownErr(err error, shutdownBegan bool, ctxErr error) error {
	if err == nil || !shutdownBegan || !errors.Is(ctxErr, context.Canceled) {
		return err
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	slog.Info("shutdown requested during startup; exiting cleanly", "error", err)
	return nil
}

// startupErr applies startupShutdownErr with the state of the server's
// shutdown sequence q and its context ctx.
func (q *serverShutdownSequence) startupErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	_, began := q.startedAt()
	return startupShutdownErr(err, began, ctx.Err())
}
