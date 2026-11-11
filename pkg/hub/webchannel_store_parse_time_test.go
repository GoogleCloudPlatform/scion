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

	"github.com/stretchr/testify/require"
)

func TestParseSQLiteTime(t *testing.T) {
	tests := []struct {
		name  string
		input string
		zero  bool
	}{
		{"empty", "", true},
		{"RFC3339Nano", "2026-01-01T00:00:00Z", false},
		{"RFC3339Nano with nanos", "2026-01-01T00:00:00.123456789Z", false},
		{"SQLite format with tz", "2026-01-01 00:00:00.000000000-07:00", false},
		{"SQLite format no tz", "2026-01-01 00:00:00.000000000", false},
		{"garbage", "not-a-time", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSQLiteTime(tt.input)
			require.Equal(t, tt.zero, result.IsZero(), "input: %q", tt.input)
		})
	}
}
