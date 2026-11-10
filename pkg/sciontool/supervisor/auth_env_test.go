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
	"testing"
)

// authEnvTestConfig is a harness child config with an overlay and a secret
// override, so the AuthEnv step is checked between the other steps.
func authEnvTestConfig(authEnv map[string]string) Config {
	return Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"OVERLAY_ONLY": "o", "EXAMPLE_API_KEY": "${EXAMPLE_API_KEY}"},
		SecretOverrides: map[string]string{"FETCHED": "f"},
		AuthEnv:         authEnv,
	}
}

var authEnvTestBase = []string{"PATH=/usr/bin", "HOME=/root", "GOOGLE_CLOUD_PROJECT=from-runtime", "FETCHED="}

// A nil or empty AuthEnv leaves the harness child's environment exactly as
// without the field (every runtime but Cloud Run passes nil).
func TestChildEnv_NilOrEmptyAuthEnvIsNoOp(t *testing.T) {
	without, err := ChildEnv(Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"OVERLAY_ONLY": "o", "EXAMPLE_API_KEY": "${EXAMPLE_API_KEY}"},
		SecretOverrides: map[string]string{"FETCHED": "f"},
	}, authEnvTestBase)
	if err != nil {
		t.Fatal(err)
	}
	for name, authEnv := range map[string]map[string]string{"nil": nil, "empty": {}} {
		got, err := ChildEnv(authEnvTestConfig(authEnv), authEnvTestBase)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, without) {
			t.Errorf("%s AuthEnv changed the child environment; keys: %v, want %v", name, envKeys(got), envKeys(without))
		}
	}
}

// AuthEnv fills names the runtime environment lacks, never replaces a
// runtime value, and wins over the harness overlay's placeholder text.
func TestChildEnv_AuthEnvFillsAbsentBeforeOverlay(t *testing.T) {
	got, err := ChildEnv(authEnvTestConfig(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "from-auth",
		"EXAMPLE_API_KEY":      "placeholder-auth-key",
	}), authEnvTestBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"GOOGLE_CLOUD_PROJECT=from-runtime", "EXAMPLE_API_KEY=placeholder-auth-key", "OVERLAY_ONLY=o", "FETCHED=f"} {
		if !slices.Contains(got, kv) {
			t.Errorf("child env lacks the expected value of %s; keys: %v", envKey(kv), envKeys(got))
		}
	}
	count := 0
	for _, kv := range got {
		switch envKey(kv) {
		case "EXAMPLE_API_KEY":
			count++
		case "GOOGLE_CLOUD_PROJECT":
			if kv != "GOOGLE_CLOUD_PROJECT=from-runtime" {
				t.Error("AuthEnv replaced a runtime value")
			}
		}
	}
	if count != 1 {
		t.Errorf("EXAMPLE_API_KEY appears %d times, want 1", count)
	}
}

// SSH sessions do not receive AuthEnv (harness child only).
func TestSSHSessionEnvWithoutAuthEnv(t *testing.T) {
	got, err := SSHSessionEnv(authEnvTestConfig(map[string]string{"AUTH_ONLY": "a"}), authEnvTestBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range got {
		if envKey(kv) == "AUTH_ONLY" {
			t.Error("an SSH session received an AuthEnv entry")
		}
	}
}
