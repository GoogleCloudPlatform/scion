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

func TestIsExcluded(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		excluded bool
	}{
		// Directory exclusions
		{"git dir", ".git", true},
		{"git subpath", ".git/objects/pack", true},
		{"scion dir", ".scion", true},
		{"scion subpath", ".scion/config.yaml", true},
		{"node_modules", "node_modules", true},
		{"node_modules subpath", "node_modules/lodash/index.js", true},

		// Extension exclusions
		{"env file", ".env", true},
		{"env file in subdir", "config/.env", true},
		{"dotenv production", "config/production.env", true},

		// Should NOT be excluded
		{"regular file", "main.go", false},
		{"nested file", "src/app/main.go", false},
		{"gitignore", ".gitignore", false},
		{"env-like name", "environment.go", false},
		{"root", "/", false},
		{"empty", "", false},
		{"dot", ".", false},

		// Leading slash normalization
		{"leading slash git", "/.git", true},
		{"leading slash file", "/main.go", false},
		{"leading slash scion", "/.scion/config", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isExcluded(tt.path)
			assert.Equal(t, tt.excluded, got, "isExcluded(%q)", tt.path)
		})
	}
}
