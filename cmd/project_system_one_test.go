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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestProjectSystemOneCLI_ListAddRemoveSetAndGates(t *testing.T) {
	// Agent mode must NOT allow any project.system-one command; Windows gate MUST allow them.
	for _, path := range []string{
		"project.system-one",
		"project.system-one.list",
		"project.system-one.add",
		"project.system-one.remove",
		"project.system-one.set",
	} {
		assert.False(t, agentAllowed[path], "%s must be hidden in agent mode", path)
		assert.True(t, windowsAllowed[path], "%s must be allowed on Windows (pure Hub API client)", path)
	}

	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-123/system-one/instances":
			_ = json.NewEncoder(w).Encode(hubclient.ProjectSystemOneInstancesResponse{
				ProjectID: "proj-123",
				Instances: []string{"gemini-a"},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/proj-123/system-one/instances/typesafe-b":
			_ = json.NewEncoder(w).Encode(hubclient.ProjectSystemOneInstancesResponse{
				ProjectID: "proj-123",
				Instances: []string{"gemini-a", "typesafe-b"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-123/system-one/instances/gemini-a":
			_ = json.NewEncoder(w).Encode(hubclient.ProjectSystemOneInstancesResponse{
				ProjectID: "proj-123",
				Instances: []string{"typesafe-b"},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/proj-123/system-one/instances":
			var req hubclient.SetProjectSystemOneInstancesRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			_ = json.NewEncoder(w).Encode(hubclient.ProjectSystemOneInstancesResponse{
				ProjectID: "proj-123",
				Instances: req.Instances,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origLoader := projectSystemOneClientLoader
	origJSON := hubOutputJSON
	origFmt := outputFormat
	origClear := projectSystemOneClear
	t.Cleanup(func() {
		projectSystemOneClientLoader = origLoader
		hubOutputJSON = origJSON
		outputFormat = origFmt
		projectSystemOneClear = origClear
		projectSystemOneListCmd.SetOut(nil)
		projectSystemOneAddCmd.SetOut(nil)
		projectSystemOneRemoveCmd.SetOut(nil)
		projectSystemOneSetCmd.SetOut(nil)
	})
	projectSystemOneClientLoader = func() (hubclient.SystemOneService, string, error) {
		return client.SystemOne(), "proj-123", nil
	}
	hubOutputJSON = false
	outputFormat = ""
	projectSystemOneClear = false

	// list
	var out bytes.Buffer
	projectSystemOneListCmd.SetOut(&out)
	require.NoError(t, projectSystemOneListCmd.RunE(projectSystemOneListCmd, nil))
	assert.Contains(t, out.String(), "gemini-a")

	// add
	out.Reset()
	projectSystemOneAddCmd.SetOut(&out)
	require.NoError(t, projectSystemOneAddCmd.RunE(projectSystemOneAddCmd, []string{"typesafe-b"}))
	assert.Contains(t, out.String(), "typesafe-b")

	// remove
	out.Reset()
	projectSystemOneRemoveCmd.SetOut(&out)
	require.NoError(t, projectSystemOneRemoveCmd.RunE(projectSystemOneRemoveCmd, []string{"gemini-a"}))
	assert.NotContains(t, out.String(), "gemini-a")

	// set --clear
	out.Reset()
	projectSystemOneClear = true
	projectSystemOneSetCmd.SetOut(&out)
	require.NoError(t, projectSystemOneSetCmd.RunE(projectSystemOneSetCmd, nil))
	assert.Contains(t, out.String(), "no allowed System One instances")

	assert.Equal(t, []string{
		"GET /api/v1/projects/proj-123/system-one/instances",
		"PUT /api/v1/projects/proj-123/system-one/instances/typesafe-b",
		"DELETE /api/v1/projects/proj-123/system-one/instances/gemini-a",
		"PUT /api/v1/projects/proj-123/system-one/instances",
	}, calls)
}
