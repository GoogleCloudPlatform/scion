//go:build !no_sqlite

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

package hub

import "testing"

// TestWorkstation_PutServerConfig_ProjectStorageFieldsValidated: a
// workstation PUT of an invalid server.project_storage is refused with
// 400 and the field errors; settings.yaml is unchanged and nothing is
// reported as requiring a restart.
func TestWorkstation_PutServerConfig_ProjectStorageFieldsValidated(t *testing.T) {
	for body, want := range invalidProjectStorageBodies {
		t.Run(want, func(t *testing.T) {
			settingsPath := workstationHome(t)
			srv, _, _ := newSQLiteHubInMode(t, true, nil)
			before := readFileString(t, settingsPath)
			rr := putServerConfig(t, srv, body)
			assertProjectStorageFieldRefusal(t, rr, want)
			if after := readFileString(t, settingsPath); after != before {
				t.Errorf("settings.yaml changed:\n%s", after)
			}
		})
	}
}
