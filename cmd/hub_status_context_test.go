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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubEndpointScope_Context: hub status reports a hub selected with
// 'scion use' as source "context", not as the --hub flag.
func TestHubEndpointScope_Context(t *testing.T) {
	for _, tc := range []struct {
		name, flag, wantSource string
	}{
		{name: "context hub", wantSource: "context"},
		{name: "--hub flag", flag: "https://flag.example.com", wantSource: "flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContextTest(t)
			saveTestContext(t, ctxTestHub, ctxTestProjectID)
			hubEndpoint = tc.flag
			require.NoError(t, resolveInvocationTarget(findCommand(t, "hub", "status"), ModeHuman))
			scope := getHubEndpointScope("", true, nil)
			assert.Equal(t, tc.wantSource, scope.Source)
			assert.False(t, scope.Inherited)
		})
	}
}

func TestEndpointSourceNote(t *testing.T) {
	assert.Equal(t, "            (current context)", endpointSourceNote(hubEndpointScope{Source: "context"}))
	assert.Equal(t, "            (from --hub flag)", endpointSourceNote(hubEndpointScope{Source: "flag"}))
	assert.Equal(t, "            (inherited from global)", endpointSourceNote(hubEndpointScope{Source: "global", Inherited: true}))
	assert.Empty(t, endpointSourceNote(hubEndpointScope{Source: "project"}))
	assert.Empty(t, endpointSourceNote(hubEndpointScope{Source: "none"}))
}

// TestHubStatusJSON_ContextSource: 'scion hub status --format json' with
// the hub from the current context reports endpointSource "context". The
// hub does not answer; status still reports the configuration.
func TestHubStatusJSON_ContextSource(t *testing.T) {
	setupContextTest(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	saveTestContext(t, url, ctxTestProjectID)
	status := findCommand(t, "hub", "status")
	require.NoError(t, resolveInvocationTarget(status, ModeHuman))
	outputFormat = "json"

	out := captureStdout(t, func() { require.NoError(t, runHubStatus(status, nil)) })
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, url, got["endpoint"])
	assert.Equal(t, "context", got["endpointSource"])
	assert.Equal(t, false, got["endpointInherited"])
}
