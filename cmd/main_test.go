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
	"os"
	"testing"
)

// TestMain disables the UTC pin for the whole cmd test binary.
//
// Production run functions pin time.Local to UTC; in the shared test binary
// that would race leaked goroutines (other tests' background work reads
// time.Local concurrently) and would silently turn every later TZ=... test
// in this binary into a UTC test, masking real timezone bugs. pkg/util's
// TestPinProcessUTC keeps covering PinProcessUTC itself, and
// pin_process_utc_test.go's AST test covers where the seam is called from.
func TestMain(m *testing.M) {
	pinProcessUTC = func() {}
	os.Exit(m.Run())
}
