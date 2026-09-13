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

// Tests that do not need SQLite. Visible under the blocking
// `make test-fast` gate (go test -tags no_sqlite ./...).

package cmd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// TestLogBoundedErrors verifies that logBoundedErrors caps the output
// at the specified limit and reports the total count.
func TestLogBoundedErrors(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	errors := make([]string, 25)
	for i := range errors {
		errors[i] = "row error " + uuid.NewString()[:8]
	}

	logBoundedErrors("test-migration", errors, 5)

	logOutput := buf.String()

	// Should see exactly 5 individual error lines.
	individualCount := strings.Count(logOutput, "test-migration: row error")
	assert.Equal(t, 5, individualCount,
		"should log exactly 5 individual errors")

	// Should see the "more not shown" summary line.
	assert.Contains(t, logOutput, "20 more row errors not shown",
		"must report how many errors were suppressed")
}

// TestLogBoundedErrors_UnderLimit verifies that when errors are under
// the limit, all are logged and no "more" message appears.
func TestLogBoundedErrors_UnderLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	origLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(origLogger)

	errors := []string{"error one", "error two"}
	logBoundedErrors("test-migration", errors, 5)

	logOutput := buf.String()
	assert.NotContains(t, logOutput, "more row errors not shown",
		"should not show 'more' message when under limit")
}
