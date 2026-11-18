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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerPresetService_ListAndCompile(t *testing.T) {
	ctx := context.Background()

	var gotMethod, gotPath, gotQuery string
	var gotCompileReq CompileTriggerPresetRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
			_ = json.NewEncoder(w).Encode([]TriggerPreset{
				{
					ID:          "github.command",
					Source:      "github",
					Version:     1,
					Title:       "Slash command mention",
					Description: "Fires on /command",
					Fields: []TriggerPresetField{
						{Name: "command", Type: "string", Required: true, Help: "Command word"},
						{Name: "allow_bots", Type: "bool", Default: false, Required: false, Help: "Allow bots"},
					},
					ActionKinds: []string{"dispatch_agent", "message", "github.comment"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCompileReq))
			if gotCompileReq.Preset == "bad" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    "validation_error",
						"message": "preset validation failed",
						"details": map[string]any{
							"fields": []map[string]any{
								{"field": "preset", "code": "unknown_preset", "message": "unknown trigger preset"},
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(CompiledTriggerPreset{
				Match: CompiledTriggerMatch{
					Types: []string{"github.issue_comment.created"},
					Rules: []CompiledTriggerRule{{Dialect: "cel", Expr: "event.attributes.command == 'review'"}},
				},
				Subject: CompiledTriggerSubject{
					Mode: "new",
				},
				Form: CompiledTriggerForm{
					Preset:  gotCompileReq.Preset,
					Version: 1,
					Fields:  gotCompileReq.Fields,
				},
				SuggestedAction: &SuggestedTriggerAction{
					Type:          "dispatch_agent",
					AgentName:     "pr-${event.attributes.number}-review",
					Template:      "default",
					Task:          "Review PR",
					Branch:        "${event.attributes.head_ref}",
					Ephemeral:     false,
					GitHubToken:   "installation",
					AckComment:    &SuggestedTriggerAckComment{Body: "Started ${result.agent_name}"},
					UntrustedVars: []string{"event.attributes.number", "event.attributes.head_ref"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL)
	require.NoError(t, err)

	// 1. List with source filter
	presets, err := c.TriggerPresets().List(ctx, "github")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/trigger-presets", gotPath)
	assert.Equal(t, "source=github", gotQuery)
	require.Len(t, presets, 1)
	assert.Equal(t, "github.command", presets[0].ID)
	require.Len(t, presets[0].Fields, 2)
	assert.Equal(t, false, presets[0].Fields[1].Default)

	// 2. List without source filter
	_, err = c.TriggerPresets().List(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, "", gotQuery)

	// 3. Compile valid preset
	compiled, err := c.TriggerPresets().Compile(ctx, &CompileTriggerPresetRequest{
		Preset: "github.command",
		Fields: map[string]any{"command": "review"},
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/trigger-presets/compile", gotPath)
	assert.Equal(t, "github.command", gotCompileReq.Preset)
	require.Len(t, compiled.Match.Rules, 1)
	assert.Equal(t, "cel", compiled.Match.Rules[0].Dialect)
	require.NotNil(t, compiled.SuggestedAction)
	assert.Equal(t, "installation", compiled.SuggestedAction.GitHubToken)
	assert.False(t, compiled.SuggestedAction.Ephemeral)

	// 4. Compile nil request rejected client-side
	_, err = c.TriggerPresets().Compile(ctx, nil)
	require.Error(t, err)

	// 5. Compile 400 validation error surfaces per-field details on *apiclient.APIError
	_, err = c.TriggerPresets().Compile(ctx, &CompileTriggerPresetRequest{Preset: "bad"})
	require.Error(t, err)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, apiclient.ErrCodeValidationError, apiErr.Code)
	require.Contains(t, apiErr.Details, "fields")
}
