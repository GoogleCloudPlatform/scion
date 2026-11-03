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

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// RelaxKeyringReleaseForTest replaces the release check
// (keyringReleaseCheck) with a no-op, as lifting the release limits will,
// and returns a function that restores it; callers install it with
// t.Cleanup. For tests only: it panics outside a test binary
// (testing.Testing), so no production path can disable the release
// refusal. It serves tests outside this package (cmd's load of the keyring
// file goes through CheckKeyringRelease), which an export_test.go accessor
// cannot reach. Not safe for parallel tests.
func RelaxKeyringReleaseForTest() (restore func()) {
	return relaxKeyringRelease(testing.Testing)
}

// relaxKeyringRelease is RelaxKeyringReleaseForTest with the test-binary
// predicate injected, so the refusal can be tested in-process.
func relaxKeyringRelease(inTestBinary func() bool) (restore func()) {
	if !inTestBinary() {
		panic("hub.RelaxKeyringReleaseForTest called outside a test binary")
	}
	prev := keyringReleaseCheck
	keyringReleaseCheck = func(*keyring.Keyring) error { return nil }
	return func() { keyringReleaseCheck = prev }
}
