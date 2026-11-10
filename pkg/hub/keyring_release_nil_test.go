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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckKeyringRelease_NilKeyringRefused: the release check refuses a
// nil keyring with an error instead of panicking, through the exported
// entry point and the production check itself.
func TestCheckKeyringRelease_NilKeyringRefused(t *testing.T) {
	for name, check := range map[string]func() error{
		"CheckKeyringRelease": func() error { return CheckKeyringRelease(nil) },
		"releaseKeyringCheck": func() error { return releaseKeyringCheck(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() { err = check() })
			require.Error(t, err)
			assert.Equal(t, "keyring file: not loaded", err.Error())
		})
	}
}
