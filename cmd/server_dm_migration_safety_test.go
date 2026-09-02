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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
)

// TestDMMigrationReport verifies the report output format.
func TestDMMigrationReport(t *testing.T) {
	var buf bytes.Buffer
	r := &messaging.DMMigrationResult{
		TotalScanned:      3,
		ParticipantsAdded: 2,
		OldFormatRekeyed:  1,
		EmptyRefSkipped:   1,
		Unparseable:       0,
		Ambiguous:         0,
		Errors:            []string{"test error"},
	}

	// Set execute mode for the report.
	origExecute := dmMigrationExecute
	dmMigrationExecute = true
	defer func() { dmMigrationExecute = origExecute }()

	printDMMigrationReport(&buf, r)

	output := buf.String()
	assert.Contains(t, output, "DM Key Migration Report")
	assert.Contains(t, output, "execute")
	assert.Contains(t, output, "Conversations scanned: 3")
	assert.Contains(t, output, "Participants added:  2")
	assert.Contains(t, output, "Old-format re-keyed: 1")
	assert.Contains(t, output, "Empty-ref skipped:   1")
	assert.Contains(t, output, "Errors:                1")
	assert.Contains(t, output, "test error")
}

// TestDMMigrationConfigFromFlags verifies the flag-to-config mapping.
// This test is critical for the default-is-dry-run safety property and
// runs under the no_sqlite gate.
func TestDMMigrationConfigFromFlags(t *testing.T) {
	origExecute := dmMigrationExecute
	origBatch := dmMigrationBatchSize
	defer func() {
		dmMigrationExecute = origExecute
		dmMigrationBatchSize = origBatch
	}()

	// Default: dry-run.
	dmMigrationExecute = false
	dmMigrationBatchSize = 0
	cfg := dmMigrationConfigFromFlags()
	assert.True(t, cfg.DryRun, "default must be dry-run")
	assert.Equal(t, 0, cfg.BatchSize)

	// With --execute.
	dmMigrationExecute = true
	dmMigrationBatchSize = 50
	cfg = dmMigrationConfigFromFlags()
	assert.False(t, cfg.DryRun, "--execute should set DryRun=false")
	assert.Equal(t, 50, cfg.BatchSize)
}

// TestDMMigrationReportBoundsErrors verifies that the error output is bounded
// to 20 entries to prevent flooding on large runs.
func TestDMMigrationReportBoundsErrors(t *testing.T) {
	var buf bytes.Buffer
	errs := make([]string, 30)
	for i := range errs {
		errs[i] = "error line"
	}
	r := &messaging.DMMigrationResult{
		TotalScanned: 30,
		Errors:       errs,
	}

	origExecute := dmMigrationExecute
	dmMigrationExecute = false
	defer func() { dmMigrationExecute = origExecute }()

	printDMMigrationReport(&buf, r)

	output := buf.String()
	assert.Contains(t, output, "... and 10 more",
		"error output must be bounded to 20 entries")
}
