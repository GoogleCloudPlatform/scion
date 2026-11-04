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
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateSettingsFields_SendsBodyAsGiven(t *testing.T) {
	var gotPath string
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"defaultModel":"m","warnings":["w1"]}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)

	res, err := c.Projects().UpdateSettingsFields(context.Background(), "p/1", map[string]interface{}{
		"defaultModel":         "m",
		"defaultThinkingLevel": nil,
		"defaultTemplate":      "",
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/projects/p%2F1/settings", gotPath, "the project id is path-escaped")
	assert.Len(t, got, 3, "exactly the given keys")
	assert.JSONEq(t, `null`, string(got["defaultThinkingLevel"]), "an explicit null is sent")
	assert.JSONEq(t, `""`, string(got["defaultTemplate"]), "an empty string is sent")
	assert.Equal(t, "m", res.DefaultModel)
	assert.Equal(t, []string{"w1"}, res.Warnings)

	_, err = c.Projects().UpdateSettingsFields(context.Background(), "p", nil)
	require.Error(t, err, "an empty change is refused before any request")
}

func TestUpdateSettingsFields_RefusalIsSettingsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"validation_error","message":"invalid defaultMaxTurns -1","details":{"field":"defaultMaxTurns"}}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)
	_, err = c.Projects().UpdateSettingsFields(context.Background(), "p", map[string]interface{}{"defaultMaxTurns": -1})
	var se *SettingsError
	require.ErrorAs(t, err, &se)
	require.Len(t, se.Errors, 1)
	assert.Equal(t, "defaultMaxTurns", se.Errors[0].Field)
}

func TestGetSettingsBody_ReturnsBodyAsReceived(t *testing.T) {
	const settings = `{"telemetryEnabled":null,"defaultHarnessAuth":"********","futureSetting":{"b":1,"a":"x"}}`
	const resolved = `{"project":{},"settings":{"k":{"projectSet":false,"hubDefault":"unknown"}}}`
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects/p/1/settings/resolved" {
			_, _ = io.WriteString(w, resolved)
			return
		}
		if r.URL.Path == "/api/v1/projects/p/1/settings" {
			_, _ = io.WriteString(w, settings)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"no"}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)

	got, err := c.Projects().GetSettingsBody(context.Background(), "p/1")
	require.NoError(t, err)
	assert.Equal(t, settings, string(got), "the body byte for byte")
	got, err = c.Projects().GetResolvedSettingsBody(context.Background(), "p/1")
	require.NoError(t, err)
	assert.Equal(t, resolved, string(got), "the body byte for byte")
	mu.Lock()
	assert.Equal(t, []string{"/api/v1/projects/p%2F1/settings", "/api/v1/projects/p%2F1/settings/resolved"}, paths)
	mu.Unlock()

	body, err := c.Projects().GetSettingsBody(context.Background(), "other")
	assert.Nil(t, body, "a refusal returns no body")
	var api *apiclient.APIError
	require.ErrorAs(t, err, &api, "a refusal is an API error, not a body")
	assert.Equal(t, http.StatusForbidden, api.StatusCode)
}
