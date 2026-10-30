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
	"fmt"
	"syscall"
	"testing"
)

func TestStartupShutdownErr(t *testing.T) {
	migrationsCanceled := fmt.Errorf("failed to run migrations: %w", context.Canceled)
	tests := []struct {
		name          string
		err           error
		shutdownBegan bool
		ctxErr        error
		wantNil       bool
	}{
		{"shutdown began, step canceled: clean exit", migrationsCanceled, true, context.Canceled, true},
		{"shutdown began, wrapped hub init cancel: clean exit",
			fmt.Errorf("hub server failed to start: %w", migrationsCanceled), true, context.Canceled, true},
		{"canceled without a shutdown", migrationsCanceled, false, context.Canceled, false},
		{"canceled while the server context is still live", migrationsCanceled, true, nil, false},
		{"other error after the shutdown began", errors.New("failed to open database: disk I/O error"), true, context.Canceled, false},
		{"deadline exceeded after the shutdown began", fmt.Errorf("failed to run migrations: %w", context.DeadlineExceeded), true, context.Canceled, false},
		{"deadline exceeded without a shutdown", context.DeadlineExceeded, false, nil, false},
		{"canceled and deadline joined", errors.Join(context.Canceled, context.DeadlineExceeded), true, context.Canceled, false},
		{"server context deadline, not a cancel", migrationsCanceled, true, context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startupShutdownErr(tt.err, tt.shutdownBegan, tt.ctxErr)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("startupShutdownErr = %v, want nil (exit 0)", got)
				}
				return
			}
			if got != tt.err {
				t.Fatalf("startupShutdownErr = %v, want the original error %v", got, tt.err)
			}
			if code := exitCodeFor(got); code != 1 {
				t.Fatalf("exitCodeFor = %d, want 1", code)
			}
		})
	}
	if startupShutdownErr(nil, true, context.Canceled) != nil {
		t.Fatal("a nil error must stay nil")
	}
}

func TestServerShutdownSequence_StartupErr(t *testing.T) {
	stepErr := fmt.Errorf("failed to run migrations: %w", context.Canceled)

	t.Run("before the shutdown begins the error is kept", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		q := newServerShutdownSequence(cancel, shutdownClock)
		cancel() // canceled, but not by a requested shutdown
		if got := q.startupErr(ctx, stepErr); got != stepErr {
			t.Fatalf("startupErr = %v, want %v", got, stepErr)
		}
	})

	t.Run("after the requested shutdown the step's cancel is a clean exit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q := newServerShutdownSequence(cancel, shutdownClock)
		q.begin(syscall.SIGTERM) // no steps registered: claims, logs, cancels ctx
		if ctx.Err() == nil {
			t.Fatal("the shutdown did not cancel the server context")
		}
		if got := q.startupErr(ctx, stepErr); got != nil {
			t.Fatalf("startupErr = %v, want nil", got)
		}
		other := errors.New("failed to open database: permission denied")
		if got := q.startupErr(ctx, other); got != other {
			t.Fatalf("startupErr(other) = %v, want it unchanged", got)
		}
		if got := q.startupErr(ctx, nil); got != nil {
			t.Fatalf("startupErr(nil) = %v", got)
		}
	})
}
