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

//go:build !linux

package control

import "testing"

// TestExecRoutesOffOnThisPlatform: off Linux the exec routes are never
// enabled, even for an agent launched with the experiment, and a run
// cannot start.
func TestExecRoutesOffOnThisPlatform(t *testing.T) {
	getenv := func(k string) string {
		if k == EnvHubExperiments {
			return ExperimentExecSchedules
		}
		return ""
	}
	if ExecEnabled(getenv) {
		t.Fatal("ExecEnabled reported true off Linux")
	}
	if _, err := startExec(execSpec{}); err == nil {
		t.Fatal("startExec succeeded off Linux")
	}
	h := New(Options{KickTokenRefresh: func() {}})
	for _, r := range h.Routes() {
		if r == "exec-start" || r == "exec-status" {
			t.Fatalf("route %s registered without exec options", r)
		}
	}
}
