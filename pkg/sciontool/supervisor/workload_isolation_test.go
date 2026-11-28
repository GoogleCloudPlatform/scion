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

package supervisor

import (
	"slices"
	"strings"
	"testing"
)

// TestWorkloadIsolation_StripsAllScionControlEnv verifies §4.3.2
// (ptone/scion#9743): for ChildEnv, SSHSessionEnv, and HarnessShellEnv with
// WorkloadIsolation enabled, every SCION_* variable (including each brief-listed
// name and any SCION_* injected via overlay/secrets/auth) is stripped, while
// HOME, USER, LOGNAME, PATH (with SCION_EXTRA_PATH applied), user env,
// EnvOverlay, and (for ChildEnv/HarnessShellEnv) AuthEnv and SecretOverrides
// survive.
func TestWorkloadIsolation_StripsAllScionControlEnv(t *testing.T) {
	briefNamedKeys := []string{
		"SCION_CLI_MODE",
		"SCION_AGENT_ID",
		"SCION_AGENT_SLUG",
		"SCION_AGENT_NAME",
		"SCION_PROJECT_ID",
		"SCION_PROJECT",
		"SCION_HOST_UID",
		"SCION_HOST_GID",
		"SCION_LAUNCH_ID",
		"SCION_HUB_EXPERIMENTS",
		"SCION_HARNESS",
		"SCION_HARNESS_READY",
		"SCION_HARNESS_DELIVERY_DISABLED",
		"SCION_AUTH_TOKEN",
		"SCION_TRANSPORT_TOKEN",
		"SCION_TRANSPORT_TOKEN_FILE",
		"SCION_HUB_ENDPOINT",
		"SCION_HUB_URL",
	}
	extraPrefixKeys := []string{
		"SCION_HUB_TOKEN",
		"SCION_TRANSPORT_MODE",
		"SCION_TRANSPORT_AUDIENCE",
		"SCION_SECRET_KEYS",
		"SCION_CREATOR",
		"SCION_BROKER_NAME",
		"SCION_TEMPLATE",
		"SCION_MODEL",
		"SCION_FUTURE_CONTROL_VAR",
	}

	base := []string{
		"PATH=/usr/bin:/bin",
		"SCION_EXTRA_PATH=/opt/tools/bin",
		"HOME=/root",
		"TERM=xterm-256color",
		"USER_VAR=hello",
		"MY_SECRET=",
	}
	for _, k := range briefNamedKeys {
		base = append(base, k+"=val-"+k)
	}
	for _, k := range extraPrefixKeys {
		base = append(base, k+"=val-"+k)
	}

	cfg := Config{
		UID:               1000,
		GID:               1000,
		Username:          "scion",
		WorkloadIsolation: true,
		AuthEnv: map[string]string{
			"AUTH_SURVIVES":    "auth-ok",
			"SCION_AUTH_EXTRA": "must-strip",
		},
		EnvOverlay: map[string]string{
			"OVERLAY_SURVIVES":    "overlay-ok",
			"SCION_OVERLAY_EXTRA": "must-strip",
		},
		SecretOverrides: map[string]string{
			"MY_SECRET":          "secret-ok",
			"SCION_SECRET_EXTRA": "must-strip",
		},
	}

	builders := []struct {
		name         string
		build        func(Config, []string) ([]string, error)
		applySecrets bool
	}{
		{name: "ChildEnv", build: ChildEnv, applySecrets: true},
		{name: "SSHSessionEnv", build: SSHSessionEnv, applySecrets: false},
		{name: "HarnessShellEnv", build: HarnessShellEnv, applySecrets: true},
	}

	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			got, err := b.build(cfg, base)
			if err != nil {
				t.Fatalf("%s returned error: %v", b.name, err)
			}

			gotKeys := make(map[string]string, len(got))
			for _, kv := range got {
				k, v, _ := strings.Cut(kv, "=")
				gotKeys[k] = v
				if strings.HasPrefix(k, "SCION_") {
					t.Errorf("%s retained SCION_* variable %s", b.name, k)
				}
			}

			// Assert every brief-listed name is absent individually.
			for _, k := range briefNamedKeys {
				if _, present := gotKeys[k]; present {
					t.Errorf("%s must strip %s", b.name, k)
				}
			}
			for _, k := range extraPrefixKeys {
				if _, present := gotKeys[k]; present {
					t.Errorf("%s must strip %s", b.name, k)
				}
			}

			// Non-SCION entries must survive, including SCION_EXTRA_PATH applied to PATH.
			wantPresent := map[string]string{
				"HOME":             "/home/scion",
				"USER":             "scion",
				"LOGNAME":          "scion",
				"PATH":             "/opt/tools/bin:/usr/bin:/bin",
				"TERM":             "xterm-256color",
				"USER_VAR":         "hello",
				"OVERLAY_SURVIVES": "overlay-ok",
			}
			if b.applySecrets {
				wantPresent["AUTH_SURVIVES"] = "auth-ok"
				wantPresent["MY_SECRET"] = "secret-ok"
			}
			for k, wantV := range wantPresent {
				if gotV, ok := gotKeys[k]; !ok || gotV != wantV {
					t.Errorf("%s: %s = %q (present=%v), want %q", b.name, k, gotV, ok, wantV)
				}
			}
		})
	}
}

