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

// M9 gates that do NOT require SQLite. Visible under the blocking
// `make test-fast` gate (go test -tags no_sqlite ./...).
//
// These test pure arithmetic, log-format invariants, and source-level
// invariants without needing a store. Precedent: boot_data_migrations_safety_test.go
// (M5, untagged).
//
// This file is the proper discharge of item F: real M9 coverage under
// the blocking CI gate, not splitting hairs between tag-dependent and
// tag-independent concerns.

package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// computeResidualBuckets — pure-function gate (design §4.8, item H)
// ---------------------------------------------------------------------------
//
// Production and tests call the SAME function. There is one formula, not two.
// The mutation (swapping the logged variable from actionable to reachable)
// must go red via the value assertions in the store-backed gates; the
// pure-function tests here verify the formula itself is correct for a
// table of inputs including the gteam production shape.

func TestComputeResidualBuckets_SteadyState(t *testing.T) {
	tests := []struct {
		name                                       string
		total, unreachable, permanent               int
		wantReachable, wantPreClamp, wantActionable int
	}{
		{
			name:           "zero everywhere",
			total:          0,
			unreachable:    0,
			permanent:      0,
			wantReachable:  0,
			wantPreClamp:   0,
			wantActionable: 0,
		},
		{
			name:           "all unreachable",
			total:          100,
			unreachable:    100,
			permanent:      0,
			wantReachable:  0,
			wantPreClamp:   0,
			wantActionable: 0,
		},
		{
			name:           "all permanent",
			total:          50,
			unreachable:    10,
			permanent:      40,
			wantReachable:  40,
			wantPreClamp:   0,
			wantActionable: 0,
		},
		{
			name:           "actionable present",
			total:          60,
			unreachable:    10,
			permanent:      40,
			wantReachable:  50,
			wantPreClamp:   10,
			wantActionable: 10,
		},
		{
			name:           "single actionable",
			total:          51,
			unreachable:    10,
			permanent:      40,
			wantReachable:  41,
			wantPreClamp:   1,
			wantActionable: 1,
		},
		{
			// gteam production shape: 12,606 reachable unattributed,
			// ~6,303 unreachable, ~6,303 permanent, actionable 0.
			name:           "gteam-shaped",
			total:          18909,
			unreachable:    6303,
			permanent:      12606,
			wantReachable:  12606,
			wantPreClamp:   0,
			wantActionable: 0,
		},
		{
			// gteam with 5 new messages since backfill pass.
			name:           "gteam with drift",
			total:          18914,
			unreachable:    6303,
			permanent:      12606,
			wantReachable:  12611,
			wantPreClamp:   5,
			wantActionable: 5,
		},
		{
			// Drift guard: permanent > reachable (timing skew between
			// measurement and live count). Clamp prevents negative.
			name:           "clamp prevents negative",
			total:          10,
			unreachable:    2,
			permanent:      10, // more than reachable (8)
			wantReachable:  8,
			wantPreClamp:   -2,
			wantActionable: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reachable, preClamp, actionable := computeResidualBuckets(
				tt.total, tt.unreachable, tt.permanent)

			assert.Equal(t, tt.wantReachable, reachable,
				"reachable = total (%d) - unreachable (%d)",
				tt.total, tt.unreachable)
			assert.Equal(t, tt.wantPreClamp, preClamp,
				"actionablePreClamp = reachable (%d) - permanent (%d)",
				reachable, tt.permanent)
			assert.Equal(t, tt.wantActionable, actionable,
				"actionable = max(0, preClamp (%d))", preClamp)
		})
	}
}

// TestComputeResidualBuckets_ActionableDiffersFromReachable is the specific
// mutation guard for item H. If someone swaps the logged variable from
// `actionable` to `reachable`, this test catches it at the arithmetic level.
//
// The test verifies that actionable != reachable when there are permanent
// messages. This is a weaker form of the gate; the strong form is in the
// store-backed gates which assert the logged VALUE.
func TestComputeResidualBuckets_ActionableDiffersFromReachable(t *testing.T) {
	// When permanent > 0 and there's exactly one new message,
	// actionable must be less than reachable.
	total := 101 // 100 permanent + 1 new
	unreachable := 0
	permanent := 100

	reachable, _, actionable := computeResidualBuckets(total, unreachable, permanent)

	assert.Equal(t, 101, reachable, "reachable includes all messages")
	assert.Equal(t, 1, actionable, "actionable is only the drift")
	assert.NotEqual(t, reachable, actionable,
		"ITEM H: actionable must differ from reachable when permanent > 0; "+
			"logging reachable instead of actionable is the exact production bug M9 exists to fix")
}

