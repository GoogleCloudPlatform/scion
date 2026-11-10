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

package cmd

import "testing"

// testPrivateTempDir returns a directory for TMP/TEMP whose DACL a scion
// ssh temp directory may inherit: on Windows the runner's temp directory
// carries inheritable entries (CREATOR OWNER) that the private-file check
// refuses, so the tests use a private home instead (privateHome).
func testPrivateTempDir(t *testing.T) string {
	t.Helper()
	return privateHome(t)
}
