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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyResourceMetadataPatch(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantOK         bool
		wantStatus     int
		wantName       string
		wantSlug       string
		wantDisplay    string
		wantDesc       string
		wantVisibility string
	}{
		{
			name:           "updates fields and derives slug",
			body:           `{"name":"New Name","displayName":"New Display","description":"New description","visibility":"private"}`,
			wantOK:         true,
			wantName:       "New Name",
			wantSlug:       "new-name",
			wantDisplay:    "New Display",
			wantDesc:       "New description",
			wantVisibility: "private",
		},
		{
			name:           "explicit slug wins",
			body:           `{"name":"New Name","slug":"custom-slug"}`,
			wantOK:         true,
			wantName:       "New Name",
			wantSlug:       "custom-slug",
			wantDisplay:    "Old Display",
			wantDesc:       "Old description",
			wantVisibility: "public",
		},
		{
			name:           "empty null and unrelated fields are ignored",
			body:           `{"name":"","slug":null,"displayName":"","description":null,"visibility":"","unrelated":123}`,
			wantOK:         true,
			wantName:       "Old Name",
			wantSlug:       "old-slug",
			wantDisplay:    "Old Display",
			wantDesc:       "Old description",
			wantVisibility: "public",
		},
		{
			name:           "duplicate fields use last value",
			body:           `{"name":"First","name":"Second"}`,
			wantOK:         true,
			wantName:       "Second",
			wantSlug:       "second",
			wantDisplay:    "Old Display",
			wantDesc:       "Old description",
			wantVisibility: "public",
		},
		{
			name:           "wrong selected type fails before applying",
			body:           `{"name":"New Name","slug":123}`,
			wantOK:         false,
			wantStatus:     http.StatusBadRequest,
			wantName:       "Old Name",
			wantSlug:       "old-slug",
			wantDisplay:    "Old Display",
			wantDesc:       "Old description",
			wantVisibility: "public",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "Old Name"
			slug := "old-slug"
			displayName := "Old Display"
			description := "Old description"
			visibility := "public"
			req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			ok := applyResourceMetadataPatch(rec, req, resourceMetadataFields{
				Name:        &name,
				Slug:        &slug,
				DisplayName: &displayName,
				Description: &description,
				Visibility:  &visibility,
			})

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantStatus != 0 {
				assert.Equal(t, tt.wantStatus, rec.Code)
			}
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantSlug, slug)
			assert.Equal(t, tt.wantDisplay, displayName)
			assert.Equal(t, tt.wantDesc, description)
			assert.Equal(t, tt.wantVisibility, visibility)
		})
	}
}
