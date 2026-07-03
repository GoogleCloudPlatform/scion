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

func TestNew_EmbedFSHarnesses(t *testing.T) {
	h := New("claude")
	assert.Equal(t, "claude", h.Name())

	h = New("gemini-cli")
	assert.Equal(t, "gemini-cli", h.Name())
}

func TestNew_UnknownFallsToGeneric(t *testing.T) {
	h := New("unknown-harness")
	assert.Equal(t, "generic", h.Name())
}

func TestEmbedOnlyHarnesses_ReturnsEmpty(t *testing.T) {
	all := EmbedOnlyHarnesses()
	assert.Empty(t, all)
}

func TestAllHarnessNames_IncludesAll(t *testing.T) {
	names := AllHarnessNames()
	assert.Contains(t, names, "claude")
	assert.Contains(t, names, "gemini-cli")
	assert.Contains(t, names, "codex")
	assert.Contains(t, names, "opencode")
	assert.Contains(t, names, "antigravity")
	assert.Contains(t, names, "copilot")
	assert.Contains(t, names, "hermes")
}
