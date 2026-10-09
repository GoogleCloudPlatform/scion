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

package runtime

import (
	"testing"
)

// TestK8sBuildPod_LogLevelEnv covers ptone/scion#4098 for Kubernetes agents.
// The broker no longer adds SCION_DEBUG to agent environments, so a debug
// level only reaches an agent through an explicit SCION_LOG_LEVEL in the
// agent env. That value must arrive in the pod's container env unchanged,
// and the runtime must not add either variable on its own.
func TestK8sBuildPod_LogLevelEnv(t *testing.T) {
	const unset = "<unset>"
	tests := []struct {
		name         string
		env          []string
		wantLogLevel string
	}{
		{
			name:         "explicit level reaches the pod",
			env:          []string{"SCION_LOG_LEVEL=debug"},
			wantLogLevel: "debug",
		},
		{
			name:         "per-component spec is kept verbatim",
			env:          []string{"SCION_LOG_LEVEL=warn,hub.auth=debug"},
			wantLogLevel: "warn,hub.auth=debug",
		},
		{
			name:         "no level set adds none",
			env:          []string{"SCION_AGENT_ID=agent-1"},
			wantLogLevel: unset,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			pod, err := rt.buildPod("default", RunConfig{
				Name:         "log-level-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
				Env:          tt.env,
			})
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			env := pod.Spec.Containers[0].Env

			got := func(name string) string {
				t.Helper()
				value, count := unset, 0
				for _, e := range env {
					if e.Name == name {
						count++
						value = e.Value
					}
				}
				if count > 1 {
					t.Errorf("%s appears %d times in the container env", name, count)
				}
				return value
			}

			if v := got("SCION_LOG_LEVEL"); v != tt.wantLogLevel {
				t.Errorf("SCION_LOG_LEVEL = %q, want %q", v, tt.wantLogLevel)
			}
			if v := got("SCION_DEBUG"); v != unset {
				t.Errorf("SCION_DEBUG = %q, want it unset", v)
			}
		})
	}
}
