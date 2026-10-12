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

func TestSlugifyAccountID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-pipeline", "my-pipeline"},
		{"My Pipeline", "my-pipeline"},
		{"my_pipeline_123", "my-pipeline-123"},
		{"My--Pipeline", "my-pipeline"},
		{"-leading-trailing-", "leading-trailing"},
		{"UPPERCASE", "uppercase"},
		{"a!b@c#d", "a-b-c-d"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, slugifyAccountID(tt.input))
		})
	}
}

func TestGCPSAAccountIDRegexp(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"scion-abc123", true},
		{"scion-a1b2c3d4", true},
		{"scion-my-pipeline", true},
		{"abc", true},        // matches pattern (length checked separately)
		{"1scion", false},    // starts with digit
		{"scion-", false},    // ends with hyphen
		{"-scion", false},    // starts with hyphen
		{"scion_abc", false}, // underscore not allowed
		{"SCION-ABC", false}, // uppercase not allowed
		{"scion-a", true},    // minimum valid pattern
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.valid, gcpSAAccountIDRegexp.MatchString(tt.input))
		})
	}
}

func TestGenerateRandomAccountID(t *testing.T) {
	id, err := generateRandomAccountID()
	assert.NoError(t, err)
	assert.True(t, len(id) >= 6 && len(id) <= 30, "ID %q should be 6-30 chars", id)
	assert.Regexp(t, `^scion-[a-f0-9]{8}$`, id)

	// Verify uniqueness (probabilistic but practically guaranteed)
	id2, err := generateRandomAccountID()
	assert.NoError(t, err)
	assert.NotEqual(t, id, id2)
}