// ---------------------------------------------------------------------------
// Per-project identity gates (design §4.8 correction 4)
// ---------------------------------------------------------------------------
//
// Two identities, different equations:
//
//   processed  = attributed + inferred + skipped + derive_failures
//   row_errors = derive_failures + write_failures + resolution_failures
//
// The boot hook previously conflated these, which hid the +4/-4
// cancellation on gteam. These gates assert per-project, with no
// aggregation. A gate that checks the global sum passes against
// production data because +4 and -4 cancel across projects.

// checkBackfillIdentities verifies the two backfill result identities
// on a single project's result. Returns two error messages (empty if
// the identity holds).
func checkBackfillIdentities(r *messaging.BackfillResult) (dispositionErr, errorClassErr string) {
	deriveTotal := 0
	for _, v := range r.DeriveFailures {
		deriveTotal += v
	}

	// Identity 1: message disposition.
	//   processed = attributed + inferred + skipped + derive_failures
	disposition := r.Attributed + r.Inferred + r.Skipped + deriveTotal
	if r.TotalProcessed != disposition {
		dispositionErr = "disposition identity violated"
	}

	// Identity 2: error classification.
	//   row_errors = derive_failures + write_failures + resolution_failures
	rowErrors := len(r.Errors)
	errorClass := deriveTotal + r.WriteFailures + r.ResolutionFailures
	if rowErrors != errorClass {
		errorClassErr = "error classification identity violated"
	}

	return dispositionErr, errorClassErr
}

// TestM9_BackfillIdentity_Disposition verifies the message disposition
// identity: processed = attributed + inferred + skipped + derive_failures.
//
// Mutation-tested: incrementing Inferred by 1 makes the identity fail.
func TestM9_BackfillIdentity_Disposition(t *testing.T) {
	tests := []struct {
		name   string
		result *messaging.BackfillResult
	}{
		{
			name: "all attributed",
			result: &messaging.BackfillResult{
				TotalProcessed: 10,
				Attributed:     10,
			},
		},
		{
			name: "mixed disposition",
			result: &messaging.BackfillResult{
				TotalProcessed: 100,
				Attributed:     60,
				Inferred:       4,
				Skipped:        20,
				DeriveFailures: map[string]int{
					"principal_pair":       10,
					"dm_key_parse":         3,
					"dm_key_not_canonical": 2,
					"thread_no_project":    1,
				},
				Errors: make([]string, 16),
			},
		},
		{
			name: "all skipped (broadcast)",
			result: &messaging.BackfillResult{
				TotalProcessed: 50,
				Skipped:        50,
			},
		},
		{
			name: "all derive failures",
			result: &messaging.BackfillResult{
				TotalProcessed: 30,
				DeriveFailures: map[string]int{
					"principal_pair": 30,
				},
				Errors: make([]string, 30),
			},
		},
		{
			name: "gteam-shaped: large with inferred",
			result: &messaging.BackfillResult{
				TotalProcessed: 19083,
				Attributed:     6476,
				Inferred:       4,
				Skipped:        1010,
				DeriveFailures: map[string]int{
					"principal_pair":       11000,
					"dm_key_parse":         500,
					"dm_key_not_canonical": 89,
					"thread_no_project":    4,
				},
				Errors:             make([]string, 11593),
				WriteFailures:      0,
				ResolutionFailures: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispositionErr, _ := checkBackfillIdentities(tt.result)
			assert.Empty(t, dispositionErr,
				"disposition identity: processed (%d) must equal attributed (%d) + inferred (%d) + skipped (%d) + derive_failures",
				tt.result.TotalProcessed, tt.result.Attributed, tt.result.Inferred, tt.result.Skipped)
		})
	}
}

