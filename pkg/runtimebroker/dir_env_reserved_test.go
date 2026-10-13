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

package runtimebroker

import (
	"strings"
	"testing"
)

func TestExpandDirEnvEntry_ReservedHostEnvIsNotResolved(t *testing.T) {
	const sentinel = "reserved-host-value"
	t.Setenv("SCION_SERVER_SESSION_SECRET", sentinel)
	t.Setenv("SESSION_SECRET", sentinel)
	t.Setenv("SCION_DEV_TOKEN", sentinel)
	t.Setenv("SCION_AUTH_TOKEN", sentinel)
	t.Setenv("ORDINARY_VAR", "ordinary")

	for _, tc := range []struct{ key, value string }{
		{"SESSION_SECRET", ""},
		{"SCION_SERVER_SESSION_SECRET", ""},
		{"A", "${SESSION_SECRET}"},
		{"B", "$SCION_SERVER_SESSION_SECRET"},
		{"${SESSION_SECRET}", "v"},
		{"SCION_DEV_TOKEN", ""},
		{"SCION_AUTH_TOKEN", ""},
		{"D", "${SCION_AUTH_TOKEN}"},
	} {
		k, v, ok := expandDirEnvEntry(tc.key, tc.value)
		if ok || strings.Contains(k, sentinel) || strings.Contains(v, sentinel) {
			t.Errorf("expandDirEnvEntry(%q, %q) = (%q, len %d, %v); want dropped", tc.key, tc.value, k, len(v), ok)
		}
	}

	if k, v, ok := expandDirEnvEntry("ORDINARY_VAR", ""); !ok || k != "ORDINARY_VAR" || v != "ordinary" {
		t.Errorf("ordinary passthrough = (%q, %q, %v); want the host value", k, v, ok)
	}
	if _, v, ok := expandDirEnvEntry("C", "x-${SESSION_SECRET}-${ORDINARY_VAR}"); !ok || v != "x--ordinary" {
		t.Errorf("mixed expansion = (%q, %v); want %q", v, ok, "x--ordinary")
	}
}

func TestAltEnvKeySatisfied_RefusedHostEnvDoesNotSatisfy(t *testing.T) {
	for _, name := range []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN"} {
		t.Setenv(name, "refused-host-value")
		if altEnvKeySatisfied(nil, []string{name}) {
			t.Errorf("%s in this process's env satisfied an auth requirement", name)
		}
		if !altEnvKeySatisfied(map[string]string{name: "resolved"}, []string{name}) {
			t.Errorf("%s in the resolved env did not satisfy an auth requirement", name)
		}
	}
	t.Setenv("ORDINARY_CREDENTIAL_PATH", "/path")
	if !altEnvKeySatisfied(nil, []string{"MISSING_VAR", "ORDINARY_CREDENTIAL_PATH"}) {
		t.Error("an ordinary host env var did not satisfy an auth requirement")
	}
	if altEnvKeySatisfied(nil, []string{"MISSING_VAR"}) {
		t.Error("an unset var satisfied an auth requirement")
	}
}
