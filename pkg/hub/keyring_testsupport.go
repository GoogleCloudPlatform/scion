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

import "github.com/GoogleCloudPlatform/scion/pkg/keyring"

// RelaxKeyringReleaseForTest replaces the release check
// (keyringReleaseCheck) with a no-op, as lifting the release limits will,
// and returns a function that restores it; callers install it with
// t.Cleanup. It is for tests outside this package (cmd's load of the
// keyring file goes through CheckKeyringRelease), which an export_test.go
// accessor cannot reach. Production code never calls it. Not safe for
// parallel tests.
func RelaxKeyringReleaseForTest() (restore func()) {
	prev := keyringReleaseCheck
	keyringReleaseCheck = func(*keyring.Keyring, *ServerConfig) error { return nil }
	return func() { keyringReleaseCheck = prev }
}