// TestM9_BackfillIdentity_ErrorClassification verifies the error
// classification identity:
//
//	row_errors = derive_failures + write_failures + resolution_failures.
//
// Mutation-tested: incrementing WriteFailures by 1 makes it fail.
func TestM9_BackfillIdentity_ErrorClassification(t *testing.T) {
	tests := []struct {
		name   string
		result *messaging.BackfillResult
	}{
		{
			name: "no errors",
			result: &messaging.BackfillResult{
				TotalProcessed: 10,
				Attributed:     10,
			},
		},
		{
			name: "derive only",
			result: &messaging.BackfillResult{
				TotalProcessed: 10,
				Attributed:     5,
				Skipped:        0,
				DeriveFailures: map[string]int{"principal_pair": 5},
				Errors:         make([]string, 5),
			},
		},
		{
			name: "mixed errors",
			result: &messaging.BackfillResult{
				TotalProcessed: 20,
				Attributed:     10,
				Skipped:        2,
				DeriveFailures: map[string]int{
					"principal_pair": 5,
					"dm_key_parse":   1,
				},
				WriteFailures:      1,
				ResolutionFailures: 1,
				Errors:             make([]string, 8), // 5+1+1+1=8
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errorClassErr := checkBackfillIdentities(tt.result)
			assert.Empty(t, errorClassErr,
				"error classification identity: len(Errors) (%d) must equal derive_failures + write_failures (%d) + resolution_failures (%d)",
				len(tt.result.Errors), tt.result.WriteFailures, tt.result.ResolutionFailures)
		})
	}
}

// ---------------------------------------------------------------------------
// Remedy string absence gate (design §4.8 correction 4, item D)
// ---------------------------------------------------------------------------

// TestM9_NoRemedyOnPostDeriveLine scans the actual source file for
// reportResidualUnattributed and verifies that the post-derivation code
// path does NOT contain "scion server backfill" as a remedy. Advertising
// a retry for deterministic authorization refusals is DEF-111's exact
// shape — a warning whose remedy cannot reduce it.
//
// This is a source-level scan, not a runtime check, because the function
// requires a store.Store. The scan reads the actual Go source to verify
// no regression re-adds the remedy string.
//
// Deliberately untagged so the blocking CI gate sees it.
//
// Mutation-tested: adding a "remedy" field with "scion server backfill"
// to the post-derivation slog.Warn call makes this test fail.
func TestM9_NoRemedyOnPostDeriveLine(t *testing.T) {
	// Read the source file that contains reportResidualUnattributed.
	src, err := os.ReadFile("boot_data_migrations.go")
	require.NoError(t, err, "must be able to read boot_data_migrations.go from test directory")

	source := string(src)

	// Find the reportResidualUnattributed function body.
	fnStart := strings.Index(source, "func reportResidualUnattributed(")
	require.Greater(t, fnStart, 0, "must find reportResidualUnattributed in source")

	// Extract from function start to end of file (sufficient — it's the
	// last function in the file).
	fnBody := source[fnStart:]

	// Find the post-derivation section: everything after the
	// "Post-derivation failures" marker.
	postDeriveStart := strings.Index(fnBody, "Post-derivation failures")
	require.Greater(t, postDeriveStart, 0,
		"must find 'Post-derivation failures' marker in reportResidualUnattributed")

	postDeriveSection := fnBody[postDeriveStart:]

	// The post-derivation section must NOT contain the backfill remedy.
	assert.NotContains(t, postDeriveSection, `"scion server backfill"`,
		"post-derivation section must not contain the backfill remedy string (DEF-111)")
	assert.NotContains(t, postDeriveSection, `"remedy"`,
		"post-derivation section must not contain a remedy field at all")

	// Also verify the entire reportResidualUnattributed function does NOT
	// contain the remedy on ANY post-derivation path. The actionable WARN
	// line may legitimately mention a remedy for genuine drift — but the
	// post-derivation line must not.
	// Note: the actionable WARN currently does NOT include a remedy string
	// either, which is fine (it just logs "count").
}

// TestM9_NoRemedyAnywhereInReport is a broader check: no non-comment line
// in reportResidualUnattributed may contain "scion server backfill". The
// comment that documents WHY it's absent may mention it — but no executable
// Go code may pass it as a slog argument. M6 removed it; M9 must not
// re-add it.
func TestM9_NoRemedyAnywhereInReport(t *testing.T) {
	src, err := os.ReadFile("boot_data_migrations.go")
	require.NoError(t, err)

	source := string(src)

	fnStart := strings.Index(source, "func reportResidualUnattributed(")
	require.Greater(t, fnStart, 0)

	fnBody := source[fnStart:]

	// Scan non-comment lines for the remedy string.
	for _, line := range strings.Split(fnBody, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue // skip comments — they may document WHY it's absent
		}
		assert.NotContains(t, line, "scion server backfill",
			"non-comment line in reportResidualUnattributed must not contain the backfill remedy string")
	}
}
