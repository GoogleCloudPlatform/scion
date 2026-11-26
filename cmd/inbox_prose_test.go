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

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inboxProseFixture struct {
	Cases []struct {
		Name      string         `json:"name"`
		Category  string         `json:"category"`
		Count     int            `json:"count"`
		Capped    bool           `json:"capped"`
		Breakdown map[string]int `json:"breakdown"`
		Expected  string         `json:"expected"`
	} `json:"cases"`
}

func TestFormatInboxProse_SharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "pkg", "hubclient", "testdata", "inbox_prose.json"))
	require.NoError(t, err)

	var fixture inboxProseFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Cases)

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := formatInboxProse(tc.Category, tc.Count, tc.Capped, tc.Breakdown)
			assert.Equal(t, tc.Expected, got)
		})
	}
}
