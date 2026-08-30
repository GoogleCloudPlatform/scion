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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// backfillStubStore is a minimal store.Store stub for testing
// maybeWarnUnbackfilledMessages. Only CountUnbackfilledMessages is implemented;
// all other methods panic if called, which is fine because the function under
// test only calls that single method.
type backfillStubStore struct {
	store.Store // embed nil interface to satisfy the type
	count       int
	err         error
}

func (s *backfillStubStore) CountUnbackfilledMessages(_ context.Context, _ string) (int, error) {
	return s.count, s.err
}

// captureWarnLogs installs a slog handler that captures WARN-level output into
// the returned buffer, and returns a cleanup function that restores the default.
func captureWarnLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return buf, func() { slog.SetDefault(prev) }
}

// AC-12-1 positive: when unbackfilled messages exist, a warning IS logged with
// count and remediation command.
func TestMaybeWarnUnbackfilledMessages_Positive(t *testing.T) {
	buf, cleanup := captureWarnLogs(t)
	defer cleanup()

	stub := &backfillStubStore{count: 42}
	maybeWarnUnbackfilledMessages(context.Background(), stub)

	out := buf.String()
	if !strings.Contains(out, "Messages without conversation attribution detected") {
		t.Fatalf("expected warning about unbackfilled messages, got: %s", out)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("expected count=42 in warning, got: %s", out)
	}
	if !strings.Contains(out, "scion server backfill") {
		t.Fatalf("expected remediation command in warning, got: %s", out)
	}
}

// AC-12-1 negative: when all messages have a conversation_id, NO warning is
// logged (no spurious warning).
func TestMaybeWarnUnbackfilledMessages_NoSpuriousWarning(t *testing.T) {
	buf, cleanup := captureWarnLogs(t)
	defer cleanup()

	stub := &backfillStubStore{count: 0}
	maybeWarnUnbackfilledMessages(context.Background(), stub)

	out := buf.String()
	if strings.Contains(out, "Messages without conversation attribution detected") {
		t.Fatalf("expected no warning when count=0, got: %s", out)
	}
}

// AC-12-7 mutation test: if CountUnbackfilledMessages always returns 0, the
// positive AC-12-1 case above (TestMaybeWarnUnbackfilledMessages_Positive)
// fails. This test proves that by confirming a count > 0 is required to
// trigger the warning; a zero-count stub produces no warning.
func TestMaybeWarnUnbackfilledMessages_MutationGuard(t *testing.T) {
	buf, cleanup := captureWarnLogs(t)
	defer cleanup()

	// Simulate the mutation: CountUnbackfilledMessages always returns 0.
	stub := &backfillStubStore{count: 0}
	maybeWarnUnbackfilledMessages(context.Background(), stub)

	out := buf.String()
	if strings.Contains(out, "Messages without conversation attribution detected") {
		t.Fatal("mutation guard failed: warning appeared even with count=0, " +
			"meaning the positive test cannot catch a mutation that forces count to 0")
	}
}

// TestMaybeWarnUnbackfilledMessages_ErrorHandling verifies that a store error
// is handled gracefully (warning logged, no panic).
func TestMaybeWarnUnbackfilledMessages_ErrorHandling(t *testing.T) {
	buf, cleanup := captureWarnLogs(t)
	defer cleanup()

	stub := &backfillStubStore{err: context.DeadlineExceeded}
	maybeWarnUnbackfilledMessages(context.Background(), stub)

	out := buf.String()
	if !strings.Contains(out, "Failed to check for unbackfilled messages") {
		t.Fatalf("expected error warning, got: %s", out)
	}
	// Must NOT contain the backfill action message on error path.
	if strings.Contains(out, "Messages without conversation attribution detected") {
		t.Fatalf("should not show backfill warning on error, got: %s", out)
	}
}
