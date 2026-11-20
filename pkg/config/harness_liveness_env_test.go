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

package config

import (
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
)

// The bundled Codex harness runs its native TUI under an npm launcher, so
// it tells the liveness sampler the harness is two levels deep
// (ptone/scion#7855). Every other bundled harness runs its TUI directly
// and leaves the default.
func TestBundledHarnessConfigs_LivenessHarnessRootDepth(t *testing.T) {
	want := map[string]string{"codex": "2"}
	for _, name := range bundledHarnessNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := fs.ReadFile(harnesses.FS, name+"/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var entry HarnessConfigEntry
			if err := yaml.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
			if got := entry.Env[state.LivenessHarnessRootDepthHintEnv]; got != want[name] {
				t.Errorf("%s/config.yaml env %s = %q, want %q", name, state.LivenessHarnessRootDepthHintEnv, got, want[name])
			}
		})
	}
}

// Shipped harness configs must not set any SCION_* key in env or env_template:
// hints use HARNESS_HINT_* and SCION_AGENT_NAME is set by the runtime/harness.
func TestBundledHarnessConfigs_NoReservedScionEnvKeys(t *testing.T) {
	for _, name := range bundledHarnessNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := fs.ReadFile(harnesses.FS, name+"/config.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var entry HarnessConfigEntry
			if err := yaml.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
			for k := range entry.Env {
				if strings.HasPrefix(k, envpolicy.ReservedHarnessConfigPrefix) {
					t.Errorf("%s/config.yaml env sets reserved key %q", name, k)
				}
			}
			for k := range entry.EnvTemplate {
				if strings.HasPrefix(k, envpolicy.ReservedHarnessConfigPrefix) {
					t.Errorf("%s/config.yaml env_template sets reserved key %q", name, k)
				}
			}
		})
	}
}
