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

package nativetelemetrytest

import (
	"os"
	"testing"
)

func TestClearEnvUnsetsPolicyKeysAndKeepsOthers(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
	t.Setenv("CODEX_HOME", "/tmp/codex")
	t.Setenv("SCION_NATIVE_TELEMETRY_TEST_PROBE", "x")
	t.Setenv("NATIVETELEMETRYTEST_UNRELATED", "keep")
	t.Run("cleared", func(t *testing.T) {
		ClearEnv(t)
		for _, key := range []string{"CLAUDE_CODE_ENABLE_TELEMETRY", "CODEX_HOME", "SCION_NATIVE_TELEMETRY_TEST_PROBE"} {
			if v, ok := os.LookupEnv(key); ok {
				t.Errorf("%s still set to %q", key, v)
			}
		}
		if os.Getenv("NATIVETELEMETRYTEST_UNRELATED") != "keep" {
			t.Error("unrelated variable was cleared")
		}
	})
	if os.Getenv("CLAUDE_CODE_ENABLE_TELEMETRY") != "1" {
		t.Error("ClearEnv did not restore CLAUDE_CODE_ENABLE_TELEMETRY after the subtest")
	}
}
