// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"net/url"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestListOptionsFromQuery(t *testing.T) {
	tests := []struct {
		name  string
		query url.Values
		want  store.ListOptions
	}{
		{
			name: "defaults",
			want: store.ListOptions{Limit: defaultListPageSize},
		},
		{
			name:  "positive limit and cursor",
			query: url.Values{"limit": {"125"}, "cursor": {"next-page"}},
			want:  store.ListOptions{Limit: 125, Cursor: "next-page"},
		},
		{
			name:  "zero limit uses default",
			query: url.Values{"limit": {"0"}},
			want:  store.ListOptions{Limit: defaultListPageSize},
		},
		{
			name:  "negative limit uses default",
			query: url.Values{"limit": {"-1"}},
			want:  store.ListOptions{Limit: defaultListPageSize},
		},
		{
			name:  "invalid limit uses default",
			query: url.Values{"limit": {"many"}},
			want:  store.ListOptions{Limit: defaultListPageSize},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, listOptionsFromQuery(tt.query))
		})
	}
}
