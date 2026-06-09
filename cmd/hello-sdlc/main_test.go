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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGreet(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "AC-001: default name",
			input:    "World",
			expected: "Hello, World! Built by the SDLC pipeline.",
		},
		{
			name:     "AC-002: custom name",
			input:    "Alice",
			expected: "Hello, Alice! Built by the SDLC pipeline.",
		},
		{
			name:     "AC-003: empty name",
			input:    "",
			expected: "Hello, ! Built by the SDLC pipeline.",
		},
		{
			name:     "AC-004: name with spaces",
			input:    "Jane Doe",
			expected: "Hello, Jane Doe! Built by the SDLC pipeline.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, Greet(tt.input))
		})
	}
}
