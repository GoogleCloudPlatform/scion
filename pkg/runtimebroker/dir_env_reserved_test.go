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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

func TestBundleEnvLayer_HostEnvIsNeverExpandedOrPassedThrough(t *testing.T) {
	const sentinel = "reserved-host-value"
	t.Setenv("SCION_SERVER_SESSION_SECRET", sentinel)
	t.Setenv("SESSION_SECRET", sentinel)
	t.Setenv("SCION_DEV_TOKEN", sentinel)
	t.Setenv("SCION_AUTH_TOKEN", sentinel)
	t.Setenv("ORDINARY_VAR", "ordinary")

	raw := map[string]string{
		"SESSION_SECRET":              "",
		"SCION_SERVER_SESSION_SECRET": "",
		"A":                           "${SESSION_SECRET}",
		"B":                           "$SCION_SERVER_SESSION_SECRET",
		"SCION_DEV_TOKEN":             "",
		"SCION_AUTH_TOKEN":            "",
		"D":                           "${SCION_AUTH_TOKEN}",
		"ORDINARY_VAR":                "",
		"C":                           "x-${SESSION_SECRET}-${ORDINARY_VAR}",
	}
	got, warnings := agent.BundleEnvLayer(raw)
	for k, v := range got {
		if strings.Contains(k, sentinel) || strings.Contains(v, sentinel) {
			t.Errorf("BundleEnvLayer leaked host env into %s=%q", k, v)
		}
	}
	// Reserved SCION_* keys are refused.
	for _, scionKey := range []string{"SCION_SERVER_SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN"} {
		if _, ok := got[scionKey]; ok {
			t.Errorf("BundleEnvLayer kept reserved key %q", scionKey)
		}
	}
	// Empty values stay empty (never read from broker host).
	if v, ok := got["ORDINARY_VAR"]; !ok || v != "" {
		t.Errorf("ORDINARY_VAR = (%q, %v), want (\"\", true)", v, ok)
	}
	// Variable syntax values are kept verbatim and emit warnings.
	if got["A"] != "${SESSION_SECRET}" {
		t.Errorf("A = %q, want literal %q", got["A"], "${SESSION_SECRET}")
	}
	if got["C"] != "x-${SESSION_SECRET}-${ORDINARY_VAR}" {
		t.Errorf("C = %q, want literal %q", got["C"], "x-${SESSION_SECRET}-${ORDINARY_VAR}")
	}
	if len(warnings) == 0 {
		t.Error("expected warnings for reserved SCION_* keys and variable syntax")
	}
}

func TestAltEnvKeySatisfied_RefusedHostEnvDoesNotSatisfy(t *testing.T) {
	for _, name := range []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN"} {
		t.Setenv(name, "refused-host-value")
		if altEnvKeySatisfied(nil, []string{name}, true) {
			t.Errorf("%s in this process's env satisfied an auth requirement", name)
		}
		if !altEnvKeySatisfied(map[string]string{name: "resolved"}, []string{name}, true) {
			t.Errorf("%s in the resolved env did not satisfy an auth requirement", name)
		}
	}
	t.Setenv("ORDINARY_CREDENTIAL_PATH", "/path")
	if !altEnvKeySatisfied(nil, []string{"MISSING_VAR", "ORDINARY_CREDENTIAL_PATH"}, true) {
		t.Error("an ordinary host env var did not satisfy an auth requirement")
	}
	if altEnvKeySatisfied(nil, []string{"ORDINARY_CREDENTIAL_PATH"}, false) {
		t.Error("a hosted broker read its own env for an auth requirement")
	}
	if !altEnvKeySatisfied(map[string]string{"ORDINARY_CREDENTIAL_PATH": "/p"}, []string{"ORDINARY_CREDENTIAL_PATH"}, false) {
		t.Error("the resolved env did not satisfy an auth requirement on a hosted broker")
	}
	if altEnvKeySatisfied(nil, []string{"MISSING_VAR"}, true) {
		t.Error("an unset var satisfied an auth requirement")
	}
}
