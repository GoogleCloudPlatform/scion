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
	"strings"
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
	clearAmbientScionEnv()
	os.Exit(m.Run())
}

// clearAmbientScionEnv unsets every SCION_* variable inherited from the
// environment before any cmd test runs. Inside an agent container these
// point at the live hub (SCION_HUB_ENDPOINT, SCION_AUTH_TOKEN, ...) and take
// precedence over the mock servers and temp projects the tests set up, so
// tests would call the real hub and fail with 401s or wrong request counts
// (ptone/scion#2102). Tests that need a value set it themselves with
// t.Setenv. SCION_TEST_* variables are deliberate test opt-ins (for example
// SCION_TEST_BASH) and are kept.
func clearAmbientScionEnv() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_") && !strings.HasPrefix(name, "SCION_TEST_") {
			os.Unsetenv(name)
		}
	}
}
