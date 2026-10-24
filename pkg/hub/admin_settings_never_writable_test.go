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
)

// TestWithoutNeverAPIWritableKeys_NilAndEmpty pins that a nil key list
// stays nil and that an empty or fully hidden list stays a non-nil empty
// list.
func TestWithoutNeverAPIWritableKeys_NilAndEmpty(t *testing.T) {
	assert.Nil(t, withoutNeverAPIWritableKeys(nil))

	empty := withoutNeverAPIWritableKeys([]string{})
	assert.NotNil(t, empty)
	assert.Empty(t, empty)

	hidden := withoutNeverAPIWritableKeys([]string{"server.keyring_file", "server.keyring_file_allow_world_readable"})
	assert.NotNil(t, hidden)
	assert.Empty(t, hidden)
}
