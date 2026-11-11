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

// Package nativetelemetrytest provides test helpers for code that runs
// under the sciontool native telemetry policy. It is internal so that only
// in-tree packages can import it, and it is imported only by tests.
package nativetelemetrytest

import (
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// ClearEnv unsets every inherited variable that the native
// telemetry policy inspects (see hooks.ValidateNativeTelemetryEnv), plus
// SCION_* runtime contract variables, for the duration of the test. Agent
// containers export several of these (OTEL_EXPORTER_OTLP_*, CODEX_HOME,
// CLAUDE_CODE_ENABLE_TELEMETRY, ...), so without this the tests depend on
// the ambient environment. t.Setenv restores the original values, which
// also means callers cannot use t.Parallel.
func ClearEnv(t testing.TB) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !testKey(key) {
			continue
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// testKey reports whether a test must clear key: every key
// the policy itself reserves (shared with hooks so the lists cannot drift),
// plus CODEX_HOME (conditionally protected by ValidateNativeTelemetryEnv)
// and SCION_* runtime contract variables, including the policy marker.
func testKey(key string) bool {
	return hooks.IsReservedNativeTelemetryKey(key) || key == "CODEX_HOME" || strings.HasPrefix(key, "SCION_")
}
