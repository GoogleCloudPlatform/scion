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

import "testing"

// commitGoldenPathEdits prepares and commits edits to dir's settings file
// under the settings lock, as the in-place editor's callers do.
func commitGoldenPathEdits(t *testing.T, dir string, edits []SettingsPathEdit) error {
	t.Helper()
	return withSettingsLock(dir, func(lock *SettingsLock) error {
		staged, err := PrepareSettingsPathEdits(dir, edits)
		if err != nil {
			return err
		}
		return staged.Commit(lock)
	})
}
