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

package state

import "testing"

func TestIsShellExecContainerEnv(t *testing.T) {
	idleReady := `{"strategy":"idle"}`
	delayReady := `{"strategy":"delay","timeout_ms":15000}`

	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "nil getenv",
			env:  nil,
			want: false,
		},
		{
			name: "READY idle + SCION_HARNESS=claude => true",
			env:  map[string]string{HarnessReadyEnv: idleReady, "SCION_HARNESS": "claude"},
			want: true,
		},
		{
			name: "READY delay + SCION_HARNESS=shell => false",
			env:  map[string]string{HarnessReadyEnv: delayReady, "SCION_HARNESS": "shell"},
			want: false,
		},
		{
			name: "READY absent + SCION_HARNESS=shell => true (old-broker fallback)",
			env:  map[string]string{"SCION_HARNESS": "shell"},
			want: true,
		},
		{
			name: "READY idle + SCION_HARNESS=shell => true",
			env:  map[string]string{HarnessReadyEnv: idleReady, "SCION_HARNESS": "shell"},
			want: true,
		},
		{
			name: "READY idle + SCION_HARNESS unset => true",
			env:  map[string]string{HarnessReadyEnv: idleReady},
			want: true,
		},
		{
			name: "READY delay + SCION_HARNESS unset => false",
			env:  map[string]string{HarnessReadyEnv: delayReady},
			want: false,
		},
		{
			name: "READY malformed + SCION_HARNESS=shell => false (fails closed when READY present)",
			env:  map[string]string{HarnessReadyEnv: "not-json", "SCION_HARNESS": "shell"},
			want: false,
		},
		{
			name: "READY absent + SCION_HARNESS=claude => false",
			env:  map[string]string{"SCION_HARNESS": "claude"},
			want: false,
		},
		{
			name: "both absent => false",
			env:  map[string]string{},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var getenv func(string) string
			if tc.env != nil {
				getenv = func(k string) string { return tc.env[k] }
			}
			if got := IsShellExecContainerEnv(getenv); got != tc.want {
				t.Errorf("IsShellExecContainerEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}
