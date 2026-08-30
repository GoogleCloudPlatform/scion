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

// NO build tag — this file runs in every CI lane, including
// `go test -tags no_sqlite ./...` (the blocking lane). It guards the
// single most dangerous property: that `scion server backfill` defaults
// to dry-run. The companion TestBackfillDefaultIsDryRun_FlagWiring in
// server_backfill_test.go verifies end-to-end with a real store but
// lives behind !no_sqlite (advisory lane). This test needs no store.

package cmd

import (
	"testing"
)

// TestBackfillExecuteFlagDefaultIsFalse asserts that the --execute flag
// on the backfill command defaults to "false". If someone changes the
// flag registration to default true, an operator typing
// `scion server backfill` to preview would silently mutate the database.
func TestBackfillExecuteFlagDefaultIsFalse(t *testing.T) {
	f := serverBackfillCmd.Flags().Lookup("execute")
	if f == nil {
		t.Fatal("--execute flag not registered on serverBackfillCmd")
	}
	if f.DefValue != "false" {
		t.Fatalf("--execute default must be \"false\" (dry-run); got %q", f.DefValue)
	}
}

// TestBackfillDryRunDerivation calls the production backfillConfigFromFlags
// function and asserts DryRun is the logical inverse of backfillExecute.
// A wiring bug that sets DryRun = backfillExecute (instead of
// !backfillExecute) would make the default perform mutations.
func TestBackfillDryRunDerivation(t *testing.T) {
	// Save and restore the global.
	orig := backfillExecute
	defer func() { backfillExecute = orig }()

	// Default state: --execute not passed → DryRun must be true.
	backfillExecute = false
	if !backfillConfigFromFlags().DryRun {
		t.Fatal("DryRun must be true when backfillExecute is false (default)")
	}

	// Explicit --execute → DryRun must be false.
	backfillExecute = true
	if backfillConfigFromFlags().DryRun {
		t.Fatal("DryRun must be false when backfillExecute is true (--execute passed)")
	}
}
