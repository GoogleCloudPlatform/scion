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

package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNew_BuiltinHarnesses(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"claude", "claude"},
		{"gemini", "gemini"},
		{"opencode", "opencode"},
		{"codex", "codex"},
	}

	for _, tt := range tests {
		h := New(tt.name)
		assert.Equal(t, tt.expected, h.Name())
	}
}

func TestNew_UnknownFallsToGeneric(t *testing.T) {
	h := New("unknown-harness")
	assert.Equal(t, "generic", h.Name())
}

func TestAll_ReturnsBuiltins(t *testing.T) {
	all := All()
	assert.Len(t, all, 4)
	names := make([]string, len(all))
	for i, h := range all {
		names[i] = h.Name()
	}
	assert.Contains(t, names, "gemini")
	assert.Contains(t, names, "claude")
	assert.Contains(t, names, "opencode")
	assert.Contains(t, names, "codex")
}