// TestWorkloadIsolation_OffPreservesExistingBehavior verifies A10: when
// WorkloadIsolation is false (non-shell LLM agents), ChildEnv, SSHSessionEnv,
// and HarnessShellEnv keep SCION_* variables unchanged and match the baseline
// output byte-for-byte.
func TestWorkloadIsolation_OffPreservesExistingBehavior(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"SCION_EXTRA_PATH=/opt/x",
		"HOME=/root",
		"SCION_CLI_MODE=agent",
		"SCION_AGENT_ID=agent-123",
		"SCION_HUB_ENDPOINT=https://hub.example.com",
		"SCION_HARNESS=claude",
		"USER_VAR=ok",
		"API_KEY=",
	}
	cfg := Config{
		UID:               1000,
		GID:               1000,
		Username:          "scion",
		WorkloadIsolation: false,
		AuthEnv:           map[string]string{"AUTH_KEY": "a"},
		EnvOverlay:        map[string]string{"OVERLAY_KEY": "o"},
		SecretOverrides:   map[string]string{"API_KEY": "fetched"},
	}

	wantChild := []string{
		"PATH=/opt/x:/usr/bin",
		"HOME=/home/scion",
		"SCION_CLI_MODE=agent",
		"SCION_AGENT_ID=agent-123",
		"SCION_HUB_ENDPOINT=https://hub.example.com",
		"SCION_HARNESS=claude",
		"USER_VAR=ok",
		"API_KEY=fetched",
		"USER=scion",
		"LOGNAME=scion",
		"AUTH_KEY=a",
		"OVERLAY_KEY=o",
	}
	wantSSH := []string{
		"PATH=/opt/x:/usr/bin",
		"HOME=/home/scion",
		"SCION_CLI_MODE=agent",
		"SCION_AGENT_ID=agent-123",
		"SCION_HUB_ENDPOINT=https://hub.example.com",
		"SCION_HARNESS=claude",
		"USER_VAR=ok",
		"API_KEY=",
		"USER=scion",
		"LOGNAME=scion",
		"OVERLAY_KEY=o",
	}

	gotChild, err := ChildEnv(cfg, base)
	if err != nil {
		t.Fatalf("ChildEnv: %v", err)
	}
	if !slices.Equal(gotChild, wantChild) {
		t.Errorf("ChildEnv (isolation off) =\n%q\nwant:\n%q", gotChild, wantChild)
	}

	gotHarness, err := HarnessShellEnv(cfg, base)
	if err != nil {
		t.Fatalf("HarnessShellEnv: %v", err)
	}
	if !slices.Equal(gotHarness, wantChild) {
		t.Errorf("HarnessShellEnv (isolation off) =\n%q\nwant:\n%q", gotHarness, wantChild)
	}

	gotSSH, err := SSHSessionEnv(cfg, base)
	if err != nil {
		t.Fatalf("SSHSessionEnv: %v", err)
	}
	if !slices.Equal(gotSSH, wantSSH) {
		t.Errorf("SSHSessionEnv (isolation off) =\n%q\nwant:\n%q", gotSSH, wantSSH)
	}
}
