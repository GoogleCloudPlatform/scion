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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisplayHubTemplateShow_SourceURL(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("text and JSON include redacted sourceUrl when set", func(t *testing.T) {
		rawURL := "https://user:ghp_secret123@github.com/acme/repo/tree/main/.scion/templates/my-tmpl?token=leak#frag"
		wantRedacted := "https://github.com/acme/repo/tree/main/.scion/templates/my-tmpl"
		match := &TemplateMatch{
			Name:     "my-tmpl",
			Location: LocationHubProject,
			HubTemplate: &hubclient.Template{
				ID:        "tpl-1",
				Name:      "my-tmpl",
				Harness:   "claude",
				Scope:     "project",
				Status:    "active",
				SourceURL: rawURL,
				Created:   now,
				Updated:   now,
			},
		}

		prev := outputFormat
		t.Cleanup(func() { outputFormat = prev })

		outputFormat = ""
		textOut := captureStdout(t, func() {
			require.NoError(t, displayHubTemplateShow(match))
		})
		assert.Contains(t, textOut, "Source URL: "+wantRedacted)
		assert.NotContains(t, textOut, "ghp_secret123")
		assert.NotContains(t, textOut, "token=leak")
		assert.NotContains(t, textOut, "#frag")

		outputFormat = "json"
		jsonOut := captureStdout(t, func() {
			require.NoError(t, displayHubTemplateShow(match))
		})
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
		assert.Equal(t, wantRedacted, parsed["sourceUrl"])
		assert.NotContains(t, jsonOut, "ghp_secret123")
		assert.NotContains(t, jsonOut, "token=leak")
		assert.NotContains(t, jsonOut, "#frag")
	})

	t.Run("text and JSON omit sourceUrl when empty", func(t *testing.T) {
		match := &TemplateMatch{
			Name:     "no-url",
			Location: LocationHubGlobal,
			HubTemplate: &hubclient.Template{
				ID:      "tpl-2",
				Name:    "no-url",
				Harness: "claude",
				Scope:   "global",
				Status:  "active",
				Created: now,
				Updated: now,
			},
		}

		prev := outputFormat
		t.Cleanup(func() { outputFormat = prev })

		outputFormat = ""
		textOut := captureStdout(t, func() {
			require.NoError(t, displayHubTemplateShow(match))
		})
		assert.NotContains(t, textOut, "Source URL:")

		outputFormat = "json"
		jsonOut := captureStdout(t, func() {
			require.NoError(t, displayHubTemplateShow(match))
		})
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
		_, hasSourceURL := parsed["sourceUrl"]
		assert.False(t, hasSourceURL, "sourceUrl key must be omitted when empty")
	})
}
